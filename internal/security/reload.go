package security

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultReloadInterval is the file polling interval for reloadable TLS
// material when callers do not select one explicitly.
const DefaultReloadInterval = 10 * time.Second

// ReloadStats is a point-in-time snapshot of file-backed TLS reload activity.
type ReloadStats struct {
	Version     uint64
	Attempts    uint64
	Successes   uint64
	Failures    uint64
	LastAttempt time.Time
	LastSuccess time.Time
	LastError   string
}

// ReloadEvent reports one attempted material change. Error is nil when the
// new snapshot was validated and published.
type ReloadEvent struct {
	Stats ReloadStats
	Error error
}

// ReloadOptions controls background polling and change notifications.
type ReloadOptions struct {
	Interval time.Duration
	OnReload func(ReloadEvent)
}

type reloadKind string

const (
	reloadServer reloadKind = "server"
	reloadClient reloadKind = "client"
)

type reloadSource struct {
	kind       reloadKind
	certFile   string
	keyFile    string
	clientCA   string
	caFile     string
	serverName string
	minVersion uint16
}

// Reloader owns an atomically replaceable, validated TLS configuration.
// Failed reads or parsing never replace the last known-good snapshot.
type Reloader struct {
	source   reloadSource
	interval time.Duration
	onReload func(ReloadEvent)

	reloadMu  sync.Mutex
	signature string
	current   atomic.Pointer[tls.Config]

	statsMu sync.Mutex
	stats   ReloadStats
}

// NewReloadableServerConfig performs the initial strict load and prepares a
// reloader for the same server certificate, key, and optional client CA files.
// Empty certificate paths return a nil Reloader, matching LoadServerConfig.
func NewReloadableServerConfig(
	certFile string,
	keyFile string,
	clientCAFile string,
	minVersion uint16,
	options ReloadOptions,
) (*Reloader, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("TLS certificate and key must be configured together")
	}
	if certFile == "" {
		if clientCAFile != "" {
			return nil, fmt.Errorf("TLS client CA requires a server certificate and key")
		}
		return nil, nil
	}
	normalized, err := normalizeMinVersion(minVersion)
	if err != nil {
		return nil, err
	}
	return newReloader(reloadSource{
		kind:       reloadServer,
		certFile:   certFile,
		keyFile:    keyFile,
		clientCA:   clientCAFile,
		minVersion: normalized,
	}, options)
}

// NewReloadableClientConfig performs the initial strict load and prepares a
// reloader for root CA, client certificate, and client key rotation. An empty
// TLS configuration returns a nil Reloader, matching LoadClientConfig.
func NewReloadableClientConfig(
	caFile string,
	certFile string,
	keyFile string,
	serverName string,
	minVersion uint16,
	options ReloadOptions,
) (*Reloader, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("TLS client certificate and key must be configured together")
	}
	if caFile == "" && certFile == "" && serverName == "" {
		return nil, nil
	}
	normalized, err := normalizeMinVersion(minVersion)
	if err != nil {
		return nil, err
	}
	return newReloader(reloadSource{
		kind:       reloadClient,
		certFile:   certFile,
		keyFile:    keyFile,
		caFile:     caFile,
		serverName: strings.TrimSpace(serverName),
		minVersion: normalized,
	}, options)
}

func newReloader(source reloadSource, options ReloadOptions) (*Reloader, error) {
	interval := options.Interval
	if interval <= 0 {
		interval = DefaultReloadInterval
	}
	signature, config, err := source.load()
	if err != nil {
		return nil, err
	}
	reloader := &Reloader{
		source:    source,
		interval:  interval,
		onReload:  options.OnReload,
		signature: signature,
		stats: ReloadStats{
			Version:     1,
			LastSuccess: time.Now().UTC(),
		},
	}
	reloader.current.Store(config)
	return reloader, nil
}

// Current returns a defensive clone of the last validated TLS snapshot.
func (reloader *Reloader) Current() *tls.Config {
	if reloader == nil {
		return nil
	}
	config := reloader.current.Load()
	if config == nil {
		return nil
	}
	return config.Clone()
}

// Stats returns a consistent reload activity snapshot.
func (reloader *Reloader) Stats() ReloadStats {
	if reloader == nil {
		return ReloadStats{}
	}
	reloader.statsMu.Lock()
	defer reloader.statsMu.Unlock()
	return reloader.stats
}

// Poll checks the configured files and atomically publishes a validated
// change. It returns false without failure when the file contents are
// unchanged. Parsing or validation errors retain the previous snapshot.
func (reloader *Reloader) Poll() (bool, error) {
	return reloader.reload(false)
}

// Reload forces one file load even when the content signature is unchanged.
func (reloader *Reloader) Reload() error {
	_, err := reloader.reload(true)
	return err
}

func (reloader *Reloader) reload(force bool) (bool, error) {
	if reloader == nil {
		return false, fmt.Errorf("TLS reloader is nil")
	}
	reloader.reloadMu.Lock()
	signature, config, loadErr := reloader.source.load()
	attemptedAt := time.Now().UTC()
	if loadErr == nil && !force && signature == reloader.signature {
		reloader.reloadMu.Unlock()
		return false, nil
	}

	var event ReloadEvent
	if loadErr != nil {
		event = reloader.recordFailureLocked(loadErr, attemptedAt)
	} else {
		reloader.current.Store(config)
		reloader.signature = signature
		event = reloader.recordSuccessLocked(attemptedAt)
	}
	reloader.reloadMu.Unlock()
	reloader.notify(event)
	if event.Error != nil {
		return false, event.Error
	}
	return true, nil
}

func (reloader *Reloader) recordSuccessLocked(at time.Time) ReloadEvent {
	reloader.statsMu.Lock()
	defer reloader.statsMu.Unlock()
	reloader.stats.Version++
	reloader.stats.Attempts++
	reloader.stats.Successes++
	reloader.stats.LastAttempt = at
	reloader.stats.LastSuccess = at
	reloader.stats.LastError = ""
	return ReloadEvent{Stats: reloader.stats}
}

func (reloader *Reloader) recordFailureLocked(err error, at time.Time) ReloadEvent {
	reloader.statsMu.Lock()
	defer reloader.statsMu.Unlock()
	reloader.stats.Attempts++
	reloader.stats.Failures++
	reloader.stats.LastAttempt = at
	reloader.stats.LastError = err.Error()
	return ReloadEvent{Stats: reloader.stats, Error: err}
}

func (reloader *Reloader) notify(event ReloadEvent) {
	if reloader.onReload != nil {
		reloader.onReload(event)
	}
}

// Run polls at the configured interval until ctx is canceled.
func (reloader *Reloader) Run(ctx context.Context) {
	if reloader == nil || ctx == nil {
		return
	}
	ticker := time.NewTicker(reloader.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = reloader.Poll()
		}
	}
}

func (source reloadSource) load() (string, *tls.Config, error) {
	files := source.files()
	contents := make(map[string][]byte, len(files))
	contentHash := sha256.New()
	_, _ = fmt.Fprintf(contentHash, "%s\x00%04x\x00%s\x00", source.kind, source.minVersion, source.serverName)
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, fmt.Errorf("read TLS material %s: %w", path, err)
		}
		contents[path] = data
		_, _ = fmt.Fprintf(contentHash, "%s\x00%d\x00", path, len(data))
		_, _ = contentHash.Write(data)
		_, _ = contentHash.Write([]byte{0})
	}
	signature := hex.EncodeToString(contentHash.Sum(nil))

	var config *tls.Config
	var err error
	switch source.kind {
	case reloadServer:
		config, err = buildServerConfig(
			contents[source.certFile],
			contents[source.keyFile],
			optionalContent(contents, source.clientCA),
			source.minVersion,
		)
	case reloadClient:
		config, err = buildClientConfig(
			optionalContent(contents, source.caFile),
			optionalContent(contents, source.certFile),
			optionalContent(contents, source.keyFile),
			source.serverName,
			source.minVersion,
		)
	default:
		err = fmt.Errorf("unsupported TLS reload source %q", source.kind)
	}
	if err != nil {
		return "", nil, err
	}
	return signature, config, nil
}

func (source reloadSource) files() []string {
	switch source.kind {
	case reloadServer:
		return nonEmpty(source.certFile, source.keyFile, source.clientCA)
	case reloadClient:
		return nonEmpty(source.caFile, source.certFile, source.keyFile)
	default:
		return nil
	}
}

func nonEmpty(paths ...string) []string {
	values := make([]string, 0, len(paths))
	for _, path := range paths {
		if path != "" {
			values = append(values, path)
		}
	}
	return values
}

func optionalContent(contents map[string][]byte, path string) []byte {
	if path == "" {
		return nil
	}
	return contents[path]
}

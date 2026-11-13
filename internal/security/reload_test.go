package security_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/internal/security"
	"github.com/zjsycwmrbig/mini-rpc/internal/testkit"
)

func TestServerReloaderPublishesValidatedMaterialAndKeepsLastKnownGood(t *testing.T) {
	t.Parallel()

	first := newTLSMaterial(t)
	second := newTLSMaterial(t)
	directory := t.TempDir()
	certFile := filepath.Join(directory, "server.crt")
	keyFile := filepath.Join(directory, "server.key")
	caFile := filepath.Join(directory, "ca.crt")
	writeServerMaterial(t, certFile, keyFile, caFile, first)

	var events []security.ReloadEvent
	reloader, err := security.NewReloadableServerConfig(
		certFile,
		keyFile,
		caFile,
		security.DefaultMinVersion,
		security.ReloadOptions{
			Interval: time.Hour,
			OnReload: func(event security.ReloadEvent) {
				events = append(events, event)
			},
		},
	)
	if err != nil {
		t.Fatalf("NewReloadableServerConfig() error = %v", err)
	}
	initial := certificateRaw(t, reloader.Current())
	if stats := reloader.Stats(); stats.Version != 1 || stats.Attempts != 0 {
		t.Fatalf("initial reload stats = %+v", stats)
	}

	if changed, err := reloader.Poll(); err != nil || changed {
		t.Fatalf("unchanged Poll() = %v, %v; want false, nil", changed, err)
	}
	writeServerMaterial(t, certFile, keyFile, caFile, second)
	changed, err := reloader.Poll()
	if err != nil || !changed {
		t.Fatalf("rotated Poll() = %v, %v; want true, nil", changed, err)
	}
	rotated := certificateRaw(t, reloader.Current())
	if bytes.Equal(initial, rotated) {
		t.Fatal("server certificate did not rotate")
	}
	if current := reloader.Current(); current.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatal("rotated server config did not retain mutual TLS")
	}

	if err := reloader.Reload(); err != nil {
		t.Fatalf("forced Reload() error = %v", err)
	}
	if got := reloader.Stats(); got.Version != 3 || got.Successes != 2 {
		t.Fatalf("forced reload stats = %+v", got)
	}

	if err := os.WriteFile(certFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write invalid certificate: %v", err)
	}
	changed, err = reloader.Poll()
	if err == nil || changed {
		t.Fatalf("invalid Poll() = %v, %v; want false, error", changed, err)
	}
	if current := certificateRaw(t, reloader.Current()); !bytes.Equal(current, rotated) {
		t.Fatal("failed reload replaced the last known-good certificate")
	}
	stats := reloader.Stats()
	if stats.Version != 3 || stats.Failures != 1 || stats.LastError == "" {
		t.Fatalf("failed reload stats = %+v", stats)
	}
	if len(events) != 3 || events[0].Error != nil || events[1].Error != nil || events[2].Error == nil {
		t.Fatalf("reload events = %+v", events)
	}
}

func TestClientReloaderRotatesRootAndClientIdentity(t *testing.T) {
	t.Parallel()

	first := newTLSMaterial(t)
	second := newTLSMaterial(t)
	directory := t.TempDir()
	caFile := filepath.Join(directory, "ca.crt")
	certFile := filepath.Join(directory, "client.crt")
	keyFile := filepath.Join(directory, "client.key")
	writeClientMaterial(t, caFile, certFile, keyFile, first)

	reloader, err := security.NewReloadableClientConfig(
		caFile,
		certFile,
		keyFile,
		"rpc.example.test",
		security.DefaultMinVersion,
		security.ReloadOptions{Interval: time.Hour},
	)
	if err != nil {
		t.Fatalf("NewReloadableClientConfig() error = %v", err)
	}
	initial := certificateRaw(t, reloader.Current())
	initialRoots := reloader.Current().RootCAs.Clone()

	writeClientMaterial(t, caFile, certFile, keyFile, second)
	if changed, err := reloader.Poll(); err != nil || !changed {
		t.Fatalf("rotated Poll() = %v, %v; want true, nil", changed, err)
	}
	current := reloader.Current()
	if current.ServerName != "rpc.example.test" || current.InsecureSkipVerify {
		t.Fatalf("rotated client security settings = %+v", current)
	}
	if bytes.Equal(initial, certificateRaw(t, current)) {
		t.Fatal("client certificate did not rotate")
	}
	if current.RootCAs.Equal(initialRoots) {
		t.Fatal("client root CA did not rotate")
	}
	expectedRoots := x509.NewCertPool()
	if !expectedRoots.AppendCertsFromPEM(second.CAPEM) || !current.RootCAs.Equal(expectedRoots) {
		t.Fatal("client root CA does not contain the rotated CA")
	}
}

func TestReloaderConcurrentSnapshotsPollingAndCallbacks(t *testing.T) {
	t.Parallel()

	first := newTLSMaterial(t)
	second := newTLSMaterial(t)
	directory := t.TempDir()
	certFile := filepath.Join(directory, "server.crt")
	keyFile := filepath.Join(directory, "server.key")
	caFile := filepath.Join(directory, "ca.crt")
	writeServerMaterial(t, certFile, keyFile, caFile, first)

	var callbackCount int
	var callbackMu sync.Mutex
	reloader, err := security.NewReloadableServerConfig(
		certFile,
		keyFile,
		caFile,
		security.DefaultMinVersion,
		security.ReloadOptions{
			Interval: time.Hour,
			OnReload: func(security.ReloadEvent) {
				callbackMu.Lock()
				callbackCount++
				callbackMu.Unlock()
			},
		},
	)
	if err != nil {
		t.Fatalf("NewReloadableServerConfig() error = %v", err)
	}

	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 100 {
				if current := reloader.Current(); current == nil {
					t.Error("Current() returned nil during concurrent reloads")
					return
				}
			}
		}()
	}
	writeServerMaterial(t, certFile, keyFile, caFile, second)
	if _, err := reloader.Poll(); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if err := reloader.Reload(); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	workers.Wait()

	callbackMu.Lock()
	defer callbackMu.Unlock()
	if callbackCount != 2 {
		t.Fatalf("callback count = %d, want 2", callbackCount)
	}
}

func TestReloaderValidationAndNilSources(t *testing.T) {
	t.Parallel()

	reloader, err := security.NewReloadableServerConfig(
		"", "", "", security.DefaultMinVersion, security.ReloadOptions{},
	)
	if err != nil || reloader != nil {
		t.Fatalf("empty server reloader = %v, %v; want nil, nil", reloader, err)
	}
	reloader, err = security.NewReloadableClientConfig(
		"", "", "", "", security.DefaultMinVersion, security.ReloadOptions{},
	)
	if err != nil || reloader != nil {
		t.Fatalf("empty client reloader = %v, %v; want nil, nil", reloader, err)
	}
	var nilReloader *security.Reloader
	if nilReloader.Current() != nil || nilReloader.Stats() != (security.ReloadStats{}) {
		t.Fatal("nil reloader did not return empty snapshots")
	}
	if _, err := nilReloader.Poll(); err == nil {
		t.Fatal("nil reloader Poll() succeeded")
	}

	directory := t.TempDir()
	if _, err := security.NewReloadableServerConfig(
		filepath.Join(directory, "missing.crt"),
		filepath.Join(directory, "missing.key"),
		"",
		security.DefaultMinVersion,
		security.ReloadOptions{},
	); err == nil {
		t.Fatal("missing server material was accepted")
	}
}

func newTLSMaterial(t *testing.T) *testkit.TLSMaterial {
	t.Helper()
	material, err := testkit.NewTLSMaterial("localhost", "127.0.0.1", "rpc.example.test")
	if err != nil {
		t.Fatalf("NewTLSMaterial() error = %v", err)
	}
	return material
}

func writeServerMaterial(
	t *testing.T,
	certFile string,
	keyFile string,
	caFile string,
	material *testkit.TLSMaterial,
) {
	t.Helper()
	writeFile(t, certFile, material.ServerCertPEM)
	writeFile(t, keyFile, material.ServerKeyPEM)
	writeFile(t, caFile, material.CAPEM)
}

func writeClientMaterial(
	t *testing.T,
	caFile string,
	certFile string,
	keyFile string,
	material *testkit.TLSMaterial,
) {
	t.Helper()
	writeFile(t, caFile, material.CAPEM)
	writeFile(t, certFile, material.ClientCertPEM)
	writeFile(t, keyFile, material.ClientKeyPEM)
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func certificateRaw(t *testing.T, config *tls.Config) []byte {
	t.Helper()
	if config == nil || len(config.Certificates) != 1 || len(config.Certificates[0].Certificate) == 0 {
		t.Fatalf("TLS config has no certificate: %+v", config)
	}
	leaf, err := x509.ParseCertificate(config.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse loaded certificate: %v", err)
	}
	return leaf.Raw
}

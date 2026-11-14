package observability

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// ReloadableOTLPTraceExporter routes batches through the latest validated
// OTLP exporter. TLS material changes rebuild the underlying HTTP client so
// new exports use the new root CAs, client identity, and ServerName while
// failed reloads retain the last known-good exporter.
type ReloadableOTLPTraceExporter struct {
	config OTLPExporterConfig
	source func() *tls.Config

	updateMu sync.Mutex
	mu       sync.RWMutex
	current  sdktrace.SpanExporter
	closed   bool
}

// NewReloadableOTLPTraceExporter creates an exporter from the current TLS
// snapshot and returns a wrapper whose Reload method can atomically replace
// the underlying client after material rotation. source must return a
// validated TLS configuration on every call.
func NewReloadableOTLPTraceExporter(
	ctx context.Context,
	config OTLPExporterConfig,
	source func() *tls.Config,
) (*ReloadableOTLPTraceExporter, error) {
	if source == nil {
		return nil, fmt.Errorf("observability: nil OTLP TLS configuration source")
	}
	if config.TLSConfig != nil {
		return nil, fmt.Errorf("observability: reloadable OTLP exporter requires a TLS source")
	}
	tlsConfig := source()
	if tlsConfig == nil {
		return nil, fmt.Errorf("observability: OTLP TLS source returned no configuration")
	}
	config.TLSConfig = tlsConfig
	exporter, err := NewOTLPTraceExporter(ctx, config)
	if err != nil {
		return nil, err
	}
	return &ReloadableOTLPTraceExporter{
		config:  config,
		source:  source,
		current: exporter,
	}, nil
}

// ExportSpans delegates one batch while preventing retirement of the active
// exporter until the export has returned.
func (exporter *ReloadableOTLPTraceExporter) ExportSpans(
	ctx context.Context,
	spans []sdktrace.ReadOnlySpan,
) error {
	exporter.mu.RLock()
	defer exporter.mu.RUnlock()
	if exporter.closed || exporter.current == nil {
		return fmt.Errorf("observability: OTLP exporter is shut down")
	}
	return exporter.current.ExportSpans(ctx, spans)
}

// Reload builds a new exporter from the latest TLS snapshot and swaps it in
// only after construction succeeds. A failed replacement leaves the previous
// exporter available for subsequent batches.
func (exporter *ReloadableOTLPTraceExporter) Reload(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	exporter.updateMu.Lock()
	defer exporter.updateMu.Unlock()

	tlsConfig := exporter.source()
	if tlsConfig == nil {
		return fmt.Errorf("observability: OTLP TLS source returned no configuration")
	}
	config := exporter.config
	config.TLSConfig = tlsConfig
	next, err := NewOTLPTraceExporter(context.Background(), config)
	if err != nil {
		return err
	}

	exporter.mu.Lock()
	if exporter.closed {
		exporter.mu.Unlock()
		_ = next.Shutdown(ctx)
		return fmt.Errorf("observability: OTLP exporter is shut down")
	}
	previous := exporter.current
	exporter.current = next
	exporter.mu.Unlock()

	// Swap under the write lock ensures that no export still references the
	// retired exporter. Shutdown is best-effort because the new exporter is
	// already active and a cleanup failure must not roll back valid TLS state.
	_ = previous.Shutdown(ctx)
	return nil
}

// Shutdown waits for in-flight exports and releases the active exporter once.
func (exporter *ReloadableOTLPTraceExporter) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	exporter.mu.Lock()
	if exporter.closed {
		exporter.mu.Unlock()
		return nil
	}
	exporter.closed = true
	current := exporter.current
	exporter.current = nil
	exporter.mu.Unlock()
	if current == nil {
		return nil
	}
	return current.Shutdown(ctx)
}

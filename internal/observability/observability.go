package observability

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/internal/protocol"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
)

const (
	TraceIDBytes   = 16
	SpanIDBytes    = 8
	RequestIDBytes = 16
)

// Config controls structured logs, request identifiers, and metrics.
type Config struct {
	Enabled  bool
	Debug    bool
	Instance string
	Output   io.Writer
}

// SpanContext is the correlation identity shared by client and server logs.
type SpanContext struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	RequestID    string
}

type spanContextKey struct{}

// Runtime owns one process-local logger, identifier generator, and metric
// registry. A nil or disabled Runtime is a no-op.
type Runtime struct {
	config  Config
	logger  *slog.Logger
	metrics *Metrics
}

// New creates an observability runtime. When disabled it emits no logs or
// metrics and the RPC path continues without identifier generation.
func New(config Config) *Runtime {
	if config.Output == nil {
		config.Output = os.Stderr
	}
	runtime := &Runtime{
		config:  config,
		metrics: NewMetrics(config.Enabled),
	}
	if config.Enabled {
		runtime.logger = slog.New(slog.NewJSONHandler(&lockedWriter{writer: config.Output}, &slog.HandlerOptions{
			Level: logLevel(config.Debug),
		}))
	}
	return runtime
}

func logLevel(debug bool) slog.Level {
	if debug {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// Enabled reports whether request correlation and exports are active.
func (runtime *Runtime) Enabled() bool {
	return runtime != nil && runtime.config.Enabled
}

// Debug reports whether verbose start events are enabled.
func (runtime *Runtime) Debug() bool {
	return runtime != nil && runtime.config.Debug
}

// Metrics returns the process-local registry. Disabled runtimes return an
// empty registry that renders no metrics.
func (runtime *Runtime) Metrics() *Metrics {
	if runtime == nil {
		return NewMetrics(false)
	}
	return runtime.metrics
}

// Span stores one client or server call lifecycle.
type Span struct {
	runtime   *Runtime
	context   SpanContext
	started   time.Time
	role      string
	service   string
	method    string
	attempts  atomic.Int64
	finishOne sync.Once
}

// StartClientSpan creates a root call span or a child of an existing Context
// span. The returned Context carries trace metadata for the network attempt.
func (runtime *Runtime) StartClientSpan(
	ctx context.Context,
	service string,
	method string,
) (context.Context, *Span) {
	if !runtime.Enabled() {
		return ctx, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	parent, _ := SpanContextFromContext(ctx)
	traceID := parent.TraceID
	if !validID(traceID, TraceIDBytes) {
		traceID = randomID(TraceIDBytes)
	}
	spanContext := SpanContext{
		TraceID:      traceID,
		SpanID:       randomID(SpanIDBytes),
		ParentSpanID: parent.SpanID,
		RequestID:    randomID(RequestIDBytes),
	}
	ctx = WithSpanContext(ctx, spanContext)
	span := runtime.startSpan(ctx, "client", service, method, spanContext)
	return ctx, span
}

// StartServerSpan continues a client trace from request Metadata or creates a
// new trace when the metadata is missing or malformed.
func (runtime *Runtime) StartServerSpan(
	ctx context.Context,
	metadata map[string]string,
	service string,
	method string,
) (context.Context, *Span) {
	if !runtime.Enabled() {
		return ctx, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	incoming := SpanContextFromMetadata(metadata)
	traceID := incoming.TraceID
	if !validID(traceID, TraceIDBytes) {
		traceID = randomID(TraceIDBytes)
	}
	requestID := validRequestID(incoming.RequestID)
	if requestID == "" {
		requestID = randomID(RequestIDBytes)
	}
	spanContext := SpanContext{
		TraceID:      traceID,
		SpanID:       randomID(SpanIDBytes),
		ParentSpanID: incoming.SpanID,
		RequestID:    requestID,
	}
	ctx = WithSpanContext(ctx, spanContext)
	span := runtime.startSpan(ctx, "server", service, method, spanContext)
	return ctx, span
}

func (runtime *Runtime) startSpan(
	_ context.Context,
	role string,
	service string,
	method string,
	spanContext SpanContext,
) *Span {
	runtime.metrics.ChangeInflight(role, 1)
	span := &Span{
		runtime: runtime,
		context: spanContext,
		started: time.Now(),
		role:    role,
		service: service,
		method:  method,
	}
	span.attempts.Store(1)
	runtime.log(slog.LevelDebug, "rpc.call.start", span, nil, time.Since(span.started), "started")
	return span
}

// Context returns immutable correlation identifiers.
func (span *Span) Context() SpanContext {
	if span == nil {
		return SpanContext{}
	}
	return span.context
}

// TraceMetadata returns values propagated in request Metadata.
func (span *Span) TraceMetadata() map[string]string {
	if span == nil {
		return nil
	}
	return map[string]string{
		protocol.MetadataKeyRequestID: span.context.RequestID,
		protocol.MetadataKeyTraceID:   span.context.TraceID,
		protocol.MetadataKeySpanID:    span.context.SpanID,
	}
}

// SetAttempt records the final attempt count for retry metrics and logs.
func (span *Span) SetAttempt(attempt int) {
	if span == nil {
		return
	}
	if attempt < 1 {
		attempt = 1
	}
	span.attempts.Store(int64(attempt))
}

// Finish records exactly one completion metric and structured log entry.
func (span *Span) Finish(err error) {
	if span == nil || span.runtime == nil {
		return
	}
	span.finishOne.Do(func() {
		duration := time.Since(span.started)
		code := ErrorCode(err)
		outcome := Outcome(code)
		attempt := int(span.attempts.Load())
		span.runtime.metrics.ChangeInflight(span.role, -1)
		span.runtime.metrics.RecordRequest(span.role, span.service, span.method, outcome, code, duration)
		if attempt > 1 {
			span.runtime.metrics.RecordRetry(span.role, span.service, span.method, attempt-1)
		}
		if outcome == "rate_limited" {
			span.runtime.metrics.RecordRateLimited(span.role, span.service, span.method)
		}
		span.runtime.log(slog.LevelInfo, "rpc.call.completed", span, err, duration, outcome)
	})
}

func (runtime *Runtime) log(
	level slog.Level,
	message string,
	span *Span,
	err error,
	duration time.Duration,
	outcome string,
) {
	if !runtime.Enabled() || runtime.logger == nil {
		return
	}
	if level == slog.LevelDebug && !runtime.config.Debug {
		return
	}
	code := ErrorCode(err)
	attributes := []any{
		"event", message,
		"role", span.role,
		"service", span.service,
		"method", span.method,
		"request_id", span.context.RequestID,
		"trace_id", span.context.TraceID,
		"span_id", span.context.SpanID,
		"parent_span_id", span.context.ParentSpanID,
		"instance", runtime.config.Instance,
		"attempt", span.attempts.Load(),
		"duration_ms", duration.Seconds() * 1000,
		"outcome", outcome,
		"error_code", code,
	}
	if err != nil {
		attributes = append(attributes, "error", err.Error())
	}
	runtime.logger.Log(context.Background(), level, message, attributes...)
}

// ErrorCode returns a stable lowercase code for logs and metrics.
func ErrorCode(err error) string {
	if err == nil {
		return "ok"
	}
	var rpcError *rpc.Error
	if errors.As(err, &rpcError) {
		return rpcError.Code.String()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return rpc.CodeDeadlineExceeded.String()
	}
	if errors.Is(err, context.Canceled) {
		return rpc.CodeCanceled.String()
	}
	return "internal"
}

// Outcome classifies a stable error code for request counters.
func Outcome(code string) string {
	switch code {
	case "ok":
		return "success"
	case rpc.CodeDeadlineExceeded.String():
		return "timeout"
	case rpc.CodeResourceExhausted.String():
		return "rate_limited"
	case rpc.CodeCanceled.String():
		return "canceled"
	default:
		return "failure"
	}
}

// SpanContextFromMetadata parses propagated trace identifiers.
func SpanContextFromMetadata(metadata map[string]string) SpanContext {
	traceID := metadata[protocol.MetadataKeyTraceID]
	if !validID(traceID, TraceIDBytes) {
		traceID = ""
	}
	spanID := metadata[protocol.MetadataKeySpanID]
	if !validID(spanID, SpanIDBytes) {
		spanID = ""
	}
	return SpanContext{
		TraceID:   traceID,
		SpanID:    spanID,
		RequestID: metadata[protocol.MetadataKeyRequestID],
	}
}

// WithSpanContext adds correlation identity to a Context.
func WithSpanContext(ctx context.Context, spanContext SpanContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, spanContextKey{}, spanContext)
}

// SpanContextFromContext reads correlation identity from a Context.
func SpanContextFromContext(ctx context.Context) (SpanContext, bool) {
	if ctx == nil {
		return SpanContext{}, false
	}
	spanContext, ok := ctx.Value(spanContextKey{}).(SpanContext)
	return spanContext, ok
}

func validID(value string, size int) bool {
	if len(value) != size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return false
	}
	for _, current := range decoded {
		if current != 0 {
			return true
		}
	}
	return false
}

func validRequestID(value string) string {
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, current := range value {
		if current < 0x21 || current > 0x7e {
			return ""
		}
	}
	return value
}

var fallbackID atomic.Uint64

func randomID(size int) string {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err == nil {
		return hex.EncodeToString(buffer)
	}
	seed := fmt.Sprintf("mini-rpc:%d:%d", time.Now().UnixNano(), fallbackID.Add(1))
	digest := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(digest[:size])
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (writer *lockedWriter) Write(value []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.writer.Write(value)
}

// Traceparent returns a W3C-style version 00 traceparent for external systems.
func (spanContext SpanContext) Traceparent() string {
	if !validID(spanContext.TraceID, TraceIDBytes) || !validID(spanContext.SpanID, SpanIDBytes) {
		return ""
	}
	return strings.Join([]string{"00", spanContext.TraceID, spanContext.SpanID, "01"}, "-")
}

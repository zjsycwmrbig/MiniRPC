package observability

import (
	"context"
	"time"

	otelattribute "go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	otelresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	defaultTraceBatchTimeout  = time.Second
	defaultTraceExportTimeout = 5 * time.Second
	tracerName                = "github.com/zjsycwmrbig/mini-rpc/internal/observability"
)

type tracingRuntime struct {
	provider *sdktrace.TracerProvider
	tracer   oteltrace.Tracer
}

func newTracingRuntime(config Config) *tracingRuntime {
	if config.TraceExporter == nil {
		return nil
	}
	batchTimeout := config.TraceBatchTimeout
	if batchTimeout <= 0 {
		batchTimeout = defaultTraceBatchTimeout
	}
	exportTimeout := config.TraceExportTimeout
	if exportTimeout <= 0 {
		exportTimeout = defaultTraceExportTimeout
	}
	sampleRatio := config.TraceSampleRatio
	if sampleRatio < 0 {
		sampleRatio = 0
	}
	if sampleRatio > 1 {
		sampleRatio = 1
	}
	serviceName := config.ServiceName
	if serviceName == "" {
		serviceName = "mini-rpc"
	}
	attributes := []otelattribute.KeyValue{
		otelattribute.String("service.name", serviceName),
	}
	if config.ServiceVersion != "" {
		attributes = append(attributes, otelattribute.String("service.version", config.ServiceVersion))
	}
	if config.Instance != "" {
		attributes = append(attributes, otelattribute.String("service.instance.id", config.Instance))
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(otelresource.NewSchemaless(attributes...)),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio))),
		sdktrace.WithBatcher(
			config.TraceExporter,
			sdktrace.WithBatchTimeout(batchTimeout),
			sdktrace.WithExportTimeout(exportTimeout),
		),
	)
	return &tracingRuntime{
		provider: provider,
		tracer:   provider.Tracer(tracerName, oteltrace.WithInstrumentationVersion(config.ServiceVersion)),
	}
}

func (runtime *Runtime) ForceFlush(ctx context.Context) error {
	if runtime == nil || runtime.tracing == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return runtime.tracing.provider.ForceFlush(ctx)
}

// Shutdown flushes queued spans and releases exporter resources exactly once.
func (runtime *Runtime) Shutdown(ctx context.Context) error {
	if runtime == nil || runtime.tracing == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runtime.shutdownOne.Do(func() {
		runtime.shutdownErr = runtime.tracing.provider.Shutdown(ctx)
	})
	return runtime.shutdownErr
}

func (runtime *tracingRuntime) startClient(
	ctx context.Context,
	service string,
	method string,
	parent SpanContext,
) (context.Context, oteltrace.Span, string) {
	ctx, parentSpanID := bridgeClientParent(ctx, parent)
	ctx, span := runtime.tracer.Start(
		ctx,
		"rpc.client/"+service+"/"+method,
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(startAttributes("client", service, method)...),
	)
	return ctx, span, parentSpanID
}

func (runtime *tracingRuntime) startServer(
	ctx context.Context,
	service string,
	method string,
	parent SpanContext,
) (context.Context, oteltrace.Span) {
	if parentContext, ok := otelSpanContext(parent); ok {
		ctx = oteltrace.ContextWithRemoteSpanContext(ctx, parentContext)
	}
	return runtime.tracer.Start(
		ctx,
		"rpc.server/"+service+"/"+method,
		oteltrace.WithSpanKind(oteltrace.SpanKindServer),
		oteltrace.WithAttributes(startAttributes("server", service, method)...),
	)
}

func bridgeClientParent(ctx context.Context, parent SpanContext) (context.Context, string) {
	active := oteltrace.SpanContextFromContext(ctx)
	if active.IsValid() {
		return ctx, active.SpanID().String()
	}
	if parentContext, ok := otelSpanContext(parent); ok {
		return oteltrace.ContextWithRemoteSpanContext(ctx, parentContext), parent.SpanID
	}
	return ctx, ""
}

func otelSpanContext(spanContext SpanContext) (oteltrace.SpanContext, bool) {
	if !validID(spanContext.TraceID, TraceIDBytes) || !validID(spanContext.SpanID, SpanIDBytes) {
		return oteltrace.SpanContext{}, false
	}
	traceID, err := oteltrace.TraceIDFromHex(spanContext.TraceID)
	if err != nil {
		return oteltrace.SpanContext{}, false
	}
	spanID, err := oteltrace.SpanIDFromHex(spanContext.SpanID)
	if err != nil {
		return oteltrace.SpanContext{}, false
	}
	flags := oteltrace.TraceFlags(0)
	if spanContext.Sampled {
		flags = oteltrace.FlagsSampled
	}
	return oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: flags,
		Remote:     true,
	}), true
}

func startAttributes(role, service, method string) []otelattribute.KeyValue {
	return []otelattribute.KeyValue{
		otelattribute.String("rpc.system", "minirpc"),
		otelattribute.String("rpc.service", service),
		otelattribute.String("rpc.method", method),
		otelattribute.String("rpc.minirpc.role", role),
	}
}

func (runtime *Runtime) finishTracing(
	span *Span,
	err error,
	code string,
	outcome string,
	attempt int,
) {
	if runtime == nil || span == nil || span.otelSpan == nil {
		return
	}
	span.otelSpan.SetAttributes(
		otelattribute.Int("rpc.minirpc.attempts", attempt),
		otelattribute.String("rpc.minirpc.outcome", outcome),
		otelattribute.String("rpc.minirpc.error_code", code),
	)
	if err != nil {
		span.otelSpan.SetStatus(otelcodes.Error, "rpc "+code)
	}
	span.otelSpan.End()
}

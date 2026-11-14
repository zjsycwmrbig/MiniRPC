package observability

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/internal/testkit"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestOTLPSpansPreserveClientServerParentageAndCorrelation(t *testing.T) {
	t.Parallel()

	exporter := tracetest.NewInMemoryExporter()
	clientRuntime := New(Config{
		Enabled:          true,
		Instance:         "client-test",
		ServiceName:      "mini-rpc-client",
		Output:           io.Discard,
		TraceExporter:    exporter,
		TraceSampleRatio: 1,
	})
	serverRuntime := New(Config{
		Enabled:          true,
		Instance:         "server-test",
		ServiceName:      "mini-rpc-server",
		Output:           io.Discard,
		TraceExporter:    exporter,
		TraceSampleRatio: 1,
	})

	clientContext, clientSpan := clientRuntime.StartClientSpan(context.Background(), "echo.v1.Echo", "Echo")
	if clientSpan == nil || !clientSpan.Context().Sampled {
		t.Fatalf("client span = %+v", clientSpan)
	}
	serverContext, serverSpan := serverRuntime.StartServerSpan(
		context.Background(),
		clientSpan.TraceMetadata(),
		"echo.v1.Echo",
		"Echo",
	)
	if serverSpan == nil {
		t.Fatal("server span = nil")
	}
	serverSpan.SetAttempt(2)
	serverSpan.Finish(rpc.NewError(rpc.CodeInternal, "boom"))
	clientSpan.Finish(nil)
	if oteltrace.SpanFromContext(clientContext).SpanContext().TraceID() !=
		oteltrace.SpanFromContext(serverContext).SpanContext().TraceID() {
		t.Fatal("OpenTelemetry contexts do not share a trace ID")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := clientRuntime.ForceFlush(ctx); err != nil {
		t.Fatalf("client ForceFlush() error = %v", err)
	}
	if err := serverRuntime.ForceFlush(ctx); err != nil {
		t.Fatalf("server ForceFlush() error = %v", err)
	}
	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("exported spans = %d, want 2", len(spans))
	}
	clientOTel := findSpan(t, spans, "rpc.client/echo.v1.Echo/Echo")
	serverOTel := findSpan(t, spans, "rpc.server/echo.v1.Echo/Echo")
	if clientOTel.SpanContext.TraceID() != serverOTel.SpanContext.TraceID() {
		t.Fatal("exported client and server trace IDs differ")
	}
	if serverOTel.Parent.SpanID() != clientOTel.SpanContext.SpanID() {
		t.Fatalf("server parent = %s, want client %s", serverOTel.Parent.SpanID(), clientOTel.SpanContext.SpanID())
	}
	if serverOTel.Status.Code != otelcodes.Error {
		t.Fatalf("server status = %v, want error", serverOTel.Status.Code)
	}
	if !hasAttribute(clientOTel, "rpc.system", "minirpc") ||
		!hasAttribute(clientOTel, "rpc.service", "echo.v1.Echo") ||
		!hasAttribute(clientOTel, "rpc.method", "Echo") {
		t.Fatalf("client span attributes = %v", clientOTel.Attributes)
	}
	if !hasAttribute(serverOTel, "rpc.minirpc.error_code", "internal") ||
		!hasAttribute(serverOTel, "rpc.minirpc.attempts", int64(2)) {
		t.Fatalf("server span attributes = %v", serverOTel.Attributes)
	}
}

func TestTraceSamplingDecisionPropagatesWithoutExport(t *testing.T) {
	t.Parallel()

	exporter := tracetest.NewInMemoryExporter()
	runtime := New(Config{
		Enabled:          true,
		Instance:         "sampled-out",
		Output:           io.Discard,
		TraceExporter:    exporter,
		TraceSampleRatio: 0,
	})
	_, span := runtime.StartClientSpan(context.Background(), "echo.v1.Echo", "Echo")
	if span.context.Sampled {
		t.Fatal("zero-ratio root span was marked sampled")
	}
	metadata := span.TraceMetadata()
	if metadata["trace_flags"] != "00" {
		t.Fatalf("trace flags = %q, want 00", metadata["trace_flags"])
	}
	if span.Context().Traceparent() != "00-"+span.Context().TraceID+"-"+span.Context().SpanID+"-00" {
		t.Fatalf("Traceparent() = %q", span.Context().Traceparent())
	}
	span.Finish(nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush() error = %v", err)
	}
	if spans := exporter.GetSpans(); len(spans) != 0 {
		t.Fatalf("sampled-out exporter received %d spans", len(spans))
	}
}

func TestOTLPExporterConfigurationValidation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	for _, test := range []struct {
		name   string
		config OTLPExporterConfig
	}{
		{name: "empty", config: OTLPExporterConfig{}},
		{name: "relative", config: OTLPExporterConfig{Endpoint: "/v1/traces"}},
		{
			name:   "implicit insecure transport",
			config: OTLPExporterConfig{Endpoint: "http://127.0.0.1:4318/v1/traces"},
		},
		{
			name:   "insecure https",
			config: OTLPExporterConfig{Endpoint: "https://collector.example/v1/traces", Insecure: true},
		},
		{
			name: "tls on http",
			config: OTLPExporterConfig{
				Endpoint:  "http://127.0.0.1:4318/v1/traces",
				Insecure:  true,
				TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			},
		},
		{
			name: "skip verification",
			config: OTLPExporterConfig{
				Endpoint:  "https://collector.example/v1/traces",
				TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // verifies the exporter rejects this policy.
			},
		},
		{
			name: "legacy minimum TLS version",
			config: OTLPExporterConfig{
				Endpoint:      "https://collector.example/v1/traces",
				TLSMinVersion: tls.VersionTLS11,
			},
		},
		{
			name: "invalid header",
			config: OTLPExporterConfig{
				Endpoint: "http://127.0.0.1:4318/v1/traces",
				Insecure: true,
				Headers:  map[string]string{"bad header": "value"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if exporter, err := NewOTLPTraceExporter(ctx, test.config); err == nil {
				_ = exporter.Shutdown(ctx)
				t.Fatal("invalid OTLP configuration was accepted")
			}
		})
	}

	exporter, err := NewOTLPTraceExporter(ctx, OTLPExporterConfig{
		Endpoint: "http://127.0.0.1:4318/v1/traces",
		Insecure: true,
		Headers:  map[string]string{"x-tenant": "mini-rpc"},
	})
	if err != nil {
		t.Fatalf("NewOTLPTraceExporter() error = %v", err)
	}
	if err := exporter.Shutdown(ctx); err != nil {
		t.Fatalf("exporter Shutdown() error = %v", err)
	}
}

func TestOTLPExporterUsesCustomTLSRoots(t *testing.T) {
	t.Parallel()

	requestPath := make(chan string, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case requestPath <- request.URL.Path:
		default:
		}
		writer.Header().Set("Content-Type", "application/x-protobuf")
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(server.Certificate())
	exporter, err := NewOTLPTraceExporter(context.Background(), OTLPExporterConfig{
		Endpoint: server.URL + "/v1/traces",
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    rootCAs,
		},
	})
	if err != nil {
		t.Fatalf("NewOTLPTraceExporter() error = %v", err)
	}
	runtime := New(Config{
		Enabled:          true,
		Instance:         "otlp-tls-test",
		ServiceName:      "mini-rpc-otlp-tls-test",
		Output:           io.Discard,
		TraceExporter:    exporter,
		TraceSampleRatio: 1,
	})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := runtime.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
	}()

	_, span := runtime.StartClientSpan(context.Background(), "echo.v1.Echo", "Echo")
	span.Finish(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runtime.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush() error = %v", err)
	}
	select {
	case path := <-requestPath:
		if path != "/v1/traces" {
			t.Fatalf("export path = %q, want /v1/traces", path)
		}
	case <-ctx.Done():
		t.Fatal("OTLP exporter did not send a trace over the custom TLS root")
	}
}

func TestOTLPExporterSupportsMutualTLS(t *testing.T) {
	t.Parallel()

	material, err := testkit.NewTLSMaterial("127.0.0.1")
	if err != nil {
		t.Fatalf("NewTLSMaterial() error = %v", err)
	}
	serverCertificate, err := tls.X509KeyPair(material.ServerCertPEM, material.ServerKeyPEM)
	if err != nil {
		t.Fatalf("load server key pair: %v", err)
	}
	clientCertificate, err := tls.X509KeyPair(material.ClientCertPEM, material.ClientKeyPEM)
	if err != nil {
		t.Fatalf("load client key pair: %v", err)
	}
	rootCAs := x509.NewCertPool()
	if !rootCAs.AppendCertsFromPEM(material.CAPEM) {
		t.Fatal("append test CA")
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(material.CAPEM) {
		t.Fatal("append test client CA")
	}

	exported := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		select {
		case exported <- struct{}{}:
		default:
		}
		writer.Header().Set("Content-Type", "application/x-protobuf")
		writer.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS12,
	}
	server.StartTLS()
	defer server.Close()

	exporter, err := NewOTLPTraceExporter(context.Background(), OTLPExporterConfig{
		Endpoint: server.URL + "/v1/traces",
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{clientCertificate},
			MinVersion:   tls.VersionTLS12,
			RootCAs:      rootCAs,
		},
	})
	if err != nil {
		t.Fatalf("NewOTLPTraceExporter() error = %v", err)
	}
	runtime := New(Config{
		Enabled:          true,
		Instance:         "otlp-mtls-test",
		ServiceName:      "mini-rpc-otlp-mtls-test",
		Output:           io.Discard,
		TraceExporter:    exporter,
		TraceSampleRatio: 1,
	})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := runtime.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
	}()

	_, span := runtime.StartClientSpan(context.Background(), "echo.v1.Echo", "Echo")
	span.Finish(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runtime.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush() error = %v", err)
	}
	select {
	case <-exported:
	case <-ctx.Done():
		t.Fatal("mTLS OTLP exporter did not send a trace")
	}
}

func TestReloadableOTLPExporterRotatesTLSAndKeepsLastKnownGood(t *testing.T) {
	t.Parallel()

	first := newOTLPTLSMaterial(t)
	second := newOTLPTLSMaterial(t)
	firstServer := newOTLPServerConfig(t, first)
	secondServer := newOTLPServerConfig(t, second)
	firstClient := newOTLPClientConfig(t, first)
	secondClient := newOTLPClientConfig(t, second)
	firstClientIssuer := clientIssuer(t, first)
	secondClientIssuer := clientIssuer(t, second)

	var activeServer atomic.Pointer[tls.Config]
	activeServer.Store(firstServer)
	peerIssuers := make(chan []byte, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.TLS == nil || len(request.TLS.PeerCertificates) == 0 {
			t.Error("OTLP request did not carry a verified client certificate")
			http.Error(writer, "client certificate required", http.StatusUnauthorized)
			return
		}
		select {
		case peerIssuers <- request.TLS.PeerCertificates[0].RawIssuer:
		default:
		}
		writer.Header().Set("Content-Type", "application/x-protobuf")
		writer.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return activeServer.Load().Clone(), nil
		},
	}
	server.StartTLS()
	defer server.Close()

	var activeClient atomic.Pointer[tls.Config]
	activeClient.Store(firstClient)
	reloader, err := NewReloadableOTLPTraceExporter(
		context.Background(),
		OTLPExporterConfig{Endpoint: server.URL + "/v1/traces"},
		func() *tls.Config { return activeClient.Load().Clone() },
	)
	if err != nil {
		t.Fatalf("NewReloadableOTLPTraceExporter() error = %v", err)
	}
	runtime := New(Config{
		Enabled:          true,
		Instance:         "otlp-reload-test",
		ServiceName:      "mini-rpc-otlp-reload-test",
		Output:           io.Discard,
		TraceExporter:    reloader,
		TraceSampleRatio: 1,
	})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := runtime.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
	}()

	exportAndAssertIssuer(t, runtime, peerIssuers, firstClientIssuer)

	activeClient.Store(secondClient)
	activeServer.Store(secondServer)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := reloader.Reload(ctx); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	exportAndAssertIssuer(t, runtime, peerIssuers, secondClientIssuer)

	activeClient.Store(&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // verifies a failed reload retains the prior exporter.
	if err := reloader.Reload(ctx); err == nil {
		t.Fatal("Reload() accepted insecure client verification")
	}
	exportAndAssertIssuer(t, runtime, peerIssuers, secondClientIssuer)
}

func newOTLPTLSMaterial(t *testing.T) *testkit.TLSMaterial {
	t.Helper()
	material, err := testkit.NewTLSMaterial("127.0.0.1")
	if err != nil {
		t.Fatalf("NewTLSMaterial() error = %v", err)
	}
	return material
}

func newOTLPServerConfig(t *testing.T, material *testkit.TLSMaterial) *tls.Config {
	t.Helper()
	serverCertificate, err := tls.X509KeyPair(material.ServerCertPEM, material.ServerKeyPEM)
	if err != nil {
		t.Fatalf("load server key pair: %v", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(material.CAPEM) {
		t.Fatal("append test client CA")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS12,
	}
}

func newOTLPClientConfig(t *testing.T, material *testkit.TLSMaterial) *tls.Config {
	t.Helper()
	clientCertificate, err := tls.X509KeyPair(material.ClientCertPEM, material.ClientKeyPEM)
	if err != nil {
		t.Fatalf("load client key pair: %v", err)
	}
	rootCAs := x509.NewCertPool()
	if !rootCAs.AppendCertsFromPEM(material.CAPEM) {
		t.Fatal("append test root CA")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{clientCertificate},
		MinVersion:   tls.VersionTLS12,
		RootCAs:      rootCAs,
	}
}

func exportAndAssertIssuer(
	t *testing.T,
	runtime *Runtime,
	peerIssuers <-chan []byte,
	expectedIssuer []byte,
) {
	t.Helper()
	_, span := runtime.StartClientSpan(context.Background(), "echo.v1.Echo", "Echo")
	span.Finish(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runtime.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush() error = %v", err)
	}
	select {
	case issuer := <-peerIssuers:
		if !bytes.Equal(issuer, expectedIssuer) {
			t.Fatalf("Collector observed client issuer %x, want %x", issuer, expectedIssuer)
		}
	case <-ctx.Done():
		t.Fatal("reloadable OTLP exporter did not send a trace")
	}
}

func clientIssuer(t *testing.T, material *testkit.TLSMaterial) []byte {
	t.Helper()
	block, _ := pem.Decode(material.ClientCertPEM)
	if block == nil {
		t.Fatal("decode expected client certificate PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse expected client certificate: %v", err)
	}
	return bytes.Clone(certificate.RawIssuer)
}

func TestParseOTLPHeaders(t *testing.T) {
	t.Parallel()

	headers, err := ParseOTLPHeaders(" authorization = Bearer token , x-tenant=mini-rpc ")
	if err != nil {
		t.Fatalf("ParseOTLPHeaders() error = %v", err)
	}
	if headers["authorization"] != "Bearer token" || headers["x-tenant"] != "mini-rpc" {
		t.Fatalf("parsed headers = %#v", headers)
	}
	for _, value := range []string{
		"missing-equals",
		"=value",
		"x-tenant=one,x-tenant=two",
		"bad header=value",
		"x-tenant=line\rbreak",
	} {
		if _, err := ParseOTLPHeaders(value); err == nil {
			t.Errorf("ParseOTLPHeaders(%q) succeeded", value)
		}
	}
}

func TestRuntimeShutdownIsIdempotentAndDisabledIsNoop(t *testing.T) {
	t.Parallel()

	runtime := New(Config{Enabled: true, Output: io.Discard})
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown without exporter error = %v", err)
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown error = %v", err)
	}
	if runtime.ForceFlush(context.Background()) != nil {
		t.Fatal("ForceFlush without exporter failed")
	}
	var disabled *Runtime
	if disabled.Shutdown(context.Background()) != nil || disabled.ForceFlush(context.Background()) != nil {
		t.Fatal("nil runtime observability methods should be no-ops")
	}
}

func findSpan(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()
	for _, span := range spans {
		if span.Name == name {
			return span
		}
	}
	t.Fatalf("exported span %q not found in %v", name, spanNames(spans))
	return tracetest.SpanStub{}
}

func spanNames(spans tracetest.SpanStubs) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name)
	}
	return names
}

func hasAttribute(span tracetest.SpanStub, key string, expected any) bool {
	for _, attribute := range span.Attributes {
		if string(attribute.Key) == key {
			switch value := expected.(type) {
			case string:
				return attribute.Value.AsString() == value
			case int64:
				return attribute.Value.AsInt64() == value
			}
		}
	}
	return false
}

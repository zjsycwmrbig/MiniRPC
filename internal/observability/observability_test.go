package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/internal/protocol"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
)

func TestSpanContinuesTraceAndCorrelatesStructuredLogs(t *testing.T) {
	t.Parallel()

	var clientOutput bytes.Buffer
	var serverOutput bytes.Buffer
	clientRuntime := New(Config{Enabled: true, Instance: "client-test", Output: &clientOutput})
	serverRuntime := New(Config{Enabled: true, Instance: "server-test", Output: &serverOutput})

	clientContext, clientSpan := clientRuntime.StartClientSpan(context.Background(), "echo.v1.Echo", "Echo")
	if clientSpan == nil {
		t.Fatal("client span = nil")
	}
	clientSpan.SetAttempt(3)
	clientSpan.Finish(nil)

	serverContext, serverSpan := serverRuntime.StartServerSpan(
		context.Background(),
		clientSpan.TraceMetadata(),
		"echo.v1.Echo",
		"Echo",
	)
	if serverSpan == nil {
		t.Fatal("server span = nil")
	}
	serverSpan.Finish(rpc.NewError(rpc.CodeInternal, "boom"))

	clientLog := decodeLog(t, clientOutput.Bytes())
	serverLog := decodeLog(t, serverOutput.Bytes())
	if clientLog["trace_id"] != serverLog["trace_id"] {
		t.Fatalf("trace ids differ: client=%v server=%v", clientLog["trace_id"], serverLog["trace_id"])
	}
	if clientLog["request_id"] != serverLog["request_id"] {
		t.Fatalf("request ids differ: client=%v server=%v", clientLog["request_id"], serverLog["request_id"])
	}
	if serverLog["parent_span_id"] != clientLog["span_id"] {
		t.Fatalf("server parent span = %v, want client span %v", serverLog["parent_span_id"], clientLog["span_id"])
	}
	if clientLog["attempt"] != float64(3) {
		t.Fatalf("client attempt = %v, want 3", clientLog["attempt"])
	}
	if clientLog["error_code"] != "ok" || serverLog["error_code"] != "internal" {
		t.Fatalf("unexpected log error codes: client=%v server=%v", clientLog["error_code"], serverLog["error_code"])
	}
	if spanContext, ok := SpanContextFromContext(clientContext); !ok || spanContext.TraceID == "" {
		t.Fatalf("client Context has no trace identity: %+v", spanContext)
	}
	if spanContext, ok := SpanContextFromContext(serverContext); !ok || spanContext.TraceID == "" {
		t.Fatalf("server Context has no trace identity: %+v", spanContext)
	}
}

func TestMetricsClassifySuccessFailureTimeoutAndRateLimit(t *testing.T) {
	t.Parallel()

	metrics := NewMetrics(true)
	metrics.RecordRequest("client", "echo.v1.Echo", "Echo", "success", "ok", 2*time.Millisecond)
	metrics.RecordRequest("client", "echo.v1.Echo", "Echo", "failure", "internal", 3*time.Millisecond)
	metrics.RecordRequest("client", "echo.v1.Echo", "Echo", "timeout", "deadline_exceeded", 4*time.Millisecond)
	metrics.RecordRequest("server", "echo.v1.Echo", "Echo", "rate_limited", "resource_exhausted", 5*time.Millisecond)
	metrics.RecordRetry("client", "echo.v1.Echo", "Echo", 2)
	metrics.RecordRateLimited("server", "echo.v1.Echo", "Echo")
	metrics.SetConnections("client", 2)
	metrics.SetCircuitState("half_open")
	metrics.SetRegistryInstances("echo.v1.Echo", 3)
	metrics.RecordTLSHandshakeFailure("server")
	metrics.RecordTLSReload("server", true)
	metrics.RecordTLSReload("server", false)
	metrics.SetTLSConfigVersion("server", 4)

	var output bytes.Buffer
	if err := metrics.WritePrometheus(&output); err != nil {
		t.Fatalf("WritePrometheus() error = %v", err)
	}
	rendered := output.String()
	for _, expected := range []string{
		`rpc_requests_total{role="client",service="echo.v1.Echo",method="Echo",outcome="success"} 1`,
		`rpc_requests_total{role="client",service="echo.v1.Echo",method="Echo",outcome="failure"} 1`,
		`rpc_requests_total{role="client",service="echo.v1.Echo",method="Echo",outcome="timeout"} 1`,
		`rpc_requests_total{role="server",service="echo.v1.Echo",method="Echo",outcome="rate_limited"} 1`,
		`rpc_requests_failed_total{role="client",service="echo.v1.Echo",method="Echo",error_code="deadline_exceeded"} 1`,
		`rpc_request_duration_seconds_count{role="client",service="echo.v1.Echo",method="Echo"} 3`,
		`rpc_retries_total{role="client",service="echo.v1.Echo",method="Echo"} 2`,
		`rpc_connections{role="client"} 2`,
		`rpc_circuit_state{role="client",state="half_open"} 1`,
		`rpc_rate_limited_total{role="server",service="echo.v1.Echo",method="Echo"} 1`,
		`rpc_registry_instances{service="echo.v1.Echo"} 3`,
		`rpc_tls_handshake_failures_total{role="server"} 1`,
		`rpc_tls_reload_total{role="server",outcome="success"} 1`,
		`rpc_tls_reload_total{role="server",outcome="failure"} 1`,
		`rpc_tls_reload_failures_total{role="server"} 1`,
		`rpc_tls_config_version{role="server"} 4`,
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("metrics output does not contain %q\n%s", expected, rendered)
		}
	}
	if strings.Contains(rendered, "request_id") || strings.Contains(rendered, "trace_id") {
		t.Fatalf("metrics contain unbounded request labels:\n%s", rendered)
	}
}

func TestPrometheusHandlerAndDisabledRuntime(t *testing.T) {
	t.Parallel()

	runtime := New(Config{Enabled: true, Output: &bytes.Buffer{}})
	spanCtx, span := runtime.StartClientSpan(context.Background(), "echo.v1.Echo", "Echo")
	span.Finish(nil)
	if spanCtx == nil {
		t.Fatal("StartClientSpan() returned nil Context")
	}
	handler := PrometheusHandler(runtime)
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "text/plain") {
		t.Fatalf("content type = %q", contentType)
	}
	if !strings.Contains(recorder.Body.String(), "rpc_requests_total") {
		t.Fatalf("metrics body missing requests counter: %s", recorder.Body.String())
	}
	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/metrics", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD metrics = status %d body %q, want 200 and empty body", head.Code, head.Body.String())
	}

	post := httptest.NewRecorder()
	handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", post.Code)
	}

	var disabledOutput bytes.Buffer
	disabled := New(Config{Enabled: false, Output: &disabledOutput})
	disabledCtx, disabledSpan := disabled.StartClientSpan(context.Background(), "echo.v1.Echo", "Echo")
	if disabledCtx == nil || disabledSpan != nil {
		t.Fatalf("disabled StartClientSpan() = %v, %v; want Context and nil span", disabledCtx, disabledSpan)
	}
	disabledSpan.Finish(errors.New("ignored"))
	var disabledMetrics bytes.Buffer
	if err := disabled.Metrics().WritePrometheus(&disabledMetrics); err != nil {
		t.Fatalf("disabled WritePrometheus() error = %v", err)
	}
	if disabledOutput.Len() != 0 || disabledMetrics.Len() != 0 {
		t.Fatalf("disabled runtime emitted logs=%q metrics=%q", disabledOutput.String(), disabledMetrics.String())
	}
}

func TestDebugSwitchControlsStartEvents(t *testing.T) {
	t.Parallel()

	var quiet bytes.Buffer
	quietRuntime := New(Config{Enabled: true, Output: &quiet})
	_, quietSpan := quietRuntime.StartClientSpan(context.Background(), "echo.v1.Echo", "Echo")
	quietSpan.Finish(nil)
	if lines := nonEmptyLines(quiet.String()); lines != 1 {
		t.Fatalf("quiet log lines = %d, want 1; output=%s", lines, quiet.String())
	}

	var debug bytes.Buffer
	debugRuntime := New(Config{Enabled: true, Debug: true, Output: &debug})
	_, debugSpan := debugRuntime.StartClientSpan(context.Background(), "echo.v1.Echo", "Echo")
	debugSpan.Finish(nil)
	if lines := nonEmptyLines(debug.String()); lines != 2 {
		t.Fatalf("debug log lines = %d, want 2; output=%s", lines, debug.String())
	}
	if !strings.Contains(debug.String(), "rpc.call.start") {
		t.Fatalf("debug output missing start event: %s", debug.String())
	}
}

func TestTraceMetadataRoundTripAndInvalidIDs(t *testing.T) {
	t.Parallel()

	traceID := strings.Repeat("1", TraceIDBytes*2)
	spanID := strings.Repeat("2", SpanIDBytes*2)
	metadata := map[string]string{
		protocol.MetadataKeyTraceID:   traceID,
		protocol.MetadataKeySpanID:    spanID,
		protocol.MetadataKeyRequestID: "request-123",
	}
	parsed := SpanContextFromMetadata(metadata)
	if parsed.TraceID != traceID || parsed.SpanID != spanID || parsed.RequestID != "request-123" {
		t.Fatalf("parsed span context = %+v", parsed)
	}
	if traceparent := parsed.Traceparent(); traceparent != "00-"+traceID+"-"+spanID+"-01" {
		t.Fatalf("Traceparent() = %q", traceparent)
	}

	invalid := SpanContextFromMetadata(map[string]string{
		protocol.MetadataKeyTraceID: strings.Repeat("0", TraceIDBytes*2),
		protocol.MetadataKeySpanID:  "short",
	})
	if invalid.TraceID != "" || invalid.SpanID != "" {
		t.Fatalf("invalid IDs were accepted: %+v", invalid)
	}
}

func TestErrorCodeAndOutcomeClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err     error
		code    string
		outcome string
	}{
		{err: nil, code: "ok", outcome: "success"},
		{err: rpc.NewError(rpc.CodeDeadlineExceeded, "late"), code: "deadline_exceeded", outcome: "timeout"},
		{err: rpc.NewError(rpc.CodeResourceExhausted, "limit"), code: "resource_exhausted", outcome: "rate_limited"},
		{err: rpc.NewError(rpc.CodeCanceled, "stop"), code: "canceled", outcome: "canceled"},
		{err: rpc.NewError(rpc.CodeInternal, "boom"), code: "internal", outcome: "failure"},
		{err: context.DeadlineExceeded, code: "deadline_exceeded", outcome: "timeout"},
	}
	for _, test := range tests {
		code := ErrorCode(test.err)
		if code != test.code || Outcome(code) != test.outcome {
			t.Errorf("ErrorCode(%v) = %q/%q, want %q/%q", test.err, code, Outcome(code), test.code, test.outcome)
		}
	}
}

func TestMetricsConcurrentRecordAndRender(t *testing.T) {
	t.Parallel()

	metrics := NewMetrics(true)
	var waitGroup sync.WaitGroup
	waitGroup.Add(24)
	for index := range 24 {
		index := index
		go func() {
			defer waitGroup.Done()
			for iteration := range 50 {
				outcome := "success"
				code := "ok"
				if (index+iteration)%3 == 0 {
					outcome = "failure"
					code = "internal"
				}
				metrics.RecordRequest("client", "echo.v1.Echo", "Echo", outcome, code, time.Millisecond)
				metrics.ChangeInflight("client", 1)
				metrics.ChangeInflight("client", -1)
				var output bytes.Buffer
				if err := metrics.WritePrometheus(&output); err != nil {
					t.Errorf("WritePrometheus() error = %v", err)
				}
			}
		}()
	}
	waitGroup.Wait()
}

func TestMetricsBoundServiceMethodCardinality(t *testing.T) {
	t.Parallel()

	metrics := NewMetrics(true)
	for index := range MaxRouteSeries + 32 {
		metrics.RecordRequest(
			"client",
			"dynamic.Service",
			"Method"+strconv.Itoa(index),
			"success",
			"ok",
			time.Millisecond,
		)
	}
	var output bytes.Buffer
	if err := metrics.WritePrometheus(&output); err != nil {
		t.Fatalf("WritePrometheus() error = %v", err)
	}
	rendered := output.String()
	if strings.Contains(rendered, `method="Method512"`) {
		t.Fatalf("metrics exceeded route cardinality bound:\n%s", rendered)
	}
	if !strings.Contains(rendered, `method="`+overflowLabel+`"`) {
		t.Fatalf("metrics missing overflow route:\n%s", rendered)
	}
}

func decodeLog(t *testing.T, data []byte) map[string]any {
	t.Helper()
	lines := nonEmptyLinesBytes(data)
	if len(lines) != 1 {
		t.Fatalf("log line count = %d, want 1; output=%s", len(lines), data)
	}
	var value map[string]any
	if err := json.Unmarshal(lines[0], &value); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; line=%s", err, lines[0])
	}
	return value
}

func nonEmptyLines(value string) int {
	return len(nonEmptyLinesBytes([]byte(value)))
}

func nonEmptyLinesBytes(value []byte) [][]byte {
	var lines [][]byte
	for _, line := range bytes.Split(value, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, line)
		}
	}
	return lines
}

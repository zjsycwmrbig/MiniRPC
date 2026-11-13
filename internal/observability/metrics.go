package observability

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var durationBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05,
	0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

const (
	// MaxRouteSeries bounds service/method label combinations per process.
	MaxRouteSeries = 512
	overflowLabel  = "__overflow__"
)

type routeLabels struct {
	Role    string
	Service string
	Method  string
}

type requestLabels struct {
	Role    string
	Service string
	Method  string
	Outcome string
}

type failureLabels struct {
	Role      string
	Service   string
	Method    string
	ErrorCode string
}

type durationLabels struct {
	Role    string
	Service string
	Method  string
}

type retryLabels struct {
	Role    string
	Service string
	Method  string
}

type histogram struct {
	Buckets []uint64
	Count   uint64
	Sum     float64
}

// Metrics stores bounded RPC labels and renders Prometheus text exposition.
type Metrics struct {
	mu sync.Mutex

	enabled   bool
	routes    map[routeLabels]struct{}
	requests  map[requestLabels]uint64
	failures  map[failureLabels]uint64
	durations map[durationLabels]*histogram
	retries   map[retryLabels]uint64
	rateLimit map[requestLabels]uint64
	inflight  map[string]int64
	conns     map[string]int64
	circuit   map[string]string
	registry  map[string]int64
}

// metricsSnapshot detaches Prometheus rendering from request recording so a
// slow exporter cannot hold the metrics lock while it serializes output.
type metricsSnapshot struct {
	requests  map[requestLabels]uint64
	failures  map[failureLabels]uint64
	durations map[durationLabels]histogram
	retries   map[retryLabels]uint64
	rateLimit map[requestLabels]uint64
	inflight  map[string]int64
	conns     map[string]int64
	circuit   map[string]string
	registry  map[string]int64
}

// NewMetrics creates an empty registry.
func NewMetrics(enabled bool) *Metrics {
	return &Metrics{
		enabled:   enabled,
		routes:    make(map[routeLabels]struct{}),
		requests:  make(map[requestLabels]uint64),
		failures:  make(map[failureLabels]uint64),
		durations: make(map[durationLabels]*histogram),
		retries:   make(map[retryLabels]uint64),
		rateLimit: make(map[requestLabels]uint64),
		inflight:  make(map[string]int64),
		conns:     make(map[string]int64),
		circuit:   make(map[string]string),
		registry:  make(map[string]int64),
	}
}

// Enabled reports whether recording and rendering are active.
func (metrics *Metrics) Enabled() bool {
	return metrics != nil && metrics.enabled
}

func (metrics *Metrics) RecordRequest(
	role string,
	service string,
	method string,
	outcome string,
	errorCode string,
	duration time.Duration,
) {
	if !metrics.Enabled() {
		return
	}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	service, method = metrics.boundRouteLocked(role, service, method)
	request := requestLabels{Role: role, Service: service, Method: method, Outcome: outcome}
	metrics.requests[request]++
	if outcome != "success" {
		metrics.failures[failureLabels{Role: role, Service: service, Method: method, ErrorCode: errorCode}]++
	}
	durationKey := durationLabels{Role: role, Service: service, Method: method}
	current := metrics.durations[durationKey]
	if current == nil {
		current = &histogram{Buckets: make([]uint64, len(durationBuckets))}
		metrics.durations[durationKey] = current
	}
	seconds := duration.Seconds()
	for index, upperBound := range durationBuckets {
		if seconds <= upperBound {
			current.Buckets[index]++
		}
	}
	current.Count++
	current.Sum += seconds
}

func (metrics *Metrics) RecordRetry(role, service, method string, attempts int) {
	if !metrics.Enabled() || attempts <= 0 {
		return
	}
	metrics.mu.Lock()
	service, method = metrics.boundRouteLocked(role, service, method)
	metrics.retries[retryLabels{Role: role, Service: service, Method: method}] += uint64(attempts)
	metrics.mu.Unlock()
}

func (metrics *Metrics) RecordRateLimited(role, service, method string) {
	if !metrics.Enabled() {
		return
	}
	metrics.mu.Lock()
	service, method = metrics.boundRouteLocked(role, service, method)
	metrics.rateLimit[requestLabels{Role: role, Service: service, Method: method}]++
	metrics.mu.Unlock()
}

func (metrics *Metrics) ChangeInflight(role string, delta int64) {
	if !metrics.Enabled() {
		return
	}
	metrics.mu.Lock()
	metrics.inflight[role] += delta
	if metrics.inflight[role] < 0 {
		metrics.inflight[role] = 0
	}
	metrics.mu.Unlock()
}

// SetConnections updates a process-local connection gauge.
func (metrics *Metrics) SetConnections(role string, count int) {
	if !metrics.Enabled() {
		return
	}
	if count < 0 {
		count = 0
	}
	metrics.mu.Lock()
	metrics.conns[role] = int64(count)
	metrics.mu.Unlock()
}

// SetCircuitState updates the client breaker gauge.
func (metrics *Metrics) SetCircuitState(state string) {
	if !metrics.Enabled() {
		return
	}
	metrics.mu.Lock()
	metrics.circuit["client"] = state
	metrics.mu.Unlock()
}

// SetRegistryInstances updates the known instance count for one service.
func (metrics *Metrics) SetRegistryInstances(service string, count int) {
	if !metrics.Enabled() {
		return
	}
	if count < 0 {
		count = 0
	}
	metrics.mu.Lock()
	if _, exists := metrics.registry[service]; !exists && len(metrics.registry) >= MaxRouteSeries {
		service = overflowLabel
	}
	metrics.registry[service] = int64(count)
	metrics.mu.Unlock()
}

func (metrics *Metrics) boundRouteLocked(role, service, method string) (string, string) {
	key := routeLabels{Role: role, Service: service, Method: method}
	if _, exists := metrics.routes[key]; exists {
		return service, method
	}
	if len(metrics.routes) >= MaxRouteSeries {
		return overflowLabel, overflowLabel
	}
	metrics.routes[key] = struct{}{}
	return service, method
}

func (metrics *Metrics) snapshotLocked() metricsSnapshot {
	snapshot := metricsSnapshot{
		requests:  maps.Clone(metrics.requests),
		failures:  maps.Clone(metrics.failures),
		durations: make(map[durationLabels]histogram, len(metrics.durations)),
		retries:   maps.Clone(metrics.retries),
		rateLimit: maps.Clone(metrics.rateLimit),
		inflight:  maps.Clone(metrics.inflight),
		conns:     maps.Clone(metrics.conns),
		circuit:   maps.Clone(metrics.circuit),
		registry:  maps.Clone(metrics.registry),
	}
	for key, value := range metrics.durations {
		snapshot.durations[key] = histogram{
			Buckets: slices.Clone(value.Buckets),
			Count:   value.Count,
			Sum:     value.Sum,
		}
	}
	return snapshot
}

// WritePrometheus renders deterministic Prometheus text format.
func (metrics *Metrics) WritePrometheus(writer io.Writer) error {
	if writer == nil {
		return fmt.Errorf("observability: nil metrics writer")
	}
	if !metrics.Enabled() {
		return nil
	}
	metrics.mu.Lock()
	snapshot := metrics.snapshotLocked()
	metrics.mu.Unlock()

	var output strings.Builder
	output.WriteString("# HELP rpc_requests_total Completed RPC calls by outcome.\n")
	output.WriteString("# TYPE rpc_requests_total counter\n")
	for _, key := range sortedKeys(snapshot.requests) {
		fmt.Fprintf(&output, "rpc_requests_total%s %d\n", labels(
			"role", key.Role,
			"service", key.Service,
			"method", key.Method,
			"outcome", key.Outcome,
		), snapshot.requests[key])
	}

	output.WriteString("# HELP rpc_requests_failed_total Failed RPC calls by stable error code.\n")
	output.WriteString("# TYPE rpc_requests_failed_total counter\n")
	for _, key := range sortedKeys(snapshot.failures) {
		fmt.Fprintf(&output, "rpc_requests_failed_total%s %d\n", labels(
			"role", key.Role,
			"service", key.Service,
			"method", key.Method,
			"error_code", key.ErrorCode,
		), snapshot.failures[key])
	}

	output.WriteString("# HELP rpc_request_duration_seconds RPC duration in seconds.\n")
	output.WriteString("# TYPE rpc_request_duration_seconds histogram\n")
	for _, key := range sortedKeys(snapshot.durations) {
		value := snapshot.durations[key]
		for index, upperBound := range durationBuckets {
			fmt.Fprintf(&output, "rpc_request_duration_seconds_bucket%s %d\n", labels(
				"role", key.Role,
				"service", key.Service,
				"method", key.Method,
				"le", strconv.FormatFloat(upperBound, 'g', -1, 64),
			), value.Buckets[index])
		}
		fmt.Fprintf(&output, "rpc_request_duration_seconds_bucket%s %d\n", labels(
			"role", key.Role,
			"service", key.Service,
			"method", key.Method,
			"le", "+Inf",
		), value.Count)
		fmt.Fprintf(&output, "rpc_request_duration_seconds_sum%s %s\n", labels(
			"role", key.Role,
			"service", key.Service,
			"method", key.Method,
		), strconv.FormatFloat(value.Sum, 'g', -1, 64))
		fmt.Fprintf(&output, "rpc_request_duration_seconds_count%s %d\n", labels(
			"role", key.Role,
			"service", key.Service,
			"method", key.Method,
		), value.Count)
	}

	output.WriteString("# HELP rpc_inflight_requests RPC calls currently executing.\n")
	output.WriteString("# TYPE rpc_inflight_requests gauge\n")
	for _, role := range sortedKeys(snapshot.inflight) {
		fmt.Fprintf(&output, "rpc_inflight_requests%s %d\n", labels("role", role), snapshot.inflight[role])
	}
	output.WriteString("# HELP rpc_connections Active transport connections.\n")
	output.WriteString("# TYPE rpc_connections gauge\n")
	for _, role := range sortedKeys(snapshot.conns) {
		fmt.Fprintf(&output, "rpc_connections%s %d\n", labels("role", role), snapshot.conns[role])
	}

	output.WriteString("# HELP rpc_retries_total Extra RPC attempts caused by retry policy.\n")
	output.WriteString("# TYPE rpc_retries_total counter\n")
	for _, key := range sortedKeys(snapshot.retries) {
		fmt.Fprintf(&output, "rpc_retries_total%s %d\n", labels(
			"role", key.Role,
			"service", key.Service,
			"method", key.Method,
		), snapshot.retries[key])
	}

	output.WriteString("# HELP rpc_circuit_state Current client circuit-breaker state.\n")
	output.WriteString("# TYPE rpc_circuit_state gauge\n")
	for _, role := range sortedKeys(snapshot.circuit) {
		fmt.Fprintf(&output, "rpc_circuit_state%s 1\n", labels(
			"role", role,
			"state", snapshot.circuit[role],
		))
	}

	output.WriteString("# HELP rpc_rate_limited_total Calls rejected with RESOURCE_EXHAUSTED.\n")
	output.WriteString("# TYPE rpc_rate_limited_total counter\n")
	for _, key := range sortedKeys(snapshot.rateLimit) {
		fmt.Fprintf(&output, "rpc_rate_limited_total%s %d\n", labels(
			"role", key.Role,
			"service", key.Service,
			"method", key.Method,
		), snapshot.rateLimit[key])
	}

	output.WriteString("# HELP rpc_registry_instances Known service instances.\n")
	output.WriteString("# TYPE rpc_registry_instances gauge\n")
	for _, service := range sortedKeys(snapshot.registry) {
		fmt.Fprintf(&output, "rpc_registry_instances%s %d\n", labels("service", service), snapshot.registry[service])
	}

	_, err := io.WriteString(writer, output.String())
	return err
}

func labels(pairs ...string) string {
	if len(pairs) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteByte('{')
	for index := 0; index < len(pairs); index += 2 {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(pairs[index])
		builder.WriteString("=\"")
		builder.WriteString(escapeLabel(pairs[index+1]))
		builder.WriteByte('"')
	}
	builder.WriteByte('}')
	return builder.String()
}

func escapeLabel(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"")
	return replacer.Replace(value)
}

func sortedKeys[K comparable, V any](values map[K]V) []K {
	keys := make([]K, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		return fmt.Sprint(keys[left]) < fmt.Sprint(keys[right])
	})
	return keys
}

// Command benchmark measures Mini-RPC latency, throughput, governance cost,
// connection reuse, multi-instance balancing, TLS/mTLS overhead, and
// connection setup cost in a local process.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
)

func main() {
	duration := flag.Duration("duration", 3*time.Second, "measured duration per case")
	warmup := flag.Duration("warmup", 500*time.Millisecond, "warmup duration per case")
	concurrency := flag.Int("concurrency", 64, "concurrent workers")
	sizes := flag.String("sizes", "32,256,1024,4096", "comma-separated Echo message sizes in bytes")
	transports := flag.String("transports", "plaintext,tls,mtls", "comma-separated transports: plaintext, tls, mtls, h2c, h2-tls, h2-mtls")
	handshakes := flag.Int("handshakes", 64, "connection setup samples per transport; 0 disables")
	targetAddress := flag.String("target", "", "external HTTP/2 server address; selects one HTTP/2 transport and disables local server setup")
	targetCA := flag.String("target-ca", "", "PEM root CA used to verify an external TLS server")
	targetCert := flag.String("target-cert", "", "PEM client certificate for an external mTLS server")
	targetKey := flag.String("target-key", "", "PEM client key paired with -target-cert")
	targetServerName := flag.String("target-server-name", "", "expected external TLS server name; empty derives it from the target host")
	targetTLSMinVersion := flag.String("target-tls-min-version", "1.2", "minimum external HTTP/2 TLS version: 1.2 or 1.3")
	jsonOutput := flag.Bool("json", false, "emit a machine-readable report")
	flag.Parse()

	if *duration <= 0 || *warmup < 0 || *concurrency <= 0 || *handshakes < 0 {
		fmt.Fprintln(os.Stderr, "duration and concurrency must be positive; warmup and handshakes cannot be negative")
		os.Exit(2)
	}
	bodySizes, err := parseSizes(*sizes)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	selectedTransports, err := parseTransports(*transports)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	externalTarget, err := loadExternalHTTP2Target(
		selectedTransports,
		*targetAddress,
		*targetCA,
		*targetCert,
		*targetKey,
		*targetServerName,
		*targetTLSMinVersion,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	environment := collectEnvironment()
	config := RunConfig{
		Duration:    *duration,
		Warmup:      *warmup,
		Concurrency: *concurrency,
		BodySizes:   bodySizes,
		Transports:  transportNames(selectedTransports),
		Handshakes:  *handshakes,
	}
	if externalTarget != nil {
		config.Topology = "external-http2"
		config.TargetAddress = externalTarget.address
	} else {
		config.Topology = "in-process"
	}
	results := make([]Result, 0, len(bodySizes)+10)
	if externalTarget != nil {
		transport := selectedTransports[0]
		benchmarkCases := []Case{http2DirectCase(transport, 1, externalTarget)}
		if *concurrency > 1 {
			benchmarkCases = append(
				benchmarkCases,
				http2DirectCase(transport, *concurrency, externalTarget),
			)
		}
		for _, benchmarkCase := range benchmarkCases {
			result, runErr := runCase(config, benchmarkCase)
			if runErr != nil {
				fmt.Fprintln(os.Stderr, runErr)
				os.Exit(1)
			}
			results = append(results, result)
		}
	} else if containsTransport(selectedTransports, transportPlaintext) {
		for _, bodySize := range bodySizes {
			result, runErr := runCase(config, directCase(bodySize, 1, 1, false))
			if runErr != nil {
				fmt.Fprintln(os.Stderr, runErr)
				os.Exit(1)
			}
			results = append(results, result)
		}

		remainingCases := []Case{
			concurrentSingleConnectionCase(*concurrency),
			concurrentConnectionPoolCase(*concurrency),
			multiInstanceCase(*concurrency, multiInstanceRoundRobin),
			multiInstanceCase(*concurrency, multiInstanceWeighted),
			directCase(1024, 1, 1, false, withName("governance-off/serial/1conn/1024B")),
			directCase(1024, 1, 1, true, withName("governance-on/serial/1conn/1024B")),
		}
		for _, benchmarkCase := range remainingCases {
			result, runErr := runCase(config, benchmarkCase)
			if runErr != nil {
				fmt.Fprintln(os.Stderr, runErr)
				os.Exit(1)
			}
			results = append(results, result)
		}
	}
	for _, transport := range []benchmarkTransport{transportTLS, transportMTLS} {
		if !containsTransport(selectedTransports, transport) {
			continue
		}
		for _, benchmarkCase := range []Case{
			secureDirectCase(transport, 1),
			secureDirectCase(transport, *concurrency),
		} {
			result, runErr := runCase(config, benchmarkCase)
			if runErr != nil {
				fmt.Fprintln(os.Stderr, runErr)
				os.Exit(1)
			}
			results = append(results, result)
		}
	}
	if externalTarget == nil {
		for _, transport := range []benchmarkTransport{transportH2C, transportH2TLS, transportH2MTLS} {
			if !containsTransport(selectedTransports, transport) {
				continue
			}
			benchmarkCases := []Case{http2DirectCase(transport, 1, nil)}
			if *concurrency > 1 {
				benchmarkCases = append(
					benchmarkCases,
					http2DirectCase(transport, *concurrency, nil),
				)
			}
			for _, benchmarkCase := range benchmarkCases {
				result, runErr := runCase(config, benchmarkCase)
				if runErr != nil {
					fmt.Fprintln(os.Stderr, runErr)
					os.Exit(1)
				}
				results = append(results, result)
			}
		}
	}

	handshakeResults := make([]HandshakeResult, 0, len(selectedTransports))
	if *handshakes > 0 {
		for _, transport := range selectedTransports {
			var result HandshakeResult
			var runErr error
			if isHTTP2Transport(transport) {
				result, runErr = measureHTTP2ConnectionSetup(
					transport,
					*handshakes,
					externalTarget,
				)
			} else {
				if externalTarget != nil {
					continue
				}
				result, runErr = measureConnectionSetup(transport, *handshakes)
			}
			if runErr != nil {
				fmt.Fprintln(os.Stderr, runErr)
				os.Exit(1)
			}
			handshakeResults = append(handshakeResults, result)
		}
	}

	report := Report{
		GeneratedAt: time.Now().UTC(),
		Environment: environment,
		Config:      config,
		Results:     results,
		Handshakes:  handshakeResults,
	}
	if *jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}
	if err := printReport(os.Stdout, report); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func parseSizes(value string) ([]int, error) {
	parts := strings.Split(value, ",")
	sizes := make([]int, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		size, err := strconv.Atoi(trimmed)
		if err != nil || size <= 0 {
			return nil, fmt.Errorf("invalid message size %q", trimmed)
		}
		sizes = append(sizes, size)
	}
	if len(sizes) == 0 {
		return nil, fmt.Errorf("at least one message size is required")
	}
	return sizes, nil
}

func parseTransports(value string) ([]benchmarkTransport, error) {
	parts := strings.Split(value, ",")
	transports := make([]benchmarkTransport, 0, len(parts))
	seen := make(map[benchmarkTransport]bool, len(parts))
	for _, part := range parts {
		transport := benchmarkTransport(strings.TrimSpace(part))
		switch transport {
		case transportPlaintext, transportTLS, transportMTLS:
		case transportH2C, transportH2TLS, transportH2MTLS:
		default:
			return nil, fmt.Errorf("invalid transport %q", transport)
		}
		if seen[transport] {
			continue
		}
		seen[transport] = true
		transports = append(transports, transport)
	}
	if len(transports) == 0 {
		return nil, fmt.Errorf("at least one transport is required")
	}
	return transports, nil
}

func containsTransport(transports []benchmarkTransport, wanted benchmarkTransport) bool {
	for _, transport := range transports {
		if transport == wanted {
			return true
		}
	}
	return false
}

func transportNames(transports []benchmarkTransport) []string {
	names := make([]string, len(transports))
	for index, transport := range transports {
		names[index] = string(transport)
	}
	return names
}

type RunConfig struct {
	Duration      time.Duration `json:"duration_ns"`
	Warmup        time.Duration `json:"warmup_ns"`
	Concurrency   int           `json:"concurrency"`
	BodySizes     []int         `json:"body_sizes"`
	Transports    []string      `json:"transports"`
	Handshakes    int           `json:"handshake_samples"`
	Topology      string        `json:"topology"`
	TargetAddress string        `json:"target_address,omitempty"`
}

type Environment struct {
	GoVersion   string            `json:"go_version"`
	GOOS        string            `json:"goos"`
	GOARCH      string            `json:"goarch"`
	NumCPU      int               `json:"num_cpu"`
	GOMAXPROCS  int               `json:"gomaxprocs"`
	CPUModel    string            `json:"cpu_model"`
	Kernel      string            `json:"kernel"`
	Hostname    string            `json:"hostname"`
	TotalMemory uint64            `json:"total_memory_bytes"`
	Commit      string            `json:"commit"`
	BuildInfo   map[string]string `json:"build_info"`
}

type Result struct {
	Case           string        `json:"case"`
	Mode           string        `json:"mode"`
	Transport      string        `json:"transport"`
	BodyBytes      int           `json:"body_bytes"`
	Concurrency    int           `json:"concurrency"`
	Connections    int           `json:"connections"`
	Instances      int           `json:"instances"`
	Requests       uint64        `json:"requests"`
	Failures       uint64        `json:"failures"`
	ErrorRate      float64       `json:"error_rate"`
	QPS            float64       `json:"qps"`
	P50            time.Duration `json:"p50_ns"`
	P95            time.Duration `json:"p95_ns"`
	P99            time.Duration `json:"p99_ns"`
	Max            time.Duration `json:"max_ns"`
	Elapsed        time.Duration `json:"elapsed_ns"`
	CPUSeconds     float64       `json:"cpu_seconds"`
	CPUCores       float64       `json:"cpu_cores"`
	HeapAllocBytes uint64        `json:"heap_alloc_bytes"`
	RSSBytes       uint64        `json:"rss_bytes"`
	Distribution   string        `json:"instance_distribution,omitempty"`
}

type HandshakeResult struct {
	Transport string        `json:"transport"`
	Samples   int           `json:"samples"`
	Failures  uint64        `json:"failures"`
	P50       time.Duration `json:"p50_ns"`
	P95       time.Duration `json:"p95_ns"`
	P99       time.Duration `json:"p99_ns"`
	Max       time.Duration `json:"max_ns"`
}

type Report struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Environment Environment       `json:"environment"`
	Config      RunConfig         `json:"config"`
	Results     []Result          `json:"results"`
	Handshakes  []HandshakeResult `json:"connection_setup"`
}

type workloadResult struct {
	requests     uint64
	failures     uint64
	latencies    []time.Duration
	elapsed      time.Duration
	distribution [2]uint64
}

func runWorkload(
	duration time.Duration,
	workers int,
	bodySize int,
	idempotent bool,
	echoClient echov1.EchoClient,
	serverCalls [2]*atomic.Uint64,
) workloadResult {
	perWorker := make([][]time.Duration, workers)
	var requestCount atomic.Uint64
	var failureCount atomic.Uint64
	startCalls := [2]uint64{}
	for index := range serverCalls {
		if serverCalls[index] != nil {
			startCalls[index] = serverCalls[index].Load()
		}
	}
	stopAt := time.Now().Add(duration)
	started := time.Now()
	var waitGroup sync.WaitGroup
	waitGroup.Add(workers)
	payload := strings.Repeat("x", bodySize)
	for worker := 0; worker < workers; worker++ {
		worker := worker
		go func() {
			defer waitGroup.Done()
			latencies := make([]time.Duration, 0, 4096)
			request := &echov1.EchoRequest{Message: payload}
			for time.Now().Before(stopAt) {
				callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if idempotent {
					callCtx = rpc.WithIdempotent(callCtx)
				}
				callStarted := time.Now()
				response, err := echoClient.Echo(callCtx, request)
				latencies = append(latencies, time.Since(callStarted))
				cancel()
				requestCount.Add(1)
				if err != nil || response.GetMessage() != payload {
					failureCount.Add(1)
				}
			}
			perWorker[worker] = latencies
		}()
	}
	waitGroup.Wait()
	elapsed := time.Since(started)

	total := 0
	for _, latencies := range perWorker {
		total += len(latencies)
	}
	allLatencies := make([]time.Duration, 0, total)
	for _, latencies := range perWorker {
		allLatencies = append(allLatencies, latencies...)
	}
	distribution := [2]uint64{}
	for index := range serverCalls {
		if serverCalls[index] != nil {
			distribution[index] = serverCalls[index].Load() - startCalls[index]
		}
	}
	return workloadResult{
		requests:     requestCount.Load(),
		failures:     failureCount.Load(),
		latencies:    allLatencies,
		elapsed:      elapsed,
		distribution: distribution,
	}
}

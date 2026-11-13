// Command benchmark measures Mini-RPC latency, throughput, governance cost,
// connection reuse, and multi-instance balancing in a local process.
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
	jsonOutput := flag.Bool("json", false, "emit a machine-readable report")
	flag.Parse()

	if *duration <= 0 || *warmup < 0 || *concurrency <= 0 {
		fmt.Fprintln(os.Stderr, "duration and concurrency must be positive; warmup cannot be negative")
		os.Exit(2)
	}
	bodySizes, err := parseSizes(*sizes)
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
	}
	results := make([]Result, 0, len(bodySizes)+6)
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

	report := Report{
		GeneratedAt: time.Now().UTC(),
		Environment: environment,
		Config:      config,
		Results:     results,
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

type RunConfig struct {
	Duration    time.Duration `json:"duration_ns"`
	Warmup      time.Duration `json:"warmup_ns"`
	Concurrency int           `json:"concurrency"`
	BodySizes   []int         `json:"body_sizes"`
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

type Report struct {
	GeneratedAt time.Time   `json:"generated_at"`
	Environment Environment `json:"environment"`
	Config      RunConfig   `json:"config"`
	Results     []Result    `json:"results"`
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

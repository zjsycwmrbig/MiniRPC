package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/balancer"
	"github.com/zjsycwmrbig/mini-rpc/internal/client"
	"github.com/zjsycwmrbig/mini-rpc/internal/discovery"
	"github.com/zjsycwmrbig/mini-rpc/internal/governance"
	"github.com/zjsycwmrbig/mini-rpc/internal/registry"
	"github.com/zjsycwmrbig/mini-rpc/internal/resolver"
	"github.com/zjsycwmrbig/mini-rpc/internal/server"
	"github.com/zjsycwmrbig/mini-rpc/internal/testkit"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
)

type Case struct {
	Name        string
	Mode        string
	Transport   benchmarkTransport
	BodyBytes   int
	Concurrency int
	Instances   int
	Governance  bool
	Setup       func(context.Context) (*benchmarkClient, error)
}

type benchmarkTransport string

const (
	transportPlaintext benchmarkTransport = "plaintext"
	transportTLS       benchmarkTransport = "tls"
	transportMTLS      benchmarkTransport = "mtls"
	transportH2C       benchmarkTransport = "h2c"
	transportH2TLS     benchmarkTransport = "h2-tls"
	transportH2MTLS    benchmarkTransport = "h2-mtls"
)

func isHTTP2Transport(transport benchmarkTransport) bool {
	return transport == transportH2C || transport == transportH2TLS || transport == transportH2MTLS
}

func transportUsesTLS(transport benchmarkTransport) bool {
	return transport == transportTLS ||
		transport == transportMTLS ||
		transport == transportH2TLS ||
		transport == transportH2MTLS
}

func transportUsesMutualTLS(transport benchmarkTransport) bool {
	return transport == transportMTLS || transport == transportH2MTLS
}

type caseOption func(*Case)

func withName(name string) caseOption {
	return func(benchmarkCase *Case) {
		benchmarkCase.Name = name
	}
}

func directCase(
	bodySize int,
	maxConnections int,
	concurrency int,
	governanceEnabled bool,
	options ...caseOption,
) Case {
	benchmarkCase := Case{
		Name:        fmt.Sprintf("serial/1conn/%dB", bodySize),
		Mode:        "direct",
		Transport:   transportPlaintext,
		BodyBytes:   bodySize,
		Concurrency: 1,
		Instances:   1,
		Governance:  governanceEnabled,
	}
	if concurrency > 1 {
		benchmarkCase.Name = fmt.Sprintf("concurrent/%dconn/%dB", maxConnections, bodySize)
		benchmarkCase.Concurrency = concurrency
		benchmarkCase.Mode = "direct-pool"
	}
	if governanceEnabled {
		benchmarkCase.Name = fmt.Sprintf("governance-on/serial/1conn/%dB", bodySize)
	}
	for _, option := range options {
		option(&benchmarkCase)
	}
	benchmarkCase.Setup = func(ctx context.Context) (*benchmarkClient, error) {
		calls := &atomic.Uint64{}
		target, err := startBenchmarkServer(calls)
		if err != nil {
			return nil, err
		}
		perConnectionConcurrency := (concurrency + maxConnections - 1) / maxConnections
		if perConnectionConcurrency < 1 {
			perConnectionConcurrency = 1
		}
		clientOptions := directClientOptions(maxConnections, perConnectionConcurrency, governanceEnabled)
		rpcClient, err := client.Dial(ctx, target.address, clientOptions...)
		if err != nil {
			_ = target.stop()
			return nil, err
		}
		return &benchmarkClient{
			conn:            rpcClient,
			idempotent:      governanceEnabled,
			connections:     func() int { return rpcClient.Stats().Connections },
			instances:       1,
			serverCallCount: [2]*atomic.Uint64{calls, nil},
			close: func() error {
				return errors.Join(rpcClient.Close(), target.stop())
			},
		}, nil
	}
	return benchmarkCase
}

func secureDirectCase(transport benchmarkTransport, concurrency int) Case {
	name := fmt.Sprintf("%s/serial/1conn/1024B", transport)
	mode := "direct-" + string(transport)
	if concurrency > 1 {
		name = fmt.Sprintf("%s/concurrent/1conn/1024B/c%d", transport, concurrency)
		mode = "direct-single-connection-" + string(transport)
	}
	return Case{
		Name:        name,
		Mode:        mode,
		Transport:   transport,
		BodyBytes:   1024,
		Concurrency: concurrency,
		Instances:   1,
		Setup: func(ctx context.Context) (*benchmarkClient, error) {
			serverTLS, clientTLS, err := benchmarkTLSConfigs(transport)
			if err != nil {
				return nil, err
			}
			calls := &atomic.Uint64{}
			target, err := startBenchmarkServerWithTLS(calls, serverTLS)
			if err != nil {
				return nil, err
			}
			clientOptions := directClientOptions(1, concurrency, false)
			clientOptions = append(clientOptions, client.WithTLSConfig(clientTLS))
			rpcClient, err := client.Dial(ctx, target.address, clientOptions...)
			if err != nil {
				_ = target.stop()
				return nil, err
			}
			return &benchmarkClient{
				conn:            rpcClient,
				connections:     func() int { return rpcClient.Stats().Connections },
				instances:       1,
				serverCallCount: [2]*atomic.Uint64{calls, nil},
				close: func() error {
					return errors.Join(rpcClient.Close(), target.stop())
				},
			}, nil
		},
	}
}

func concurrentSingleConnectionCase(concurrency int) Case {
	benchmarkCase := directCase(1024, 1, concurrency, false)
	benchmarkCase.Name = fmt.Sprintf("concurrent/1conn/1024B/c%d", concurrency)
	benchmarkCase.Mode = "direct-single-connection"
	return benchmarkCase
}

func concurrentConnectionPoolCase(concurrency int) Case {
	benchmarkCase := directCase(1024, 8, concurrency, false)
	benchmarkCase.Name = fmt.Sprintf("concurrent/8conn/1024B/c%d", concurrency)
	benchmarkCase.Mode = "direct-pool"
	return benchmarkCase
}

type multiInstanceMode string

const (
	multiInstanceRoundRobin multiInstanceMode = "round-robin"
	multiInstanceWeighted   multiInstanceMode = "weighted-1-2"
)

func multiInstanceCase(concurrency int, mode multiInstanceMode) Case {
	name := fmt.Sprintf("multi-instance/%s/1024B/c%d", mode, concurrency)
	return Case{
		Name:        name,
		Mode:        "discovery" + string(mode),
		Transport:   transportPlaintext,
		BodyBytes:   1024,
		Concurrency: concurrency,
		Instances:   2,
		Governance:  true,
		Setup: func(ctx context.Context) (*benchmarkClient, error) {
			callsA := &atomic.Uint64{}
			callsB := &atomic.Uint64{}
			first, err := startBenchmarkServer(callsA)
			if err != nil {
				return nil, err
			}
			second, err := startBenchmarkServer(callsB)
			if err != nil {
				_ = first.stop()
				return nil, err
			}
			memory := testkit.NewMemoryRegistry()
			weights := []int{1, 1}
			if mode == multiInstanceWeighted {
				weights = []int{1, 2}
			}
			instances := []registry.Instance{
				{ID: "a", Service: echov1.EchoServiceName, Address: first.address, Weight: weights[0]},
				{ID: "b", Service: echov1.EchoServiceName, Address: second.address, Weight: weights[1]},
			}
			for _, instance := range instances {
				if _, err := memory.Register(ctx, instance, time.Minute); err != nil {
					_ = first.stop()
					_ = second.stop()
					_ = memory.Close()
					return nil, err
				}
			}
			var selectedBalancer balancer.Balancer = balancer.NewRoundRobin()
			if mode == multiInstanceWeighted {
				selectedBalancer = balancer.NewWeightedRoundRobin()
			}
			serviceResolver := resolver.New(memory, resolver.Options{
				InitialBackoff: 5 * time.Millisecond,
				MaxBackoff:     50 * time.Millisecond,
			})
			discoveryClient, err := discovery.New(ctx, discovery.Config{
				Service:  echov1.EchoServiceName,
				Resolver: serviceResolver,
				Balancer: selectedBalancer,
				Retry: governance.RetryPolicy{
					MaxAttempts:    2,
					InitialBackoff: time.Millisecond,
					MaxBackoff:     time.Millisecond,
					MaxElapsed:     time.Second,
					Jitter:         func(time.Duration) time.Duration { return 0 },
				},
				ClientOptions: []client.Option{
					client.WithMaxConnections(2),
					client.WithMaxIdleConnections(2),
					client.WithMaxConcurrentCalls(concurrency + 32),
					client.WithObserver(nil),
				},
			})
			if err != nil {
				_ = serviceResolver.Close()
				_ = memory.Close()
				_ = first.stop()
				_ = second.stop()
				return nil, err
			}
			return &benchmarkClient{
				conn:            discoveryClient,
				idempotent:      true,
				connections:     func() int { return discoveryClient.Stats().Connections },
				instances:       2,
				serverCallCount: [2]*atomic.Uint64{callsA, callsB},
				close: func() error {
					return errors.Join(
						discoveryClient.Close(),
						serviceResolver.Close(),
						memory.Close(),
						first.stop(),
						second.stop(),
					)
				},
			}, nil
		},
	}
}

func directClientOptions(maxConnections, maxConcurrentCalls int, governanceEnabled bool) []client.Option {
	options := []client.Option{
		client.WithMaxConnections(maxConnections),
		client.WithMaxIdleConnections(maxConnections),
		client.WithMaxConcurrentCalls(maxConcurrentCalls),
		client.WithObserver(nil),
	}
	if governanceEnabled {
		options = append(options,
			client.WithRetryPolicy(governance.RetryPolicy{
				MaxAttempts:    3,
				InitialBackoff: time.Millisecond,
				MaxBackoff:     10 * time.Millisecond,
				MaxElapsed:     time.Second,
				Jitter:         func(time.Duration) time.Duration { return 0 },
			}),
			client.WithCircuitBreaker(governance.CircuitBreakerConfig{
				FailureThreshold: 5,
				OpenTimeout:      time.Second,
			}),
			client.WithRateLimits(governance.RateLimitConfig{
				Services: map[string]governance.RateLimitRule{
					echov1.EchoServiceName: {Rate: 1e9, Burst: maxConcurrentCalls},
				},
			}),
		)
	} else {
		options = append(options,
			client.WithRetryPolicy(governance.RetryPolicy{
				MaxAttempts:    1,
				InitialBackoff: time.Nanosecond,
				MaxBackoff:     time.Nanosecond,
				MaxElapsed:     time.Nanosecond,
				Jitter:         func(time.Duration) time.Duration { return 0 },
			}),
			client.WithCircuitBreaker(governance.CircuitBreakerConfig{
				FailureThreshold: math.MaxInt,
				OpenTimeout:      time.Hour,
			}),
		)
	}
	return options
}

type benchmarkClient struct {
	conn            rpc.ClientConn
	idempotent      bool
	connections     func() int
	instances       int
	serverCallCount [2]*atomic.Uint64
	close           func() error
}

func runCase(config RunConfig, benchmarkCase Case) (Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	target, err := benchmarkCase.Setup(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("setup %s: %w", benchmarkCase.Name, err)
	}
	defer func() { _ = target.close() }()
	echoClient := echov1.NewEchoClient(target.conn)

	if config.Warmup > 0 {
		warm := runWorkload(
			config.Warmup,
			benchmarkCase.Concurrency,
			benchmarkCase.BodyBytes,
			target.idempotent,
			echoClient,
			target.serverCallCount,
		)
		if warm.failures > 0 {
			return Result{}, fmt.Errorf("warmup %s recorded %d failures", benchmarkCase.Name, warm.failures)
		}
	}

	runtime.GC()
	cpuBefore, _ := readCPUUsage()
	measurement := runWorkload(
		config.Duration,
		benchmarkCase.Concurrency,
		benchmarkCase.BodyBytes,
		target.idempotent,
		echoClient,
		target.serverCallCount,
	)
	cpuAfter, _ := readCPUUsage()
	memory := readMemoryUsage()

	result := Result{
		Case:           benchmarkCase.Name,
		Mode:           benchmarkCase.Mode,
		Transport:      string(benchmarkCase.Transport),
		BodyBytes:      benchmarkCase.BodyBytes,
		Concurrency:    benchmarkCase.Concurrency,
		Connections:    target.connections(),
		Instances:      target.instances,
		Requests:       measurement.requests,
		Failures:       measurement.failures,
		Elapsed:        measurement.elapsed,
		HeapAllocBytes: memory.heapAlloc,
		RSSBytes:       memory.rss,
	}
	if measurement.requests > 0 {
		result.ErrorRate = float64(measurement.failures) / float64(measurement.requests)
	}
	if measurement.elapsed > 0 {
		result.QPS = float64(measurement.requests) / measurement.elapsed.Seconds()
	}
	result.P50 = percentile(measurement.latencies, 0.50)
	result.P95 = percentile(measurement.latencies, 0.95)
	result.P99 = percentile(measurement.latencies, 0.99)
	for _, latency := range measurement.latencies {
		if latency > result.Max {
			result.Max = latency
		}
	}
	if cpuBefore.valid && cpuAfter.valid {
		result.CPUSeconds = cpuAfter.seconds - cpuBefore.seconds
		if result.Elapsed > 0 {
			result.CPUCores = result.CPUSeconds / result.Elapsed.Seconds()
		}
	}
	if benchmarkCase.Instances == 2 {
		result.Distribution = fmt.Sprintf("a=%d,b=%d", measurement.distribution[0], measurement.distribution[1])
	}
	return result, nil
}

func percentile(values []time.Duration, fraction float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	if fraction <= 0 {
		fraction = 0
	}
	if fraction >= 1 {
		fraction = 1
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(left, right int) bool { return sorted[left] < sorted[right] })
	index := int(math.Ceil(fraction*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

type benchmarkServer struct {
	address  string
	listener net.Listener
	rpc      *server.Server
	done     chan error
	stopOnce sync.Once
}

func startBenchmarkServer(calls *atomic.Uint64) (*benchmarkServer, error) {
	return startBenchmarkServerWithTLS(calls, nil)
}

func startBenchmarkServerWithTLS(
	calls *atomic.Uint64,
	tlsConfig *tls.Config,
) (*benchmarkServer, error) {
	return startBenchmarkServerWithProtocol(calls, tlsConfig, false)
}

func startBenchmarkHTTP2Server(
	calls *atomic.Uint64,
	tlsConfig *tls.Config,
) (*benchmarkServer, error) {
	return startBenchmarkServerWithProtocol(calls, tlsConfig, true)
}

func startBenchmarkServerWithProtocol(
	calls *atomic.Uint64,
	tlsConfig *tls.Config,
	http2Protocol bool,
) (*benchmarkServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	options := []server.Option{
		server.WithMaxConnections(1024),
		server.WithMaxConcurrentRequests(4096),
	}
	if tlsConfig != nil {
		options = append(options, server.WithTLSConfig(tlsConfig))
	}
	rpcServer, err := server.NewWithOptions(options...)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	if err := echov1.RegisterEchoServer(rpcServer, benchmarkEcho{calls: calls}); err != nil {
		_ = listener.Close()
		return nil, err
	}
	target := &benchmarkServer{
		address:  listener.Addr().String(),
		listener: listener,
		rpc:      rpcServer,
		done:     make(chan error, 1),
	}
	go func() {
		if http2Protocol {
			target.done <- rpcServer.ServeHTTP2(listener)
			return
		}
		target.done <- rpcServer.Serve(listener)
	}()
	return target, nil
}

func (target *benchmarkServer) stop() error {
	var stopErr error
	target.stopOnce.Do(func() {
		listenerErr := target.listener.Close()
		if errors.Is(listenerErr, net.ErrClosed) {
			listenerErr = nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stopErr = errors.Join(listenerErr, target.rpc.GracefulStop(ctx))
		select {
		case serveErr := <-target.done:
			if serveErr != nil &&
				!errors.Is(serveErr, server.ErrServerClosed) &&
				!errors.Is(serveErr, net.ErrClosed) {
				stopErr = errors.Join(stopErr, serveErr)
			}
		case <-time.After(5 * time.Second):
			stopErr = errors.Join(stopErr, errors.New("benchmark server stop timeout"))
		}
	})
	return stopErr
}

type benchmarkEcho struct {
	calls *atomic.Uint64
}

func (service benchmarkEcho) Echo(
	_ context.Context,
	request *echov1.EchoRequest,
) (*echov1.EchoResponse, error) {
	service.calls.Add(1)
	return &echov1.EchoResponse{Message: request.GetMessage()}, nil
}

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/client"
	"github.com/zjsycwmrbig/mini-rpc/internal/discovery"
	"github.com/zjsycwmrbig/mini-rpc/internal/governance"
	"github.com/zjsycwmrbig/mini-rpc/internal/protocol"
	"github.com/zjsycwmrbig/mini-rpc/internal/registry"
	"github.com/zjsycwmrbig/mini-rpc/internal/resolver"
	"github.com/zjsycwmrbig/mini-rpc/internal/server"
	"github.com/zjsycwmrbig/mini-rpc/internal/testkit"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
)

func instanceFailoverScenario(ctx context.Context) Result {
	result := Result{
		Name:        "instance-failover",
		Expectation: "first selected instance fails and the idempotent request succeeds on another",
	}
	memory := testkit.NewMemoryRegistry()
	defer func() { _ = memory.Close() }()
	serverA, err := startScenarioServer(nil)
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = serverA.stop() }()
	serverB, err := startScenarioServer(nil)
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = serverB.stop() }()
	if err := registerScenarioInstance(ctx, memory, "a", serverA.address); err != nil {
		return failedResult(result, err)
	}
	if err := registerScenarioInstance(ctx, memory, "b", serverB.address); err != nil {
		return failedResult(result, err)
	}
	serviceResolver := newScenarioResolver(memory)
	defer func() { _ = serviceResolver.Close() }()
	discoveryClient, err := newScenarioDiscovery(ctx, serviceResolver)
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = discoveryClient.Close() }()

	started := time.Now()
	if err := serverA.stop(); err != nil {
		return failedResult(result, fmt.Errorf("stop instance a: %w", err))
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	response, err := echov1.NewEchoClient(discoveryClient).Echo(
		rpc.WithIdempotent(callCtx),
		&echov1.EchoRequest{Message: "failover"},
	)
	if err != nil {
		return failedResult(result, err)
	}
	stats := discoveryClient.Stats()
	result.Attempts = int(stats.RetryAttempts)
	result.Failures = max(0, result.Attempts-1)
	result.Recovery = time.Since(started)
	result.Passed = response.GetMessage() == "failover" &&
		stats.LastInstanceID == "b" &&
		stats.RetryAttempts == 2
	result.Detail = fmt.Sprintf(
		"attempts=%d selected=%s response=%q",
		stats.RetryAttempts,
		stats.LastInstanceID,
		response.GetMessage(),
	)
	return result
}

func allInstancesDownScenario(ctx context.Context) Result {
	result := Result{
		Name:        "all-instances-down",
		Expectation: "bounded attempts end with an identifiable UNAVAILABLE error",
	}
	memory := testkit.NewMemoryRegistry()
	defer func() { _ = memory.Close() }()
	serverA, err := startScenarioServer(nil)
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = serverA.stop() }()
	serverB, err := startScenarioServer(nil)
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = serverB.stop() }()
	if err := registerScenarioInstance(ctx, memory, "a", serverA.address); err != nil {
		return failedResult(result, err)
	}
	if err := registerScenarioInstance(ctx, memory, "b", serverB.address); err != nil {
		return failedResult(result, err)
	}
	serviceResolver := newScenarioResolver(memory)
	defer func() { _ = serviceResolver.Close() }()
	discoveryClient, err := newScenarioDiscovery(ctx, serviceResolver)
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = discoveryClient.Close() }()
	_ = serverA.stop()
	_ = serverB.stop()

	callCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	started := time.Now()
	_, err = echov1.NewEchoClient(discoveryClient).Echo(
		rpc.WithIdempotent(callCtx),
		&echov1.EchoRequest{Message: "all-down"},
	)
	stats := discoveryClient.Stats()
	result.Attempts = int(stats.RetryAttempts)
	result.Failures = result.Attempts
	result.Recovery = time.Since(started)
	result.Passed = errorCode(err) == rpc.CodeUnavailable &&
		result.Attempts == 2 &&
		result.Recovery < time.Second
	result.Detail = fmt.Sprintf("attempts=%d error=%v elapsed=%s", result.Attempts, err, result.Recovery.Round(time.Millisecond))
	return result
}

func instanceRecoveryScenario(ctx context.Context) Result {
	result := Result{
		Name:        "instance-recovery",
		Expectation: "a re-registered instance receives calls after joining the registry snapshot",
	}
	memory := testkit.NewMemoryRegistry()
	defer func() { _ = memory.Close() }()
	firstServer, err := startScenarioServer(nil)
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = firstServer.stop() }()
	if err := registerScenarioInstance(ctx, memory, "a", firstServer.address); err != nil {
		return failedResult(result, err)
	}
	serviceResolver := newScenarioResolver(memory)
	defer func() { _ = serviceResolver.Close() }()
	discoveryClient, err := newScenarioDiscovery(ctx, serviceResolver)
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = discoveryClient.Close() }()
	_ = firstServer.stop()
	if err := memory.Deregister(ctx, registry.Instance{
		ID:      "a",
		Service: echov1.EchoServiceName,
		Address: firstServer.address,
	}); err != nil {
		return failedResult(result, err)
	}
	if _, err := waitForResolverSnapshot(ctx, serviceResolver, 0); err != nil {
		return failedResult(result, err)
	}

	recoveredServer, err := startScenarioServer(nil)
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = recoveredServer.stop() }()
	recoveryStarted := time.Now()
	if err := registerScenarioInstance(ctx, memory, "a", recoveredServer.address); err != nil {
		return failedResult(result, err)
	}
	if _, err := waitForResolverSnapshot(ctx, serviceResolver, 1); err != nil {
		return failedResult(result, err)
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	response, err := echov1.NewEchoClient(discoveryClient).Echo(
		rpc.WithIdempotent(callCtx),
		&echov1.EchoRequest{Message: "instance-recovered"},
	)
	stats := discoveryClient.Stats()
	result.Attempts = int(stats.RetryAttempts)
	result.Failures = max(0, result.Attempts-1)
	result.Recovery = time.Since(recoveryStarted)
	result.Passed = err == nil && response.GetMessage() == "instance-recovered"
	result.Detail = fmt.Sprintf("instances=%d response=%q error=%v", stats.Instances, response.GetMessage(), err)
	return result
}

func responseErrorScenario(ctx context.Context) Result {
	result := Result{
		Name:        "response-error",
		Expectation: "one injected UNAVAILABLE response is retried and the next response succeeds",
	}
	target, err := startScenarioServer(nil)
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = target.stop() }()
	proxy, err := testkit.NewFaultProxy(target.address, func(
		direction testkit.Direction,
		sequence int,
		frame protocol.Frame,
	) testkit.Fault {
		if direction != testkit.ServerToClient || sequence != 1 {
			return testkit.Fault{}
		}
		replacement := frame
		replacement.Flags |= protocol.FlagError
		replacement.Header = map[string]string{
			protocol.MetadataKeyErrorCode: rpc.FormatErrorCode(rpc.CodeUnavailable),
		}
		replacement.Body = []byte("injected response error")
		return testkit.Fault{Replace: &replacement}
	})
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = proxy.Close() }()
	clientConn, err := newScenarioDirectClient(ctx, proxy.Addr())
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = clientConn.Close() }()

	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	started := time.Now()
	response, err := echov1.NewEchoClient(clientConn).Echo(
		rpc.WithIdempotent(callCtx),
		&echov1.EchoRequest{Message: "after-response-error"},
	)
	proxyStats := proxy.Stats()
	clientStats := clientConn.Stats()
	result.Attempts = int(clientStats.RetryAttempts)
	result.Failures = int(proxyStats.ReplacedFrames)
	result.Recovery = time.Since(started)
	result.Passed = err == nil &&
		response.GetMessage() == "after-response-error" &&
		result.Attempts == 2 &&
		proxyStats.ReplacedFrames == 1
	result.Detail = fmt.Sprintf("attempts=%d replaced=%d response=%q error=%v", result.Attempts, proxyStats.ReplacedFrames, response.GetMessage(), err)
	return result
}

func droppedResponseScenario(ctx context.Context) Result {
	result := Result{
		Name:        "dropped-response",
		Expectation: "a lost response surfaces as a deadline and the next call recovers",
	}
	target, err := startScenarioServer(nil)
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = target.stop() }()
	var dropUsed atomic.Bool
	proxy, err := testkit.NewFaultProxy(target.address, func(
		direction testkit.Direction,
		_ int,
		_ protocol.Frame,
	) testkit.Fault {
		if direction == testkit.ServerToClient && dropUsed.CompareAndSwap(false, true) {
			return testkit.Fault{Drop: true}
		}
		return testkit.Fault{}
	})
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = proxy.Close() }()
	clientConn, err := newScenarioDirectClient(ctx, proxy.Addr())
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = clientConn.Close() }()

	started := time.Now()
	lostCtx, lostCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	_, lostErr := echov1.NewEchoClient(clientConn).Echo(
		lostCtx,
		&echov1.EchoRequest{Message: "lost-response"},
	)
	lostCancel()
	recoveryCtx, recoveryCancel := context.WithTimeout(ctx, time.Second)
	defer recoveryCancel()
	response, recoveryErr := echov1.NewEchoClient(clientConn).Echo(
		recoveryCtx,
		&echov1.EchoRequest{Message: "after-dropped-response"},
	)
	proxyStats := proxy.Stats()
	clientStats := clientConn.Stats()
	result.Attempts = int(clientStats.RetryAttempts)
	result.Failures = 1
	result.Recovery = time.Since(started)
	result.Passed = errorCode(lostErr) == rpc.CodeDeadlineExceeded &&
		recoveryErr == nil &&
		response.GetMessage() == "after-dropped-response" &&
		proxyStats.DroppedFrames == 1 &&
		result.Attempts == 2
	result.Detail = fmt.Sprintf(
		"dropped=%d attempts=%d lost=%v recovery=%q",
		proxyStats.DroppedFrames,
		result.Attempts,
		lostErr,
		response.GetMessage(),
	)
	return result
}

func delayedResponseScenario(ctx context.Context) Result {
	result := Result{
		Name:        "delayed-response",
		Expectation: "a response beyond the deadline fails once without corrupting the next call",
	}
	target, err := startScenarioServer(nil)
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = target.stop() }()
	var delayUsed atomic.Bool
	proxy, err := testkit.NewFaultProxy(target.address, func(
		direction testkit.Direction,
		_ int,
		_ protocol.Frame,
	) testkit.Fault {
		if direction == testkit.ServerToClient && delayUsed.CompareAndSwap(false, true) {
			return testkit.Fault{Delay: 100 * time.Millisecond}
		}
		return testkit.Fault{}
	})
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = proxy.Close() }()
	clientConn, err := newScenarioDirectClient(ctx, proxy.Addr())
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = clientConn.Close() }()

	started := time.Now()
	fastCtx, fastCancel := context.WithTimeout(ctx, 30*time.Millisecond)
	_, firstErr := echov1.NewEchoClient(clientConn).Echo(
		fastCtx,
		&echov1.EchoRequest{Message: "too-slow"},
	)
	fastCancel()
	time.Sleep(120 * time.Millisecond)
	recoveryCtx, recoveryCancel := context.WithTimeout(ctx, time.Second)
	defer recoveryCancel()
	response, recoveryErr := echov1.NewEchoClient(clientConn).Echo(
		recoveryCtx,
		&echov1.EchoRequest{Message: "after-delay"},
	)
	stats := clientConn.Stats()
	result.Attempts = int(stats.RetryAttempts)
	result.Failures = 1
	result.Recovery = time.Since(started)
	result.Passed = errorCode(firstErr) == rpc.CodeDeadlineExceeded &&
		recoveryErr == nil &&
		response.GetMessage() == "after-delay"
	result.Detail = fmt.Sprintf("first=%v recovery=%q attempts=%d", firstErr, response.GetMessage(), result.Attempts)
	return result
}

func forcedDisconnectScenario(ctx context.Context) Result {
	result := Result{
		Name:        "forced-disconnect",
		Expectation: "one forced TCP close is retried on a new connection",
	}
	target, err := startScenarioServer(nil)
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = target.stop() }()
	var requests atomic.Int32
	proxy, err := testkit.NewFaultProxy(target.address, func(
		direction testkit.Direction,
		_ int,
		_ protocol.Frame,
	) testkit.Fault {
		if direction == testkit.ClientToServer && requests.Add(1) == 1 {
			return testkit.Fault{Close: true}
		}
		return testkit.Fault{}
	})
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = proxy.Close() }()
	clientConn, err := newScenarioDirectClient(ctx, proxy.Addr())
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = clientConn.Close() }()

	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	started := time.Now()
	response, err := echov1.NewEchoClient(clientConn).Echo(
		rpc.WithIdempotent(callCtx),
		&echov1.EchoRequest{Message: "after-disconnect"},
	)
	proxyStats := proxy.Stats()
	clientStats := clientConn.Stats()
	result.Attempts = int(clientStats.RetryAttempts)
	result.Failures = max(0, result.Attempts-1)
	result.Recovery = time.Since(started)
	result.Passed = err == nil &&
		response.GetMessage() == "after-disconnect" &&
		result.Attempts == 2 &&
		proxyStats.AcceptedConnections >= 2
	result.Detail = fmt.Sprintf("attempts=%d connections=%d response=%q error=%v", result.Attempts, proxyStats.AcceptedConnections, response.GetMessage(), err)
	return result
}

func registryOutageScenario(ctx context.Context) Result {
	result := Result{
		Name:        "registry-outage",
		Expectation: "resolver keeps the last snapshot and reconnects after a temporary registry outage",
	}
	memory := testkit.NewMemoryRegistry()
	defer func() { _ = memory.Close() }()
	if err := registerScenarioInstance(ctx, memory, "a", "127.0.0.1:1"); err != nil {
		return failedResult(result, err)
	}
	serviceResolver := newScenarioResolver(memory)
	defer func() { _ = serviceResolver.Close() }()
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	updates, err := serviceResolver.Watch(watchCtx, echov1.EchoServiceName)
	if err != nil {
		return failedResult(result, err)
	}
	if _, err := receiveScenarioSnapshot(ctx, updates, func(snapshot resolver.Snapshot) bool {
		return snapshot.Err == nil && len(snapshot.Instances) == 1
	}); err != nil {
		return failedResult(result, err)
	}

	started := time.Now()
	memory.SetUnavailable(errors.New("injected etcd outage"))
	memory.DisconnectWatches(echov1.EchoServiceName, errors.New("injected watch outage"))
	if _, err := receiveScenarioSnapshot(ctx, updates, func(snapshot resolver.Snapshot) bool {
		return snapshot.Err != nil && len(snapshot.Instances) == 1
	}); err != nil {
		return failedResult(result, err)
	}
	memory.SetUnavailable(nil)
	if err := registerScenarioInstance(ctx, memory, "b", "127.0.0.1:2"); err != nil {
		return failedResult(result, err)
	}
	recovered, err := receiveScenarioSnapshot(ctx, updates, func(snapshot resolver.Snapshot) bool {
		return snapshot.Err == nil && len(snapshot.Instances) == 2
	})
	if err != nil {
		return failedResult(result, err)
	}
	listCalls, watchCalls := memory.Calls()
	result.Recovery = time.Since(started)
	result.Passed = result.Recovery < 2*time.Second && watchCalls >= 2 && listCalls >= 2
	result.Detail = fmt.Sprintf(
		"instances=%d list_calls=%d watch_calls=%d snapshot_update=%s",
		len(recovered.Instances),
		listCalls,
		watchCalls,
		recovered.UpdatedAt.Format(time.RFC3339Nano),
	)
	return result
}

func resourceExhaustionScenario(ctx context.Context) Result {
	result := Result{
		Name:        "resource-exhaustion",
		Expectation: "concurrency rejection retries a bounded number of times and recovers without a loop",
	}
	startedHandler := make(chan struct{})
	releaseHandler := make(chan struct{})
	var startOnce sync.Once
	var handlerCalls atomic.Int32
	blocking := func(handlerCtx context.Context, request *echov1.EchoRequest) (*echov1.EchoResponse, error) {
		handlerCalls.Add(1)
		startOnce.Do(func() { close(startedHandler) })
		select {
		case <-releaseHandler:
			return &echov1.EchoResponse{Message: request.GetMessage()}, nil
		case <-handlerCtx.Done():
			return nil, handlerCtx.Err()
		}
	}
	target, err := startScenarioServer(blocking, server.WithMaxConcurrentRequests(1))
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = target.stop() }()
	clientConn, err := newScenarioDirectClient(ctx, target.address)
	if err != nil {
		return failedResult(result, err)
	}
	defer func() { _ = clientConn.Close() }()

	firstCtx, firstCancel := context.WithTimeout(ctx, 3*time.Second)
	defer firstCancel()
	firstDone := make(chan error, 1)
	go func() {
		_, callErr := echov1.NewEchoClient(clientConn).Echo(
			rpc.WithIdempotent(firstCtx),
			&echov1.EchoRequest{Message: "occupies-handler"},
		)
		firstDone <- callErr
	}()
	select {
	case <-startedHandler:
	case <-time.After(time.Second):
		return failedResult(result, errors.New("timed out waiting for blocking handler"))
	}

	rejectedCtx, rejectedCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	rejectedStarted := time.Now()
	_, rejectedErr := echov1.NewEchoClient(clientConn).Echo(
		rpc.WithIdempotent(rejectedCtx),
		&echov1.EchoRequest{Message: "rejected"},
	)
	rejectedCancel()
	statsAtRejection := clientConn.Stats()
	handlerCallsAtRejection := handlerCalls.Load()
	close(releaseHandler)
	select {
	case err := <-firstDone:
		if err != nil {
			return failedResult(result, fmt.Errorf("blocking call: %w", err))
		}
	case <-time.After(time.Second):
		return failedResult(result, errors.New("blocking call did not recover"))
	}

	recoveryCtx, recoveryCancel := context.WithTimeout(ctx, 2*time.Second)
	defer recoveryCancel()
	response, recoveryErr := echov1.NewEchoClient(clientConn).Echo(
		rpc.WithIdempotent(recoveryCtx),
		&echov1.EchoRequest{Message: "after-exhaustion"},
	)
	result.Attempts = int(statsAtRejection.RetryAttempts)
	result.Failures = result.Attempts
	result.Recovery = time.Since(rejectedStarted)
	result.Passed = errorCode(rejectedErr) == rpc.CodeResourceExhausted &&
		result.Attempts == 2 &&
		handlerCallsAtRejection == 1 &&
		recoveryErr == nil &&
		response.GetMessage() == "after-exhaustion"
	result.Detail = fmt.Sprintf(
		"bounded_attempts=%d handler_calls=%d rejected=%v response=%q",
		result.Attempts,
		handlerCalls.Load(),
		rejectedErr,
		response.GetMessage(),
	)
	return result
}

type scenarioServer struct {
	address  string
	listener net.Listener
	rpc      *server.Server
	done     chan error
	stopOnce sync.Once
}

func startScenarioServer(
	handler func(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error),
	options ...server.Option,
) (*scenarioServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	rpcServer, err := server.NewWithOptions(options...)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	if err := echov1.RegisterEchoServer(rpcServer, scenarioEcho{handler: handler}); err != nil {
		_ = listener.Close()
		return nil, err
	}
	instance := &scenarioServer{
		address:  listener.Addr().String(),
		listener: listener,
		rpc:      rpcServer,
		done:     make(chan error, 1),
	}
	go func() { instance.done <- rpcServer.Serve(listener) }()
	return instance, nil
}

func (instance *scenarioServer) stop() error {
	var stopErr error
	instance.stopOnce.Do(func() {
		listenerErr := instance.listener.Close()
		if errors.Is(listenerErr, net.ErrClosed) {
			listenerErr = nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		stopErr = errors.Join(listenerErr, instance.rpc.GracefulStop(ctx))
		select {
		case serveErr := <-instance.done:
			if serveErr != nil &&
				!errors.Is(serveErr, server.ErrServerClosed) &&
				!errors.Is(serveErr, net.ErrClosed) {
				stopErr = errors.Join(stopErr, serveErr)
			}
		case <-time.After(2 * time.Second):
			stopErr = errors.Join(stopErr, errors.New("server stop timeout"))
		}
	})
	return stopErr
}

type scenarioEcho struct {
	handler func(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error)
}

func (service scenarioEcho) Echo(
	ctx context.Context,
	request *echov1.EchoRequest,
) (*echov1.EchoResponse, error) {
	if service.handler != nil {
		return service.handler(ctx, request)
	}
	return &echov1.EchoResponse{Message: request.GetMessage()}, nil
}

func newScenarioResolver(memory registry.Registry) *resolver.Resolver {
	return resolver.New(memory, resolver.Options{
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
	})
}

func newScenarioDiscovery(ctx context.Context, serviceResolver *resolver.Resolver) (*discovery.Client, error) {
	return discovery.New(ctx, discovery.Config{
		Service:  echov1.EchoServiceName,
		Resolver: serviceResolver,
		Retry:    scenarioRetryPolicy(),
	})
}

func newScenarioDirectClient(ctx context.Context, address string) (*client.Client, error) {
	return client.Dial(
		ctx,
		address,
		client.WithRetryPolicy(scenarioRetryPolicy()),
	)
}

func scenarioRetryPolicy() governance.RetryPolicy {
	return governance.RetryPolicy{
		MaxAttempts:    2,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		MaxElapsed:     time.Second,
		Jitter:         func(time.Duration) time.Duration { return 0 },
	}
}

func registerScenarioInstance(
	ctx context.Context,
	memory *testkit.MemoryRegistry,
	id string,
	address string,
) error {
	_, err := memory.Register(ctx, registry.Instance{
		ID:      id,
		Service: echov1.EchoServiceName,
		Address: address,
		Weight:  1,
	}, time.Minute)
	return err
}

func waitForResolverSnapshot(
	ctx context.Context,
	serviceResolver *resolver.Resolver,
	want int,
) (resolver.Snapshot, error) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snapshot, ok := serviceResolver.Snapshot(echov1.EchoServiceName); ok &&
			snapshot.Err == nil && len(snapshot.Instances) == want {
			return snapshot, nil
		}
		select {
		case <-ctx.Done():
			return resolver.Snapshot{}, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	return resolver.Snapshot{}, fmt.Errorf("timed out waiting for %d instances", want)
}

func receiveScenarioSnapshot(
	ctx context.Context,
	updates <-chan resolver.Snapshot,
	ready func(resolver.Snapshot) bool,
) (resolver.Snapshot, error) {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case snapshot, ok := <-updates:
			if !ok {
				return resolver.Snapshot{}, errors.New("resolver updates closed")
			}
			if ready(snapshot) {
				return snapshot, nil
			}
		case <-timer.C:
			return resolver.Snapshot{}, errors.New("timed out waiting for resolver snapshot")
		case <-ctx.Done():
			return resolver.Snapshot{}, ctx.Err()
		}
	}
}

func failedResult(result Result, err error) Result {
	result.Passed = false
	result.Detail = err.Error()
	return result
}

func errorCode(err error) rpc.ErrorCode {
	var rpcError *rpc.Error
	if errors.As(err, &rpcError) {
		return rpcError.Code
	}
	return rpc.CodeInternal
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}

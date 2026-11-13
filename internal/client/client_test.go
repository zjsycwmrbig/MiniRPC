package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/client"
	"github.com/zjsycwmrbig/mini-rpc/internal/governance"
	"github.com/zjsycwmrbig/mini-rpc/internal/observability"
	"github.com/zjsycwmrbig/mini-rpc/internal/server"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
)

func TestGeneratedClientCallsEchoServer(t *testing.T) {
	t.Parallel()

	address := startTestServer(t, echoServer{})
	clientConn := dialTestClient(t, address)
	echoClient := echov1.NewEchoClient(clientConn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := echoClient.Echo(ctx, &echov1.EchoRequest{Message: "hello tcp"})
	if err != nil {
		t.Fatalf("Echo() error = %v", err)
	}
	if response.GetMessage() != "hello tcp" {
		t.Fatalf("Echo() message = %q, want %q", response.GetMessage(), "hello tcp")
	}
}

func TestConcurrentCallsShareOneConnection(t *testing.T) {
	t.Parallel()

	address := startTestServer(t, echoServer{})
	clientConn := dialTestClient(t, address)
	echoClient := echov1.NewEchoClient(clientConn)

	const calls = 100
	var waitGroup sync.WaitGroup
	callErrors := make([]error, calls)
	waitGroup.Add(calls)
	for index := range calls {
		index := index
		go func() {
			defer waitGroup.Done()
			message := fmt.Sprintf("message-%d", index)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			response, err := echoClient.Echo(ctx, &echov1.EchoRequest{Message: message})
			if err != nil {
				callErrors[index] = err
				return
			}
			if response.GetMessage() != message {
				callErrors[index] = fmt.Errorf("message = %q, want %q", response.GetMessage(), message)
			}
		}()
	}
	waitGroup.Wait()

	for index, err := range callErrors {
		if err != nil {
			t.Fatalf("call %d error = %v", index, err)
		}
	}
}

func TestPoolReusesConnectionsForThousandCalls(t *testing.T) {
	t.Parallel()

	address, testServer := startTestServerWithHandle(t, echoServer{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clientConn, err := client.Dial(
		ctx,
		address,
		client.WithMaxConnections(4),
		client.WithMaxConcurrentCalls(1024),
	)
	if err != nil {
		t.Fatalf("client.Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	const calls = 1000
	var waitGroup sync.WaitGroup
	callErrors := make([]error, calls)
	waitGroup.Add(calls)
	for index := range calls {
		index := index
		go func() {
			defer waitGroup.Done()
			callCtx, callCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer callCancel()
			response, callErr := echov1.NewEchoClient(clientConn).Echo(
				callCtx,
				&echov1.EchoRequest{Message: fmt.Sprintf("pooled-%d", index)},
			)
			if callErr == nil && response.GetMessage() != fmt.Sprintf("pooled-%d", index) {
				callErr = fmt.Errorf("message = %q", response.GetMessage())
			}
			callErrors[index] = callErr
		}()
	}
	waitGroup.Wait()
	for index, callErr := range callErrors {
		if callErr != nil {
			t.Fatalf("call %d error = %v", index, callErr)
		}
	}

	clientStats := clientConn.Stats()
	serverStats := testServer.Stats()
	if clientStats.ConnectionsDialed != 1 {
		t.Fatalf("client dialed %d connections, want 1", clientStats.ConnectionsDialed)
	}
	if serverStats.AcceptedConnections != 1 {
		t.Fatalf("server accepted %d connections, want 1", serverStats.AcceptedConnections)
	}
}

func TestPoolCapacityFailureIsPredictable(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	address := startTestServer(t, blockingEchoServer{
		handler: func(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			close(started)
			<-release
			return &echov1.EchoResponse{}, nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	clientConn, err := client.Dial(
		ctx,
		address,
		client.WithMaxConnections(1),
		client.WithMaxConcurrentCalls(1),
	)
	if err != nil {
		t.Fatalf("client.Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- clientConn.Invoke(
			context.Background(),
			echov1.EchoServiceName,
			"Echo",
			&echov1.EchoRequest{},
			&echov1.EchoResponse{},
		)
	}()
	waitForClosed(t, started, "first handler")

	secondCtx, secondCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer secondCancel()
	err = clientConn.Invoke(
		secondCtx,
		echov1.EchoServiceName,
		"Echo",
		&echov1.EchoRequest{},
		&echov1.EchoResponse{},
	)
	if !isRPCCode(err, rpc.CodeResourceExhausted) {
		t.Fatalf("capacity error = %v, want CodeResourceExhausted", err)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Invoke() error = %v", err)
	}
}

func TestPoolWaitCancellationReturnsCanceled(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	address := startTestServer(t, blockingEchoServer{
		handler: func(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			close(started)
			<-release
			return &echov1.EchoResponse{}, nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	clientConn, err := client.Dial(
		ctx,
		address,
		client.WithMaxConnections(1),
		client.WithMaxConcurrentCalls(1),
	)
	if err != nil {
		t.Fatalf("client.Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- clientConn.Invoke(
			context.Background(),
			echov1.EchoServiceName,
			"Echo",
			&echov1.EchoRequest{},
			&echov1.EchoResponse{},
		)
	}()
	waitForClosed(t, started, "first handler")

	waitCtx, waitCancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- clientConn.Invoke(
			waitCtx,
			echov1.EchoServiceName,
			"Echo",
			&echov1.EchoRequest{},
			&echov1.EchoResponse{},
		)
	}()
	waitForStats(t, clientConn, func(stats client.Stats) bool {
		return stats.PoolWaits == 1
	})
	waitCancel()
	if err := <-secondDone; !isRPCCode(err, rpc.CodeCanceled) {
		t.Fatalf("pool wait error = %v, want CodeCanceled", err)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Invoke() error = %v", err)
	}
}

func TestPoolReclaimsIdleConnections(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	address := startTestServer(t, blockingEchoServer{
		handler: func(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			started <- struct{}{}
			<-release
			return &echov1.EchoResponse{}, nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	clientConn, err := client.Dial(
		ctx,
		address,
		client.WithMaxConnections(2),
		client.WithMaxIdleConnections(1),
		client.WithMaxConcurrentCalls(1),
	)
	if err != nil {
		t.Fatalf("client.Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	callErrors := make(chan error, 2)
	for range 2 {
		go func() {
			callErrors <- clientConn.Invoke(
				context.Background(),
				echov1.EchoServiceName,
				"Echo",
				&echov1.EchoRequest{},
				&echov1.EchoResponse{},
			)
		}()
	}
	for range 2 {
		waitForClosed(t, started, "concurrent handler")
	}
	waitForStats(t, clientConn, func(stats client.Stats) bool {
		return stats.Connections == 2
	})
	close(release)
	for range 2 {
		if err := <-callErrors; err != nil {
			t.Fatalf("Invoke() error = %v", err)
		}
	}
	waitForStats(t, clientConn, func(stats client.Stats) bool {
		return stats.Connections == 1 && stats.LastCloseReason == "idle_limit"
	})
}

func TestPoolRecoversAfterConnectionLoss(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	address, firstServer := startTestServerWithHandle(t, blockingEchoServer{
		handler: func(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			<-block
			return &echov1.EchoResponse{Message: "first"}, nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	clientConn, err := client.Dial(
		ctx,
		address,
		client.WithMaxConnections(1),
		client.WithMaxConcurrentCalls(1),
	)
	if err != nil {
		t.Fatalf("client.Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- clientConn.Invoke(
			context.Background(),
			echov1.EchoServiceName,
			"Echo",
			&echov1.EchoRequest{},
			&echov1.EchoResponse{},
		)
	}()
	waitForStats(t, clientConn, func(stats client.Stats) bool {
		return stats.ActiveRequests == 1
	})
	if err := firstServer.Close(); err != nil {
		t.Fatalf("first Server.Close() error = %v", err)
	}
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first call did not return after connection loss")
	}
	close(block)

	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("net.Listen(%q) error = %v", address, err)
	}
	secondServer := server.New()
	if err := echov1.RegisterEchoServer(secondServer, echoServer{}); err != nil {
		t.Fatalf("RegisterEchoServer() error = %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- secondServer.Serve(listener) }()
	t.Cleanup(func() {
		_ = secondServer.Close()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Error("second Serve() did not stop")
		}
	})

	response, err := echov1.NewEchoClient(clientConn).Echo(
		context.Background(),
		&echov1.EchoRequest{Message: "recovered"},
	)
	if err != nil {
		t.Fatalf("recovery Echo() error = %v", err)
	}
	if response.GetMessage() != "recovered" {
		t.Fatalf("recovery response = %q, want %q", response.GetMessage(), "recovered")
	}
	if clientConn.Stats().ConnectionsDialed < 2 {
		t.Fatal("client did not redial after connection loss")
	}
}

func TestGracefulStopWaitsForAcceptedRequest(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	address, testServer := startTestServerWithHandle(t, blockingEchoServer{
		handler: func(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			close(started)
			<-release
			return &echov1.EchoResponse{Message: "completed"}, nil
		},
	})
	clientConn := dialTestClient(t, address)

	callDone := make(chan error, 1)
	go func() {
		_, err := echov1.NewEchoClient(clientConn).Echo(
			context.Background(),
			&echov1.EchoRequest{Message: "drain"},
		)
		callDone <- err
	}()
	waitForClosed(t, started, "handler")

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	stopDone := make(chan error, 1)
	go func() { stopDone <- testServer.GracefulStop(stopCtx) }()
	waitForStats(t, clientConn, func(stats client.Stats) bool {
		return stats.ActiveRequests == 1
	})
	close(release)

	if err := <-callDone; err != nil {
		t.Fatalf("Echo() error = %v", err)
	}
	if err := <-stopDone; err != nil {
		t.Fatalf("GracefulStop() error = %v", err)
	}
	stats := testServer.Stats()
	if stats.CloseReason != "graceful" {
		t.Fatalf("close reason = %q, want graceful", stats.CloseReason)
	}
	if stats.CompletedRequests != 1 {
		t.Fatalf("completed requests = %d, want 1", stats.CompletedRequests)
	}
}

func TestGracefulStopHonorsDeadline(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	address, testServer := startTestServerWithHandle(t, blockingEchoServer{
		handler: func(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			close(started)
			<-release
			return &echov1.EchoResponse{}, nil
		},
	})
	clientConn := dialTestClient(t, address)

	callDone := make(chan error, 1)
	go func() {
		_, err := echov1.NewEchoClient(clientConn).Echo(
			context.Background(),
			&echov1.EchoRequest{Message: "timeout"},
		)
		callDone <- err
	}()
	waitForClosed(t, started, "handler")

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stopCancel()
	if err := testServer.GracefulStop(stopCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GracefulStop() error = %v, want DeadlineExceeded", err)
	}
	stats := testServer.Stats()
	if stats.CloseReason != "graceful_timeout" {
		t.Fatalf("close reason = %q, want graceful_timeout", stats.CloseReason)
	}
	close(release)
	select {
	case <-callDone:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked call did not return after graceful timeout")
	}
}

func TestCanceledCallCleansPendingAndAllowsRecovery(t *testing.T) {
	t.Parallel()

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var blockFirst atomic.Bool
	blockFirst.Store(true)
	address := startTestServer(t, blockingEchoServer{
		handler: func(_ context.Context, request *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			if blockFirst.CompareAndSwap(true, false) {
				close(firstStarted)
				<-releaseFirst
			}
			return &echov1.EchoResponse{Message: request.GetMessage()}, nil
		},
	})
	clientConn := dialTestClient(t, address)
	echoClient := echov1.NewEchoClient(clientConn)

	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := echoClient.Echo(ctx, &echov1.EchoRequest{Message: "first"})
		firstDone <- err
	}()

	waitForClosed(t, firstStarted, "first handler")
	cancel()
	if err := <-firstDone; !isRPCCode(err, rpc.CodeCanceled) {
		t.Fatalf("canceled Echo() error = %v, want CodeCanceled", err)
	}
	waitForPending(t, clientConn, 0)
	close(releaseFirst)

	response, err := echoClient.Echo(
		context.Background(),
		&echov1.EchoRequest{Message: "recovered"},
	)
	if err != nil {
		t.Fatalf("recovery Echo() error = %v", err)
	}
	if response.GetMessage() != "recovered" {
		t.Fatalf("recovery response = %q, want %q", response.GetMessage(), "recovered")
	}
}

func TestCancelFrameCancelsServerContext(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	serverCanceled := make(chan struct{})
	address, testServer := startTestServerWithHandle(t, blockingEchoServer{
		handler: func(ctx context.Context, _ *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			close(started)
			<-ctx.Done()
			close(serverCanceled)
			return nil, ctx.Err()
		},
	})
	clientConn := dialTestClient(t, address)

	ctx, cancel := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		_, err := echov1.NewEchoClient(clientConn).Echo(
			ctx,
			&echov1.EchoRequest{Message: "cancel"},
		)
		callDone <- err
	}()
	waitForClosed(t, started, "handler")
	cancel()

	if err := <-callDone; !isRPCCode(err, rpc.CodeCanceled) {
		t.Fatalf("Echo() error = %v, want CodeCanceled", err)
	}
	waitForClosed(t, serverCanceled, "server cancellation")
	waitForPending(t, clientConn, 0)
	waitForServerStats(t, testServer, func(stats server.Stats) bool {
		return stats.CancelFrames == 1
	})
}

func TestDeadlinePropagatesToServerContext(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	serverDeadline := make(chan error, 1)
	address := startTestServer(t, blockingEchoServer{
		handler: func(ctx context.Context, _ *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			close(started)
			<-ctx.Done()
			serverDeadline <- ctx.Err()
			return nil, ctx.Err()
		},
	})
	clientConn := dialTestClient(t, address)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := echov1.NewEchoClient(clientConn).Echo(
		ctx,
		&echov1.EchoRequest{Message: "deadline"},
	)
	if !isRPCCode(err, rpc.CodeDeadlineExceeded) {
		t.Fatalf("Echo() error = %v, want CodeDeadlineExceeded", err)
	}
	waitForClosed(t, started, "handler")
	select {
	case err := <-serverDeadline:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("server context error = %v, want DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server context was not canceled by deadline")
	}
}

func TestFallbackCallTimeout(t *testing.T) {
	t.Parallel()

	address := startTestServer(t, blockingEchoServer{
		handler: func(ctx context.Context, _ *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dialCancel()
	clientConn, err := client.Dial(dialCtx, address, client.WithCallTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatalf("client.Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	started := time.Now()
	_, err = echov1.NewEchoClient(clientConn).Echo(
		context.Background(),
		&echov1.EchoRequest{Message: "fallback"},
	)
	if !isRPCCode(err, rpc.CodeDeadlineExceeded) {
		t.Fatalf("Echo() error = %v, want CodeDeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed < 25*time.Millisecond || elapsed > time.Second {
		t.Fatalf("fallback timeout elapsed = %v", elapsed)
	}
}

func TestManyTimeoutsReleasePendingAndServerSlots(t *testing.T) {
	t.Parallel()

	var timedOutHandlers atomic.Int64
	address, testServer := startTestServerWithHandle(t, blockingEchoServer{
		handler: func(ctx context.Context, request *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			if request.GetMessage() == "recovered" {
				return &echov1.EchoResponse{Message: request.GetMessage()}, nil
			}
			timedOutHandlers.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	clientConn := dialGovernedClient(t, address, testRetryPolicy(1), testBreakerConfig(1000, time.Second))
	echoClient := echov1.NewEchoClient(clientConn)

	const timeoutCalls = 128
	var waitGroup sync.WaitGroup
	callErrors := make([]error, timeoutCalls)
	waitGroup.Add(timeoutCalls)
	for index := range timeoutCalls {
		index := index
		go func() {
			defer waitGroup.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, err := echoClient.Echo(ctx, &echov1.EchoRequest{Message: fmt.Sprintf("timeout-%d", index)})
			callErrors[index] = err
		}()
	}
	waitGroup.Wait()

	for index, err := range callErrors {
		if !isRPCCode(err, rpc.CodeDeadlineExceeded) {
			t.Fatalf("timeout call %d error = %v, want CodeDeadlineExceeded", index, err)
		}
	}
	if elapsed := timedOutHandlers.Load(); elapsed != timeoutCalls {
		t.Fatalf("timed-out handlers = %d, want %d", elapsed, timeoutCalls)
	}
	waitForPending(t, clientConn, 0)
	waitForServerStats(t, testServer, func(stats server.Stats) bool {
		return stats.ActiveRequests == 0 && stats.CompletedRequests >= timeoutCalls
	})

	response, err := echoClient.Echo(
		context.Background(),
		&echov1.EchoRequest{Message: "recovered"},
	)
	if err != nil {
		t.Fatalf("recovery Echo() error = %v", err)
	}
	if response.GetMessage() != "recovered" {
		t.Fatalf("recovery response = %q, want %q", response.GetMessage(), "recovered")
	}
}

func TestIdempotentCallRetriesUnavailable(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	address := startTestServer(t, blockingEchoServer{
		handler: func(_ context.Context, request *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			if calls.Add(1) < 3 {
				return nil, rpc.NewError(rpc.CodeUnavailable, "temporary outage")
			}
			return &echov1.EchoResponse{Message: request.GetMessage()}, nil
		},
	})
	clientConn := dialGovernedClient(t, address, testRetryPolicy(3), testBreakerConfig(100, time.Second))

	ctx := rpc.WithIdempotent(context.Background())
	response, err := echov1.NewEchoClient(clientConn).Echo(
		ctx,
		&echov1.EchoRequest{Message: "retried"},
	)
	if err != nil {
		t.Fatalf("Echo() error = %v", err)
	}
	if response.GetMessage() != "retried" {
		t.Fatalf("Echo() response = %q, want retried", response.GetMessage())
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("handler calls = %d, want 3", got)
	}
	stats := clientConn.Stats()
	if stats.RetryAttempts != 3 || stats.RetriedCalls != 1 {
		t.Fatalf("retry stats = %+v, want 3 attempts and 1 retried call", stats)
	}
	t.Logf("idempotent call attempts: %d", stats.RetryAttempts)
}

func TestNonIdempotentCallDoesNotRetry(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	address := startTestServer(t, blockingEchoServer{
		handler: func(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			calls.Add(1)
			return nil, rpc.NewError(rpc.CodeUnavailable, "temporary outage")
		},
	})
	clientConn := dialGovernedClient(t, address, testRetryPolicy(4), testBreakerConfig(100, time.Second))

	_, err := echov1.NewEchoClient(clientConn).Echo(
		context.Background(),
		&echov1.EchoRequest{Message: "not-idempotent"},
	)
	if !isRPCCode(err, rpc.CodeUnavailable) {
		t.Fatalf("Echo() error = %v, want CodeUnavailable", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
	if stats := clientConn.Stats(); stats.RetryAttempts != 1 || stats.RetriedCalls != 0 {
		t.Fatalf("retry stats = %+v, want one attempt", stats)
	}
}

func TestCircuitBreakerDegradesAndRecovers(t *testing.T) {
	t.Parallel()

	var unavailable atomic.Bool
	unavailable.Store(true)
	var calls atomic.Int64
	address := startTestServer(t, blockingEchoServer{
		handler: func(_ context.Context, request *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			calls.Add(1)
			if unavailable.Load() {
				return nil, rpc.NewError(rpc.CodeUnavailable, "injected outage")
			}
			return &echov1.EchoResponse{Message: request.GetMessage()}, nil
		},
	})
	clientConn := dialGovernedClient(t, address, testRetryPolicy(1), testBreakerConfig(2, 50*time.Millisecond))
	echoClient := echov1.NewEchoClient(clientConn)
	ctx := rpc.WithIdempotent(context.Background())

	for range 2 {
		if _, err := echoClient.Echo(ctx, &echov1.EchoRequest{Message: "fail"}); !isRPCCode(err, rpc.CodeUnavailable) {
			t.Fatalf("outage Echo() error = %v, want CodeUnavailable", err)
		}
	}
	if stats := clientConn.Stats(); stats.CircuitState != string(governance.CircuitOpen) || stats.CircuitOpens != 1 {
		t.Fatalf("breaker stats after outage = %+v", stats)
	}

	if _, err := echoClient.Echo(ctx, &echov1.EchoRequest{Message: "reject"}); !isRPCCode(err, rpc.CodeUnavailable) {
		t.Fatalf("open-breaker Echo() error = %v, want CodeUnavailable", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls while open = %d, want 2", got)
	}
	if stats := clientConn.Stats(); stats.CircuitRejected != 1 {
		t.Fatalf("breaker rejected calls = %d, want 1", stats.CircuitRejected)
	}

	unavailable.Store(false)
	time.Sleep(70 * time.Millisecond)
	response, err := echoClient.Echo(ctx, &echov1.EchoRequest{Message: "recovered"})
	if err != nil {
		t.Fatalf("recovery Echo() error = %v", err)
	}
	if response.GetMessage() != "recovered" {
		t.Fatalf("recovery response = %q, want recovered", response.GetMessage())
	}
	if stats := clientConn.Stats(); stats.CircuitState != string(governance.CircuitClosed) {
		t.Fatalf("breaker state after recovery = %s, want closed", stats.CircuitState)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("handler calls after recovery = %d, want 3", got)
	}
}

func TestRemoteBusinessErrorCode(t *testing.T) {
	t.Parallel()

	address := startTestServer(t, echoServer{
		err: rpc.NewError(rpc.CodeInvalidArgument, "invalid echo message"),
	})
	clientConn := dialTestClient(t, address)
	echoClient := echov1.NewEchoClient(clientConn)

	_, err := echoClient.Echo(context.Background(), &echov1.EchoRequest{Message: "bad"})
	var remoteError *rpc.Error
	if !errors.As(err, &remoteError) {
		t.Fatalf("Echo() error = %v, want *rpc.Error", err)
	}
	if remoteError.Code != rpc.CodeInvalidArgument {
		t.Fatalf("error code = %v, want %v", remoteError.Code, rpc.CodeInvalidArgument)
	}
	if remoteError.Message != "invalid echo message" {
		t.Fatalf("error message = %q", remoteError.Message)
	}
}

func TestServerPanicBecomesInternalError(t *testing.T) {
	t.Parallel()

	address := startTestServer(t, blockingEchoServer{
		handler: func(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			panic("boom")
		},
	})
	clientConn := dialTestClient(t, address)
	echoClient := echov1.NewEchoClient(clientConn)

	_, err := echoClient.Echo(context.Background(), &echov1.EchoRequest{Message: "panic"})
	if !isRPCCode(err, rpc.CodeInternal) {
		t.Fatalf("Echo() error = %v, want CodeInternal", err)
	}
}

func TestServerCloseWakesPendingClientCall(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	address, testServer := startTestServerWithHandle(t, blockingEchoServer{
		handler: func(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			close(started)
			<-release
			return &echov1.EchoResponse{}, nil
		},
	})
	clientConn := dialTestClient(t, address)
	echoClient := echov1.NewEchoClient(clientConn)

	callDone := make(chan error, 1)
	go func() {
		_, err := echoClient.Echo(context.Background(), &echov1.EchoRequest{Message: "wait"})
		callDone <- err
	}()

	waitForClosed(t, started, "handler")
	if err := testServer.Close(); err != nil {
		t.Fatalf("Server.Close() error = %v", err)
	}
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("pending call returned nil error after server close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending call did not return after server close")
	}
	close(release)
}

func TestMissingServiceReturnsNotFound(t *testing.T) {
	t.Parallel()

	address := startTestServer(t, echoServer{})
	clientConn := dialTestClient(t, address)

	err := clientConn.Invoke(
		context.Background(),
		"missing.Service",
		"Method",
		&echov1.EchoRequest{},
		&echov1.EchoResponse{},
	)
	if !isRPCCode(err, rpc.CodeNotFound) {
		t.Fatalf("Invoke() error = %v, want CodeNotFound", err)
	}
}

func TestMissingMethodReturnsNotFound(t *testing.T) {
	t.Parallel()

	address := startTestServer(t, echoServer{})
	clientConn := dialTestClient(t, address)

	err := clientConn.Invoke(
		context.Background(),
		echov1.EchoServiceName,
		"Missing",
		&echov1.EchoRequest{},
		&echov1.EchoResponse{},
	)
	if !isRPCCode(err, rpc.CodeNotFound) {
		t.Fatalf("Invoke() error = %v, want CodeNotFound", err)
	}
}

func TestClientRateLimitRejectsBeforeNetwork(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	address, testServer := startTestServerWithHandle(t, blockingEchoServer{
		handler: func(_ context.Context, request *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			calls.Add(1)
			return &echov1.EchoResponse{Message: request.GetMessage()}, nil
		},
	})
	limits := governance.RateLimitConfig{
		Services: map[string]governance.RateLimitRule{
			echov1.EchoServiceName: {Rate: 0.01, Burst: 2},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	clientConn, err := client.Dial(ctx, address, client.WithRateLimits(limits))
	if err != nil {
		t.Fatalf("client.Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	echoClient := echov1.NewEchoClient(clientConn)

	for index := range 2 {
		if _, err := echoClient.Echo(ctx, &echov1.EchoRequest{Message: fmt.Sprintf("allowed-%d", index)}); err != nil {
			t.Fatalf("allowed Echo() %d error = %v", index, err)
		}
	}
	_, err = echoClient.Echo(ctx, &echov1.EchoRequest{Message: "rejected"})
	if !isRPCCode(err, rpc.CodeResourceExhausted) {
		t.Fatalf("rate-limited Echo() error = %v, want CodeResourceExhausted", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls = %d, want 2", got)
	}
	clientStats := clientConn.Stats()
	serverStats := testServer.Stats()
	if clientStats.RateLimitAllowed != 2 || clientStats.RateLimitRejected != 1 {
		t.Fatalf("client rate-limit stats = %+v, want 2 allowed and 1 rejected", clientStats)
	}
	if serverStats.RateLimitAllowed != 0 || serverStats.RateLimitRejected != 0 {
		t.Fatalf("rejected call reached server limiter: %+v", serverStats)
	}
}

func TestRateLimitAppliesToEveryRetryAttempt(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	address := startTestServer(t, blockingEchoServer{
		handler: func(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			calls.Add(1)
			return nil, rpc.NewError(rpc.CodeUnavailable, "temporary outage")
		},
	})
	limits := governance.RateLimitConfig{
		Methods: map[string]governance.RateLimitRule{
			governance.MethodRateLimitKey(echov1.EchoServiceName, "Echo"): {Rate: 0.01, Burst: 1},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	clientConn, err := client.Dial(
		ctx,
		address,
		client.WithRetryPolicy(testRetryPolicy(3)),
		client.WithCircuitBreaker(testBreakerConfig(100, time.Second)),
		client.WithRateLimits(limits),
	)
	if err != nil {
		t.Fatalf("client.Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	_, err = echov1.NewEchoClient(clientConn).Echo(
		rpc.WithIdempotent(ctx),
		&echov1.EchoRequest{Message: "retry-limited"},
	)
	if !isRPCCode(err, rpc.CodeResourceExhausted) {
		t.Fatalf("Echo() error = %v, want CodeResourceExhausted", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
	stats := clientConn.Stats()
	if stats.RetryAttempts != 2 || stats.RateLimitAllowed != 1 || stats.RateLimitRejected != 1 {
		t.Fatalf("unexpected retry/rate-limit stats: %+v", stats)
	}
}

func TestServerRateLimitRejectsBeforeHandler(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	limits := governance.RateLimitConfig{
		Methods: map[string]governance.RateLimitRule{
			governance.MethodRateLimitKey(echov1.EchoServiceName, "Echo"): {Rate: 0.01, Burst: 1},
		},
	}
	address, testServer := startTestServerWithOptions(t, blockingEchoServer{
		handler: func(_ context.Context, request *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			calls.Add(1)
			return &echov1.EchoResponse{Message: request.GetMessage()}, nil
		},
	}, server.WithRateLimits(limits))
	clientConn := dialTestClient(t, address)
	echoClient := echov1.NewEchoClient(clientConn)

	if _, err := echoClient.Echo(context.Background(), &echov1.EchoRequest{Message: "first"}); err != nil {
		t.Fatalf("first Echo() error = %v", err)
	}
	_, err := echoClient.Echo(context.Background(), &echov1.EchoRequest{Message: "second"})
	if !isRPCCode(err, rpc.CodeResourceExhausted) {
		t.Fatalf("second Echo() error = %v, want CodeResourceExhausted", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
	stats := testServer.Stats()
	if stats.RateLimitAllowed != 1 || stats.RateLimitRejected != 1 {
		t.Fatalf("server rate-limit stats = %+v", stats)
	}
}

func TestServerConcurrencyLimitRejectsExcessHandlers(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	address, testServer := startTestServerWithOptions(t, blockingEchoServer{
		handler: func(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			close(started)
			<-release
			return &echov1.EchoResponse{Message: "released"}, nil
		},
	}, server.WithMaxConcurrentRequests(1))
	clientConn := dialTestClient(t, address)
	echoClient := echov1.NewEchoClient(clientConn)

	firstDone := make(chan error, 1)
	go func() {
		_, err := echoClient.Echo(context.Background(), &echov1.EchoRequest{Message: "first"})
		firstDone <- err
	}()
	waitForClosed(t, started, "first handler")

	_, err := echoClient.Echo(context.Background(), &echov1.EchoRequest{Message: "second"})
	if !isRPCCode(err, rpc.CodeResourceExhausted) {
		t.Fatalf("second Echo() error = %v, want CodeResourceExhausted", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Echo() error = %v", err)
	}
	stats := testServer.Stats()
	if stats.ConcurrencyRejected != 1 {
		t.Fatalf("concurrency rejections = %d, want 1", stats.ConcurrencyRejected)
	}
}

func TestServerRequestBodyLimitReturnsResourceExhausted(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	address, testServer := startTestServerWithOptions(t, blockingEchoServer{
		handler: func(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			calls.Add(1)
			return &echov1.EchoResponse{}, nil
		},
	}, server.WithMaxRequestBodyBytes(8))
	clientConn := dialTestClient(t, address)

	_, err := echov1.NewEchoClient(clientConn).Echo(
		context.Background(),
		&echov1.EchoRequest{Message: "body greater than eight bytes"},
	)
	if !isRPCCode(err, rpc.CodeResourceExhausted) {
		t.Fatalf("Echo() error = %v, want CodeResourceExhausted", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("handler calls = %d, want 0", got)
	}
	if stats := testServer.Stats(); stats.BodyRejected != 1 {
		t.Fatalf("body rejections = %d, want 1", stats.BodyRejected)
	}
}

func TestServerRateLimitWaitHonorsCallDeadline(t *testing.T) {
	t.Parallel()

	limits := governance.RateLimitConfig{
		Methods: map[string]governance.RateLimitRule{
			governance.MethodRateLimitKey(echov1.EchoServiceName, "Echo"): {Rate: 0.1, Burst: 1, Wait: true},
		},
	}
	address, testServer := startTestServerWithOptions(t, echoServer{}, server.WithRateLimits(limits))
	clientConn := dialTestClient(t, address)
	echoClient := echov1.NewEchoClient(clientConn)

	if _, err := echoClient.Echo(context.Background(), &echov1.EchoRequest{Message: "consume"}); err != nil {
		t.Fatalf("initial Echo() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := echoClient.Echo(ctx, &echov1.EchoRequest{Message: "wait"})
	if !isRPCCode(err, rpc.CodeDeadlineExceeded) {
		t.Fatalf("waiting Echo() error = %v, want CodeDeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("waiting Echo() elapsed = %v", elapsed)
	}
	waitForServerStats(t, testServer, func(stats server.Stats) bool {
		return stats.RateLimitWaits == 1 &&
			len(stats.RateLimits) == 1 &&
			stats.RateLimits[0].WaitCanceled == 1
	})
}

func TestObservabilityCorrelatesClientAndServerCalls(t *testing.T) {
	t.Parallel()

	var clientLogs bytes.Buffer
	var serverLogs bytes.Buffer
	clientObserver := observability.New(observability.Config{
		Enabled:  true,
		Instance: "client-test",
		Output:   &clientLogs,
	})
	serverObserver := observability.New(observability.Config{
		Enabled:  true,
		Instance: "server-test",
		Output:   &serverLogs,
	})
	address, _ := startTestServerWithOptions(
		t,
		echoServer{},
		server.WithObserver(serverObserver),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	clientConn, err := client.Dial(ctx, address, client.WithObserver(clientObserver))
	if err != nil {
		t.Fatalf("client.Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	response, err := echov1.NewEchoClient(clientConn).Echo(ctx, &echov1.EchoRequest{Message: "observed"})
	if err != nil {
		t.Fatalf("Echo() error = %v", err)
	}
	if response.GetMessage() != "observed" {
		t.Fatalf("Echo() response = %q", response.GetMessage())
	}

	clientLog := decodeStructuredLog(t, clientLogs.Bytes())
	serverLog := decodeStructuredLog(t, serverLogs.Bytes())
	if clientLog["trace_id"] != serverLog["trace_id"] {
		t.Fatalf("trace ids differ: client=%v server=%v", clientLog["trace_id"], serverLog["trace_id"])
	}
	if clientLog["request_id"] != serverLog["request_id"] {
		t.Fatalf("request ids differ: client=%v server=%v", clientLog["request_id"], serverLog["request_id"])
	}
	if serverLog["parent_span_id"] != clientLog["span_id"] {
		t.Fatalf("server parent span = %v, want %v", serverLog["parent_span_id"], clientLog["span_id"])
	}
	if clientLog["instance"] != "client-test" || serverLog["instance"] != "server-test" {
		t.Fatalf("unexpected instances: client=%v server=%v", clientLog["instance"], serverLog["instance"])
	}

	var clientMetrics bytes.Buffer
	if err := clientObserver.Metrics().WritePrometheus(&clientMetrics); err != nil {
		t.Fatalf("client metrics error = %v", err)
	}
	var serverMetrics bytes.Buffer
	if err := serverObserver.Metrics().WritePrometheus(&serverMetrics); err != nil {
		t.Fatalf("server metrics error = %v", err)
	}
	if !strings.Contains(clientMetrics.String(), `rpc_requests_total{role="client"`) {
		t.Fatalf("client metrics missing request counter:\n%s", clientMetrics.String())
	}
	if !strings.Contains(serverMetrics.String(), `rpc_requests_total{role="server"`) {
		t.Fatalf("server metrics missing request counter:\n%s", serverMetrics.String())
	}
	if strings.Contains(clientMetrics.String(), "request_id") || strings.Contains(serverMetrics.String(), "request_id") {
		t.Fatal("metrics contain request_id labels")
	}
}

func TestDisabledObservabilityDoesNotChangeRPC(t *testing.T) {
	t.Parallel()

	var clientLogs bytes.Buffer
	var serverLogs bytes.Buffer
	clientObserver := observability.New(observability.Config{Enabled: false, Output: &clientLogs})
	serverObserver := observability.New(observability.Config{Enabled: false, Output: &serverLogs})
	address, _ := startTestServerWithOptions(t, echoServer{}, server.WithObserver(serverObserver))
	clientConn := dialTestClient(t, address, client.WithObserver(clientObserver))

	response, err := echov1.NewEchoClient(clientConn).Echo(
		context.Background(),
		&echov1.EchoRequest{Message: "disabled-observability"},
	)
	if err != nil {
		t.Fatalf("Echo() error = %v", err)
	}
	if response.GetMessage() != "disabled-observability" {
		t.Fatalf("Echo() response = %q", response.GetMessage())
	}
	if clientLogs.Len() != 0 || serverLogs.Len() != 0 {
		t.Fatalf("disabled observability emitted logs: client=%q server=%q", clientLogs.String(), serverLogs.String())
	}
}

func decodeStructuredLog(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(data)))
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode structured log: %v; output=%s", err, data)
	}
	return value
}

type echoServer struct {
	err error
}

func (implementation echoServer) Echo(_ context.Context, request *echov1.EchoRequest) (*echov1.EchoResponse, error) {
	if implementation.err != nil {
		return nil, implementation.err
	}
	return &echov1.EchoResponse{Message: request.GetMessage()}, nil
}

type blockingEchoServer struct {
	handler func(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error)
}

func (implementation blockingEchoServer) Echo(
	ctx context.Context,
	request *echov1.EchoRequest,
) (*echov1.EchoResponse, error) {
	return implementation.handler(ctx, request)
}

func startTestServer(t *testing.T, implementation echov1.EchoServer) string {
	t.Helper()
	address, _ := startTestServerWithHandle(t, implementation)
	return address
}

func startTestServerWithHandle(
	t *testing.T,
	implementation echov1.EchoServer,
) (string, *server.Server) {
	t.Helper()
	return startTestServerWithOptions(t, implementation)
}

func startTestServerWithOptions(
	t *testing.T,
	implementation echov1.EchoServer,
	options ...server.Option,
) (string, *server.Server) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	testServer, err := server.NewWithOptions(options...)
	if err != nil {
		_ = listener.Close()
		t.Fatalf("server.NewWithOptions() error = %v", err)
	}
	if err := echov1.RegisterEchoServer(testServer, implementation); err != nil {
		_ = listener.Close()
		t.Fatalf("RegisterEchoServer() error = %v", err)
	}

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- testServer.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = testServer.Close()
		select {
		case err := <-serveDone:
			if !errors.Is(err, server.ErrServerClosed) {
				t.Errorf("Serve() error = %v, want ErrServerClosed", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Serve() did not stop")
		}
	})
	return listener.Addr().String(), testServer
}

func dialTestClient(t *testing.T, address string, options ...client.Option) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	clientConn, err := client.Dial(ctx, address, options...)
	if err != nil {
		t.Fatalf("client.Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	return clientConn
}

func dialGovernedClient(
	t *testing.T,
	address string,
	policy governance.RetryPolicy,
	breaker governance.CircuitBreakerConfig,
) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	clientConn, err := client.Dial(
		ctx,
		address,
		client.WithRetryPolicy(policy),
		client.WithCircuitBreaker(breaker),
	)
	if err != nil {
		t.Fatalf("client.Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	return clientConn
}

func testRetryPolicy(maxAttempts int) governance.RetryPolicy {
	return governance.RetryPolicy{
		MaxAttempts:    maxAttempts,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		MaxElapsed:     time.Second,
		Jitter:         func(time.Duration) time.Duration { return 0 },
	}
}

func testBreakerConfig(
	failureThreshold int,
	openTimeout time.Duration,
) governance.CircuitBreakerConfig {
	return governance.CircuitBreakerConfig{
		FailureThreshold: failureThreshold,
		OpenTimeout:      openTimeout,
	}
}

func isRPCCode(err error, want rpc.ErrorCode) bool {
	var remoteError *rpc.Error
	return errors.As(err, &remoteError) && remoteError.Code == want
}

func waitForClosed(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitForPending(t *testing.T, clientConn *client.Client, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if clientConn.Pending() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending calls = %d, want %d", clientConn.Pending(), want)
}

func waitForStats(t *testing.T, clientConn *client.Client, ready func(client.Stats) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ready(clientConn.Stats()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("client stats did not reach expected state: %+v", clientConn.Stats())
}

func waitForServerStats(t *testing.T, testServer *server.Server, ready func(server.Stats) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ready(testServer.Stats()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("server stats did not reach expected state: %+v", testServer.Stats())
}

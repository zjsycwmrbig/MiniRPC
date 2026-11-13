package client_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/client"
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

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	testServer := server.New()
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

func dialTestClient(t *testing.T, address string) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	clientConn, err := client.Dial(ctx, address)
	if err != nil {
		t.Fatalf("client.Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	return clientConn
}

func isRPCCode(err error, want rpc.ErrorCode) bool {
	var remoteError *rpc.Error
	return errors.As(err, &remoteError) && remoteError.Code == want
}

func waitForClosed(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
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

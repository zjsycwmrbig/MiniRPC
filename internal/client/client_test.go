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

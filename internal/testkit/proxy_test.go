package testkit_test

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/client"
	"github.com/zjsycwmrbig/mini-rpc/internal/governance"
	"github.com/zjsycwmrbig/mini-rpc/internal/protocol"
	"github.com/zjsycwmrbig/mini-rpc/internal/server"
	"github.com/zjsycwmrbig/mini-rpc/internal/testkit"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
)

func TestFaultProxyReplacesOneResponseAndClientRetries(t *testing.T) {
	t.Parallel()

	target := startProxyEchoServer(t)
	proxy, err := testkit.NewFaultProxy(target, func(
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
		t.Fatalf("NewFaultProxy() error = %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	clientConn := dialProxyClient(t, proxy.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := echov1.NewEchoClient(clientConn).Echo(
		rpc.WithIdempotent(ctx),
		&echov1.EchoRequest{Message: "retry-after-error"},
	)
	if err != nil {
		t.Fatalf("Echo() error = %v", err)
	}
	if response.GetMessage() != "retry-after-error" {
		t.Fatalf("Echo() response = %q", response.GetMessage())
	}
	stats := proxy.Stats()
	if stats.Requests != 2 || stats.Responses != 2 || stats.ReplacedFrames != 1 {
		t.Fatalf("proxy stats = %+v", stats)
	}
	if attempts := clientConn.Stats().RetryAttempts; attempts != 2 {
		t.Fatalf("client attempts = %d, want 2", attempts)
	}
}

func TestFaultProxyClosesConnectionAndNextAttemptRecovers(t *testing.T) {
	t.Parallel()

	target := startProxyEchoServer(t)
	var closeCount atomic.Int32
	proxy, err := testkit.NewFaultProxy(target, func(
		direction testkit.Direction,
		_ int,
		_ protocol.Frame,
	) testkit.Fault {
		if direction == testkit.ClientToServer && closeCount.Add(1) == 1 {
			return testkit.Fault{Close: true}
		}
		return testkit.Fault{}
	})
	if err != nil {
		t.Fatalf("NewFaultProxy() error = %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	clientConn := dialProxyClient(t, proxy.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := echov1.NewEchoClient(clientConn).Echo(
		rpc.WithIdempotent(ctx),
		&echov1.EchoRequest{Message: "reconnect"},
	)
	if err != nil {
		t.Fatalf("Echo() error = %v", err)
	}
	if response.GetMessage() != "reconnect" {
		t.Fatalf("Echo() response = %q", response.GetMessage())
	}
	if stats := proxy.Stats(); stats.AcceptedConnections < 2 || stats.ClosedConnections == 0 {
		t.Fatalf("proxy reconnect stats = %+v", stats)
	}
}

func TestFaultProxyDelaysResponseWithoutPermanentFailure(t *testing.T) {
	t.Parallel()

	target := startProxyEchoServer(t)
	var delayed atomic.Bool
	proxy, err := testkit.NewFaultProxy(target, func(
		direction testkit.Direction,
		_ int,
		_ protocol.Frame,
	) testkit.Fault {
		if direction == testkit.ServerToClient && delayed.CompareAndSwap(false, true) {
			return testkit.Fault{Delay: 100 * time.Millisecond}
		}
		return testkit.Fault{}
	})
	if err != nil {
		t.Fatalf("NewFaultProxy() error = %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	clientConn := dialProxyClient(t, proxy.Addr())

	fastCtx, fastCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	_, err = echov1.NewEchoClient(clientConn).Echo(
		fastCtx,
		&echov1.EchoRequest{Message: "too-slow"},
	)
	fastCancel()
	var rpcError *rpc.Error
	if !errors.As(err, &rpcError) || rpcError.Code != rpc.CodeDeadlineExceeded {
		t.Fatalf("delayed Echo() error = %v, want DEADLINE_EXCEEDED", err)
	}

	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), time.Second)
	defer recoveryCancel()
	response, err := echov1.NewEchoClient(clientConn).Echo(
		recoveryCtx,
		&echov1.EchoRequest{Message: "recovered"},
	)
	if err != nil || response.GetMessage() != "recovered" {
		t.Fatalf("recovery Echo() = %q, %v", response.GetMessage(), err)
	}
}

func TestFaultProxyDropsOneResponseAndNextCallRecovers(t *testing.T) {
	t.Parallel()

	target := startProxyEchoServer(t)
	var dropped atomic.Bool
	proxy, err := testkit.NewFaultProxy(target, func(
		direction testkit.Direction,
		_ int,
		_ protocol.Frame,
	) testkit.Fault {
		if direction == testkit.ServerToClient && dropped.CompareAndSwap(false, true) {
			return testkit.Fault{Drop: true}
		}
		return testkit.Fault{}
	})
	if err != nil {
		t.Fatalf("NewFaultProxy() error = %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	clientConn := dialProxyClient(t, proxy.Addr())

	lostCtx, lostCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err = echov1.NewEchoClient(clientConn).Echo(
		lostCtx,
		&echov1.EchoRequest{Message: "lost"},
	)
	lostCancel()
	var rpcError *rpc.Error
	if !errors.As(err, &rpcError) || rpcError.Code != rpc.CodeDeadlineExceeded {
		t.Fatalf("lost-response Echo() error = %v, want DEADLINE_EXCEEDED", err)
	}

	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), time.Second)
	defer recoveryCancel()
	response, err := echov1.NewEchoClient(clientConn).Echo(
		recoveryCtx,
		&echov1.EchoRequest{Message: "recovered"},
	)
	if err != nil || response.GetMessage() != "recovered" {
		t.Fatalf("recovery Echo() = %q, %v", response.GetMessage(), err)
	}
	if stats := proxy.Stats(); stats.DroppedFrames != 1 {
		t.Fatalf("proxy dropped frames = %d, want 1", stats.DroppedFrames)
	}
}

func dialProxyClient(t *testing.T, address string) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	clientConn, err := client.Dial(
		ctx,
		address,
		client.WithRetryPolicy(governance.RetryPolicy{
			MaxAttempts:    2,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     time.Millisecond,
			MaxElapsed:     time.Second,
			Jitter:         func(time.Duration) time.Duration { return 0 },
		}),
	)
	if err != nil {
		t.Fatalf("client.Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	return clientConn
}

func startProxyEchoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	rpcServer := server.New()
	if err := echov1.RegisterEchoServer(rpcServer, proxyEcho{}); err != nil {
		t.Fatalf("RegisterEchoServer() error = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- rpcServer.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = rpcServer.GracefulStop(ctx)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("timed out waiting for proxy Echo server stop")
		}
	})
	return listener.Addr().String()
}

type proxyEcho struct{}

func (proxyEcho) Echo(_ context.Context, request *echov1.EchoRequest) (*echov1.EchoResponse, error) {
	return &echov1.EchoResponse{Message: request.GetMessage()}, nil
}

package server

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/protocol"
)

func TestRegisterServiceValidatesAndRejectsDuplicate(t *testing.T) {
	t.Parallel()

	testServer := New()
	if err := echov1.RegisterEchoServer(testServer, echoImplementation{}); err != nil {
		t.Fatalf("RegisterEchoServer() error = %v", err)
	}
	if err := echov1.RegisterEchoServer(testServer, echoImplementation{}); !errors.Is(err, ErrServiceRegistered) {
		t.Fatalf("duplicate registration error = %v, want ErrServiceRegistered", err)
	}

	invalid := echov1.EchoServiceDesc
	if err := testServer.RegisterService(&invalid, struct{}{}); !errors.Is(err, ErrInvalidImplementation) {
		t.Fatalf("invalid implementation error = %v, want ErrInvalidImplementation", err)
	}
	if err := testServer.RegisterService(nil, nil); !errors.Is(err, ErrInvalidService) {
		t.Fatalf("nil descriptor error = %v, want ErrInvalidService", err)
	}
}

func TestServeAfterCloseReturnsServerClosed(t *testing.T) {
	t.Parallel()

	testServer := New()
	if err := testServer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := testServer.Serve(closedListener{}); !errors.Is(err, ErrServerClosed) {
		t.Fatalf("Serve() error = %v, want ErrServerClosed", err)
	}
}

func TestRequestContextUsesDeadlineMetadata(t *testing.T) {
	t.Parallel()

	deadline := time.Now().Add(time.Minute).Round(time.Millisecond)
	ctx, cancel, err := requestContext(context.Background(), map[string]string{
		protocol.MetadataKeyDeadlineUnixNano: strconv.FormatInt(deadline.UnixNano(), 10),
	})
	if err != nil {
		t.Fatalf("requestContext() error = %v", err)
	}
	defer cancel()

	got, ok := ctx.Deadline()
	if !ok || !got.Equal(deadline) {
		t.Fatalf("context deadline = %v, %v; want %v, true", got, ok, deadline)
	}
}

func TestNewWithOptionsValidatesResourceLimits(t *testing.T) {
	t.Parallel()

	testServer, err := NewWithOptions(
		WithMaxConnections(2),
		WithMaxConcurrentRequests(3),
		WithMaxRequestBodyBytes(1024),
	)
	if err != nil {
		t.Fatalf("NewWithOptions() error = %v", err)
	}
	if testServer.config.MaxConnections != 2 ||
		testServer.config.MaxConcurrentRequests != 3 ||
		testServer.config.MaxRequestBodyBytes != 1024 {
		t.Fatalf("unexpected server config: %+v", testServer.config)
	}
	if _, err := NewWithOptions(WithMaxConnections(0)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid NewWithOptions() error = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewWithOptions(WithTLSHandshakeTimeout(0)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("zero TLS handshake timeout error = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewWithOptions(WithTLSConfig(&tls.Config{})); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("certificate-less TLS config error = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewWithOptions(WithTLSConfig(&tls.Config{MinVersion: tls.VersionTLS11})); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("legacy TLS version error = %v, want ErrInvalidConfig", err)
	}
}

func TestConnectionLimitRejectsExcessTransports(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	testServer, err := NewWithOptions(WithMaxConnections(1))
	if err != nil {
		t.Fatalf("NewWithOptions() error = %v", err)
	}
	if err := echov1.RegisterEchoServer(testServer, echoImplementation{}); err != nil {
		t.Fatalf("RegisterEchoServer() error = %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- testServer.Serve(listener) }()
	first, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("first net.Dial() error = %v", err)
	}
	t.Cleanup(func() {
		_ = first.Close()
		_ = testServer.Close()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Error("Serve() did not stop")
		}
	})
	waitForConnections(t, testServer, 1)

	second, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("second net.Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if testServer.Stats().ConnectionRejected == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("connection rejections = %d, want 1", testServer.Stats().ConnectionRejected)
}

func waitForConnections(t *testing.T, testServer *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if testServer.Stats().ActiveConnections == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("active connections = %d, want %d", testServer.Stats().ActiveConnections, want)
}

type echoImplementation struct{}

func (echoImplementation) Echo(context.Context, *echov1.EchoRequest) (*echov1.EchoResponse, error) {
	return &echov1.EchoResponse{}, nil
}

type closedListener struct{}

func (closedListener) Accept() (net.Conn, error) {
	return nil, net.ErrClosed
}

func (closedListener) Close() error {
	return nil
}

func (closedListener) Addr() net.Addr {
	return nil
}

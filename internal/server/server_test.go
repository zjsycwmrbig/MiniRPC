package server

import (
	"context"
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
	ctx, cancel, err := requestContext(map[string]string{
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

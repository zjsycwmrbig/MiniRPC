package client

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
)

func TestDialTimeoutIsClassified(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	clientConn := newClient(
		"example.invalid:1",
		DefaultConfig(),
		func(context.Context, string) (net.Conn, error) {
			return nil, context.DeadlineExceeded
		},
	)
	defer func() { _ = clientConn.Close() }()

	_, err := clientConn.dial(ctx, clientConn.address)
	if !errors.Is(err, ErrDialTimeout) {
		t.Fatalf("dial() error = %v, want ErrDialTimeout", err)
	}
	rpcError, ok := normalizeContextError(err).(*rpc.Error)
	if !ok || rpcError.Code != rpc.CodeDeadlineExceeded {
		t.Fatalf("normalized error = %#v, want CodeDeadlineExceeded", rpcError)
	}
}

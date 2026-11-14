package transport

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/internal/protocol"
	"golang.org/x/net/http2"
)

func TestHTTP2ConnMultiplexesConcurrentCalls(t *testing.T) {
	t.Parallel()

	var accepted atomic.Int64
	address, closeServer := startHTTP2TestServer(t, func(_ context.Context, request *protocol.Frame) (*protocol.Frame, error) {
		return &protocol.Frame{
			Type:      protocol.FrameTypeResponse,
			RequestID: request.RequestID,
			Body:      append([]byte(nil), request.Body...),
		}, nil
	}, &accepted)
	defer closeServer()

	netConn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("net.Dial() error = %v", err)
	}
	conn, err := NewHTTP2Conn(netConn, protocol.DefaultCodec)
	if err != nil {
		t.Fatalf("NewHTTP2Conn() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	const calls = 100
	var waitGroup sync.WaitGroup
	callErrors := make([]error, calls)
	waitGroup.Add(calls)
	for index := range calls {
		index := index
		go func() {
			defer waitGroup.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			response, callErr := conn.Call(ctx, protocol.Frame{
				Type: protocol.FrameTypeRequest,
				Body: []byte{byte(index)},
			})
			if callErr != nil {
				callErrors[index] = callErr
				return
			}
			if len(response.Body) != 1 || response.Body[0] != byte(index) {
				callErrors[index] = errors.New("response body mismatch")
			}
		}()
	}
	waitGroup.Wait()
	for index, callErr := range callErrors {
		if callErr != nil {
			t.Fatalf("call %d error = %v", index, callErr)
		}
	}
	if got := accepted.Load(); got != 1 {
		t.Fatalf("accepted HTTP/2 connections = %d, want 1", got)
	}
}

func TestHTTP2ConnCancellationPropagatesToServerStream(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	serverCanceled := make(chan error, 1)
	address, closeServer := startHTTP2TestServer(t, func(ctx context.Context, request *protocol.Frame) (*protocol.Frame, error) {
		close(started)
		<-ctx.Done()
		serverCanceled <- ctx.Err()
		return nil, ctx.Err()
	}, nil)
	defer closeServer()

	netConn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("net.Dial() error = %v", err)
	}
	conn, err := NewHTTP2Conn(netConn, protocol.DefaultCodec)
	if err != nil {
		t.Fatalf("NewHTTP2Conn() error = %v", err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		_, callErr := conn.Call(ctx, protocol.Frame{Type: protocol.FrameTypeRequest})
		callDone <- callErr
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive the HTTP/2 stream")
	}
	cancel()
	if err := <-callDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Call() error = %v, want context.Canceled", err)
	}
	select {
	case err := <-serverCanceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("server stream error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server stream was not canceled")
	}
	if conn.Pending() != 0 {
		t.Fatalf("pending streams = %d, want 0", conn.Pending())
	}
}

func startHTTP2TestServer(
	t *testing.T,
	handle func(context.Context, *protocol.Frame) (*protocol.Frame, error),
	accepted *atomic.Int64,
) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			netConn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			if accepted != nil {
				accepted.Add(1)
			}
			go (&http2.Server{}).ServeConn(netConn, &http2.ServeConnOpts{
				Context: ctx,
				Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Method != http.MethodPost ||
						request.URL.Path != HTTP2InvokePath ||
						request.Header.Get("Content-Type") != HTTP2FrameContentType {
						http.Error(writer, "invalid Mini-RPC HTTP/2 request", http.StatusBadRequest)
						return
					}
					frame, readErr := protocol.DefaultCodec.Read(request.Body)
					if readErr != nil {
						http.Error(writer, "invalid Mini-RPC frame", http.StatusBadRequest)
						return
					}
					response, handleErr := handle(request.Context(), frame)
					if handleErr != nil {
						return
					}
					writer.Header().Set("Content-Type", HTTP2FrameContentType)
					if writeErr := protocol.DefaultCodec.Write(writer, *response); writeErr != nil {
						return
					}
				}),
			})
		}
	}()
	return listener.Addr().String(), func() {
		cancel()
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("HTTP/2 test server did not stop")
		}
	}
}

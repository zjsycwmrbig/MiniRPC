package transport

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/internal/protocol"
)

func TestCallResolvesMatchingResponse(t *testing.T) {
	t.Parallel()

	clientSide, serverSide := net.Pipe()
	conn := NewConn(clientSide, protocol.DefaultCodec)
	defer func() { _ = conn.Close() }()
	defer func() { _ = serverSide.Close() }()

	serverDone := make(chan error, 1)
	go func() {
		request, err := protocol.DefaultCodec.Read(serverSide)
		if err != nil {
			serverDone <- err
			return
		}
		if request.RequestID == 0 {
			serverDone <- errors.New("request ID is zero")
			return
		}
		serverDone <- protocol.DefaultCodec.Write(serverSide, protocol.Frame{
			Type:      protocol.FrameTypeResponse,
			RequestID: request.RequestID,
			Body:      []byte("response"),
		})
	}()

	response, err := conn.Call(context.Background(), protocol.Frame{
		Type: protocol.FrameTypeRequest,
		Body: []byte("request"),
	})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if string(response.Body) != "response" {
		t.Fatalf("response body = %q, want %q", response.Body, "response")
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error = %v", err)
	}
}

func TestCallPreservesResponseBeforeImmediateEOF(t *testing.T) {
	t.Parallel()

	for iteration := 0; iteration < 256; iteration++ {
		clientSide, serverSide := net.Pipe()
		conn := NewConn(clientSide, protocol.DefaultCodec)
		serverDone := make(chan error, 1)
		go func() {
			request, err := protocol.DefaultCodec.Read(serverSide)
			if err != nil {
				serverDone <- err
				return
			}
			if err := protocol.DefaultCodec.Write(serverSide, protocol.Frame{
				Type:      protocol.FrameTypeResponse,
				RequestID: request.RequestID,
				Body:      []byte("final"),
			}); err != nil {
				serverDone <- err
				return
			}
			serverDone <- serverSide.Close()
		}()

		response, err := conn.Call(context.Background(), protocol.Frame{
			Type: protocol.FrameTypeRequest,
		})
		if err != nil {
			t.Fatalf("iteration %d: Call() error = %v", iteration, err)
		}
		if string(response.Body) != "final" {
			t.Fatalf("iteration %d: response body = %q, want %q", iteration, response.Body, "final")
		}
		if err := <-serverDone; err != nil {
			t.Fatalf("iteration %d: server error = %v", iteration, err)
		}
		_ = conn.Close()
	}
}

func TestConcurrentCallsResolveByRequestID(t *testing.T) {
	t.Parallel()

	const calls = 64
	clientSide, serverSide := net.Pipe()
	conn := NewConn(clientSide, protocol.DefaultCodec)
	defer func() { _ = conn.Close() }()
	defer func() { _ = serverSide.Close() }()

	serverDone := make(chan error, 1)
	go func() {
		for range calls {
			request, err := protocol.DefaultCodec.Read(serverSide)
			if err != nil {
				serverDone <- err
				return
			}
			if err := protocol.DefaultCodec.Write(serverSide, protocol.Frame{
				Type:      protocol.FrameTypeResponse,
				RequestID: request.RequestID,
				Body:      request.Body,
			}); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()

	var waitGroup sync.WaitGroup
	errorsByCall := make([]error, calls)
	bodies := make([][]byte, calls)
	waitGroup.Add(calls)
	for index := range calls {
		index := index
		go func() {
			defer waitGroup.Done()
			body := []byte{byte(index + 1)}
			response, err := conn.Call(context.Background(), protocol.Frame{
				Type: protocol.FrameTypeRequest,
				Body: body,
			})
			errorsByCall[index] = err
			if response != nil {
				bodies[index] = response.Body
			}
		}()
	}
	waitGroup.Wait()

	for index, err := range errorsByCall {
		if err != nil {
			t.Fatalf("call %d error = %v", index, err)
		}
		if len(bodies[index]) != 1 || bodies[index][0] != byte(index+1) {
			t.Fatalf("call %d response = %v", index, bodies[index])
		}
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error = %v", err)
	}
}

func TestCancelRemovesPendingRequestAndAllowsNextCall(t *testing.T) {
	t.Parallel()

	clientSide, serverSide := net.Pipe()
	conn := NewConn(clientSide, protocol.DefaultCodec)
	defer func() { _ = conn.Close() }()
	defer func() { _ = serverSide.Close() }()

	firstRequestRead := make(chan struct{})
	releaseFirstResponse := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		first, err := protocol.DefaultCodec.Read(serverSide)
		if err != nil {
			serverDone <- err
			return
		}
		close(firstRequestRead)
		<-releaseFirstResponse
		if err := protocol.DefaultCodec.Write(serverSide, protocol.Frame{
			Type:      protocol.FrameTypeResponse,
			RequestID: first.RequestID,
			Body:      []byte("late"),
		}); err != nil {
			serverDone <- err
			return
		}

		cancel, err := protocol.DefaultCodec.Read(serverSide)
		if err != nil {
			serverDone <- err
			return
		}
		if cancel.Type != protocol.FrameTypeCancel || cancel.RequestID != first.RequestID {
			serverDone <- errors.New("expected matching cancel frame")
			return
		}

		second, err := protocol.DefaultCodec.Read(serverSide)
		if err != nil {
			serverDone <- err
			return
		}
		serverDone <- protocol.DefaultCodec.Write(serverSide, protocol.Frame{
			Type:      protocol.FrameTypeResponse,
			RequestID: second.RequestID,
			Body:      []byte("next"),
		})
	}()

	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := conn.Call(ctx, protocol.Frame{Type: protocol.FrameTypeRequest})
		firstDone <- err
	}()

	waitForSignal(t, firstRequestRead, "first request")
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Call() error = %v, want context.Canceled", err)
	}
	waitForPending(t, conn, 0)
	close(releaseFirstResponse)

	response, err := conn.Call(context.Background(), protocol.Frame{
		Type: protocol.FrameTypeRequest,
		Body: []byte("second"),
	})
	if err != nil {
		t.Fatalf("second Call() error = %v", err)
	}
	if string(response.Body) != "next" {
		t.Fatalf("second response = %q, want %q", response.Body, "next")
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error = %v", err)
	}
}

func TestCloseWakesPendingCall(t *testing.T) {
	t.Parallel()

	clientSide, serverSide := net.Pipe()
	conn := NewConn(clientSide, protocol.DefaultCodec)
	defer func() { _ = serverSide.Close() }()

	requestRead := make(chan struct{})
	go func() {
		_, err := protocol.DefaultCodec.Read(serverSide)
		if err == nil {
			close(requestRead)
		}
	}()

	callDone := make(chan error, 1)
	go func() {
		_, err := conn.Call(context.Background(), protocol.Frame{Type: protocol.FrameTypeRequest})
		callDone <- err
	}()

	waitForSignal(t, requestRead, "request")
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case err := <-callDone:
		if !errors.Is(err, ErrConnClosed) {
			t.Fatalf("Call() error = %v, want ErrConnClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after Close")
	}
}

func TestCallValidatesInput(t *testing.T) {
	t.Parallel()

	clientSide, serverSide := net.Pipe()
	conn := NewConn(clientSide, protocol.DefaultCodec)
	defer func() { _ = conn.Close() }()
	defer func() { _ = serverSide.Close() }()

	var nilContext context.Context
	if _, err := conn.Call(nilContext, protocol.Frame{Type: protocol.FrameTypeRequest}); !errors.Is(err, ErrNilContext) {
		t.Fatalf("nil context error = %v, want ErrNilContext", err)
	}
	if _, err := conn.Call(context.Background(), protocol.Frame{Type: protocol.FrameTypeResponse}); !errors.Is(err, ErrNotRequest) {
		t.Fatalf("non-request error = %v, want ErrNotRequest", err)
	}
	if _, err := conn.Call(context.Background(), protocol.Frame{
		Type:  protocol.FrameTypeRequest,
		Flags: protocol.FlagOneWay,
	}); !errors.Is(err, ErrOneWayUnsupported) {
		t.Fatalf("one-way error = %v, want ErrOneWayUnsupported", err)
	}
	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := conn.Call(canceledContext, protocol.Frame{Type: protocol.FrameTypeRequest}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled context error = %v, want context.Canceled", err)
	}
}

func TestConcurrencyLimitWaitsForAvailableSlot(t *testing.T) {
	t.Parallel()

	clientSide, serverSide := net.Pipe()
	conn := NewConnWithOptions(
		clientSide,
		protocol.DefaultCodec,
		ConnOptions{MaxConcurrentCalls: 1},
	)
	defer func() { _ = conn.Close() }()
	defer func() { _ = serverSide.Close() }()

	requestRead := make(chan struct{})
	release := make(chan struct{})
	go func() {
		request, err := protocol.DefaultCodec.Read(serverSide)
		if err != nil {
			return
		}
		close(requestRead)
		<-release
		_ = protocol.DefaultCodec.Write(serverSide, protocol.Frame{
			Type:      protocol.FrameTypeResponse,
			RequestID: request.RequestID,
		})
	}()

	firstDone := make(chan error, 1)
	go func() {
		_, err := conn.Call(context.Background(), protocol.Frame{Type: protocol.FrameTypeRequest})
		firstDone <- err
	}()
	waitForSignal(t, requestRead, "first request")

	secondCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := conn.Call(secondCtx, protocol.Frame{Type: protocol.FrameTypeRequest})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Call() error = %v, want context.DeadlineExceeded", err)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Call() error = %v", err)
	}
}

func TestWriteTimeoutIsReported(t *testing.T) {
	t.Parallel()

	clientSide, serverSide := net.Pipe()
	conn := NewConn(clientSide, protocol.DefaultCodec)
	defer func() { _ = conn.Close() }()
	defer func() { _ = serverSide.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := conn.Call(ctx, protocol.Frame{
		Type: protocol.FrameTypeRequest,
		Body: make([]byte, 1024),
	})
	if !errors.Is(err, ErrWriteTimeout) {
		t.Fatalf("Call() error = %v, want ErrWriteTimeout", err)
	}
}

func TestResponseFrameIsPreserved(t *testing.T) {
	t.Parallel()

	clientSide, serverSide := net.Pipe()
	conn := NewConn(clientSide, protocol.DefaultCodec)
	defer func() { _ = conn.Close() }()
	defer func() { _ = serverSide.Close() }()

	wantHeader := map[string]string{protocol.MetadataKeyTraceID: "abc"}
	go func() {
		request, err := protocol.DefaultCodec.Read(serverSide)
		if err != nil {
			return
		}
		_ = protocol.DefaultCodec.Write(serverSide, protocol.Frame{
			Type:      protocol.FrameTypeResponse,
			Flags:     protocol.FlagError,
			RequestID: request.RequestID,
			Header:    wantHeader,
			Body:      []byte("error"),
		})
	}()

	response, err := conn.Call(context.Background(), protocol.Frame{Type: protocol.FrameTypeRequest})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if response.Flags != protocol.FlagError || !reflect.DeepEqual(response.Header, wantHeader) {
		t.Fatalf("response = %#v", response)
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitForPending(t *testing.T, conn *Conn, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if conn.Pending() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending calls = %d, want %d", conn.Pending(), want)
}

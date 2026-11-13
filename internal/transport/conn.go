package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/zjsycwmrbig/mini-rpc/internal/protocol"
)

type callResult struct {
	frame *protocol.Frame
	err   error
}

// DefaultMaxConcurrentCalls is the protocol v1 per-connection request limit.
const DefaultMaxConcurrentCalls = 1024

// ConnOptions configures a multiplexed transport connection.
type ConnOptions struct {
	MaxConcurrentCalls int
}

// Conn multiplexes concurrent RPC calls over one framed stream.
//
// One reader goroutine decodes responses and resolves them through a pending
// map keyed by RequestID. Writes are serialized because frame payloads cannot
// interleave on a TCP stream.
type Conn struct {
	netConn net.Conn
	codec   *protocol.Codec

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[uint64]chan callResult
	closed    bool
	closeErr  error

	nextRequestID atomic.Uint64
	done          chan struct{}
	slots         chan struct{}
}

// NewConn starts response processing for netConn.
func NewConn(netConn net.Conn, codec *protocol.Codec) *Conn {
	return NewConnWithOptions(netConn, codec, ConnOptions{})
}

// NewConnWithOptions starts response processing with an explicit concurrency
// limit.
func NewConnWithOptions(netConn net.Conn, codec *protocol.Codec, options ConnOptions) *Conn {
	if codec == nil {
		codec = protocol.DefaultCodec
	}
	if options.MaxConcurrentCalls <= 0 {
		options.MaxConcurrentCalls = DefaultMaxConcurrentCalls
	}
	conn := &Conn{
		netConn: netConn,
		codec:   codec,
		pending: make(map[uint64]chan callResult),
		done:    make(chan struct{}),
		slots:   make(chan struct{}, options.MaxConcurrentCalls),
	}
	go conn.readLoop()
	return conn
}

// Call sends one request and waits for the matching response or context
// cancellation.
func (conn *Conn) Call(ctx context.Context, request protocol.Frame) (*protocol.Frame, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Type != protocol.FrameTypeRequest {
		return nil, ErrNotRequest
	}
	if request.Flags&protocol.FlagOneWay != 0 {
		return nil, ErrOneWayUnsupported
	}
	if err := conn.acquireSlot(ctx); err != nil {
		return nil, err
	}
	defer conn.releaseSlot()

	request.RequestID = conn.allocateRequestID()
	resultChannel := make(chan callResult, 1)

	conn.pendingMu.Lock()
	if conn.closed {
		closeErr := conn.closeErr
		conn.pendingMu.Unlock()
		return nil, closeErr
	}
	conn.pending[request.RequestID] = resultChannel
	conn.pendingMu.Unlock()

	if err := conn.writeFrame(request); err != nil {
		conn.removePending(request.RequestID)
		conn.shutdown(err)
		return nil, err
	}

	select {
	case result := <-resultChannel:
		return result.frame, result.err
	case <-ctx.Done():
		conn.removePending(request.RequestID)
		return nil, ctx.Err()
	case <-conn.done:
		conn.pendingMu.Lock()
		closeErr := conn.closeErr
		conn.pendingMu.Unlock()
		return nil, closeErr
	}
}

// acquireSlot reserves one of the bounded per-connection request slots.
func (conn *Conn) acquireSlot(ctx context.Context) error {
	select {
	case conn.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.done:
		if err := conn.Err(); err != nil {
			return err
		}
		return ErrConnClosed
	}
}

func (conn *Conn) releaseSlot() {
	<-conn.slots
}

// writeFrame writes one frame under the stream write lock.
func (conn *Conn) writeFrame(frame protocol.Frame) error {
	conn.writeMu.Lock()
	defer conn.writeMu.Unlock()
	return conn.codec.Write(conn.netConn, frame)
}

func (conn *Conn) readLoop() {
	for {
		frame, err := conn.codec.Read(conn.netConn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				conn.shutdown(ErrConnClosed)
			} else {
				conn.shutdown(err)
			}
			return
		}

		switch frame.Type {
		case protocol.FrameTypeResponse:
			conn.resolve(frame.RequestID, frame)
		default:
			// Server-originated operations such as heartbeats will be handled
			// when the corresponding lifecycle support is introduced.
		}
	}
}

func (conn *Conn) resolve(requestID uint64, frame *protocol.Frame) {
	conn.pendingMu.Lock()
	resultChannel := conn.pending[requestID]
	if resultChannel != nil {
		delete(conn.pending, requestID)
	}
	conn.pendingMu.Unlock()
	if resultChannel != nil {
		resultChannel <- callResult{frame: frame}
	}
}

func (conn *Conn) removePending(requestID uint64) {
	conn.pendingMu.Lock()
	delete(conn.pending, requestID)
	conn.pendingMu.Unlock()
}

func (conn *Conn) shutdown(err error) {
	conn.pendingMu.Lock()
	if conn.closed {
		conn.pendingMu.Unlock()
		return
	}
	conn.closed = true
	conn.closeErr = err
	pending := conn.pending
	conn.pending = make(map[uint64]chan callResult)
	conn.pendingMu.Unlock()

	close(conn.done)
	_ = conn.netConn.Close()
	for _, resultChannel := range pending {
		resultChannel <- callResult{err: err}
	}
}

func (conn *Conn) allocateRequestID() uint64 {
	for {
		requestID := conn.nextRequestID.Add(1)
		if requestID != 0 {
			return requestID
		}
	}
}

// Close shuts down the underlying network connection and wakes every pending
// request with ErrConnClosed.
func (conn *Conn) Close() error {
	conn.shutdown(ErrConnClosed)
	return nil
}

// Done closes when the connection stops serving requests.
func (conn *Conn) Done() <-chan struct{} {
	return conn.done
}

// Err returns the terminal connection error, or nil while open.
func (conn *Conn) Err() error {
	conn.pendingMu.Lock()
	defer conn.pendingMu.Unlock()
	return conn.closeErr
}

// Pending returns the number of requests currently awaiting a response.
func (conn *Conn) Pending() int {
	conn.pendingMu.Lock()
	defer conn.pendingMu.Unlock()
	return len(conn.pending)
}

// LocalAddr returns the local network address when available.
func (conn *Conn) LocalAddr() net.Addr {
	return conn.netConn.LocalAddr()
}

// RemoteAddr returns the remote network address when available.
func (conn *Conn) RemoteAddr() net.Addr {
	return conn.netConn.RemoteAddr()
}

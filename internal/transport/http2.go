package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"

	"github.com/zjsycwmrbig/mini-rpc/internal/protocol"
	"golang.org/x/net/http2"
)

const (
	// HTTP2InvokePath is the unary RPC endpoint exposed by the HTTP/2 adapter.
	HTTP2InvokePath = "/mini-rpc.v1/Invoke"

	// HTTP2FrameContentType identifies one encoded Mini-RPC v1 frame.
	HTTP2FrameContentType = "application/x-mini-rpc-frame"
)

// HTTP2Conn carries unary Mini-RPC frames as independent HTTP/2 streams over
// one long-lived connection. HTTP/2 owns stream multiplexing and cancellation;
// the v1 frame remains the application-level request and response envelope.
type HTTP2Conn struct {
	netConn   net.Conn
	codec     *protocol.Codec
	client    *http2.ClientConn
	scheme    string
	authority string

	nextRequestID atomic.Uint64
	pending       atomic.Int64

	errMu sync.RWMutex
	err   error

	closeOnce sync.Once
	closeErr  error
}

// NewHTTP2Conn starts an HTTP/2 client session on an established TCP or TLS
// connection. h2c uses prior knowledge when netConn is not a TLS connection;
// TLS callers must negotiate the h2 ALPN protocol before calling this function.
func NewHTTP2Conn(netConn net.Conn, codec *protocol.Codec) (*HTTP2Conn, error) {
	if netConn == nil {
		return nil, fmt.Errorf("%w: nil HTTP/2 network connection", ErrConnClosed)
	}
	if codec == nil {
		codec = protocol.DefaultCodec
	}
	authority := "localhost"
	if remote := netConn.RemoteAddr(); remote != nil && remote.String() != "" {
		authority = remote.String()
	}
	scheme := "http"
	if _, ok := netConn.(*tls.Conn); ok {
		scheme = "https"
	}
	transport := &http2.Transport{AllowHTTP: true}
	client, err := transport.NewClientConn(netConn)
	if err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("initialize HTTP/2 client connection: %w", err)
	}
	return &HTTP2Conn{
		netConn:   netConn,
		codec:     codec,
		client:    client,
		scheme:    scheme,
		authority: authority,
	}, nil
}

// Call sends one unary request as an HTTP/2 POST stream and decodes the
// response frame. Request IDs are assigned per connection so both sides retain
// the protocol v1 correlation contract even though HTTP/2 also has stream IDs.
func (conn *HTTP2Conn) Call(ctx context.Context, request protocol.Frame) (*protocol.Frame, error) {
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
	if err := conn.Err(); err != nil {
		return nil, err
	}
	if !conn.client.CanTakeNewRequest() {
		conn.setError(ErrConnClosed)
		return nil, ErrConnClosed
	}

	request.RequestID = conn.allocateRequestID()
	encoded, err := conn.codec.Encode(request)
	if err != nil {
		return nil, err
	}
	endpoint := url.URL{
		Scheme: conn.scheme,
		Host:   conn.authority,
		Path:   HTTP2InvokePath,
	}
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpoint.String(),
		bytes.NewReader(encoded),
	)
	if err != nil {
		return nil, fmt.Errorf("build HTTP/2 RPC request: %w", err)
	}
	httpRequest.Header.Set("Accept", HTTP2FrameContentType)
	httpRequest.Header.Set("Content-Type", HTTP2FrameContentType)

	conn.pending.Add(1)
	response, err := conn.client.RoundTrip(httpRequest)
	conn.pending.Add(-1)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if !conn.client.CanTakeNewRequest() {
			conn.setError(ErrConnClosed)
			return nil, fmt.Errorf("%w: %v", ErrConnClosed, err)
		}
		return nil, fmt.Errorf("HTTP/2 RPC round trip: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP/2 RPC status %d", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != HTTP2FrameContentType {
		return nil, fmt.Errorf("HTTP/2 RPC response content type %q", contentType)
	}

	frame, err := conn.codec.Read(response.Body)
	if err != nil {
		return nil, err
	}
	var trailing [1]byte
	if read, trailingErr := response.Body.Read(trailing[:]); read != 0 || !errors.Is(trailingErr, io.EOF) {
		return nil, fmt.Errorf("HTTP/2 RPC response contains trailing frame data")
	}
	if frame.RequestID != request.RequestID {
		return nil, fmt.Errorf(
			"HTTP/2 RPC response request ID %d does not match request %d",
			frame.RequestID,
			request.RequestID,
		)
	}
	return frame, nil
}

func (conn *HTTP2Conn) allocateRequestID() uint64 {
	for {
		requestID := conn.nextRequestID.Add(1)
		if requestID != 0 {
			return requestID
		}
	}
}

// Pending reports HTTP/2 streams currently waiting for a response.
func (conn *HTTP2Conn) Pending() int {
	return int(conn.pending.Load())
}

// Err reports the terminal connection failure, if any.
func (conn *HTTP2Conn) Err() error {
	conn.errMu.RLock()
	defer conn.errMu.RUnlock()
	return conn.err
}

func (conn *HTTP2Conn) setError(err error) {
	if err == nil {
		return
	}
	conn.errMu.Lock()
	if conn.err == nil {
		conn.err = err
	}
	conn.errMu.Unlock()
}

// Close terminates the HTTP/2 session and the underlying transport.
func (conn *HTTP2Conn) Close() error {
	conn.closeOnce.Do(func() {
		conn.closeErr = errors.Join(conn.client.Close(), conn.netConn.Close())
		conn.setError(ErrConnClosed)
	})
	return conn.closeErr
}

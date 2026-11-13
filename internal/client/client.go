package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/internal/protocol"
	"github.com/zjsycwmrbig/mini-rpc/internal/transport"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
	"google.golang.org/protobuf/proto"
)

var _ rpc.ClientConn = (*Client)(nil)

// Client is a single-connection Mini-RPC client.
//
// Connection pooling and instance selection are introduced in later stages.
type Client struct {
	conn *transport.Conn
}

// Dial opens a TCP connection and starts response processing.
func Dial(ctx context.Context, address string) (*Client, error) {
	if ctx == nil {
		return nil, rpc.NewError(rpc.CodeInvalidArgument, "nil context")
	}
	if address == "" {
		return nil, rpc.NewError(rpc.CodeInvalidArgument, "empty address")
	}

	dialer := net.Dialer{Timeout: 10 * time.Second}
	netConn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	return New(netConn), nil
}

// New wraps an established network connection.
func New(netConn net.Conn) *Client {
	return &Client{conn: transport.NewConn(netConn, protocol.DefaultCodec)}
}

// Invoke sends one unary RPC and decodes its response into out.
func (client *Client) Invoke(
	ctx context.Context,
	service string,
	method string,
	in proto.Message,
	out proto.Message,
) error {
	if ctx == nil {
		return rpc.NewError(rpc.CodeInvalidArgument, "nil context")
	}
	if service == "" || method == "" {
		return rpc.NewError(rpc.CodeInvalidArgument, "service and method are required")
	}
	if in == nil || out == nil {
		return rpc.NewError(rpc.CodeInvalidArgument, "request and response messages are required")
	}

	body, err := proto.Marshal(in)
	if err != nil {
		return rpc.NewError(rpc.CodeInvalidArgument, fmt.Sprintf("marshal request: %v", err))
	}

	header := map[string]string{
		protocol.MetadataKeyService:     service,
		protocol.MetadataKeyMethod:      method,
		protocol.MetadataKeyContentType: rpc.ContentTypeProtobuf,
	}
	if deadline, ok := ctx.Deadline(); ok {
		header[protocol.MetadataKeyDeadlineUnixNano] = strconv.FormatInt(deadline.UnixNano(), 10)
	}

	response, err := client.conn.Call(ctx, protocol.Frame{
		Type:   protocol.FrameTypeRequest,
		Header: header,
		Body:   body,
	})
	if err != nil {
		return normalizeContextError(err)
	}
	if response.Type != protocol.FrameTypeResponse {
		return rpc.NewError(
			rpc.CodeProtocolError,
			fmt.Sprintf("unexpected response frame type %s", response.Type),
		)
	}
	if response.Flags&protocol.FlagError != 0 {
		code := rpc.CodeInternal
		if value, ok := response.Header[protocol.MetadataKeyErrorCode]; ok {
			parsed, valid := rpc.ParseErrorCode(value)
			if valid {
				code = parsed
			}
		}
		return rpc.NewError(code, string(response.Body))
	}
	if err := proto.Unmarshal(response.Body, out); err != nil {
		return rpc.NewError(rpc.CodeProtocolError, fmt.Sprintf("unmarshal response: %v", err))
	}
	return nil
}

func normalizeContextError(err error) error {
	if errors.Is(err, context.Canceled) {
		return rpc.NewError(rpc.CodeCanceled, "request canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return rpc.NewError(rpc.CodeDeadlineExceeded, "request deadline exceeded")
	}
	return err
}

// Close closes the underlying connection and wakes pending calls.
func (client *Client) Close() error {
	return client.conn.Close()
}

// Pending returns the number of requests awaiting responses.
func (client *Client) Pending() int {
	return client.conn.Pending()
}

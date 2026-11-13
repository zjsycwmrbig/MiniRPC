package rpc

import (
	"context"

	"google.golang.org/protobuf/proto"
)

// ClientConn is the stable client transport consumed by generated code.
//
// Implementations route a unary request to the named service and method and
// decode the response into out. Close releases connections and background
// resources owned by the client connection.
type ClientConn interface {
	Invoke(ctx context.Context, service string, method string, in proto.Message, out proto.Message) error
	Close() error
}

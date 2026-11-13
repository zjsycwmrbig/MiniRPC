package rpc

import (
	"context"

	"google.golang.org/protobuf/proto"
)

// MethodHandler adapts a generated typed unary handler to the server
// dispatcher.
type MethodHandler func(implementation any, ctx context.Context, request proto.Message) (proto.Message, error)

// MethodDesc describes one unary RPC method exposed by a service.
type MethodDesc struct {
	MethodName string
	Handler    MethodHandler
	NewRequest func() proto.Message
}

// ServiceDesc is the generated service metadata consumed by the RPC server.
//
// Metadata may be used by later middleware and observability layers without
// changing the generated method tables.
type ServiceDesc struct {
	ServiceName string
	HandlerType any
	Methods     []MethodDesc
	Metadata    any
}

// ServiceRegistrar is implemented by servers that accept generated service
// descriptions.
type ServiceRegistrar interface {
	RegisterService(desc *ServiceDesc, implementation any) error
}

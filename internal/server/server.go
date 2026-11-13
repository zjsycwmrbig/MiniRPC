package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/internal/protocol"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
	"google.golang.org/protobuf/proto"
)

type registeredService struct {
	description    *rpc.ServiceDesc
	implementation any
	methods        map[string]*rpc.MethodDesc
}

type connection struct {
	netConn net.Conn
	writeMu sync.Mutex
}

// Server dispatches generated unary services over framed TCP connections.
type Server struct {
	mu             sync.Mutex
	services       map[string]registeredService
	listener       net.Listener
	connections    map[*connection]struct{}
	serving        bool
	closed         bool
	draining       bool
	workers        sync.WaitGroup
	activeRequests int
	requestsDone   chan struct{}
	stats          Stats
}

// Stats is a point-in-time snapshot of server lifecycle activity.
type Stats struct {
	AcceptedConnections uint64
	ActiveConnections   int
	ActiveRequests      int
	CompletedRequests   uint64
	RejectedRequests    uint64
	CloseReason         string
}

// New creates an empty server.
func New() *Server {
	requestsDone := make(chan struct{})
	close(requestsDone)
	return &Server{
		services:     make(map[string]registeredService),
		connections:  make(map[*connection]struct{}),
		requestsDone: requestsDone,
	}
}

// RegisterService validates and stores a generated service descriptor.
func (server *Server) RegisterService(desc *rpc.ServiceDesc, implementation any) error {
	registered, err := validateService(desc, implementation)
	if err != nil {
		return err
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if server.closed {
		return ErrServerClosed
	}
	if _, exists := server.services[desc.ServiceName]; exists {
		return fmt.Errorf("%w: %s", ErrServiceRegistered, desc.ServiceName)
	}
	server.services[desc.ServiceName] = registered
	return nil
}

func validateService(desc *rpc.ServiceDesc, implementation any) (registeredService, error) {
	if desc == nil || desc.ServiceName == "" || implementation == nil {
		return registeredService{}, ErrInvalidService
	}

	handlerType := reflect.TypeOf(desc.HandlerType)
	if handlerType == nil || handlerType.Kind() != reflect.Pointer || handlerType.Elem().Kind() != reflect.Interface {
		return registeredService{}, fmt.Errorf("%w: %s handler type", ErrInvalidService, desc.ServiceName)
	}
	if !reflect.TypeOf(implementation).Implements(handlerType.Elem()) {
		return registeredService{}, fmt.Errorf(
			"%w: %T does not implement %s",
			ErrInvalidImplementation,
			implementation,
			handlerType.Elem(),
		)
	}

	methods := make(map[string]*rpc.MethodDesc, len(desc.Methods))
	for index := range desc.Methods {
		methodValue := desc.Methods[index]
		method := &methodValue
		if method.MethodName == "" || method.Handler == nil || method.NewRequest == nil {
			return registeredService{}, fmt.Errorf(
				"%w: %s method %d",
				ErrInvalidService,
				desc.ServiceName,
				index,
			)
		}
		if _, exists := methods[method.MethodName]; exists {
			return registeredService{}, fmt.Errorf(
				"%w: duplicate method %s.%s",
				ErrInvalidService,
				desc.ServiceName,
				method.MethodName,
			)
		}
		methods[method.MethodName] = method
	}

	return registeredService{
		description:    desc,
		implementation: implementation,
		methods:        methods,
	}, nil
}

// Serve accepts connections until the listener or server closes.
func (server *Server) Serve(listener net.Listener) error {
	if listener == nil {
		return ErrInvalidListener
	}

	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		return ErrServerClosed
	}
	if server.serving {
		server.mu.Unlock()
		return ErrAlreadyServing
	}
	server.listener = listener
	server.serving = true
	server.mu.Unlock()

	for {
		netConn, err := listener.Accept()
		if err != nil {
			server.mu.Lock()
			closed := server.closed
			server.mu.Unlock()
			if closed {
				return ErrServerClosed
			}
			return err
		}

		state := &connection{netConn: netConn}
		server.mu.Lock()
		if server.closed || server.draining {
			server.mu.Unlock()
			_ = netConn.Close()
			return ErrServerClosed
		}
		server.connections[state] = struct{}{}
		server.stats.AcceptedConnections++
		server.mu.Unlock()

		server.workers.Add(1)
		go server.serveConnection(state)
	}
}

func (server *Server) serveConnection(state *connection) {
	defer server.workers.Done()
	defer func() {
		_ = state.netConn.Close()
		server.mu.Lock()
		delete(server.connections, state)
		server.mu.Unlock()
	}()

	for {
		request, err := protocol.DefaultCodec.Read(state.netConn)
		if err != nil {
			return
		}
		if request.Type != protocol.FrameTypeRequest {
			continue
		}

		server.mu.Lock()
		if server.draining || server.closed {
			server.stats.RejectedRequests++
			server.mu.Unlock()
			_ = state.write(rpcErrorFrame(
				request.RequestID,
				rpc.CodeUnavailable,
				"server is shutting down",
			))
			continue
		}
		if server.activeRequests == 0 {
			server.requestsDone = make(chan struct{})
		}
		server.activeRequests++
		server.mu.Unlock()

		server.workers.Add(1)
		go func() {
			defer server.workers.Done()
			response := server.dispatch(request)
			if err := state.write(response); err != nil {
				_ = state.netConn.Close()
			}
			server.finishRequest()
		}()
	}
}

func (server *Server) finishRequest() {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.activeRequests--
	server.stats.CompletedRequests++
	if server.activeRequests == 0 {
		close(server.requestsDone)
	}
}

func (state *connection) write(frame *protocol.Frame) error {
	state.writeMu.Lock()
	defer state.writeMu.Unlock()
	return protocol.DefaultCodec.Write(state.netConn, *frame)
}

func (server *Server) dispatch(request *protocol.Frame) (response *protocol.Frame) {
	defer func() {
		if recovered := recover(); recovered != nil {
			response = rpcErrorFrame(request.RequestID, rpc.CodeInternal, "internal server error")
		}
	}()

	if request.Flags != 0 {
		return rpcErrorFrame(
			request.RequestID,
			rpc.CodeProtocolError,
			"unsupported request flags",
		)
	}
	if contentType := request.Header[protocol.MetadataKeyContentType]; contentType != "" &&
		contentType != rpc.ContentTypeProtobuf {
		return rpcErrorFrame(
			request.RequestID,
			rpc.CodeInvalidArgument,
			"unsupported content type",
		)
	}

	serviceName := request.Header[protocol.MetadataKeyService]
	methodName := request.Header[protocol.MetadataKeyMethod]
	server.mu.Lock()
	service, exists := server.services[serviceName]
	server.mu.Unlock()
	if !exists {
		return rpcErrorFrame(request.RequestID, rpc.CodeNotFound, "service not found")
	}
	method := service.methods[methodName]
	if method == nil {
		return rpcErrorFrame(request.RequestID, rpc.CodeNotFound, "method not found")
	}

	ctx, cancel, err := requestContext(request.Header)
	if err != nil {
		return rpcErrorFrame(request.RequestID, rpc.CodeInvalidArgument, err.Error())
	}
	defer cancel()

	input := method.NewRequest()
	if err := proto.Unmarshal(request.Body, input); err != nil {
		return rpcErrorFrame(
			request.RequestID,
			rpc.CodeInvalidArgument,
			fmt.Sprintf("decode request: %v", err),
		)
	}

	output, err := method.Handler(service.implementation, ctx, input)
	if err != nil {
		return errorFrame(request.RequestID, err)
	}
	if output == nil {
		return rpcErrorFrame(request.RequestID, rpc.CodeInternal, "handler returned nil response")
	}
	body, err := proto.Marshal(output)
	if err != nil {
		return rpcErrorFrame(
			request.RequestID,
			rpc.CodeInternal,
			fmt.Sprintf("encode response: %v", err),
		)
	}
	return &protocol.Frame{
		Type:      protocol.FrameTypeResponse,
		RequestID: request.RequestID,
		Header: map[string]string{
			protocol.MetadataKeyContentType: rpc.ContentTypeProtobuf,
		},
		Body: body,
	}
}

func requestContext(header map[string]string) (context.Context, context.CancelFunc, error) {
	ctx := context.Background()
	value := header[protocol.MetadataKeyDeadlineUnixNano]
	if value == "" {
		return ctx, func() {}, nil
	}

	nanoseconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || nanoseconds <= 0 {
		return nil, nil, fmt.Errorf("invalid deadline")
	}
	ctx, cancel := context.WithDeadline(ctx, time.Unix(0, nanoseconds))
	return ctx, cancel, nil
}

func errorFrame(requestID uint64, err error) *protocol.Frame {
	var rpcError *rpc.Error
	if errors.As(err, &rpcError) {
		return rpcErrorFrame(requestID, rpcError.Code, rpcError.Message)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return rpcErrorFrame(requestID, rpc.CodeDeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled):
		return rpcErrorFrame(requestID, rpc.CodeCanceled, err.Error())
	default:
		return rpcErrorFrame(requestID, rpc.CodeInternal, err.Error())
	}
}

func rpcErrorFrame(requestID uint64, code rpc.ErrorCode, message string) *protocol.Frame {
	return &protocol.Frame{
		Type:      protocol.FrameTypeResponse,
		Flags:     protocol.FlagError,
		RequestID: requestID,
		Header: map[string]string{
			protocol.MetadataKeyContentType: rpc.ContentTypeProtobuf,
			protocol.MetadataKeyErrorCode:   rpc.FormatErrorCode(code),
		},
		Body: []byte(message),
	}
}

// Close stops accepting and closes all active TCP connections.
func (server *Server) Close() error {
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		return nil
	}
	server.closed = true
	server.draining = true
	server.stats.CloseReason = "immediate"
	listener := server.listener
	connections := make([]*connection, 0, len(server.connections))
	for state := range server.connections {
		connections = append(connections, state)
	}
	server.mu.Unlock()

	var closeErrors []error
	if listener != nil {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErrors = append(closeErrors, err)
		}
	}
	for _, state := range connections {
		if err := state.netConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErrors = append(closeErrors, err)
		}
	}
	return errors.Join(closeErrors...)
}

// GracefulStop stops accepting new requests, waits for active calls, and then
// closes the active connections. The returned error is context.Err when the
// shutdown deadline expires.
func (server *Server) GracefulStop(ctx context.Context) error {
	if ctx == nil {
		return rpc.NewError(rpc.CodeInvalidArgument, "nil shutdown context")
	}

	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		return nil
	}
	server.closed = true
	server.draining = true
	server.stats.CloseReason = "graceful"
	listener := server.listener
	requestsDone := server.requestsDone
	server.mu.Unlock()

	var closeErrors []error
	if listener != nil {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErrors = append(closeErrors, err)
		}
	}

	select {
	case <-requestsDone:
	case <-ctx.Done():
		server.markCloseReason("graceful_timeout")
		closeErrors = append(closeErrors, server.closeConnections())
		return errors.Join(append(closeErrors, ctx.Err())...)
	}

	closeErrors = append(closeErrors, server.closeConnections())
	workersDone := make(chan struct{})
	go func() {
		server.workers.Wait()
		close(workersDone)
	}()
	select {
	case <-workersDone:
	case <-ctx.Done():
		server.markCloseReason("graceful_timeout")
		return errors.Join(append(closeErrors, ctx.Err())...)
	}
	return errors.Join(closeErrors...)
}

func (server *Server) markCloseReason(reason string) {
	server.mu.Lock()
	server.stats.CloseReason = reason
	server.mu.Unlock()
}

func (server *Server) closeConnections() error {
	server.mu.Lock()
	connections := make([]*connection, 0, len(server.connections))
	for state := range server.connections {
		connections = append(connections, state)
	}
	server.mu.Unlock()

	var closeErrors []error
	for _, state := range connections {
		if err := state.netConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErrors = append(closeErrors, err)
		}
	}
	return errors.Join(closeErrors...)
}

// Stats returns a consistent lifecycle snapshot.
func (server *Server) Stats() Stats {
	server.mu.Lock()
	defer server.mu.Unlock()
	snapshot := server.stats
	snapshot.ActiveConnections = len(server.connections)
	snapshot.ActiveRequests = server.activeRequests
	return snapshot
}

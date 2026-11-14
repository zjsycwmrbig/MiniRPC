package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/internal/governance"
	"github.com/zjsycwmrbig/mini-rpc/internal/observability"
	"github.com/zjsycwmrbig/mini-rpc/internal/protocol"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/proto"
)

const (
	defaultMaxConnections       = 1024
	defaultMaxConcurrentRequest = 1024
	defaultTLSHandshakeTimeout  = 10 * time.Second
)

// Config controls server resource admission and local rate limits.
type Config struct {
	MaxConnections        int
	MaxConcurrentRequests int
	MaxRequestBodyBytes   int
	RateLimits            governance.RateLimitConfig
	Observer              *observability.Runtime
	TLSConfig             *tls.Config
	TLSConfigSource       func() *tls.Config
	TLSHandshakeTimeout   time.Duration
}

// DefaultConfig returns conservative resource-protection defaults.
func DefaultConfig() Config {
	return Config{
		MaxConnections:        defaultMaxConnections,
		MaxConcurrentRequests: defaultMaxConcurrentRequest,
		MaxRequestBodyBytes:   int(protocol.DefaultMaxBodySize),
		TLSHandshakeTimeout:   defaultTLSHandshakeTimeout,
	}
}

// Option mutates server configuration before it is validated.
type Option func(*Config)

// WithMaxConnections caps simultaneously accepted transport connections.
func WithMaxConnections(value int) Option {
	return func(config *Config) {
		config.MaxConnections = value
	}
}

// WithMaxConcurrentRequests caps handlers executing at one time.
func WithMaxConcurrentRequests(value int) Option {
	return func(config *Config) {
		config.MaxConcurrentRequests = value
	}
}

// WithMaxRequestBodyBytes rejects larger decoded Protobuf request bodies.
func WithMaxRequestBodyBytes(value int) Option {
	return func(config *Config) {
		config.MaxRequestBodyBytes = value
	}
}

// WithRateLimits configures server-side service and method token buckets.
func WithRateLimits(limits governance.RateLimitConfig) Option {
	return func(config *Config) {
		config.RateLimits = limits
	}
}

// WithObserver enables structured logs, trace continuation, and metrics.
func WithObserver(observer *observability.Runtime) Option {
	return func(config *Config) {
		config.Observer = observer
	}
}

// WithTLSConfig wraps accepted RPC connections in TLS. Configure ClientAuth
// and ClientCAs on the supplied config to require mutual TLS.
func WithTLSConfig(config *tls.Config) Option {
	return func(serverConfig *Config) {
		serverConfig.TLSConfigSource = nil
		if config == nil {
			serverConfig.TLSConfig = nil
			return
		}
		serverConfig.TLSConfig = config.Clone()
	}
}

// WithTLSConfigSource selects the current server TLS configuration for each
// new handshake. Existing connections continue with the configuration used to
// establish them. The source must be concurrency-safe and return a non-nil
// configuration; callers commonly provide security.Reloader.Current.
func WithTLSConfigSource(source func() *tls.Config) Option {
	return func(serverConfig *Config) {
		serverConfig.TLSConfig = nil
		serverConfig.TLSConfigSource = source
	}
}

// WithTLSHandshakeTimeout bounds the time a peer may spend completing TLS.
func WithTLSHandshakeTimeout(timeout time.Duration) Option {
	return func(config *Config) {
		config.TLSHandshakeTimeout = timeout
	}
}

type registeredService struct {
	description    *rpc.ServiceDesc
	implementation any
	methods        map[string]*rpc.MethodDesc
}

type connection struct {
	netConn          net.Conn
	gracefulShutdown func(context.Context) error
	writeMu          sync.Mutex

	cancelMu sync.Mutex
	requests map[uint64]*requestState
}

type requestState struct {
	cancel    context.CancelFunc
	cancelled bool
}

// Server dispatches generated unary services over framed TCP connections.
type Server struct {
	mu             sync.Mutex
	config         Config
	services       map[string]registeredService
	limiter        *governance.RateLimiter
	observer       *observability.Runtime
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
	AcceptedConnections  uint64
	ActiveConnections    int
	ActiveRequests       int
	CompletedRequests    uint64
	RejectedRequests     uint64
	ConnectionRejected   uint64
	ConcurrencyRejected  uint64
	BodyRejected         uint64
	CancelFrames         uint64
	RateLimitAllowed     uint64
	RateLimitRejected    uint64
	RateLimitWaits       uint64
	RateLimits           []governance.RateLimitStats
	TLSHandshakeFailures uint64
	CloseReason          string
}

// New creates an empty server.
func New() *Server {
	return newServer(DefaultConfig())
}

// NewWithOptions validates resource limits and creates an empty server.
func NewWithOptions(options ...Option) (*Server, error) {
	config := DefaultConfig()
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return newServer(config), nil
}

func validateConfig(config Config) error {
	if config.MaxConnections <= 0 {
		return fmt.Errorf("%w: max connections must be positive", ErrInvalidConfig)
	}
	if config.MaxConcurrentRequests <= 0 {
		return fmt.Errorf("%w: max concurrent requests must be positive", ErrInvalidConfig)
	}
	if config.MaxRequestBodyBytes <= 0 {
		return fmt.Errorf("%w: max request body must be positive", ErrInvalidConfig)
	}
	if config.TLSHandshakeTimeout <= 0 {
		return fmt.Errorf("%w: TLS handshake timeout must be positive", ErrInvalidConfig)
	}
	if config.TLSConfigSource != nil {
		if _, err := validateServerTLSConfig(config.TLSConfigSource()); err != nil {
			return err
		}
	} else if config.TLSConfig != nil {
		if _, err := validateServerTLSConfig(config.TLSConfig); err != nil {
			return err
		}
	}
	if err := config.RateLimits.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	return nil
}

func validateServerTLSConfig(config *tls.Config) (*tls.Config, error) {
	if config == nil {
		return nil, fmt.Errorf("%w: TLS configuration source returned nil", ErrInvalidConfig)
	}
	cloned := config.Clone()
	if cloned.MinVersion == 0 {
		cloned.MinVersion = tls.VersionTLS12
	}
	if cloned.MinVersion < tls.VersionTLS12 {
		return nil, fmt.Errorf("%w: minimum TLS version must be 1.2 or newer", ErrInvalidConfig)
	}
	if len(cloned.Certificates) == 0 &&
		cloned.GetCertificate == nil &&
		cloned.GetConfigForClient == nil {
		return nil, fmt.Errorf("%w: TLS certificate source is required", ErrInvalidConfig)
	}
	return cloned, nil
}

func dynamicServerTLSConfigForProtocol(
	source func() *tls.Config,
	http2Protocol bool,
) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			config, err := validateServerTLSConfig(source())
			if err != nil {
				return nil, err
			}
			if http2Protocol {
				enableHTTP2ALPN(config)
			}
			return config, nil
		},
	}
}

func enableHTTP2ALPN(config *tls.Config) {
	for _, protocol := range config.NextProtos {
		if protocol == http2.NextProtoTLS {
			return
		}
	}
	config.NextProtos = append(config.NextProtos, http2.NextProtoTLS)
}

func newServer(config Config) *Server {
	requestsDone := make(chan struct{})
	close(requestsDone)
	return &Server{
		config:       config,
		services:     make(map[string]registeredService),
		limiter:      governance.NewRateLimiter(config.RateLimits),
		observer:     config.Observer,
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

// Serve accepts framed TCP connections until the listener or server closes.
func (server *Server) Serve(listener net.Listener) error {
	return server.serveListener(listener, false, server.serveConnection)
}

// ServeHTTP2 accepts HTTP/2 connections until the listener or server closes.
// Without TLS it uses HTTP/2 prior knowledge (h2c); with configured TLS it
// advertises h2 through ALPN. Each unary call occupies one HTTP/2 stream.
func (server *Server) ServeHTTP2(listener net.Listener) error {
	return server.serveListener(listener, true, server.serveHTTP2Connection)
}

func (server *Server) serveListener(
	listener net.Listener,
	http2Protocol bool,
	serveConnection func(*connection),
) error {
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
	acceptListener := listener
	if server.config.TLSConfigSource != nil {
		acceptListener = tls.NewListener(
			listener,
			dynamicServerTLSConfigForProtocol(server.config.TLSConfigSource, http2Protocol),
		)
	} else if server.config.TLSConfig != nil {
		tlsConfig := server.config.TLSConfig.Clone()
		if http2Protocol {
			enableHTTP2ALPN(tlsConfig)
		}
		acceptListener = tls.NewListener(listener, tlsConfig)
	}
	server.serving = true
	server.mu.Unlock()

	for {
		netConn, err := acceptListener.Accept()
		if err != nil {
			server.mu.Lock()
			closed := server.closed
			server.mu.Unlock()
			if closed {
				return ErrServerClosed
			}
			return err
		}
		server.mu.Lock()
		if !server.closed && !server.draining && len(server.connections) >= server.config.MaxConnections {
			server.stats.ConnectionRejected++
			server.mu.Unlock()
			_ = netConn.Close()
			continue
		}
		server.mu.Unlock()

		state := &connection{
			netConn:  netConn,
			requests: make(map[uint64]*requestState),
		}
		server.mu.Lock()
		if server.closed || server.draining {
			server.mu.Unlock()
			_ = netConn.Close()
			return ErrServerClosed
		}
		server.connections[state] = struct{}{}
		server.stats.AcceptedConnections++
		server.observeConnectionsLocked()
		server.mu.Unlock()

		server.workers.Add(1)
		go serveConnection(state)
	}
}

func (server *Server) serveConnection(state *connection) {
	defer server.workers.Done()
	defer func() {
		state.cancelAll()
		_ = state.netConn.Close()
		server.mu.Lock()
		delete(server.connections, state)
		server.observeConnectionsLocked()
		server.mu.Unlock()
	}()
	if !server.completeTLSHandshake(state) {
		return
	}

	for {
		request, err := protocol.DefaultCodec.Read(state.netConn)
		if err != nil {
			return
		}
		if request.Type == protocol.FrameTypeCancel {
			server.mu.Lock()
			server.stats.CancelFrames++
			server.mu.Unlock()
			state.cancelRequest(request.RequestID)
			continue
		}
		if request.Type != protocol.FrameTypeRequest {
			continue
		}

		server.mu.Lock()
		if server.draining || server.closed {
			server.stats.RejectedRequests++
			server.mu.Unlock()
			server.recordImmediateRejection(
				request,
				rpc.CodeUnavailable,
				"server is shutting down",
			)
			_ = state.write(rpcErrorFrame(
				request.RequestID,
				rpc.CodeUnavailable,
				"server is shutting down",
			))
			continue
		}
		if server.activeRequests >= server.config.MaxConcurrentRequests {
			server.stats.ConcurrencyRejected++
			server.mu.Unlock()
			server.recordImmediateRejection(
				request,
				rpc.CodeResourceExhausted,
				"server concurrency limit exceeded",
			)
			_ = state.write(rpcErrorFrame(
				request.RequestID,
				rpc.CodeResourceExhausted,
				"server concurrency limit exceeded",
			))
			continue
		}
		if server.activeRequests == 0 {
			server.requestsDone = make(chan struct{})
		}
		server.activeRequests++
		server.mu.Unlock()
		state.prepareRequest(request.RequestID)

		server.workers.Add(1)
		go func() {
			defer server.workers.Done()
			defer state.releaseRequest(request.RequestID)
			response := server.dispatch(state, request)
			if err := state.write(response); err != nil {
				_ = state.netConn.Close()
			}
			server.finishRequest()
		}()
	}
}

func (server *Server) completeTLSHandshake(state *connection) bool {
	tlsConn, ok := state.netConn.(*tls.Conn)
	if !ok {
		return true
	}
	handshakeCtx, cancel := context.WithTimeout(context.Background(), server.config.TLSHandshakeTimeout)
	err := tlsConn.HandshakeContext(handshakeCtx)
	cancel()
	if err == nil {
		return true
	}
	server.mu.Lock()
	server.stats.TLSHandshakeFailures++
	server.mu.Unlock()
	server.observer.Metrics().RecordTLSHandshakeFailure("server")
	return false
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

func (state *connection) prepareRequest(requestID uint64) {
	state.cancelMu.Lock()
	state.requests[requestID] = &requestState{}
	state.cancelMu.Unlock()
}

func (state *connection) bindCancel(requestID uint64, cancel context.CancelFunc) {
	state.cancelMu.Lock()
	request := state.requests[requestID]
	if request == nil {
		state.cancelMu.Unlock()
		cancel()
		return
	}
	request.cancel = cancel
	cancelled := request.cancelled
	state.cancelMu.Unlock()
	if cancelled {
		cancel()
	}
}

func (state *connection) cancelRequest(requestID uint64) {
	state.cancelMu.Lock()
	request := state.requests[requestID]
	if request != nil {
		request.cancelled = true
		cancel := request.cancel
		state.cancelMu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	state.cancelMu.Unlock()
}

func (state *connection) releaseRequest(requestID uint64) {
	state.cancelMu.Lock()
	delete(state.requests, requestID)
	state.cancelMu.Unlock()
}

func (state *connection) cancelAll() {
	state.cancelMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(state.requests))
	for _, request := range state.requests {
		if request.cancel != nil {
			cancels = append(cancels, request.cancel)
		}
	}
	state.cancelMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (server *Server) dispatch(
	state *connection,
	request *protocol.Frame,
) (response *protocol.Frame) {
	return server.dispatchWithContext(context.Background(), state, request)
}

func (server *Server) dispatchWithContext(
	parent context.Context,
	state *connection,
	request *protocol.Frame,
) (response *protocol.Frame) {
	serviceName := request.Header[protocol.MetadataKeyService]
	methodName := request.Header[protocol.MetadataKeyMethod]
	server.mu.Lock()
	service, exists := server.services[serviceName]
	server.mu.Unlock()
	observedService := serviceName
	observedMethod := methodName
	if !exists {
		observedService = "unknown"
		observedMethod = "unknown"
	} else if service.methods[methodName] == nil {
		observedMethod = "unknown"
	}
	ctx, span := server.observer.StartServerSpan(
		parent,
		request.Header,
		observedService,
		observedMethod,
	)
	defer func() {
		if recovered := recover(); recovered != nil {
			response = rpcErrorFrame(request.RequestID, rpc.CodeInternal, "internal server error")
		}
		span.Finish(frameError(response))
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

	if !exists {
		return rpcErrorFrame(request.RequestID, rpc.CodeNotFound, "service not found")
	}
	method := service.methods[methodName]
	if method == nil {
		return rpcErrorFrame(request.RequestID, rpc.CodeNotFound, "method not found")
	}

	ctx, cancel, err := requestContext(ctx, request.Header)
	if err != nil {
		return rpcErrorFrame(request.RequestID, rpc.CodeInvalidArgument, err.Error())
	}
	defer cancel()
	state.bindCancel(request.RequestID, cancel)
	if len(request.Body) > server.config.MaxRequestBodyBytes {
		server.recordBodyRejected()
		return rpcErrorFrame(
			request.RequestID,
			rpc.CodeResourceExhausted,
			"request body exceeds server limit",
		)
	}
	if err := server.limiter.Acquire(ctx, serviceName, methodName); err != nil {
		if errors.Is(err, governance.ErrRateLimited) {
			return rpcErrorFrame(
				request.RequestID,
				rpc.CodeResourceExhausted,
				"server rate limit exceeded",
			)
		}
		return errorFrame(request.RequestID, err)
	}

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

func (server *Server) recordImmediateRejection(
	request *protocol.Frame,
	code rpc.ErrorCode,
	message string,
) {
	_, span := server.observer.StartServerSpan(
		context.Background(),
		request.Header,
		request.Header[protocol.MetadataKeyService],
		request.Header[protocol.MetadataKeyMethod],
	)
	span.Finish(rpc.NewError(code, message))
}

func (server *Server) recordBodyRejected() {
	server.mu.Lock()
	server.stats.BodyRejected++
	server.mu.Unlock()
}

func requestContext(parent context.Context, header map[string]string) (context.Context, context.CancelFunc, error) {
	if parent == nil {
		parent = context.Background()
	}
	value := header[protocol.MetadataKeyDeadlineUnixNano]
	if value == "" {
		return parent, func() {}, nil
	}

	nanoseconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || nanoseconds <= 0 {
		return nil, nil, fmt.Errorf("invalid deadline")
	}
	ctx, cancel := context.WithDeadline(parent, time.Unix(0, nanoseconds))
	return ctx, cancel, nil
}

func frameError(frame *protocol.Frame) error {
	if frame == nil || frame.Flags&protocol.FlagError == 0 {
		return nil
	}
	code := rpc.CodeInternal
	if value := frame.Header[protocol.MetadataKeyErrorCode]; value != "" {
		if parsed, ok := rpc.ParseErrorCode(value); ok {
			code = parsed
		}
	}
	return rpc.NewError(code, string(frame.Body))
}

func (server *Server) observeConnectionsLocked() {
	if server.observer != nil {
		server.observer.Metrics().SetConnections("server", len(server.connections))
	}
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

	// Half-close first so responses written by drained requests reach clients as
	// a normal TCP stream followed by FIN. Clients then close their side and the
	// connection workers can exit without racing an immediate full close.
	closeErrors = append(closeErrors, server.gracefullyHalfCloseConnections(ctx))
	workersDone := make(chan struct{})
	go func() {
		server.workers.Wait()
		close(workersDone)
	}()
	select {
	case <-workersDone:
	case <-ctx.Done():
		server.markCloseReason("graceful_timeout")
		closeErrors = append(closeErrors, server.closeConnections(), ctx.Err())
		return errors.Join(closeErrors...)
	}
	closeErrors = append(closeErrors, server.closeConnections())
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

func (server *Server) gracefullyHalfCloseConnections(ctx context.Context) error {
	server.mu.Lock()
	connections := make([]*connection, 0, len(server.connections))
	for state := range server.connections {
		connections = append(connections, state)
	}
	server.mu.Unlock()

	var closeErrors []error
	for _, state := range connections {
		if state.gracefulShutdown != nil {
			if err := state.gracefulShutdown(ctx); err != nil {
				closeErrors = append(closeErrors, err)
			}
			continue
		}
		halfCloser, ok := state.netConn.(interface{ CloseWrite() error })
		if !ok {
			continue
		}
		if err := halfCloser.CloseWrite(); err != nil && !errors.Is(err, net.ErrClosed) {
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
	snapshot.RateLimits = server.limiter.Stats()
	for _, limitStats := range snapshot.RateLimits {
		snapshot.RateLimitAllowed += limitStats.Allowed
		snapshot.RateLimitRejected += limitStats.Rejected
		snapshot.RateLimitWaits += limitStats.Waited
	}
	return snapshot
}

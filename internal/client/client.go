package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/internal/governance"
	"github.com/zjsycwmrbig/mini-rpc/internal/observability"
	"github.com/zjsycwmrbig/mini-rpc/internal/protocol"
	"github.com/zjsycwmrbig/mini-rpc/internal/transport"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
	"google.golang.org/protobuf/proto"
)

const (
	defaultMaxConnections       = 4
	defaultMaxIdleConnections   = 2
	defaultMaxConcurrentCalls   = transport.DefaultMaxConcurrentCalls
	defaultIdleTimeout          = 5 * time.Minute
	defaultDialTimeout          = 10 * time.Second
	defaultCallTimeout          = 10 * time.Second
	minIdleReapInterval         = 10 * time.Millisecond
	closeReasonClientClosed     = "client_closed"
	closeReasonTransportFailure = "transport_failure"
)

var _ rpc.ClientConn = (*Client)(nil)

// localRateLimitError keeps the public RPC status while preserving the local
// limiter identity so an outer discovery retry layer does not fail over a call
// that never reached the network.
type localRateLimitError struct {
	rpcError *rpc.Error
}

func (err *localRateLimitError) Error() string {
	return err.rpcError.Error()
}

func (err *localRateLimitError) Unwrap() []error {
	return []error{err.rpcError, governance.ErrRateLimited}
}

// Config controls the per-address connection pool.
type Config struct {
	MaxConnections     int
	MaxIdleConnections int
	MaxConcurrentCalls int
	IdleTimeout        time.Duration
	DialTimeout        time.Duration
	CallTimeout        time.Duration
	Retry              governance.RetryPolicy
	CircuitBreaker     governance.CircuitBreakerConfig
	RateLimits         governance.RateLimitConfig
	Observer           *observability.Runtime
	TLSConfig          *tls.Config
}

// DefaultConfig returns the stage 4 pool defaults.
func DefaultConfig() Config {
	return Config{
		MaxConnections:     defaultMaxConnections,
		MaxIdleConnections: defaultMaxIdleConnections,
		MaxConcurrentCalls: defaultMaxConcurrentCalls,
		IdleTimeout:        defaultIdleTimeout,
		DialTimeout:        defaultDialTimeout,
		CallTimeout:        defaultCallTimeout,
		Retry:              governance.DefaultRetryPolicy(),
		CircuitBreaker:     governance.DefaultCircuitBreakerConfig(),
	}
}

// Stats is a point-in-time snapshot of client pool activity.
type Stats struct {
	Connections       int
	IdleConnections   int
	ActiveRequests    int
	MaxConnections    int
	ConnectionsDialed uint64
	ConnectionsClosed uint64
	DialFailures      uint64
	PoolWaits         uint64
	PoolWaitFailures  uint64
	RetryAttempts     uint64
	RetriedCalls      uint64
	CircuitState      string
	CircuitRejected   uint64
	CircuitOpens      uint64
	RateLimitAllowed  uint64
	RateLimitRejected uint64
	RateLimitWaits    uint64
	RateLimits        []governance.RateLimitStats
	LastCloseReason   string
}

type pooledConn struct {
	conn     *transport.Conn
	active   int
	lastUsed time.Time
}

// Client reuses a bounded pool of multiplexed TCP connections per address.
type Client struct {
	address  string
	config   Config
	dialer   func(context.Context, string) (net.Conn, error)
	retry    governance.RetryPolicy
	breaker  *governance.CircuitBreaker
	limiter  *governance.RateLimiter
	observer *observability.Runtime

	mu      sync.Mutex
	conns   []*pooledConn
	dialing int
	closed  bool
	notify  chan struct{}
	stats   Stats

	reaperStop chan struct{}
	reaperDone chan struct{}
	closeOnce  sync.Once
	closeErr   error
}

// Option mutates pool configuration before it is validated.
type Option func(*Config)

// WithMaxConnections sets the maximum number of live connections per pool.
func WithMaxConnections(value int) Option {
	return func(config *Config) {
		config.MaxConnections = value
	}
}

// WithMaxIdleConnections sets the target number of idle connections.
func WithMaxIdleConnections(value int) Option {
	return func(config *Config) {
		config.MaxIdleConnections = value
	}
}

// WithMaxConcurrentCalls sets the request limit on each connection.
func WithMaxConcurrentCalls(value int) Option {
	return func(config *Config) {
		config.MaxConcurrentCalls = value
	}
}

// WithIdleTimeout sets the age after which idle connections are reclaimed.
func WithIdleTimeout(value time.Duration) Option {
	return func(config *Config) {
		config.IdleTimeout = value
	}
}

// WithDialTimeout sets the timeout used for reconnect attempts.
func WithDialTimeout(value time.Duration) Option {
	return func(config *Config) {
		config.DialTimeout = value
	}
}

// WithCallTimeout sets the fallback deadline for calls without one.
func WithCallTimeout(value time.Duration) Option {
	return func(config *Config) {
		config.CallTimeout = value
	}
}

// WithRetryPolicy sets retry limits and backoff behavior for idempotent calls.
func WithRetryPolicy(policy governance.RetryPolicy) Option {
	return func(config *Config) {
		config.Retry = policy
	}
}

// WithCircuitBreaker sets per-address circuit-breaker thresholds.
func WithCircuitBreaker(breaker governance.CircuitBreakerConfig) Option {
	return func(config *Config) {
		config.CircuitBreaker = breaker
	}
}

// WithRateLimits sets client-side service and method token buckets. A method
// rule overrides the service rule for that RPC.
func WithRateLimits(limits governance.RateLimitConfig) Option {
	return func(config *Config) {
		config.RateLimits = limits
	}
}

// WithObserver enables structured logs, trace propagation, and metrics.
func WithObserver(observer *observability.Runtime) Option {
	return func(config *Config) {
		config.Observer = observer
	}
}

// WithTLSConfig enables TLS for every dialed connection. The configuration is
// cloned before storage so callers can safely reuse their base configuration.
func WithTLSConfig(config *tls.Config) Option {
	return func(clientConfig *Config) {
		if config == nil {
			clientConfig.TLSConfig = nil
			return
		}
		clientConfig.TLSConfig = config.Clone()
	}
}

// Dial opens the first pooled connection and validates the address.
func Dial(ctx context.Context, address string, options ...Option) (*Client, error) {
	if ctx == nil {
		return nil, rpc.NewError(rpc.CodeInvalidArgument, "nil context")
	}
	if address == "" {
		return nil, rpc.NewError(rpc.CodeInvalidArgument, "empty address")
	}

	config := DefaultConfig()
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	if err := validateConfig(&config); err != nil {
		return nil, err
	}

	dialer := net.Dialer{Timeout: config.DialTimeout}
	clientConn := newClient(
		address,
		config,
		func(ctx context.Context, address string) (net.Conn, error) {
			if config.TLSConfig == nil {
				return dialer.DialContext(ctx, "tcp", address)
			}
			tlsConfig := config.TLSConfig.Clone()
			if tlsConfig.ServerName == "" {
				tlsConfig.ServerName = serverNameFromAddress(address)
			}
			tlsDialer := tls.Dialer{NetDialer: &dialer, Config: tlsConfig}
			return tlsDialer.DialContext(ctx, "tcp", address)
		},
	)

	netConn, err := clientConn.dial(ctx, address)
	if err != nil {
		_ = clientConn.Close()
		return nil, err
	}
	if err := clientConn.addInitial(netConn); err != nil {
		_ = netConn.Close()
		_ = clientConn.Close()
		return nil, err
	}
	return clientConn, nil
}

// New wraps an established network connection in a one-connection pool.
func New(netConn net.Conn) *Client {
	config := DefaultConfig()
	config.MaxConnections = 1
	config.MaxIdleConnections = 1
	config.IdleTimeout = 0
	clientConn := newClient("", config, nil)
	if err := clientConn.addInitial(netConn); err != nil {
		_ = clientConn.Close()
	}
	return clientConn
}

func newClient(
	address string,
	config Config,
	dialer func(context.Context, string) (net.Conn, error),
) *Client {
	clientConn := &Client{
		address:    address,
		config:     config,
		dialer:     dialer,
		retry:      config.Retry.Normalize(),
		breaker:    governance.NewCircuitBreaker(config.CircuitBreaker),
		limiter:    governance.NewRateLimiter(config.RateLimits),
		observer:   config.Observer,
		notify:     make(chan struct{}),
		reaperStop: make(chan struct{}),
		reaperDone: make(chan struct{}),
	}
	clientConn.stats.MaxConnections = config.MaxConnections
	if config.IdleTimeout > 0 {
		go clientConn.reapIdleLoop()
	} else {
		close(clientConn.reaperDone)
	}
	return clientConn
}

func validateConfig(config *Config) error {
	if config.MaxConnections <= 0 {
		return rpc.NewError(rpc.CodeInvalidArgument, "max connections must be positive")
	}
	if config.MaxIdleConnections < 0 {
		return rpc.NewError(rpc.CodeInvalidArgument, "max idle connections cannot be negative")
	}
	if config.MaxIdleConnections > config.MaxConnections {
		config.MaxIdleConnections = config.MaxConnections
	}
	if config.MaxConcurrentCalls <= 0 {
		return rpc.NewError(rpc.CodeInvalidArgument, "max concurrent calls must be positive")
	}
	if config.IdleTimeout < 0 {
		return rpc.NewError(rpc.CodeInvalidArgument, "idle timeout cannot be negative")
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = defaultDialTimeout
	}
	if config.CallTimeout <= 0 {
		config.CallTimeout = defaultCallTimeout
	}
	config.Retry = config.Retry.Normalize()
	config.CircuitBreaker = config.CircuitBreaker.Normalize()
	if config.TLSConfig != nil {
		config.TLSConfig = config.TLSConfig.Clone()
		if config.TLSConfig.MinVersion == 0 {
			config.TLSConfig.MinVersion = tls.VersionTLS12
		}
		if config.TLSConfig.MinVersion < tls.VersionTLS12 {
			return rpc.NewError(rpc.CodeInvalidArgument, "minimum TLS version must be 1.2 or newer")
		}
	}
	if err := config.RateLimits.Validate(); err != nil {
		return rpc.NewError(rpc.CodeInvalidArgument, err.Error())
	}
	return nil
}

func serverNameFromAddress(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ""
	}
	return host
}

func (client *Client) addInitial(netConn net.Conn) error {
	if netConn == nil {
		return rpc.NewError(rpc.CodeInvalidArgument, "nil network connection")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return ErrPoolClosed
	}
	client.conns = append(client.conns, client.wrapConn(netConn))
	client.stats.ConnectionsDialed++
	return nil
}

func (client *Client) wrapConn(netConn net.Conn) *pooledConn {
	return &pooledConn{
		conn: transport.NewConnWithOptions(
			netConn,
			protocol.DefaultCodec,
			transport.ConnOptions{MaxConcurrentCalls: client.config.MaxConcurrentCalls},
		),
		lastUsed: time.Now(),
	}
}

// Invoke sends one unary RPC through a healthy pooled connection.
func (client *Client) Invoke(
	ctx context.Context,
	service string,
	method string,
	in proto.Message,
	out proto.Message,
) (returnErr error) {
	if ctx == nil {
		return rpc.NewError(rpc.CodeInvalidArgument, "nil context")
	}
	if service == "" || method == "" {
		return rpc.NewError(rpc.CodeInvalidArgument, "service and method are required")
	}
	if in == nil || out == nil {
		return rpc.NewError(rpc.CodeInvalidArgument, "request and response messages are required")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		callCtx, cancel := context.WithTimeout(ctx, client.config.CallTimeout)
		defer cancel()
		ctx = callCtx
	}
	attempts := 0
	ctx, span := client.observer.StartClientSpan(ctx, service, method)
	defer func() {
		span.SetAttempt(attempts)
		span.Finish(returnErr)
		client.syncObservabilityState()
	}()

	body, err := proto.Marshal(in)
	if err != nil {
		return rpc.NewError(rpc.CodeInvalidArgument, fmt.Sprintf("marshal request: %v", err))
	}

	var callErr error
	attempts, callErr = client.retry.Do(ctx, rpc.IsIdempotent(ctx), func(attempt int) error {
		permit, permitErr := client.breaker.Allow()
		if permitErr != nil {
			return permitErr
		}
		if limitErr := client.limiter.Acquire(ctx, service, method); limitErr != nil {
			permit.Release()
			return limitErr
		}

		attemptErr := client.invokeAttempt(ctx, service, method, attempt, body, out)
		client.observeBreaker(ctx, permit, attemptErr)
		return attemptErr
	})
	client.recordAttempts(attempts)
	if callErr != nil {
		returnErr = normalizeContextError(callErr)
		if errors.Is(callErr, governance.ErrRateLimited) {
			var normalized *rpc.Error
			if errors.As(returnErr, &normalized) {
				returnErr = &localRateLimitError{rpcError: normalized}
			}
		}
		return returnErr
	}
	return nil
}

func (client *Client) invokeAttempt(
	ctx context.Context,
	service string,
	method string,
	attempt int,
	body []byte,
	out proto.Message,
) error {
	header := map[string]string{
		protocol.MetadataKeyService:      service,
		protocol.MetadataKeyMethod:       method,
		protocol.MetadataKeyContentType:  rpc.ContentTypeProtobuf,
		protocol.MetadataKeyRetryAttempt: strconv.Itoa(attempt - 1),
	}
	if spanContext, ok := observability.SpanContextFromContext(ctx); ok {
		header[protocol.MetadataKeyRequestID] = spanContext.RequestID
		header[protocol.MetadataKeyTraceID] = spanContext.TraceID
		header[protocol.MetadataKeySpanID] = spanContext.SpanID
	}
	if deadline, ok := ctx.Deadline(); ok {
		header[protocol.MetadataKeyDeadlineUnixNano] = strconv.FormatInt(deadline.UnixNano(), 10)
	}

	pooled, err := client.acquire(ctx)
	if err != nil {
		return normalizeContextError(err)
	}

	response, callErr := pooled.conn.Call(ctx, protocol.Frame{
		Type:   protocol.FrameTypeRequest,
		Header: header,
		Body:   body,
	})
	client.release(pooled, callErr)
	if callErr != nil {
		return normalizeContextError(callErr)
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

func (client *Client) observeBreaker(ctx context.Context, permit *governance.Permit, err error) {
	if err == nil {
		permit.RecordSuccess()
		return
	}
	if governance.IsBreakerFailure(err) {
		permit.RecordFailure()
		return
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		permit.Release()
		return
	}
	var rpcError *rpc.Error
	if errors.As(err, &rpcError) && rpcError.Code == rpc.CodeCanceled {
		permit.Release()
		return
	}
	permit.RecordSuccess()
}

func (client *Client) recordAttempts(attempts int) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.stats.RetryAttempts += uint64(attempts)
	if attempts > 1 {
		client.stats.RetriedCalls++
	}
}

func (client *Client) syncObservabilityState() {
	if client.observer == nil {
		return
	}
	stats := client.Stats()
	client.observer.Metrics().SetConnections("client", stats.Connections)
	client.observer.Metrics().SetCircuitState(stats.CircuitState)
}

func (client *Client) acquire(ctx context.Context) (*pooledConn, error) {
	for {
		client.mu.Lock()
		if client.closed {
			client.mu.Unlock()
			return nil, ErrPoolClosed
		}
		client.reapIdleLocked(time.Now())
		if pooled := client.takeAvailableLocked(); pooled != nil {
			client.mu.Unlock()
			return pooled, nil
		}
		if client.address != "" &&
			client.dialer != nil &&
			len(client.conns)+client.dialing < client.config.MaxConnections {
			client.dialing++
			client.mu.Unlock()

			netConn, err := client.dial(ctx, client.address)
			client.mu.Lock()
			client.dialing--
			if err != nil {
				client.stats.DialFailures++
				client.signalLocked()
				client.mu.Unlock()
				return nil, err
			}
			if client.closed {
				client.mu.Unlock()
				_ = netConn.Close()
				return nil, ErrPoolClosed
			}
			pooled := client.wrapConn(netConn)
			pooled.active = 1
			client.conns = append(client.conns, pooled)
			client.stats.ConnectionsDialed++
			client.stats.ActiveRequests++
			client.signalLocked()
			client.mu.Unlock()
			return pooled, nil
		}

		notify := client.notify
		client.stats.PoolWaits++
		client.mu.Unlock()

		select {
		case <-ctx.Done():
			client.mu.Lock()
			client.stats.PoolWaitFailures++
			client.mu.Unlock()
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, rpc.NewError(
					rpc.CodeCanceled,
					"request canceled while waiting for connection pool capacity",
				)
			}
			return nil, rpc.NewError(
				rpc.CodeResourceExhausted,
				"connection pool capacity wait timed out",
			)
		case <-notify:
		}
	}
}

func (client *Client) takeAvailableLocked() *pooledConn {
	for index := 0; index < len(client.conns); index++ {
		pooled := client.conns[index]
		if pooled.conn.Err() != nil {
			if pooled.active == 0 {
				client.removeConnLocked(index, closeReasonTransportFailure)
				index--
			}
			continue
		}
		if pooled.active >= client.config.MaxConcurrentCalls {
			continue
		}
		pooled.active++
		pooled.lastUsed = time.Now()
		client.stats.ActiveRequests++
		return pooled
	}
	return nil
}

func (client *Client) release(pooled *pooledConn, callErr error) {
	client.mu.Lock()
	defer client.mu.Unlock()

	if pooled.active > 0 {
		pooled.active--
	}
	if client.stats.ActiveRequests > 0 {
		client.stats.ActiveRequests--
	}
	pooled.lastUsed = time.Now()

	reason := ""
	if callErr != nil && (errors.Is(callErr, transport.ErrConnClosed) || pooled.conn.Err() != nil) {
		reason = closeReasonTransportFailure
	}
	if reason != "" && pooled.active == 0 {
		for index, candidate := range client.conns {
			if candidate == pooled {
				client.removeConnLocked(index, reason)
				break
			}
		}
	}
	client.reapIdleLocked(time.Now())
	client.signalLocked()
}

func (client *Client) dial(ctx context.Context, address string) (net.Conn, error) {
	if client.dialer == nil {
		return nil, rpc.NewError(rpc.CodeUnavailable, "client has no dialer")
	}
	dialCtx := ctx
	cancel := func() {}
	if client.config.DialTimeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, client.config.DialTimeout)
	}
	defer cancel()
	netConn, err := client.dialer(dialCtx, address)
	if err == nil {
		return netConn, nil
	}
	if errors.Is(dialCtx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w: %v", ErrDialTimeout, err)
	}
	return nil, err
}

func (client *Client) reapIdleLocked(now time.Time) {
	idle := make([]int, 0, len(client.conns))
	for index, pooled := range client.conns {
		if pooled.active == 0 {
			idle = append(idle, index)
		}
	}
	for len(idle) > client.config.MaxIdleConnections {
		index := idle[0]
		client.removeConnLocked(index, "idle_limit")
		idle = idle[1:]
		for position := range idle {
			idle[position]--
		}
	}
	if client.config.IdleTimeout <= 0 {
		return
	}
	for index := len(client.conns) - 1; index >= 0; index-- {
		pooled := client.conns[index]
		if pooled.active == 0 && now.Sub(pooled.lastUsed) >= client.config.IdleTimeout {
			client.removeConnLocked(index, "idle_timeout")
		}
	}
}

func (client *Client) removeConnLocked(index int, reason string) {
	pooled := client.conns[index]
	client.conns = append(client.conns[:index], client.conns[index+1:]...)
	_ = pooled.conn.Close()
	client.stats.ConnectionsClosed++
	client.stats.LastCloseReason = reason
	client.signalLocked()
}

func (client *Client) signalLocked() {
	close(client.notify)
	client.notify = make(chan struct{})
}

func (client *Client) reapIdleLoop() {
	defer close(client.reaperDone)
	interval := client.config.IdleTimeout / 2
	if interval < minIdleReapInterval {
		interval = minIdleReapInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			client.mu.Lock()
			client.reapIdleLocked(now)
			client.mu.Unlock()
		case <-client.reaperStop:
			return
		}
	}
}

func normalizeContextError(err error) error {
	var rpcError *rpc.Error
	if errors.As(err, &rpcError) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return rpc.NewError(rpc.CodeCanceled, "request canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return rpc.NewError(rpc.CodeDeadlineExceeded, "request deadline exceeded")
	}
	if errors.Is(err, transport.ErrWriteTimeout) {
		return rpc.NewError(rpc.CodeDeadlineExceeded, "request write timed out")
	}
	if errors.Is(err, ErrDialTimeout) {
		return rpc.NewError(rpc.CodeDeadlineExceeded, "connection establishment timed out")
	}
	if errors.Is(err, governance.ErrCircuitOpen) {
		return rpc.NewError(rpc.CodeUnavailable, "circuit breaker open")
	}
	if errors.Is(err, governance.ErrRateLimited) {
		return rpc.NewError(rpc.CodeResourceExhausted, "client rate limit exceeded")
	}
	if errors.Is(err, ErrPoolClosed) {
		return rpc.NewError(rpc.CodeUnavailable, "client is closed")
	}
	return rpc.NewError(rpc.CodeUnavailable, err.Error())
}

// Close marks the pool closed, wakes waiters, and closes all connections.
func (client *Client) Close() error {
	client.closeOnce.Do(func() {
		client.mu.Lock()
		client.closed = true
		for range client.conns {
			client.stats.ConnectionsClosed++
		}
		client.stats.LastCloseReason = closeReasonClientClosed
		client.signalLocked()
		conns := client.conns
		client.conns = nil
		client.mu.Unlock()

		close(client.reaperStop)
		for _, pooled := range conns {
			if err := pooled.conn.Close(); err != nil {
				client.closeErr = errors.Join(client.closeErr, err)
			}
		}
		<-client.reaperDone
		client.observer.Metrics().SetConnections("client", 0)
	})
	return client.closeErr
}

// Pending returns requests awaiting responses across the pool.
func (client *Client) Pending() int {
	client.mu.Lock()
	defer client.mu.Unlock()
	pending := 0
	for _, pooled := range client.conns {
		pending += pooled.conn.Pending()
	}
	return pending
}

// Stats returns a consistent pool snapshot.
func (client *Client) Stats() Stats {
	client.mu.Lock()
	snapshot := client.stats
	snapshot.Connections = len(client.conns)
	snapshot.ActiveRequests = 0
	snapshot.IdleConnections = 0
	for _, pooled := range client.conns {
		snapshot.ActiveRequests += pooled.active
		if pooled.active == 0 {
			snapshot.IdleConnections++
		}
	}
	client.mu.Unlock()

	breakerStats := client.breaker.Stats()
	snapshot.CircuitState = string(breakerStats.State)
	snapshot.CircuitRejected = breakerStats.Rejected
	snapshot.CircuitOpens = breakerStats.Opens
	snapshot.RateLimits = client.limiter.Stats()
	for _, limitStats := range snapshot.RateLimits {
		snapshot.RateLimitAllowed += limitStats.Allowed
		snapshot.RateLimitRejected += limitStats.Rejected
		snapshot.RateLimitWaits += limitStats.Waited
	}
	return snapshot
}

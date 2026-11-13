package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

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

// Config controls the per-address connection pool.
type Config struct {
	MaxConnections     int
	MaxIdleConnections int
	MaxConcurrentCalls int
	IdleTimeout        time.Duration
	DialTimeout        time.Duration
	CallTimeout        time.Duration
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
	LastCloseReason   string
}

type pooledConn struct {
	conn     *transport.Conn
	active   int
	lastUsed time.Time
}

// Client reuses a bounded pool of multiplexed TCP connections per address.
type Client struct {
	address string
	config  Config
	dialer  func(context.Context, string) (net.Conn, error)

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
			return dialer.DialContext(ctx, "tcp", address)
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
	return nil
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
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		callCtx, cancel := context.WithTimeout(ctx, client.config.CallTimeout)
		defer cancel()
		ctx = callCtx
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
			return nil, rpc.NewError(
				rpc.CodeResourceExhausted,
				"connection pool capacity exhausted",
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
	return client.dialer(dialCtx, address)
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
	if errors.Is(err, ErrPoolClosed) {
		return rpc.NewError(rpc.CodeUnavailable, "client is closed")
	}
	return err
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
	defer client.mu.Unlock()
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
	return snapshot
}

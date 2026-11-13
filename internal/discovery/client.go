// Package discovery routes idempotent RPC calls over changing registry
// snapshots and transparently retries transient failures on another instance.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/internal/balancer"
	clientpkg "github.com/zjsycwmrbig/mini-rpc/internal/client"
	"github.com/zjsycwmrbig/mini-rpc/internal/governance"
	"github.com/zjsycwmrbig/mini-rpc/internal/observability"
	"github.com/zjsycwmrbig/mini-rpc/internal/registry"
	"github.com/zjsycwmrbig/mini-rpc/internal/resolver"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
	"google.golang.org/protobuf/proto"
)

const defaultCallTimeout = 10 * time.Second

var _ rpc.ClientConn = (*Client)(nil)

// Config controls discovery, instance selection, and cross-instance retries.
type Config struct {
	Service       string
	Resolver      *resolver.Resolver
	Balancer      balancer.Balancer
	Retry         governance.RetryPolicy
	CallTimeout   time.Duration
	ClientOptions []clientpkg.Option
	Observer      *observability.Runtime
}

// Stats is a point-in-time discovery client snapshot.
type Stats struct {
	Instances      int
	SnapshotError  error
	CachedClients  int
	Connections    int
	Requests       uint64
	Failures       uint64
	RetryAttempts  uint64
	RetriedCalls   uint64
	WatchUpdates   uint64
	LastInstanceID string
}

type instanceClient struct {
	ready  chan struct{}
	client *clientpkg.Client
	err    error
}

// Client resolves one service, picks an instance per attempt, and reuses a
// bounded per-address pool. Safe cross-instance failover is limited to calls
// explicitly marked with rpc.WithIdempotent.
type Client struct {
	service       string
	resolver      *resolver.Resolver
	balancer      balancer.Balancer
	retry         governance.RetryPolicy
	callTimeout   time.Duration
	clientOptions []clientpkg.Option
	observer      *observability.Runtime
	cancelWatch   context.CancelFunc

	mu      sync.Mutex
	clients map[string]*instanceClient
	stats   Stats
	closed  bool

	closeOnce sync.Once
	closeErr  error
}

// New creates a discovery-backed client and starts consuming watch snapshots.
// The resolver and its registry remain owned by the caller.
func New(ctx context.Context, config Config) (*Client, error) {
	if ctx == nil {
		return nil, rpc.NewError(rpc.CodeInvalidArgument, "nil context")
	}
	if config.Service == "" || config.Resolver == nil {
		return nil, rpc.NewError(rpc.CodeInvalidArgument, "service and resolver are required")
	}
	if config.Balancer == nil {
		config.Balancer = balancer.NewRoundRobin()
	}
	config.Retry = config.Retry.Normalize()
	if config.CallTimeout <= 0 {
		config.CallTimeout = defaultCallTimeout
	}

	snapshot, err := config.Resolver.Resolve(ctx, config.Service)
	if err != nil {
		return nil, rpc.NewError(rpc.CodeUnavailable, fmt.Sprintf("resolve service: %v", err))
	}
	if len(snapshot.Instances) == 0 {
		return nil, rpc.NewError(rpc.CodeUnavailable, "resolve service: no instances")
	}

	watchContext, cancelWatch := context.WithCancel(context.Background())
	updates, err := config.Resolver.Watch(watchContext, config.Service)
	if err != nil {
		cancelWatch()
		return nil, rpc.NewError(rpc.CodeUnavailable, fmt.Sprintf("watch service: %v", err))
	}
	discoveryClient := &Client{
		service:       config.Service,
		resolver:      config.Resolver,
		balancer:      config.Balancer,
		retry:         config.Retry,
		callTimeout:   config.CallTimeout,
		clientOptions: append([]clientpkg.Option(nil), config.ClientOptions...),
		observer:      config.Observer,
		cancelWatch:   cancelWatch,
		clients:       make(map[string]*instanceClient),
	}
	discoveryClient.stats.Instances = len(snapshot.Instances)
	go discoveryClient.consumeUpdates(updates)
	discoveryClient.syncObservability()
	return discoveryClient, nil
}

// Invoke selects the next available instance for each bounded attempt. A
// non-idempotent call still receives one safe attempt through this client.
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
	if service != client.service {
		return rpc.NewError(rpc.CodeInvalidArgument, fmt.Sprintf("client is bound to service %s", client.service))
	}
	if method == "" || in == nil || out == nil {
		return rpc.NewError(rpc.CodeInvalidArgument, "method, request, and response are required")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		callContext, cancel := context.WithTimeout(ctx, client.callTimeout)
		defer cancel()
		ctx = callContext
	}

	attempts := 0
	ctx, span := client.observer.StartClientSpan(ctx, service, method)
	defer func() {
		span.SetAttempt(attempts)
		span.Finish(returnErr)
		client.recordCall(attempts, returnErr)
		client.syncObservability()
	}()

	lastAddress := ""
	var callErr error
	attempts, callErr = client.retry.Do(ctx, rpc.IsIdempotent(ctx), func(_ int) error {
		snapshot, ok := client.resolver.Snapshot(client.service)
		if !ok || len(snapshot.Instances) == 0 {
			return rpc.NewError(rpc.CodeUnavailable, "no service instances available")
		}
		instance, pickErr := client.pick(snapshot.Instances, lastAddress)
		if pickErr != nil {
			return rpc.NewError(rpc.CodeUnavailable, pickErr.Error())
		}
		lastAddress = instance.Address
		client.mu.Lock()
		client.stats.LastInstanceID = instance.ID
		client.mu.Unlock()

		instanceClient, instanceErr := client.clientFor(ctx, instance.Address)
		if instanceErr != nil {
			return normalizeError(instanceErr)
		}
		return normalizeError(instanceClient.Invoke(ctx, service, method, in, out))
	})
	if callErr != nil {
		returnErr = normalizeError(callErr)
		return returnErr
	}
	return nil
}

// Snapshot returns the resolver's latest local service view.
func (client *Client) Snapshot() (resolver.Snapshot, bool) {
	return client.resolver.Snapshot(client.service)
}

// Stats returns aggregate discovery and per-address connection information.
func (client *Client) Stats() Stats {
	client.mu.Lock()
	snapshot := client.stats
	snapshot.CachedClients = len(client.clients)
	clients := make([]*clientpkg.Client, 0, len(client.clients))
	for _, instance := range client.clients {
		if instance.client != nil {
			clients = append(clients, instance.client)
		}
	}
	client.mu.Unlock()
	for _, instanceClient := range clients {
		snapshot.Connections += instanceClient.Stats().Connections
	}
	if serviceSnapshot, ok := client.resolver.Snapshot(client.service); ok {
		snapshot.Instances = len(serviceSnapshot.Instances)
		snapshot.SnapshotError = serviceSnapshot.Err
	}
	return snapshot
}

// Close stops snapshot consumption and closes all per-instance pools.
func (client *Client) Close() error {
	client.closeOnce.Do(func() {
		client.cancelWatch()
		client.mu.Lock()
		client.closed = true
		clients := make([]*clientpkg.Client, 0, len(client.clients))
		for _, instance := range client.clients {
			if instance.client != nil {
				clients = append(clients, instance.client)
			}
		}
		client.clients = make(map[string]*instanceClient)
		client.mu.Unlock()
		for _, instanceClient := range clients {
			client.closeErr = errors.Join(client.closeErr, instanceClient.Close())
		}
		client.observer.Metrics().SetConnections("client", 0)
	})
	return client.closeErr
}

func (client *Client) pick(instances []registry.Instance, lastAddress string) (registry.Instance, error) {
	var fallback registry.Instance
	for range len(instances) {
		instance, err := client.balancer.Pick(instances)
		if err != nil {
			return registry.Instance{}, err
		}
		fallback = instance
		if lastAddress == "" || instance.Address != lastAddress {
			return instance, nil
		}
	}
	if fallback.Address == "" {
		return registry.Instance{}, balancer.ErrNoInstances
	}
	return fallback, nil
}

func (client *Client) clientFor(ctx context.Context, address string) (*clientpkg.Client, error) {
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return nil, rpc.NewError(rpc.CodeUnavailable, "discovery client is closed")
	}
	entry := client.clients[address]
	creator := false
	if entry == nil {
		entry = &instanceClient{ready: make(chan struct{})}
		client.clients[address] = entry
		creator = true
	}
	client.mu.Unlock()

	if creator {
		options := append([]clientpkg.Option(nil), client.clientOptions...)
		options = append(
			options,
			clientpkg.WithObserver(nil),
			clientpkg.WithRetryPolicy(singleAttemptPolicy()),
		)
		instanceClient, err := clientpkg.Dial(ctx, address, options...)
		client.mu.Lock()
		entry.client = instanceClient
		entry.err = err
		if err != nil {
			delete(client.clients, address)
		}
		close(entry.ready)
		client.mu.Unlock()
	}

	select {
	case <-entry.ready:
		if entry.err != nil {
			return nil, entry.err
		}
		return entry.client, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (client *Client) consumeUpdates(updates <-chan resolver.Snapshot) {
	for snapshot := range updates {
		client.mu.Lock()
		client.stats.WatchUpdates++
		client.stats.Instances = len(snapshot.Instances)
		client.stats.SnapshotError = snapshot.Err
		client.mu.Unlock()
		if snapshot.Err == nil {
			client.prune(snapshot.Instances)
		}
		client.syncObservability()
	}
}

func (client *Client) prune(instances []registry.Instance) {
	active := make(map[string]struct{}, len(instances))
	for _, instance := range instances {
		active[instance.Address] = struct{}{}
	}

	client.mu.Lock()
	removed := make([]*clientpkg.Client, 0)
	for address, instance := range client.clients {
		if _, ok := active[address]; ok {
			continue
		}
		delete(client.clients, address)
		if instance.client != nil {
			removed = append(removed, instance.client)
		}
	}
	client.mu.Unlock()
	for _, instanceClient := range removed {
		_ = instanceClient.Close()
	}
}

func (client *Client) recordCall(attempts int, err error) {
	client.mu.Lock()
	client.stats.Requests++
	client.stats.RetryAttempts += uint64(attempts)
	if attempts > 1 {
		client.stats.RetriedCalls++
	}
	if err != nil {
		client.stats.Failures++
	}
	client.mu.Unlock()
}

func (client *Client) syncObservability() {
	if !client.observer.Enabled() {
		return
	}
	stats := client.Stats()
	client.observer.Metrics().SetConnections("client", stats.Connections)
	client.observer.Metrics().SetRegistryInstances(client.service, stats.Instances)
	client.observer.Metrics().SetCircuitState(client.aggregateCircuitState())
}

func (client *Client) aggregateCircuitState() string {
	client.mu.Lock()
	clients := make([]*clientpkg.Client, 0, len(client.clients))
	for _, instance := range client.clients {
		if instance.client != nil {
			clients = append(clients, instance.client)
		}
	}
	client.mu.Unlock()
	state := string(governance.CircuitClosed)
	for _, instanceClient := range clients {
		current := instanceClient.Stats().CircuitState
		if current == string(governance.CircuitOpen) {
			return current
		}
		if current == string(governance.CircuitHalfOpen) {
			state = current
		}
	}
	return state
}

func normalizeError(err error) error {
	if err == nil {
		return nil
	}
	var rpcError *rpc.Error
	if errors.As(err, &rpcError) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return rpc.NewError(rpc.CodeDeadlineExceeded, "discovery call deadline exceeded")
	}
	if errors.Is(err, context.Canceled) {
		return rpc.NewError(rpc.CodeCanceled, "discovery call canceled")
	}
	return rpc.NewError(rpc.CodeUnavailable, err.Error())
}

func singleAttemptPolicy() governance.RetryPolicy {
	return governance.RetryPolicy{
		MaxAttempts:    1,
		InitialBackoff: time.Nanosecond,
		MaxBackoff:     time.Nanosecond,
		MaxElapsed:     time.Nanosecond,
		Jitter:         func(time.Duration) time.Duration { return 0 },
	}
}

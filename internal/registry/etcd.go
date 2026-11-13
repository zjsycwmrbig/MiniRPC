package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

const defaultPrefix = "/mini-rpc/registry"

// EtcdRegistry stores instance records as leased etcd keys.
type EtcdRegistry struct {
	client *clientv3.Client
	prefix string

	mu     sync.Mutex
	closed bool
	leases map[*etcdLease]struct{}
}

// NewEtcd creates an etcd registry. The caller owns the client lifecycle.
func NewEtcd(client *clientv3.Client, prefix string) *EtcdRegistry {
	if prefix == "" {
		prefix = defaultPrefix
	}
	return &EtcdRegistry{
		client: client,
		prefix: prefix,
		leases: make(map[*etcdLease]struct{}),
	}
}

// DialEtcd creates an etcd registry from endpoint addresses.
func DialEtcd(ctx context.Context, endpoints []string, dialTimeout time.Duration) (*EtcdRegistry, error) {
	if ctx == nil {
		return nil, errors.New("registry: nil context")
	}
	if len(endpoints) == 0 {
		return nil, errors.New("registry: no etcd endpoints")
	}
	client, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: dialTimeout,
	})
	if err != nil {
		return nil, err
	}
	return NewEtcd(client, defaultPrefix), nil
}

// Register writes an instance under a lease and starts automatic KeepAlive.
func (registry *EtcdRegistry) Register(
	ctx context.Context,
	instance Instance,
	ttl time.Duration,
) (Lease, error) {
	if err := validateInstance(instance); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, ErrInvalidTTL
	}

	registry.mu.Lock()
	if registry.closed {
		registry.mu.Unlock()
		return nil, errors.New("registry: closed")
	}
	registry.mu.Unlock()

	seconds := int64(math.Ceil(ttl.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	grant, err := registry.client.Grant(ctx, seconds)
	if err != nil {
		return nil, fmt.Errorf("registry: grant lease: %w", err)
	}

	value, err := json.Marshal(instance)
	if err != nil {
		_, _ = registry.client.Revoke(context.Background(), grant.ID)
		return nil, fmt.Errorf("registry: encode instance: %w", err)
	}
	key := registry.key(instance.Service, instance.ID)
	if _, err := registry.client.Put(ctx, key, string(value), clientv3.WithLease(grant.ID)); err != nil {
		_, _ = registry.client.Revoke(context.Background(), grant.ID)
		return nil, fmt.Errorf("registry: put instance: %w", err)
	}

	keepCtx, cancel := context.WithCancel(context.Background())
	lease := &etcdLease{
		registry: registry,
		id:       grant.ID,
		key:      key,
		done:     make(chan struct{}),
		cancel:   cancel,
	}
	registry.mu.Lock()
	if registry.closed {
		registry.mu.Unlock()
		cancel()
		_, _ = registry.client.Revoke(context.Background(), grant.ID)
		return nil, errors.New("registry: closed")
	}
	registry.leases[lease] = struct{}{}
	registry.mu.Unlock()

	responses, err := registry.client.KeepAlive(keepCtx, grant.ID)
	if err != nil {
		lease.finish()
		_, _ = registry.client.Revoke(context.Background(), grant.ID)
		return nil, fmt.Errorf("registry: start keepalive: %w", err)
	}
	go lease.keepAlive(keepCtx, responses)
	return lease, nil
}

// Deregister removes an instance registration.
func (registry *EtcdRegistry) Deregister(ctx context.Context, instance Instance) error {
	if err := validateInstance(instance); err != nil {
		return err
	}
	_, err := registry.client.Delete(ctx, registry.key(instance.Service, instance.ID))
	return err
}

// List returns all instances for one service.
func (registry *EtcdRegistry) List(ctx context.Context, service string) ([]Instance, error) {
	if service == "" {
		return nil, fmt.Errorf("%w: missing service", ErrInvalidInstance)
	}
	response, err := registry.client.Get(
		ctx,
		registry.servicePrefix(service),
		clientv3.WithPrefix(),
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend),
	)
	if err != nil {
		return nil, err
	}
	instances := make([]Instance, 0, len(response.Kvs))
	for _, pair := range response.Kvs {
		var instance Instance
		if err := json.Unmarshal(pair.Value, &instance); err != nil {
			return nil, fmt.Errorf("registry: decode %q: %w", pair.Key, err)
		}
		instances = append(instances, instance)
	}
	return instances, nil
}

// Watch streams instance changes. A transport failure is emitted as Event.Err
// before the channel closes so callers can refresh and reconnect.
func (registry *EtcdRegistry) Watch(ctx context.Context, service string) (<-chan Event, error) {
	if service == "" {
		return nil, fmt.Errorf("%w: missing service", ErrInvalidInstance)
	}
	watch := registry.client.Watch(
		ctx,
		registry.servicePrefix(service),
		clientv3.WithPrefix(),
		clientv3.WithPrevKV(),
	)
	events := make(chan Event, 16)
	go func() {
		defer close(events)
		for response := range watch {
			if response.Err() != nil {
				events <- Event{Err: response.Err()}
				return
			}
			for _, event := range response.Events {
				instance, err := instanceFromEvent(event)
				if err != nil {
					events <- Event{Err: err}
					return
				}
				eventType := EventPut
				if event.Type == clientv3.EventTypeDelete {
					eventType = EventDelete
				}
				select {
				case events <- Event{Type: eventType, Instance: instance}:
				case <-ctx.Done():
					return
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case events <- Event{Err: errors.New("registry: watch closed")}:
		case <-ctx.Done():
		}
	}()
	return events, nil
}

func instanceFromEvent(event *clientv3.Event) (Instance, error) {
	raw := event.Kv.Value
	if event.Type == clientv3.EventTypeDelete && event.PrevKv != nil {
		raw = event.PrevKv.Value
	}
	var instance Instance
	if err := json.Unmarshal(raw, &instance); err != nil {
		return Instance{}, fmt.Errorf("registry: decode watch event: %w", err)
	}
	return instance, nil
}

func (registry *EtcdRegistry) key(service, id string) string {
	return registry.servicePrefix(service) + "/" + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func (registry *EtcdRegistry) servicePrefix(service string) string {
	return registry.prefix + "/" + base64.RawURLEncoding.EncodeToString([]byte(service))
}

// Close stops leases and closes the etcd client.
func (registry *EtcdRegistry) Close() error {
	registry.mu.Lock()
	if registry.closed {
		registry.mu.Unlock()
		return nil
	}
	registry.closed = true
	leases := make([]*etcdLease, 0, len(registry.leases))
	for lease := range registry.leases {
		leases = append(leases, lease)
	}
	registry.mu.Unlock()

	for _, lease := range leases {
		lease.cancelKeepAlive()
	}
	return registry.client.Close()
}

type etcdLease struct {
	registry *EtcdRegistry
	id       clientv3.LeaseID
	key      string
	done     chan struct{}
	closeOne sync.Once
	cancel   context.CancelFunc
}

func (lease *etcdLease) Done() <-chan struct{} {
	return lease.done
}

func (lease *etcdLease) Revoke(ctx context.Context) error {
	_, err := lease.registry.client.Revoke(ctx, lease.id)
	lease.finish()
	return err
}

func (lease *etcdLease) keepAlive(ctx context.Context, responses <-chan *clientv3.LeaseKeepAliveResponse) {
	defer lease.finish()
	for {
		select {
		case response, ok := <-responses:
			if !ok || response == nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (lease *etcdLease) finish() {
	lease.closeOne.Do(func() {
		lease.cancel()
		close(lease.done)
		lease.registry.mu.Lock()
		delete(lease.registry.leases, lease)
		lease.registry.mu.Unlock()
	})
}

func (lease *etcdLease) cancelKeepAlive() {
	lease.cancel()
}

package testkit

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/internal/registry"
)

var errMemoryRegistryClosed = errors.New("testkit: memory registry closed")

// MemoryRegistry is a controllable registry.Registry implementation for
// recovery scenarios. It does not expire leases automatically.
type MemoryRegistry struct {
	mu sync.Mutex

	instances map[string]registry.Instance
	watchers  map[string]map[*memoryWatch]struct{}
	listErr   error
	watchErr  error
	closed    bool
	listCalls int
	watchCall int
}

// NewMemoryRegistry creates an empty in-process registry.
func NewMemoryRegistry() *MemoryRegistry {
	return &MemoryRegistry{
		instances: make(map[string]registry.Instance),
		watchers:  make(map[string]map[*memoryWatch]struct{}),
	}
}

// Register stores or replaces an instance and broadcasts EventPut.
func (memory *MemoryRegistry) Register(
	_ context.Context,
	instance registry.Instance,
	ttl time.Duration,
) (registry.Lease, error) {
	if ttl <= 0 {
		return nil, registry.ErrInvalidTTL
	}
	if err := validateMemoryInstance(instance); err != nil {
		return nil, err
	}
	memory.mu.Lock()
	if err := memory.availableLocked(); err != nil {
		memory.mu.Unlock()
		return nil, err
	}
	memory.instances[instanceKey(instance.Service, instance.ID)] = instance
	watchers := memory.watchersLocked(instance.Service)
	memory.mu.Unlock()
	broadcast(watchers, registry.Event{Type: registry.EventPut, Instance: instance})
	lease := &memoryLease{registry: memory, instance: instance, done: make(chan struct{})}
	return lease, nil
}

// Deregister removes an instance and broadcasts EventDelete.
func (memory *MemoryRegistry) Deregister(_ context.Context, instance registry.Instance) error {
	if err := validateMemoryInstance(instance); err != nil {
		return err
	}
	memory.mu.Lock()
	if err := memory.availableLocked(); err != nil {
		memory.mu.Unlock()
		return err
	}
	delete(memory.instances, instanceKey(instance.Service, instance.ID))
	watchers := memory.watchersLocked(instance.Service)
	memory.mu.Unlock()
	broadcast(watchers, registry.Event{Type: registry.EventDelete, Instance: instance})
	return nil
}

// List returns a stable, ID-sorted service snapshot or the configured outage.
func (memory *MemoryRegistry) List(_ context.Context, service string) ([]registry.Instance, error) {
	if service == "" {
		return nil, registry.ErrInvalidInstance
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	memory.listCalls++
	if err := memory.availableLocked(); err != nil {
		return nil, err
	}
	instances := make([]registry.Instance, 0)
	for _, instance := range memory.instances {
		if instance.Service == service {
			instances = append(instances, instance)
		}
	}
	sort.Slice(instances, func(left, right int) bool {
		return instances[left].ID < instances[right].ID
	})
	return instances, nil
}

// Watch registers a buffered event stream or returns the configured outage.
func (memory *MemoryRegistry) Watch(_ context.Context, service string) (<-chan registry.Event, error) {
	if service == "" {
		return nil, registry.ErrInvalidInstance
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	memory.watchCall++
	if err := memory.availableLocked(); err != nil {
		return nil, err
	}
	watch := &memoryWatch{events: make(chan registry.Event, 64)}
	if memory.watchers[service] == nil {
		memory.watchers[service] = make(map[*memoryWatch]struct{})
	}
	memory.watchers[service][watch] = struct{}{}
	return watch.events, nil
}

// SetUnavailable simulates a temporary registry control-plane outage.
func (memory *MemoryRegistry) SetUnavailable(err error) {
	memory.mu.Lock()
	memory.listErr = err
	memory.watchErr = err
	memory.mu.Unlock()
}

// DisconnectWatches emits one error per active watcher and closes its stream.
// Resolver must then reconnect and perform a full List.
func (memory *MemoryRegistry) DisconnectWatches(service string, err error) {
	memory.mu.Lock()
	watchers := memory.watchersLocked(service)
	delete(memory.watchers, service)
	memory.mu.Unlock()
	for watch := range watchers {
		watch.closeWithError(err)
	}
}

// Instances returns a diagnostic snapshot even while the registry is marked
// unavailable. It is intended for fault scenario verification.
func (memory *MemoryRegistry) Instances(service string) []registry.Instance {
	memory.mu.Lock()
	defer memory.mu.Unlock()
	instances := make([]registry.Instance, 0)
	for _, instance := range memory.instances {
		if instance.Service == service {
			instances = append(instances, instance)
		}
	}
	sort.Slice(instances, func(left, right int) bool {
		return instances[left].ID < instances[right].ID
	})
	return instances
}

// Calls returns diagnostic List and Watch invocation counts.
func (memory *MemoryRegistry) Calls() (list int, watch int) {
	memory.mu.Lock()
	defer memory.mu.Unlock()
	return memory.listCalls, memory.watchCall
}

// Close closes all active watcher streams.
func (memory *MemoryRegistry) Close() error {
	memory.mu.Lock()
	if memory.closed {
		memory.mu.Unlock()
		return nil
	}
	memory.closed = true
	watchers := make([]*memoryWatch, 0)
	for _, serviceWatchers := range memory.watchers {
		for watch := range serviceWatchers {
			watchers = append(watchers, watch)
		}
	}
	memory.watchers = make(map[string]map[*memoryWatch]struct{})
	memory.mu.Unlock()
	for _, watch := range watchers {
		watch.close()
	}
	return nil
}

func (memory *MemoryRegistry) availableLocked() error {
	if memory.closed {
		return errMemoryRegistryClosed
	}
	if memory.listErr != nil {
		return memory.listErr
	}
	return memory.watchErr
}

func (memory *MemoryRegistry) watchersLocked(service string) map[*memoryWatch]struct{} {
	watchers := make(map[*memoryWatch]struct{}, len(memory.watchers[service]))
	for watch := range memory.watchers[service] {
		watchers[watch] = struct{}{}
	}
	return watchers
}

func broadcast(watchers map[*memoryWatch]struct{}, event registry.Event) {
	for watch := range watchers {
		watch.send(event)
	}
}

type memoryWatch struct {
	mu     sync.Mutex
	events chan registry.Event
	closed bool
}

func (watch *memoryWatch) send(event registry.Event) {
	watch.mu.Lock()
	defer watch.mu.Unlock()
	if watch.closed {
		return
	}
	select {
	case watch.events <- event:
	default:
	}
}

func (watch *memoryWatch) closeWithError(err error) {
	watch.mu.Lock()
	defer watch.mu.Unlock()
	if watch.closed {
		return
	}
	select {
	case watch.events <- registry.Event{Err: err}:
	default:
	}
	watch.closed = true
	close(watch.events)
}

func (watch *memoryWatch) close() {
	watch.mu.Lock()
	defer watch.mu.Unlock()
	if watch.closed {
		return
	}
	watch.closed = true
	close(watch.events)
}

func validateMemoryInstance(instance registry.Instance) error {
	if instance.ID == "" || instance.Service == "" || instance.Address == "" {
		return registry.ErrInvalidInstance
	}
	return nil
}

func instanceKey(service, id string) string {
	return service + "\x00" + id
}

type memoryLease struct {
	registry *MemoryRegistry
	instance registry.Instance
	done     chan struct{}
	once     sync.Once
}

func (lease *memoryLease) Done() <-chan struct{} {
	return lease.done
}

func (lease *memoryLease) Revoke(ctx context.Context) error {
	var err error
	lease.once.Do(func() {
		err = lease.registry.Deregister(ctx, lease.instance)
		close(lease.done)
	})
	return err
}

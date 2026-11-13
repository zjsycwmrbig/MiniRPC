// Package resolver maintains client-side service instance snapshots.
package resolver

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/internal/registry"
)

var (
	// ErrClosed indicates that the resolver has been closed.
	ErrClosed = errors.New("resolver: closed")
	// ErrServiceRequired indicates an empty service name.
	ErrServiceRequired = errors.New("resolver: service is required")
)

// Snapshot is a local immutable service instance view. Err reports the most
// recent refresh failure while Instances retains the last known good data.
type Snapshot struct {
	Service   string
	Instances []registry.Instance
	UpdatedAt time.Time
	Err       error
}

// Options controls Watch reconnect behavior.
type Options struct {
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// DefaultOptions returns resolver retry defaults.
func DefaultOptions() Options {
	return Options{
		InitialBackoff: 50 * time.Millisecond,
		MaxBackoff:     2 * time.Second,
	}
}

// Resolver keeps local snapshots and reconnects failed watches.
type Resolver struct {
	backend registry.Registry
	options Options

	mu        sync.RWMutex
	snapshots map[string]Snapshot
	closed    bool
}

// New creates a resolver backed by registry.
func New(backend registry.Registry, options Options) *Resolver {
	if options.InitialBackoff <= 0 {
		options.InitialBackoff = DefaultOptions().InitialBackoff
	}
	if options.MaxBackoff < options.InitialBackoff {
		options.MaxBackoff = options.InitialBackoff
	}
	return &Resolver{
		backend:   backend,
		options:   options,
		snapshots: make(map[string]Snapshot),
	}
}

// Resolve returns the cached snapshot. The first call performs one synchronous
// List; subsequent calls stay local.
func (resolver *Resolver) Resolve(ctx context.Context, service string) (Snapshot, error) {
	if service == "" {
		return Snapshot{}, ErrServiceRequired
	}
	if snapshot, ok := resolver.Snapshot(service); ok {
		return snapshot, nil
	}
	return resolver.Refresh(ctx, service)
}

// Snapshot returns the latest local view without contacting the registry.
func (resolver *Resolver) Snapshot(service string) (Snapshot, bool) {
	resolver.mu.RLock()
	defer resolver.mu.RUnlock()
	snapshot, ok := resolver.snapshots[service]
	return cloneSnapshot(snapshot), ok
}

// Refresh performs a full List and replaces the local snapshot on success.
func (resolver *Resolver) Refresh(ctx context.Context, service string) (Snapshot, error) {
	if service == "" {
		return Snapshot{}, ErrServiceRequired
	}
	resolver.mu.RLock()
	closed := resolver.closed
	previous := resolver.snapshots[service]
	resolver.mu.RUnlock()
	if closed {
		return Snapshot{}, ErrClosed
	}

	instances, err := resolver.backend.List(ctx, service)
	if err != nil {
		if previous.Service != "" {
			resolver.mu.Lock()
			previous.Err = err
			resolver.snapshots[service] = previous
			resolver.mu.Unlock()
			return cloneSnapshot(previous), err
		}
		return Snapshot{}, err
	}
	snapshot := newSnapshot(service, instances, nil)
	resolver.mu.Lock()
	resolver.snapshots[service] = cloneSnapshot(snapshot)
	resolver.mu.Unlock()
	return snapshot, nil
}

// Watch starts a resilient watch loop and immediately publishes a full
// snapshot. Closing ctx stops the loop and closes the returned channel.
func (resolver *Resolver) Watch(ctx context.Context, service string) (<-chan Snapshot, error) {
	if service == "" {
		return nil, ErrServiceRequired
	}
	if ctx == nil {
		return nil, errors.New("resolver: nil context")
	}
	resolver.mu.RLock()
	closed := resolver.closed
	resolver.mu.RUnlock()
	if closed {
		return nil, ErrClosed
	}

	updates := make(chan Snapshot, 1)
	go resolver.watchLoop(ctx, service, updates)
	return updates, nil
}

func (resolver *Resolver) watchLoop(ctx context.Context, service string, updates chan Snapshot) {
	defer close(updates)
	backoff := resolver.options.InitialBackoff

	for {
		if ctx.Err() != nil {
			return
		}

		watchCtx, cancelWatch := context.WithCancel(ctx)
		events, watchErr := resolver.backend.Watch(watchCtx, service)
		if watchErr != nil {
			cancelWatch()
			resolver.publishFailure(ctx, service, updates, watchErr)
			if !wait(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, resolver.options.MaxBackoff)
			continue
		}

		instances, listErr := resolver.backend.List(ctx, service)
		if listErr != nil {
			cancelWatch()
			resolver.publishFailure(ctx, service, updates, listErr)
			if !wait(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, resolver.options.MaxBackoff)
			continue
		} else {
			snapshot := newSnapshot(service, instances, nil)
			resolver.publish(ctx, service, updates, snapshot)
			backoff = resolver.options.InitialBackoff
		}

		reconnect := false
		for !reconnect {
			select {
			case event, ok := <-events:
				if !ok {
					if ctx.Err() == nil {
						resolver.publishFailure(
							ctx,
							service,
							updates,
							errors.New("resolver: registry watch closed"),
						)
					}
					reconnect = true
					continue
				}
				if event.Err != nil {
					resolver.publishFailure(ctx, service, updates, event.Err)
					reconnect = true
					continue
				}
				if snapshot, applied := resolver.applyEvent(service, event); applied {
					resolver.publish(ctx, service, updates, snapshot)
					backoff = resolver.options.InitialBackoff
				}
			case <-ctx.Done():
				cancelWatch()
				return
			}
		}
		cancelWatch()
		if !wait(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, resolver.options.MaxBackoff)
	}
}

func (resolver *Resolver) applyEvent(service string, event registry.Event) (Snapshot, bool) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	snapshot, ok := resolver.snapshots[service]
	if !ok {
		snapshot = newSnapshot(service, nil, nil)
	}

	instances := append([]registry.Instance(nil), snapshot.Instances...)
	switch event.Type {
	case registry.EventPut:
		found := false
		for index := range instances {
			if instances[index].ID == event.Instance.ID {
				instances[index] = event.Instance
				found = true
				break
			}
		}
		if !found {
			instances = append(instances, event.Instance)
		}
	case registry.EventDelete:
		for index := 0; index < len(instances); index++ {
			if instances[index].ID == event.Instance.ID {
				instances = append(instances[:index], instances[index+1:]...)
				break
			}
		}
	default:
		return cloneSnapshot(snapshot), false
	}
	snapshot = newSnapshot(service, instances, nil)
	resolver.snapshots[service] = cloneSnapshot(snapshot)
	return snapshot, true
}

func (resolver *Resolver) publishFailure(
	ctx context.Context,
	service string,
	updates chan Snapshot,
	err error,
) {
	resolver.mu.Lock()
	snapshot, ok := resolver.snapshots[service]
	if !ok {
		snapshot = Snapshot{Service: service, UpdatedAt: time.Now()}
	}
	snapshot.Err = err
	resolver.snapshots[service] = cloneSnapshot(snapshot)
	resolver.mu.Unlock()
	resolver.publish(ctx, service, updates, snapshot)
}

func (resolver *Resolver) publish(
	ctx context.Context,
	service string,
	updates chan Snapshot,
	snapshot Snapshot,
) {
	resolver.mu.Lock()
	resolver.snapshots[service] = cloneSnapshot(snapshot)
	resolver.mu.Unlock()
	select {
	case updates <- cloneSnapshot(snapshot):
	default:
		select {
		case <-updates:
		default:
		}
		select {
		case updates <- cloneSnapshot(snapshot):
		case <-ctx.Done():
		}
	}
}

// Close prevents new watches and marks the resolver terminal.
func (resolver *Resolver) Close() error {
	resolver.mu.Lock()
	resolver.closed = true
	resolver.mu.Unlock()
	return nil
}

func newSnapshot(
	service string,
	instances []registry.Instance,
	err error,
) Snapshot {
	cloned := append([]registry.Instance(nil), instances...)
	sort.Slice(cloned, func(left, right int) bool {
		return cloned[left].ID < cloned[right].ID
	})
	return Snapshot{
		Service:   service,
		Instances: cloned,
		UpdatedAt: time.Now(),
		Err:       err,
	}
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Instances = append([]registry.Instance(nil), snapshot.Instances...)
	for index := range snapshot.Instances {
		if snapshot.Instances[index].Metadata != nil {
			metadata := make(map[string]string, len(snapshot.Instances[index].Metadata))
			for key, value := range snapshot.Instances[index].Metadata {
				metadata[key] = value
			}
			snapshot.Instances[index].Metadata = metadata
		}
	}
	return snapshot
}

func nextBackoff(current, maximum time.Duration) time.Duration {
	next := current * 2
	if next > maximum {
		return maximum
	}
	return next
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

package resolver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/internal/registry"
)

func TestResolveCachesInitialList(t *testing.T) {
	t.Parallel()

	backend := newFakeRegistry()
	backend.instances["b"] = registry.Instance{ID: "b", Service: "svc", Address: "b:8080"}
	backend.instances["a"] = registry.Instance{ID: "a", Service: "svc", Address: "a:8080"}
	resolver := New(backend, Options{InitialBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond})

	snapshot, err := resolver.Resolve(context.Background(), "svc")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(snapshot.Instances) != 2 || snapshot.Instances[0].ID != "a" || snapshot.Instances[1].ID != "b" {
		t.Fatalf("instances = %+v", snapshot.Instances)
	}
	if _, err := resolver.Resolve(context.Background(), "svc"); err != nil {
		t.Fatalf("cached Resolve() error = %v", err)
	}
	if calls := backend.listCalls(); calls != 1 {
		t.Fatalf("List calls = %d, want 1", calls)
	}
}

func TestWatchAppliesPutAndDelete(t *testing.T) {
	t.Parallel()

	backend := newFakeRegistry()
	backend.instances["a"] = registry.Instance{ID: "a", Service: "svc", Address: "a:8080"}
	resolver := New(backend, Options{InitialBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates, err := resolver.Watch(ctx, "svc")
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	initial := receiveSnapshot(t, updates)
	if len(initial.Instances) != 1 || initial.Instances[0].ID != "a" {
		t.Fatalf("initial instances = %+v", initial.Instances)
	}
	events := backend.latestWatch(t)
	events <- registry.Event{
		Type: registry.EventPut,
		Instance: registry.Instance{
			ID:      "b",
			Service: "svc",
			Address: "b:8080",
		},
	}
	added := receiveSnapshot(t, updates)
	if len(added.Instances) != 2 || added.Instances[1].ID != "b" {
		t.Fatalf("added instances = %+v", added.Instances)
	}

	events <- registry.Event{
		Type:     registry.EventDelete,
		Instance: registry.Instance{ID: "a", Service: "svc"},
	}
	removed := receiveSnapshot(t, updates)
	if len(removed.Instances) != 1 || removed.Instances[0].ID != "b" {
		t.Fatalf("removed instances = %+v", removed.Instances)
	}
}

func TestWatchReconnectsWithFullRefresh(t *testing.T) {
	t.Parallel()

	backend := newFakeRegistry()
	backend.instances["a"] = registry.Instance{ID: "a", Service: "svc", Address: "a:8080"}
	resolver := New(backend, Options{InitialBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates, err := resolver.Watch(ctx, "svc")
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	initial := receiveSnapshot(t, updates)
	if len(initial.Instances) != 1 {
		t.Fatalf("initial instances = %+v", initial.Instances)
	}
	firstEvents := backend.latestWatch(t)

	backend.setInstance(registry.Instance{ID: "b", Service: "svc", Address: "b:8080"})
	close(firstEvents)
	receiveHealthySnapshot(t, updates, 2)
	backend.waitForWatchCount(t, 2)
	if calls := backend.listCalls(); calls < 2 {
		t.Fatalf("List calls = %d, want at least 2", calls)
	}
}

func TestRefreshFailureKeepsLastSnapshot(t *testing.T) {
	t.Parallel()

	backend := newFakeRegistry()
	backend.instances["a"] = registry.Instance{ID: "a", Service: "svc", Address: "a:8080"}
	resolver := New(backend, DefaultOptions())
	if _, err := resolver.Refresh(context.Background(), "svc"); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	backend.setListError(errors.New("etcd unavailable"))
	snapshot, err := resolver.Refresh(context.Background(), "svc")
	if err == nil {
		t.Fatal("Refresh() error = nil")
	}
	if len(snapshot.Instances) != 1 || snapshot.Err == nil {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	cached, ok := resolver.Snapshot("svc")
	if !ok || len(cached.Instances) != 1 {
		t.Fatalf("cached snapshot = %+v, ok = %v", cached, ok)
	}
}

type fakeRegistry struct {
	mu         sync.Mutex
	instances  map[string]registry.Instance
	listErr    error
	listCount  int
	watchCount int
	watches    []chan registry.Event
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{instances: make(map[string]registry.Instance)}
}

func (fake *fakeRegistry) Register(
	context.Context,
	registry.Instance,
	time.Duration,
) (registry.Lease, error) {
	return nil, errors.New("not implemented")
}

func (fake *fakeRegistry) Deregister(context.Context, registry.Instance) error {
	return errors.New("not implemented")
}

func (fake *fakeRegistry) List(_ context.Context, service string) ([]registry.Instance, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.listCount++
	if fake.listErr != nil {
		return nil, fake.listErr
	}
	instances := make([]registry.Instance, 0, len(fake.instances))
	for _, instance := range fake.instances {
		if instance.Service == service {
			instances = append(instances, instance)
		}
	}
	return instances, nil
}

func (fake *fakeRegistry) Watch(context.Context, string) (<-chan registry.Event, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.watchCount++
	events := make(chan registry.Event, 32)
	fake.watches = append(fake.watches, events)
	return events, nil
}

func (fake *fakeRegistry) Close() error {
	return nil
}

func (fake *fakeRegistry) setInstance(instance registry.Instance) {
	fake.mu.Lock()
	fake.instances[instance.ID] = instance
	fake.mu.Unlock()
}

func (fake *fakeRegistry) setListError(err error) {
	fake.mu.Lock()
	fake.listErr = err
	fake.mu.Unlock()
}

func (fake *fakeRegistry) listCalls() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.listCount
}

func (fake *fakeRegistry) latestWatch(t *testing.T) chan registry.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		if len(fake.watches) > 0 {
			events := fake.watches[len(fake.watches)-1]
			fake.mu.Unlock()
			return events
		}
		fake.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for watch")
	return nil
}

func (fake *fakeRegistry) waitForWatchCount(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		count := len(fake.watches)
		fake.mu.Unlock()
		if count >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d watches", want)
}

func receiveSnapshot(t *testing.T, updates <-chan Snapshot) Snapshot {
	t.Helper()
	select {
	case snapshot := <-updates:
		return snapshot
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for snapshot")
		return Snapshot{}
	}
}

func receiveHealthySnapshot(t *testing.T, updates <-chan Snapshot, wantInstances int) Snapshot {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case snapshot := <-updates:
			if snapshot.Err == nil && len(snapshot.Instances) == wantInstances {
				return snapshot
			}
		case <-deadline:
			t.Fatalf("timed out waiting for healthy snapshot with %d instances", wantInstances)
			return Snapshot{}
		}
	}
}

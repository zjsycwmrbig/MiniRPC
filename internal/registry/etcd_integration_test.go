//go:build integration

package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestEtcdRegistryLifecycle(t *testing.T) {
	endpoint := os.Getenv("ETCD_ENDPOINT")
	if endpoint == "" {
		endpoint = "127.0.0.1:2379"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{endpoint},
		DialTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("clientv3.New() error = %v", err)
	}
	prefix := fmt.Sprintf("/mini-rpc/test-%d", time.Now().UnixNano())
	registry := NewEtcd(client, prefix)
	defer func() {
		_, _ = client.Delete(context.Background(), prefix, clientv3.WithPrefix())
		_ = registry.Close()
	}()

	service := "rpc.echo.v1.Echo"
	first := Instance{ID: "echo-a", Service: service, Address: "127.0.0.1:8081", Weight: 1}
	second := Instance{ID: "echo-b", Service: service, Address: "127.0.0.1:8082", Weight: 2}
	firstLease, err := registry.Register(ctx, first, 2*time.Second)
	if err != nil {
		t.Fatalf("Register(first) error = %v", err)
	}
	secondLease, err := registry.Register(ctx, second, 2*time.Second)
	if err != nil {
		t.Fatalf("Register(second) error = %v", err)
	}

	instances, err := registry.List(ctx, service)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(instances) != 2 {
		t.Fatalf("instances = %+v, want 2", instances)
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	events, err := registry.Watch(watchCtx, service)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	third := Instance{ID: "echo-c", Service: service, Address: "127.0.0.1:8083", Weight: 1}
	thirdLease, err := registry.Register(ctx, third, 2*time.Second)
	if err != nil {
		t.Fatalf("Register(third) error = %v", err)
	}
	put := receiveEvent(t, events)
	if put.Err != nil || put.Type != EventPut || put.Instance.ID != third.ID {
		t.Fatalf("put event = %+v", put)
	}

	if err := secondLease.Revoke(ctx); err != nil {
		t.Fatalf("Revoke(second) error = %v", err)
	}
	deleted := receiveEvent(t, events)
	if deleted.Err != nil || deleted.Type != EventDelete || deleted.Instance.ID != second.ID {
		t.Fatalf("delete event = %+v", deleted)
	}

	time.Sleep(2500 * time.Millisecond)
	instances, err = registry.List(ctx, service)
	if err != nil {
		t.Fatalf("List() after TTL error = %v", err)
	}
	if len(instances) != 2 {
		t.Fatalf("instances after keepalive = %+v, want first and third", instances)
	}

	watchCancel()
	if err := thirdLease.Revoke(ctx); err != nil {
		t.Fatalf("Revoke(third) error = %v", err)
	}
	if err := firstLease.Revoke(ctx); err != nil {
		t.Fatalf("Revoke(first) error = %v", err)
	}
	instances, err = registry.List(ctx, service)
	if err != nil {
		t.Fatalf("List() after revoke error = %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("instances after revoke = %+v", instances)
	}
}

func receiveEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("watch channel closed")
		}
		return event
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for watch event")
		return Event{Err: errors.New("unreachable")}
	}
}

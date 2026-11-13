package discovery_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	clientpkg "github.com/zjsycwmrbig/mini-rpc/internal/client"
	"github.com/zjsycwmrbig/mini-rpc/internal/discovery"
	"github.com/zjsycwmrbig/mini-rpc/internal/governance"
	"github.com/zjsycwmrbig/mini-rpc/internal/registry"
	"github.com/zjsycwmrbig/mini-rpc/internal/resolver"
	"github.com/zjsycwmrbig/mini-rpc/internal/server"
	"github.com/zjsycwmrbig/mini-rpc/internal/testkit"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
)

func TestDiscoveryFailsOverToHealthyInstance(t *testing.T) {
	t.Parallel()

	memory := testkit.NewMemoryRegistry()
	t.Cleanup(func() { _ = memory.Close() })
	serverA := startEchoServer(t)
	serverB := startEchoServer(t)
	registerInstance(t, memory, "a", serverA)
	registerInstance(t, memory, "b", serverB)

	serviceResolver := resolver.New(memory, resolver.Options{
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
	})
	client := startDiscoveryClient(t, serviceResolver)

	serverA.stop(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx = rpc.WithIdempotent(ctx)
	response, err := echov1.NewEchoClient(client).Echo(ctx, &echov1.EchoRequest{Message: "failover"})
	if err != nil {
		t.Fatalf("Echo() after first instance failure = %v", err)
	}
	if response.GetMessage() != "failover" {
		t.Fatalf("Echo() response = %q", response.GetMessage())
	}
	stats := client.Stats()
	if stats.RetryAttempts != 2 || stats.RetriedCalls != 1 {
		t.Fatalf("discovery stats after failover = %+v", stats)
	}
	if stats.LastInstanceID != "b" {
		t.Fatalf("last instance = %q, want b", stats.LastInstanceID)
	}

	if err := memory.Deregister(context.Background(), registry.Instance{
		ID:      "a",
		Service: echov1.EchoServiceName,
		Address: serverA.address,
	}); err != nil {
		t.Fatalf("Deregister() error = %v", err)
	}
	waitForInstances(t, client, 1)
	response, err = echov1.NewEchoClient(client).Echo(
		ctx,
		&echov1.EchoRequest{Message: "after-removal"},
	)
	if err != nil || response.GetMessage() != "after-removal" {
		t.Fatalf("Echo() after removal = %q, %v", response.GetMessage(), err)
	}
}

func TestDiscoveryReturnsBoundedUnavailableWhenAllInstancesFail(t *testing.T) {
	t.Parallel()

	memory := testkit.NewMemoryRegistry()
	t.Cleanup(func() { _ = memory.Close() })
	serverA := startEchoServer(t)
	serverB := startEchoServer(t)
	registerInstance(t, memory, "a", serverA)
	registerInstance(t, memory, "b", serverB)

	serviceResolver := resolver.New(memory, resolver.Options{
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
	})
	client := startDiscoveryClient(t, serviceResolver)
	serverA.stop(t)
	serverB.stop(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	_, err := echov1.NewEchoClient(client).Echo(
		rpc.WithIdempotent(ctx),
		&echov1.EchoRequest{Message: "all-down"},
	)
	var rpcError *rpc.Error
	if !errors.As(err, &rpcError) || rpcError.Code != rpc.CodeUnavailable {
		t.Fatalf("Echo() error = %v, want UNAVAILABLE", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("all-instance failure took %v, want bounded below 1s", elapsed)
	}
	stats := client.Stats()
	if stats.RetryAttempts != 2 || stats.Failures != 1 {
		t.Fatalf("bounded failure stats = %+v", stats)
	}
}

func TestDiscoveryDoesNotFailOverNonIdempotentCall(t *testing.T) {
	t.Parallel()

	memory := testkit.NewMemoryRegistry()
	t.Cleanup(func() { _ = memory.Close() })
	serverA := startEchoServer(t)
	serverB := startEchoServer(t)
	registerInstance(t, memory, "a", serverA)
	registerInstance(t, memory, "b", serverB)
	serviceResolver := resolver.New(memory, resolver.Options{
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
	})
	client := startDiscoveryClient(t, serviceResolver)
	serverA.stop(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := echov1.NewEchoClient(client).Echo(ctx, &echov1.EchoRequest{Message: "unsafe"})
	var rpcError *rpc.Error
	if !errors.As(err, &rpcError) || rpcError.Code != rpc.CodeUnavailable {
		t.Fatalf("non-idempotent Echo() error = %v, want UNAVAILABLE", err)
	}
	if attempts := client.Stats().RetryAttempts; attempts != 1 {
		t.Fatalf("non-idempotent attempts = %d, want 1", attempts)
	}
}

func TestDiscoveryDoesNotRetryLocalRateLimit(t *testing.T) {
	t.Parallel()

	memory := testkit.NewMemoryRegistry()
	t.Cleanup(func() { _ = memory.Close() })
	serverA := startEchoServer(t)
	registerInstance(t, memory, "a", serverA)
	serviceResolver := resolver.New(memory, resolver.Options{
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := discovery.New(ctx, discovery.Config{
		Service:  echov1.EchoServiceName,
		Resolver: serviceResolver,
		Retry: governance.RetryPolicy{
			MaxAttempts:    2,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     time.Millisecond,
			MaxElapsed:     time.Second,
			Jitter:         func(time.Duration) time.Duration { return 0 },
		},
		ClientOptions: []clientpkg.Option{clientpkg.WithRateLimits(governance.RateLimitConfig{
			Services: map[string]governance.RateLimitRule{
				echov1.EchoServiceName: {Rate: 0.1, Burst: 1},
			},
		})},
	})
	if err != nil {
		t.Fatalf("discovery.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	callCtx := rpc.WithIdempotent(ctx)
	if _, err := echov1.NewEchoClient(client).Echo(callCtx, &echov1.EchoRequest{Message: "consume"}); err != nil {
		t.Fatalf("initial Echo() error = %v", err)
	}
	_, err = echov1.NewEchoClient(client).Echo(callCtx, &echov1.EchoRequest{Message: "limited"})
	var rpcError *rpc.Error
	if !errors.As(err, &rpcError) || rpcError.Code != rpc.CodeResourceExhausted {
		t.Fatalf("rate-limited Echo() error = %v, want RESOURCE_EXHAUSTED", err)
	}
	if attempts := client.Stats().RetryAttempts; attempts != 2 {
		t.Fatalf("attempts = %d, want one per call (2 total)", attempts)
	}
}

func TestDiscoveryRetainsSnapshotDuringRegistryOutage(t *testing.T) {
	t.Parallel()

	memory := testkit.NewMemoryRegistry()
	t.Cleanup(func() { _ = memory.Close() })
	lifecycleServer := startEchoServer(t)
	registerInstance(t, memory, "a", lifecycleServer)
	serviceResolver := resolver.New(memory, resolver.Options{
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
	})
	client := startDiscoveryClient(t, serviceResolver)

	faultStarted := time.Now()
	memory.SetUnavailable(errors.New("injected etcd outage"))
	memory.DisconnectWatches(echov1.EchoServiceName, errors.New("injected watch outage"))
	waitForSnapshotError(t, client)
	if instances := client.Stats().Instances; instances != 1 {
		t.Fatalf("instances during registry outage = %d, want cached 1", instances)
	}

	memory.SetUnavailable(nil)
	secondServer := startEchoServer(t)
	registerInstance(t, memory, "b", secondServer)
	waitForInstances(t, client, 2)
	if recovery := time.Since(faultStarted); recovery > 2*time.Second {
		t.Fatalf("registry recovery took %v", recovery)
	}
}

func startDiscoveryClient(t *testing.T, serviceResolver *resolver.Resolver) *discovery.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := discovery.New(ctx, discovery.Config{
		Service:  echov1.EchoServiceName,
		Resolver: serviceResolver,
		Retry: governance.RetryPolicy{
			MaxAttempts:    2,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     time.Millisecond,
			MaxElapsed:     time.Second,
			Jitter:         func(time.Duration) time.Duration { return 0 },
		},
	})
	if err != nil {
		t.Fatalf("discovery.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

type echoFixture struct {
	address  string
	listener net.Listener
	rpc      *server.Server
	done     chan error
	stopOne  sync.Once
}

func startEchoServer(t *testing.T) *echoFixture {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	rpcServer := server.New()
	if err := echov1.RegisterEchoServer(rpcServer, echoImplementation{}); err != nil {
		t.Fatalf("RegisterEchoServer() error = %v", err)
	}
	fixture := &echoFixture{
		address:  listener.Addr().String(),
		listener: listener,
		rpc:      rpcServer,
		done:     make(chan error, 1),
	}
	go func() { fixture.done <- rpcServer.Serve(listener) }()
	t.Cleanup(func() { fixture.stop(t) })
	return fixture
}

func (fixture *echoFixture) stop(t *testing.T) {
	t.Helper()
	fixture.stopOne.Do(func() {
		_ = fixture.listener.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = fixture.rpc.GracefulStop(ctx)
		select {
		case <-fixture.done:
		case <-time.After(2 * time.Second):
			t.Error("timed out waiting for Echo server stop")
		}
	})
}

type echoImplementation struct{}

func (echoImplementation) Echo(
	_ context.Context,
	request *echov1.EchoRequest,
) (*echov1.EchoResponse, error) {
	return &echov1.EchoResponse{Message: request.GetMessage()}, nil
}

func registerInstance(t *testing.T, memory *testkit.MemoryRegistry, id string, fixture *echoFixture) {
	t.Helper()
	_, err := memory.Register(context.Background(), registry.Instance{
		ID:      id,
		Service: echov1.EchoServiceName,
		Address: fixture.address,
		Weight:  1,
	}, time.Minute)
	if err != nil {
		t.Fatalf("Register(%s) error = %v", id, err)
	}
}

func waitForInstances(t *testing.T, client *discovery.Client, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snapshot, ok := client.Snapshot(); ok && snapshot.Err == nil && len(snapshot.Instances) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	snapshot, _ := client.Snapshot()
	t.Fatalf("snapshot instances = %d, want %d; snapshot=%+v", len(snapshot.Instances), want, snapshot)
}

func waitForSnapshotError(t *testing.T, client *discovery.Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snapshot, ok := client.Snapshot(); ok && snapshot.Err != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for registry snapshot error")
}

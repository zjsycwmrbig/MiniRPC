package balancer

import (
	"errors"
	"sync"
	"testing"

	"github.com/zjsycwmrbig/mini-rpc/internal/registry"
)

func TestRoundRobinOrder(t *testing.T) {
	t.Parallel()

	instances := testInstances()
	balancer := NewRoundRobin()
	want := []string{"a", "b", "c", "a", "b", "c"}
	for index, wantID := range want {
		got, err := balancer.Pick(instances)
		if err != nil {
			t.Fatalf("pick %d error = %v", index, err)
		}
		if got.ID != wantID {
			t.Fatalf("pick %d = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestRoundRobinConcurrentPick(t *testing.T) {
	t.Parallel()

	instances := testInstances()
	balancer := NewRoundRobin()
	const picks = 600

	var waitGroup sync.WaitGroup
	var lock sync.Mutex
	counts := make(map[string]int)
	waitGroup.Add(picks)
	for range picks {
		go func() {
			defer waitGroup.Done()
			instance, err := balancer.Pick(instances)
			if err != nil {
				t.Errorf("Pick() error = %v", err)
				return
			}
			lock.Lock()
			counts[instance.ID]++
			lock.Unlock()
		}()
	}
	waitGroup.Wait()
	for _, id := range []string{"a", "b", "c"} {
		if counts[id] != picks/len(instances) {
			t.Fatalf("count[%s] = %d, want %d", id, counts[id], picks/len(instances))
		}
	}
}

func TestWeightedRoundRobinDistribution(t *testing.T) {
	t.Parallel()

	instances := []registry.Instance{
		{ID: "a", Address: "a:8080", Weight: 1},
		{ID: "b", Address: "b:8080", Weight: 2},
		{ID: "c", Address: "c:8080", Weight: 3},
	}
	balancer := NewWeightedRoundRobin()
	const picks = 600_000
	counts := make(map[string]int)
	for range picks {
		instance, err := balancer.Pick(instances)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		counts[instance.ID]++
	}
	want := map[string]int{"a": 100_000, "b": 200_000, "c": 300_000}
	for id, wantCount := range want {
		if counts[id] != wantCount {
			t.Fatalf("count[%s] = %d, want %d", id, counts[id], wantCount)
		}
	}
	t.Logf("100000+ weighted picks: %v", counts)
}

func TestWeightedRoundRobinTracksSnapshotChanges(t *testing.T) {
	t.Parallel()

	balancer := NewWeightedRoundRobin()
	initial := []registry.Instance{
		{ID: "a", Address: "a:8080", Weight: 1},
		{ID: "b", Address: "b:8080", Weight: 1},
	}
	for range 6 {
		if _, err := balancer.Pick(initial); err != nil {
			t.Fatalf("initial Pick() error = %v", err)
		}
	}

	reduced := []registry.Instance{
		{ID: "b", Address: "b:8080", Weight: 3},
		{ID: "c", Address: "c:8080", Weight: 1},
	}
	counts := make(map[string]int)
	for range 400 {
		instance, err := balancer.Pick(reduced)
		if err != nil {
			t.Fatalf("reduced Pick() error = %v", err)
		}
		if instance.ID == "a" {
			t.Fatal("removed instance was selected")
		}
		counts[instance.ID]++
	}
	if counts["b"] != 300 || counts["c"] != 100 {
		t.Fatalf("counts after snapshot change = %v", counts)
	}
}

func TestBalancerRejectsEmptyAndInvalidInputs(t *testing.T) {
	t.Parallel()

	roundRobin := NewRoundRobin()
	if _, err := roundRobin.Pick(nil); !errors.Is(err, ErrNoInstances) {
		t.Fatalf("round-robin empty error = %v", err)
	}
	duplicate := []registry.Instance{
		{ID: "a", Address: "a:8080"},
		{ID: "a", Address: "b:8080"},
	}
	if _, err := roundRobin.Pick(duplicate); !errors.Is(err, ErrInvalidInstances) {
		t.Fatalf("round-robin duplicate error = %v", err)
	}

	weighted := NewWeightedRoundRobin()
	zeroWeights := []registry.Instance{
		{ID: "a", Address: "a:8080", Weight: 0},
		{ID: "b", Address: "b:8080", Weight: 0},
	}
	if _, err := weighted.Pick(zeroWeights); !errors.Is(err, ErrNoWeightedInstances) {
		t.Fatalf("weighted zero error = %v", err)
	}
}

func BenchmarkRoundRobin(b *testing.B) {
	balancer := NewRoundRobin()
	instances := testInstances()
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		if _, err := balancer.Pick(instances); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWeightedRoundRobin(b *testing.B) {
	balancer := NewWeightedRoundRobin()
	instances := []registry.Instance{
		{ID: "a", Address: "a:8080", Weight: 1},
		{ID: "b", Address: "b:8080", Weight: 2},
		{ID: "c", Address: "c:8080", Weight: 3},
	}
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		if _, err := balancer.Pick(instances); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWeightedDistribution(b *testing.B) {
	balancer := NewWeightedRoundRobin()
	instances := []registry.Instance{
		{ID: "a", Address: "a:8080", Weight: 1},
		{ID: "b", Address: "b:8080", Weight: 2},
		{ID: "c", Address: "c:8080", Weight: 3},
	}
	counts := make(map[string]int)
	for index := 0; index < b.N; index++ {
		instance, err := balancer.Pick(instances)
		if err != nil {
			b.Fatal(err)
		}
		counts[instance.ID]++
	}
	b.StopTimer()
	for id, count := range counts {
		b.ReportMetric(float64(count)/float64(b.N), "selected_ratio_"+id)
	}
}

func testInstances() []registry.Instance {
	return []registry.Instance{
		{ID: "a", Service: "svc", Address: "a:8080", Weight: 1},
		{ID: "b", Service: "svc", Address: "b:8080", Weight: 1},
		{ID: "c", Service: "svc", Address: "c:8080", Weight: 1},
	}
}

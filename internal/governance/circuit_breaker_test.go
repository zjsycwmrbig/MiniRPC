package governance

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCircuitBreakerStateTransitions(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0)
	breaker := newCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 2,
		OpenTimeout:      10 * time.Millisecond,
	}, func() time.Time { return now })

	for range 2 {
		permit, err := breaker.Allow()
		if err != nil {
			t.Fatalf("closed Allow() error = %v", err)
		}
		permit.RecordFailure()
	}
	if breaker.State() != CircuitOpen {
		t.Fatalf("state = %s, want open", breaker.State())
	}
	if _, err := breaker.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("open Allow() error = %v, want ErrCircuitOpen", err)
	}

	now = now.Add(11 * time.Millisecond)
	probe, err := breaker.Allow()
	if err != nil {
		t.Fatalf("half-open Allow() error = %v", err)
	}
	if breaker.State() != CircuitHalfOpen {
		t.Fatalf("state = %s, want half_open", breaker.State())
	}
	if _, err := breaker.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("concurrent probe error = %v, want ErrCircuitOpen", err)
	}
	probe.RecordSuccess()
	if breaker.State() != CircuitClosed {
		t.Fatalf("state after successful probe = %s, want closed", breaker.State())
	}

	permit, err := breaker.Allow()
	if err != nil {
		t.Fatalf("Allow() after recovery error = %v", err)
	}
	permit.RecordFailure()
	permit, err = breaker.Allow()
	if err != nil {
		t.Fatalf("second Allow() after recovery error = %v", err)
	}
	permit.RecordFailure()
	now = now.Add(11 * time.Millisecond)
	probe, err = breaker.Allow()
	if err != nil {
		t.Fatalf("second half-open Allow() error = %v", err)
	}
	probe.RecordFailure()
	if breaker.State() != CircuitOpen {
		t.Fatalf("state after failed probe = %s, want open", breaker.State())
	}

	stats := breaker.Stats()
	if stats.Opens != 3 || stats.HalfOpenProbes != 2 || stats.Rejected < 2 {
		t.Fatalf("unexpected breaker stats: %+v", stats)
	}
}

func TestCircuitBreakerAllowsOnlyOneConcurrentHalfOpenProbe(t *testing.T) {
	t.Parallel()

	now := time.Unix(200, 0)
	breaker := newCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1,
		OpenTimeout:      time.Millisecond,
	}, func() time.Time { return now })
	permit, err := breaker.Allow()
	if err != nil {
		t.Fatalf("Allow() error = %v", err)
	}
	permit.RecordFailure()
	now = now.Add(2 * time.Millisecond)

	var allowed atomic.Int64
	probes := make(chan *Permit, 64)
	var waitGroup sync.WaitGroup
	waitGroup.Add(64)
	for range 64 {
		go func() {
			defer waitGroup.Done()
			permit, err := breaker.Allow()
			if err == nil {
				allowed.Add(1)
				probes <- permit
			}
		}()
	}
	waitGroup.Wait()
	if got := allowed.Load(); got != 1 {
		t.Fatalf("half-open allowed probes = %d, want 1", got)
	}
	(<-probes).Release()
}

func TestCircuitBreakerIgnoresLateClosedCallResultAfterOpening(t *testing.T) {
	t.Parallel()

	breaker := NewCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 1})
	latePermit, err := breaker.Allow()
	if err != nil {
		t.Fatalf("first Allow() error = %v", err)
	}
	openingPermit, err := breaker.Allow()
	if err != nil {
		t.Fatalf("second Allow() error = %v", err)
	}
	openingPermit.RecordFailure()
	latePermit.RecordSuccess()
	if breaker.State() != CircuitOpen {
		t.Fatalf("late success closed breaker: state = %s", breaker.State())
	}
}

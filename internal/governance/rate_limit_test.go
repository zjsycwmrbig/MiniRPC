package governance

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimiterAllowsBurstThenRefillsAtConfiguredRate(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0)
	limiter := newRateLimiter(RateLimitConfig{
		Services: map[string]RateLimitRule{
			"echo.v1.Echo": {Rate: 2, Burst: 3},
		},
	}, func() time.Time { return now })
	ctx := context.Background()

	for index := range 3 {
		if err := limiter.Acquire(ctx, "echo.v1.Echo", "Echo"); err != nil {
			t.Fatalf("burst request %d error = %v", index, err)
		}
	}
	if err := limiter.Acquire(ctx, "echo.v1.Echo", "Echo"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("request after burst error = %v, want ErrRateLimited", err)
	}

	now = now.Add(500 * time.Millisecond)
	if err := limiter.Acquire(ctx, "echo.v1.Echo", "Echo"); err != nil {
		t.Fatalf("request after refill error = %v", err)
	}
	if err := limiter.Acquire(ctx, "echo.v1.Echo", "Echo"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second refill request error = %v, want ErrRateLimited", err)
	}

	stats := limiter.Stats()
	if len(stats) != 1 {
		t.Fatalf("Stats() length = %d, want 1", len(stats))
	}
	if stats[0].Allowed != 4 || stats[0].Rejected != 2 {
		t.Fatalf("unexpected limiter stats: %+v", stats[0])
	}
	if math.Abs(stats[0].Tokens) > 0.0001 {
		t.Fatalf("remaining tokens = %.6f, want 0", stats[0].Tokens)
	}
}

func TestRateLimiterMethodRuleOverridesServiceRule(t *testing.T) {
	t.Parallel()

	now := time.Unix(200, 0)
	limiter := newRateLimiter(RateLimitConfig{
		Services: map[string]RateLimitRule{
			"echo.v1.Echo": {Rate: 1, Burst: 1},
		},
		Methods: map[string]RateLimitRule{
			MethodRateLimitKey("echo.v1.Echo", "Echo"):   {Rate: 1, Burst: 2},
			MethodRateLimitKey("echo.v1.Echo", "Health"): {Rate: 1, Burst: 1},
		},
	}, func() time.Time { return now })

	for range 2 {
		if err := limiter.Acquire(context.Background(), "echo.v1.Echo", "Echo"); err != nil {
			t.Fatalf("method burst error = %v", err)
		}
	}
	if err := limiter.Acquire(context.Background(), "echo.v1.Echo", "Echo"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("method third request error = %v, want ErrRateLimited", err)
	}
	if err := limiter.Acquire(context.Background(), "echo.v1.Echo", "Health"); err != nil {
		t.Fatalf("health service fallback error = %v", err)
	}
}

func TestRateLimiterConcurrentAcquireDoesNotOversubscribe(t *testing.T) {
	t.Parallel()

	limiter := NewRateLimiter(RateLimitConfig{
		Services: map[string]RateLimitRule{
			"echo.v1.Echo": {Rate: 0.000001, Burst: 10},
		},
	})

	var allowed atomic.Int64
	var rejected atomic.Int64
	var waitGroup sync.WaitGroup
	waitGroup.Add(128)
	for range 128 {
		go func() {
			defer waitGroup.Done()
			err := limiter.Acquire(context.Background(), "echo.v1.Echo", "Echo")
			switch {
			case err == nil:
				allowed.Add(1)
			case errors.Is(err, ErrRateLimited):
				rejected.Add(1)
			default:
				t.Errorf("Acquire() error = %v", err)
			}
		}()
	}
	waitGroup.Wait()

	if got := allowed.Load(); got != 10 {
		t.Fatalf("allowed requests = %d, want 10", got)
	}
	if got := rejected.Load(); got != 118 {
		t.Fatalf("rejected requests = %d, want 118", got)
	}
}

func TestRateLimiterWaitHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	limiter := NewRateLimiter(RateLimitConfig{
		Services: map[string]RateLimitRule{
			"echo.v1.Echo": {Rate: 0.01, Burst: 1, Wait: true},
		},
	})
	if err := limiter.Acquire(context.Background(), "echo.v1.Echo", "Echo"); err != nil {
		t.Fatalf("initial Acquire() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- limiter.Acquire(ctx, "echo.v1.Echo", "Echo")
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting Acquire() error = %v, want Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting Acquire() did not return after cancellation")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("cancellation elapsed = %v", elapsed)
	}

	stats := limiter.Stats()
	if len(stats) != 1 || stats[0].Waited != 1 || stats[0].WaitCanceled != 1 {
		t.Fatalf("unexpected wait stats: %+v", stats)
	}
}

func TestRateLimitConfigValidation(t *testing.T) {
	t.Parallel()

	valid := RateLimitConfig{
		Services: map[string]RateLimitRule{"echo.v1.Echo": {Rate: 10, Burst: 2}},
		Methods: map[string]RateLimitRule{
			MethodRateLimitKey("echo.v1.Echo", "Echo"): {Rate: 5, Burst: 1},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config error = %v", err)
	}
	invalid := RateLimitConfig{
		Methods: map[string]RateLimitRule{"echo.v1.Echo": {Rate: 1, Burst: 1}},
	}
	if err := invalid.Validate(); !errors.Is(err, ErrInvalidRateLimit) {
		t.Fatalf("invalid config error = %v, want ErrInvalidRateLimit", err)
	}
}

func TestLocalRateLimitIsNotRetryable(t *testing.T) {
	t.Parallel()

	limiter := NewRateLimiter(RateLimitConfig{
		Services: map[string]RateLimitRule{"echo.v1.Echo": {Rate: 1, Burst: 1}},
	})
	if err := limiter.Acquire(context.Background(), "echo.v1.Echo", "Echo"); err != nil {
		t.Fatalf("initial Acquire() error = %v", err)
	}
	err := limiter.Acquire(context.Background(), "echo.v1.Echo", "Echo")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second Acquire() error = %v, want ErrRateLimited", err)
	}
	if IsRetryable(err) {
		t.Fatal("local rate-limit error must not be retryable")
	}
}

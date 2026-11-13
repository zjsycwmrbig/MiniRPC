package governance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
)

func TestRetryPolicyRetriesIdempotentTransientErrors(t *testing.T) {
	t.Parallel()

	policy := RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: time.Nanosecond,
		MaxBackoff:     time.Nanosecond,
		MaxElapsed:     time.Second,
		Jitter:         func(time.Duration) time.Duration { return 0 },
	}
	attempts, err := policy.Do(context.Background(), true, func(attempt int) error {
		if attempt < 3 {
			return rpc.NewError(rpc.CodeUnavailable, "temporary failure")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if attempts != 3 {
		t.Fatalf("Do() attempts = %d, want 3", attempts)
	}
	t.Logf("idempotent transient call attempts: %d", attempts)
}

func TestRetryPolicyRejectsNonIdempotentAndPermanentErrors(t *testing.T) {
	t.Parallel()

	policy := RetryPolicy{
		MaxAttempts:    4,
		InitialBackoff: time.Nanosecond,
		MaxBackoff:     time.Nanosecond,
		MaxElapsed:     time.Second,
		Jitter:         func(time.Duration) time.Duration { return 0 },
	}

	var nonIdempotentAttempts int
	attempts, err := policy.Do(context.Background(), false, func(int) error {
		nonIdempotentAttempts++
		return rpc.NewError(rpc.CodeUnavailable, "temporary failure")
	})
	if attempts != 1 || nonIdempotentAttempts != 1 {
		t.Fatalf("non-idempotent attempts = %d/%d, want 1/1", attempts, nonIdempotentAttempts)
	}
	if !errors.As(err, new(*rpc.Error)) {
		t.Fatalf("Do() error = %v, want *rpc.Error", err)
	}

	attempts, err = policy.Do(context.Background(), true, func(int) error {
		return rpc.NewError(rpc.CodeInvalidArgument, "bad request")
	})
	if attempts != 1 {
		t.Fatalf("permanent error attempts = %d, want 1", attempts)
	}
	if !errors.As(err, new(*rpc.Error)) {
		t.Fatalf("Do() error = %v, want *rpc.Error", err)
	}
}

func TestRetryPolicyHonorsCallDeadline(t *testing.T) {
	t.Parallel()

	policy := RetryPolicy{
		MaxAttempts:    10,
		InitialBackoff: 20 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
		MaxElapsed:     time.Second,
		Jitter:         func(time.Duration) time.Duration { return 0 },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Millisecond)
	defer cancel()
	started := time.Now()
	attempts, err := policy.Do(ctx, true, func(int) error {
		return rpc.NewError(rpc.CodeUnavailable, "temporary failure")
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do() error = %v, want DeadlineExceeded", err)
	}
	if attempts >= policy.MaxAttempts {
		t.Fatalf("Do() attempts = %d, deadline should stop retries", attempts)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Do() elapsed = %v, retry exceeded call deadline", elapsed)
	}
}

func TestRetryPolicyHonorsMaxElapsedWithoutExtraWait(t *testing.T) {
	t.Parallel()

	policy := RetryPolicy{
		MaxAttempts:    5,
		InitialBackoff: 50 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
		MaxElapsed:     10 * time.Millisecond,
		Jitter:         func(time.Duration) time.Duration { return 0 },
	}
	started := time.Now()
	attempts, err := policy.Do(context.Background(), true, func(int) error {
		return rpc.NewError(rpc.CodeUnavailable, "temporary failure")
	})
	if attempts != 1 {
		t.Fatalf("Do() attempts = %d, want 1", attempts)
	}
	if !IsRetryable(err) {
		t.Fatalf("Do() error = %v, want last retryable error", err)
	}
	if elapsed := time.Since(started); elapsed > 20*time.Millisecond {
		t.Fatalf("Do() waited past MaxElapsed: %v", elapsed)
	}
}

func TestRetryBackoffIsExponentialCappedAndJittered(t *testing.T) {
	t.Parallel()

	policy := RetryPolicy{
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     40 * time.Millisecond,
		Jitter:         func(time.Duration) time.Duration { return time.Millisecond },
	}
	for index, want := range []time.Duration{
		11 * time.Millisecond,
		21 * time.Millisecond,
		40 * time.Millisecond,
	} {
		if got := policy.Backoff(index + 1); got != want {
			t.Fatalf("Backoff(%d) = %v, want %v", index+1, got, want)
		}
	}
}

func TestRetryableErrorSet(t *testing.T) {
	t.Parallel()

	retryable := []rpc.ErrorCode{
		rpc.CodeUnavailable,
		rpc.CodeResourceExhausted,
		rpc.CodeDeadlineExceeded,
	}
	for _, code := range retryable {
		if !IsRetryable(rpc.NewError(code, "transient")) {
			t.Fatalf("code %v should be retryable", code)
		}
	}
	for _, code := range []rpc.ErrorCode{
		rpc.CodeInvalidArgument,
		rpc.CodeNotFound,
		rpc.CodeInternal,
		rpc.CodeCanceled,
		rpc.CodeProtocolError,
	} {
		if IsRetryable(rpc.NewError(code, "permanent")) {
			t.Fatalf("code %v should not be retryable", code)
		}
	}
}

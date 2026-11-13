package governance

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
)

const (
	defaultMaxAttempts    = 3
	defaultInitialBackoff = 50 * time.Millisecond
	defaultMaxBackoff     = time.Second
	defaultMaxElapsed     = 2 * time.Second
)

// RetryPolicy controls idempotent retries for transient RPC failures.
//
// Jitter, when set, returns an additional delay in [0, Jitter(backoff)] and is
// primarily an injection point for deterministic tests. A nil Jitter adds a
// random delay of up to half the exponential backoff.
type RetryPolicy struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	MaxElapsed     time.Duration
	Jitter         func(time.Duration) time.Duration
}

// DefaultRetryPolicy returns bounded retry defaults for transient failures.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:    defaultMaxAttempts,
		InitialBackoff: defaultInitialBackoff,
		MaxBackoff:     defaultMaxBackoff,
		MaxElapsed:     defaultMaxElapsed,
	}
}

// Normalize fills non-positive fields with safe defaults.
func (policy RetryPolicy) Normalize() RetryPolicy {
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = defaultMaxAttempts
	}
	if policy.InitialBackoff <= 0 {
		policy.InitialBackoff = defaultInitialBackoff
	}
	if policy.MaxBackoff <= 0 {
		policy.MaxBackoff = defaultMaxBackoff
	}
	if policy.MaxBackoff < policy.InitialBackoff {
		policy.MaxBackoff = policy.InitialBackoff
	}
	if policy.MaxElapsed <= 0 {
		policy.MaxElapsed = defaultMaxElapsed
	}
	return policy
}

// Backoff returns the delay before retryNumber, where retryNumber starts at 1
// immediately after the first failed attempt.
func (policy RetryPolicy) Backoff(retryNumber int) time.Duration {
	policy = policy.Normalize()
	if retryNumber < 1 {
		retryNumber = 1
	}

	base := policy.InitialBackoff
	for index := 1; index < retryNumber && base < policy.MaxBackoff; index++ {
		if base > policy.MaxBackoff/2 {
			base = policy.MaxBackoff
			break
		}
		base *= 2
	}
	if base > policy.MaxBackoff {
		base = policy.MaxBackoff
	}

	var jitter time.Duration
	if policy.Jitter != nil {
		jitter = policy.Jitter(base)
	} else if half := base / 2; half > 0 {
		jitter = time.Duration(rand.Int64N(int64(half) + 1))
	}
	if jitter < 0 {
		jitter = 0
	}
	if base+jitter > policy.MaxBackoff {
		return policy.MaxBackoff
	}
	return base + jitter
}

// Do executes attempt until it succeeds, becomes non-retryable, exhausts the
// attempt budget, exceeds MaxElapsed, or the Context ends. The returned attempt
// count includes the initial call.
func (policy RetryPolicy) Do(
	ctx context.Context,
	idempotent bool,
	attempt func(attempt int) error,
) (int, error) {
	if ctx == nil {
		return 0, ErrNilContext
	}
	if attempt == nil {
		return 0, errors.New("governance: nil retry attempt")
	}
	policy = policy.Normalize()
	started := time.Now()

	for attempts := 1; ; attempts++ {
		err := attempt(attempts)
		if err == nil {
			return attempts, nil
		}
		if !idempotent || !IsRetryable(err) || ctx.Err() != nil || attempts >= policy.MaxAttempts {
			return attempts, err
		}

		elapsed := time.Since(started)
		if policy.MaxElapsed > 0 && elapsed >= policy.MaxElapsed {
			return attempts, err
		}
		delay := policy.Backoff(attempts)
		if policy.MaxElapsed > 0 && elapsed+delay > policy.MaxElapsed {
			return attempts, err
		}

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return attempts, ctx.Err()
		}
	}
}

// IsRetryable reports whether an error represents a transient failure that may
// be retried for an explicitly idempotent call.
func IsRetryable(err error) bool {
	var rpcError *rpc.Error
	if !errors.As(err, &rpcError) {
		return false
	}
	switch rpcError.Code {
	case rpc.CodeUnavailable, rpc.CodeResourceExhausted, rpc.CodeDeadlineExceeded:
		return true
	default:
		return false
	}
}

// IsBreakerFailure reports whether an error should affect availability state.
func IsBreakerFailure(err error) bool {
	return IsRetryable(err)
}

package governance

import "errors"

var (
	// ErrCircuitOpen indicates that the breaker is rejecting calls without
	// executing an attempt.
	ErrCircuitOpen = errors.New("governance: circuit breaker open")

	// ErrNilContext indicates that a retry or circuit-breaker operation received
	// no Context.
	ErrNilContext = errors.New("governance: nil context")

	// ErrRateLimited indicates that a local token bucket rejected a call before
	// it reached the network. This error is intentionally not retryable.
	ErrRateLimited = errors.New("governance: rate limit exceeded")

	// ErrInvalidRateLimit indicates an invalid service or method bucket rule.
	ErrInvalidRateLimit = errors.New("governance: invalid rate limit")
)

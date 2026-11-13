package governance

import "errors"

var (
	// ErrCircuitOpen indicates that the breaker is rejecting calls without
	// executing an attempt.
	ErrCircuitOpen = errors.New("governance: circuit breaker open")

	// ErrNilContext indicates that a retry or circuit-breaker operation received
	// no Context.
	ErrNilContext = errors.New("governance: nil context")
)

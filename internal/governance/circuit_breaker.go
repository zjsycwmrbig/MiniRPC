package governance

import (
	"sync"
	"time"
)

// CircuitState is the externally visible breaker state.
type CircuitState string

const (
	CircuitClosed   CircuitState = "closed"
	CircuitOpen     CircuitState = "open"
	CircuitHalfOpen CircuitState = "half_open"
)

const (
	defaultFailureThreshold = 5
	defaultOpenTimeout      = 30 * time.Second
)

// CircuitBreakerConfig controls consecutive-failure and cooldown thresholds.
type CircuitBreakerConfig struct {
	FailureThreshold int
	OpenTimeout      time.Duration
}

// DefaultCircuitBreakerConfig returns conservative local defaults.
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		FailureThreshold: defaultFailureThreshold,
		OpenTimeout:      defaultOpenTimeout,
	}
}

// Normalize fills non-positive fields with defaults.
func (config CircuitBreakerConfig) Normalize() CircuitBreakerConfig {
	if config.FailureThreshold <= 0 {
		config.FailureThreshold = defaultFailureThreshold
	}
	if config.OpenTimeout <= 0 {
		config.OpenTimeout = defaultOpenTimeout
	}
	return config
}

// CircuitBreakerStats is a point-in-time state and decision snapshot.
type CircuitBreakerStats struct {
	State               CircuitState
	ConsecutiveFailures int
	Allowed             uint64
	Rejected            uint64
	Successes           uint64
	Failures            uint64
	Opens               uint64
	HalfOpenProbes      uint64
	OpenedAt            time.Time
}

// CircuitBreaker permits at most one probe while half-open and rejects calls
// immediately while open. Closed-state calls may proceed concurrently.
type CircuitBreaker struct {
	mu     sync.Mutex
	config CircuitBreakerConfig
	now    func() time.Time

	state               CircuitState
	consecutiveFailures int
	openedAt            time.Time
	halfOpenInFlight    bool
	stats               CircuitBreakerStats
}

// NewCircuitBreaker creates a closed breaker.
func NewCircuitBreaker(config CircuitBreakerConfig) *CircuitBreaker {
	return newCircuitBreaker(config, time.Now)
}

func newCircuitBreaker(config CircuitBreakerConfig, now func() time.Time) *CircuitBreaker {
	if now == nil {
		now = time.Now
	}
	return &CircuitBreaker{
		config: config.Normalize(),
		now:    now,
		state:  CircuitClosed,
	}
}

// Permit is the result of one breaker admission decision. Exactly one of
// RecordSuccess, RecordFailure, or Release should be called.
type Permit struct {
	breaker *CircuitBreaker
	probe   bool
	done    bool
}

// Allow admits or fast-rejects one call.
func (breaker *CircuitBreaker) Allow() (*Permit, error) {
	if breaker == nil {
		return &Permit{}, nil
	}

	breaker.mu.Lock()
	defer breaker.mu.Unlock()

	now := breaker.now()
	if breaker.state == CircuitOpen {
		if now.Sub(breaker.openedAt) < breaker.config.OpenTimeout {
			breaker.stats.Rejected++
			return nil, ErrCircuitOpen
		}
		breaker.state = CircuitHalfOpen
		breaker.halfOpenInFlight = false
	}

	if breaker.state == CircuitHalfOpen {
		if breaker.halfOpenInFlight {
			breaker.stats.Rejected++
			return nil, ErrCircuitOpen
		}
		breaker.halfOpenInFlight = true
		breaker.stats.HalfOpenProbes++
	}
	breaker.stats.Allowed++
	return &Permit{breaker: breaker, probe: breaker.state == CircuitHalfOpen}, nil
}

// RecordSuccess resets closed-state failures and closes a successful probe.
func (permit *Permit) RecordSuccess() {
	permit.finish(func(breaker *CircuitBreaker) {
		breaker.stats.Successes++
		if permit.probe {
			if breaker.state == CircuitHalfOpen {
				breaker.state = CircuitClosed
				breaker.halfOpenInFlight = false
				breaker.consecutiveFailures = 0
				breaker.openedAt = time.Time{}
			}
			return
		}
		if breaker.state == CircuitClosed {
			breaker.consecutiveFailures = 0
		}
	})
}

// RecordFailure counts a closed-state failure or reopens after a failed probe.
func (permit *Permit) RecordFailure() {
	permit.finish(func(breaker *CircuitBreaker) {
		breaker.stats.Failures++
		if permit.probe {
			if breaker.state == CircuitHalfOpen {
				breaker.open()
			}
			return
		}
		if breaker.state != CircuitClosed {
			return
		}
		breaker.consecutiveFailures++
		if breaker.consecutiveFailures >= breaker.config.FailureThreshold {
			breaker.open()
		}
	})
}

// Release returns a half-open probe slot without changing availability state.
func (permit *Permit) Release() {
	permit.finish(func(breaker *CircuitBreaker) {
		if permit.probe && breaker.state == CircuitHalfOpen {
			breaker.halfOpenInFlight = false
		}
	})
}

func (permit *Permit) finish(record func(*CircuitBreaker)) {
	if permit == nil || permit.breaker == nil {
		return
	}
	breaker := permit.breaker
	breaker.mu.Lock()
	defer breaker.mu.Unlock()
	if permit.done {
		return
	}
	permit.done = true
	record(breaker)
}

func (breaker *CircuitBreaker) open() {
	breaker.state = CircuitOpen
	breaker.openedAt = breaker.now()
	breaker.halfOpenInFlight = false
	breaker.stats.OpenedAt = breaker.openedAt
	breaker.stats.Opens++
}

// Stats returns a consistent breaker snapshot.
func (breaker *CircuitBreaker) Stats() CircuitBreakerStats {
	if breaker == nil {
		return CircuitBreakerStats{}
	}
	breaker.mu.Lock()
	defer breaker.mu.Unlock()
	snapshot := breaker.stats
	snapshot.State = breaker.state
	snapshot.ConsecutiveFailures = breaker.consecutiveFailures
	return snapshot
}

// State returns the current breaker state.
func (breaker *CircuitBreaker) State() CircuitState {
	return breaker.Stats().State
}

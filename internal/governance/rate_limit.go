package governance

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// RateLimitRule configures one local token bucket. Rate is measured in tokens
// per second. Wait selects blocking admission instead of immediate rejection.
type RateLimitRule struct {
	Rate  float64
	Burst int
	Wait  bool
}

// RateLimitConfig groups service-level and method-level token buckets.
//
// Methods are keyed by MethodRateLimitKey and take precedence over their
// service rule. A rule that is absent leaves that scope unlimited.
type RateLimitConfig struct {
	Services map[string]RateLimitRule
	Methods  map[string]RateLimitRule
}

// MethodRateLimitKey returns the stable key used in RateLimitConfig.Methods.
func MethodRateLimitKey(service, method string) string {
	return service + "/" + method
}

// Validate rejects ambiguous keys and unsafe bucket values.
func (config RateLimitConfig) Validate() error {
	for service, rule := range config.Services {
		if service == "" {
			return fmt.Errorf("%w: empty service key", ErrInvalidRateLimit)
		}
		if err := rule.validate(); err != nil {
			return fmt.Errorf("%w: service %q: %v", ErrInvalidRateLimit, service, err)
		}
	}
	for key, rule := range config.Methods {
		service, method, ok := strings.Cut(key, "/")
		if !ok || service == "" || method == "" || strings.Contains(method, "/") {
			return fmt.Errorf("%w: invalid method key %q", ErrInvalidRateLimit, key)
		}
		if err := rule.validate(); err != nil {
			return fmt.Errorf("%w: method %q: %v", ErrInvalidRateLimit, key, err)
		}
	}
	return nil
}

func (rule RateLimitRule) validate() error {
	if rule.Rate <= 0 || math.IsInf(rule.Rate, 0) || math.IsNaN(rule.Rate) {
		return fmt.Errorf("rate must be finite and positive")
	}
	if rule.Burst <= 0 {
		return fmt.Errorf("burst must be positive")
	}
	return nil
}

// RateLimitStats is a consistent snapshot of one configured token bucket.
type RateLimitStats struct {
	Key          string
	Rate         float64
	Burst        int
	Wait         bool
	Tokens       float64
	Allowed      uint64
	Rejected     uint64
	Waited       uint64
	WaitCanceled uint64
}

// RateLimiter applies independent local token buckets to service and method
// scopes. It is safe for concurrent use.
type RateLimiter struct {
	mu      sync.Mutex
	rules   map[string]RateLimitRule
	buckets map[string]*tokenBucket
	now     func() time.Time
}

type tokenBucket struct {
	mu     sync.Mutex
	rule   RateLimitRule
	tokens float64
	last   time.Time
	stats  RateLimitStats
}

// NewRateLimiter builds an immutable rule set. Invalid rules are omitted; use
// RateLimitConfig.Validate when configuration should fail closed.
func NewRateLimiter(config RateLimitConfig) *RateLimiter {
	return newRateLimiter(config, time.Now)
}

func newRateLimiter(config RateLimitConfig, now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	rules := make(map[string]RateLimitRule, len(config.Services)+len(config.Methods))
	for service, rule := range config.Services {
		if service != "" && rule.validate() == nil {
			rules[service] = rule
		}
	}
	for key, rule := range config.Methods {
		service, method, ok := strings.Cut(key, "/")
		if ok && service != "" && method != "" && !strings.Contains(method, "/") && rule.validate() == nil {
			rules[key] = rule
		}
	}
	limiter := &RateLimiter{
		rules:   rules,
		buckets: make(map[string]*tokenBucket),
		now:     now,
	}
	for key, rule := range rules {
		limiter.buckets[key] = limiter.newBucket(key, rule)
	}
	return limiter
}

// Acquire reserves one token from the most specific rule for the RPC. It
// returns ErrRateLimited for non-waiting rejection and the Context error when a
// waiting request is cancelled or reaches its deadline.
func (limiter *RateLimiter) Acquire(ctx context.Context, service, method string) error {
	if limiter == nil {
		return nil
	}
	if ctx == nil {
		return ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	key, rule, limited := limiter.ruleFor(service, method)
	if !limited {
		return nil
	}
	bucket := limiter.bucketFor(key, rule)
	waited := false

	for {
		if err := ctx.Err(); err != nil {
			if waited {
				bucket.recordWaitCanceled()
			}
			return err
		}

		bucket.mu.Lock()
		bucket.refillLocked(limiter.now())
		if bucket.tokens >= 1 {
			bucket.tokens--
			bucket.stats.Allowed++
			bucket.mu.Unlock()
			return nil
		}
		if !rule.Wait {
			bucket.stats.Rejected++
			bucket.mu.Unlock()
			return fmt.Errorf("%w: %s", ErrRateLimited, key)
		}
		if !waited {
			bucket.stats.Waited++
			waited = true
		}
		delay := time.Duration(math.Ceil((1 - bucket.tokens) / rule.Rate * float64(time.Second)))
		if delay <= 0 {
			delay = time.Nanosecond
		}
		bucket.mu.Unlock()

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			bucket.recordWaitCanceled()
			return ctx.Err()
		}
	}
}

func (limiter *RateLimiter) ruleFor(service, method string) (string, RateLimitRule, bool) {
	methodKey := MethodRateLimitKey(service, method)
	if rule, ok := limiter.rules[methodKey]; ok {
		return methodKey, rule, true
	}
	rule, ok := limiter.rules[service]
	return service, rule, ok
}

func (limiter *RateLimiter) bucketFor(key string, rule RateLimitRule) *tokenBucket {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if bucket := limiter.buckets[key]; bucket != nil {
		return bucket
	}
	bucket := limiter.newBucket(key, rule)
	limiter.buckets[key] = bucket
	return bucket
}

func (limiter *RateLimiter) newBucket(key string, rule RateLimitRule) *tokenBucket {
	now := limiter.now()
	return &tokenBucket{
		rule:   rule,
		tokens: float64(rule.Burst),
		last:   now,
		stats: RateLimitStats{
			Key:    key,
			Rate:   rule.Rate,
			Burst:  rule.Burst,
			Wait:   rule.Wait,
			Tokens: float64(rule.Burst),
		},
	}
}

func (bucket *tokenBucket) refillLocked(now time.Time) {
	elapsed := now.Sub(bucket.last)
	if elapsed <= 0 {
		return
	}
	bucket.last = now
	bucket.tokens += elapsed.Seconds() * bucket.rule.Rate
	if capacity := float64(bucket.rule.Burst); bucket.tokens > capacity {
		bucket.tokens = capacity
	}
}

func (bucket *tokenBucket) recordWaitCanceled() {
	bucket.mu.Lock()
	bucket.stats.WaitCanceled++
	bucket.mu.Unlock()
}

// Stats returns per-bucket counters and refilled token snapshots in key order.
func (limiter *RateLimiter) Stats() []RateLimitStats {
	if limiter == nil {
		return nil
	}
	limiter.mu.Lock()
	keys := make([]string, 0, len(limiter.buckets))
	buckets := make(map[string]*tokenBucket, len(limiter.buckets))
	for key, bucket := range limiter.buckets {
		keys = append(keys, key)
		buckets[key] = bucket
	}
	limiter.mu.Unlock()
	sort.Strings(keys)

	now := limiter.now()
	snapshots := make([]RateLimitStats, 0, len(keys))
	for _, key := range keys {
		bucket := buckets[key]
		bucket.mu.Lock()
		bucket.refillLocked(now)
		snapshot := bucket.stats
		snapshot.Tokens = bucket.tokens
		bucket.mu.Unlock()
		snapshots = append(snapshots, snapshot)
	}
	return snapshots
}

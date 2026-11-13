package governance

import (
	"context"
	"testing"
)

func BenchmarkRateLimiterAcquire(b *testing.B) {
	limiter := NewRateLimiter(RateLimitConfig{
		Methods: map[string]RateLimitRule{
			MethodRateLimitKey("echo.v1.Echo", "Echo"): {Rate: 1e9, Burst: 1_000_000},
		},
	})
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := limiter.Acquire(ctx, "echo.v1.Echo", "Echo"); err != nil {
				b.Errorf("Acquire() error = %v", err)
				return
			}
		}
	})
}

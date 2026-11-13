package rpc

import "context"

type idempotentContextKey struct{}

// WithIdempotent marks a call as safe to retry after an ambiguous transport
// failure. The marker is carried in the request Context and never crosses the
// wire as trusted server input.
func WithIdempotent(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, idempotentContextKey{}, true)
}

// IsIdempotent reports whether a call was explicitly marked as idempotent.
func IsIdempotent(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value, _ := ctx.Value(idempotentContextKey{}).(bool)
	return value
}

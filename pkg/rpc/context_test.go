package rpc

import (
	"context"
	"testing"
)

func TestWithIdempotentMarksOnlyChildContext(t *testing.T) {
	t.Parallel()

	parent := context.Background()
	idempotent := WithIdempotent(parent)
	if !IsIdempotent(idempotent) {
		t.Fatal("idempotent marker is missing")
	}
	if IsIdempotent(parent) {
		t.Fatal("idempotent marker leaked into parent context")
	}
	var nilContext context.Context
	if WithIdempotent(nilContext) != nil || IsIdempotent(nilContext) {
		t.Fatal("nil context handling is not safe")
	}
}

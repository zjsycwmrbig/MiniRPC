package main

import (
	"testing"
	"time"
)

func TestPercentileUsesNearestRank(t *testing.T) {
	t.Parallel()

	values := []time.Duration{
		5 * time.Millisecond,
		1 * time.Millisecond,
		4 * time.Millisecond,
		2 * time.Millisecond,
		3 * time.Millisecond,
	}
	if got := percentile(values, 0.50); got != 3*time.Millisecond {
		t.Fatalf("p50 = %s, want 3ms", got)
	}
	if got := percentile(values, 0.95); got != 5*time.Millisecond {
		t.Fatalf("p95 = %s, want 5ms", got)
	}
	if got := percentile(nil, 0.99); got != 0 {
		t.Fatalf("empty p99 = %s, want 0", got)
	}
}

func TestParseSizesRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	if _, err := parseSizes("32,0,1024"); err == nil {
		t.Fatal("parseSizes accepted a non-positive size")
	}
	if sizes, err := parseSizes(" 32, 256 ,1024 "); err != nil || len(sizes) != 3 {
		t.Fatalf("parseSizes() = %v, %v", sizes, err)
	}
}

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

func TestParseTransportsRejectsInvalidInputAndDeduplicates(t *testing.T) {
	t.Parallel()

	if _, err := parseTransports("plaintext,quic"); err == nil {
		t.Fatal("parseTransports accepted an unsupported transport")
	}
	transports, err := parseTransports(" plaintext, tls, tls, mtls ")
	if err != nil {
		t.Fatalf("parseTransports() error = %v", err)
	}
	want := []benchmarkTransport{transportPlaintext, transportTLS, transportMTLS}
	if len(transports) != len(want) {
		t.Fatalf("parseTransports() = %v, want %v", transports, want)
	}
	for index := range want {
		if transports[index] != want[index] {
			t.Fatalf("parseTransports()[%d] = %q, want %q", index, transports[index], want[index])
		}
	}
}

func TestSecureBenchmarkCasesCompleteRealRPC(t *testing.T) {
	t.Parallel()

	config := RunConfig{
		Duration:    100 * time.Millisecond,
		Warmup:      20 * time.Millisecond,
		Concurrency: 4,
	}
	for _, transport := range []benchmarkTransport{transportTLS, transportMTLS} {
		transport := transport
		t.Run(string(transport), func(t *testing.T) {
			t.Parallel()
			result, err := runCase(config, secureDirectCase(transport, config.Concurrency))
			if err != nil {
				t.Fatalf("runCase(%s) error = %v", transport, err)
			}
			if result.Transport != string(transport) {
				t.Fatalf("result transport = %q, want %q", result.Transport, transport)
			}
			if result.Requests == 0 || result.Failures != 0 {
				t.Fatalf("unexpected %s workload result: %+v", transport, result)
			}
			if result.Connections != 1 {
				t.Fatalf("%s connections = %d, want 1", transport, result.Connections)
			}
		})
	}
}

func TestConnectionSetupMeasuresPlaintextTLSAndMTLS(t *testing.T) {
	t.Parallel()

	for _, transport := range []benchmarkTransport{transportPlaintext, transportTLS, transportMTLS} {
		transport := transport
		t.Run(string(transport), func(t *testing.T) {
			t.Parallel()
			result, err := measureConnectionSetup(transport, 5)
			if err != nil {
				t.Fatalf("measureConnectionSetup(%s) error = %v", transport, err)
			}
			if result.Samples != 5 || result.Failures != 0 || result.P50 <= 0 {
				t.Fatalf("unexpected %s setup result: %+v", transport, result)
			}
			if result.P50 > result.P99 || result.P99 > result.Max {
				t.Fatalf("%s setup percentiles are inconsistent: %+v", transport, result)
			}
		})
	}
}

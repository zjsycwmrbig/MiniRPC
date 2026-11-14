package main

import (
	"sync/atomic"
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
	transports, err := parseTransports(" plaintext, tls, tls, mtls, h2c, h2-tls, h2-mtls ")
	if err != nil {
		t.Fatalf("parseTransports() error = %v", err)
	}
	want := []benchmarkTransport{
		transportPlaintext,
		transportTLS,
		transportMTLS,
		transportH2C,
		transportH2TLS,
		transportH2MTLS,
	}
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

func TestHTTP2BenchmarkCasesCompleteRealRPC(t *testing.T) {
	t.Parallel()

	config := RunConfig{
		Duration:    100 * time.Millisecond,
		Warmup:      20 * time.Millisecond,
		Concurrency: 8,
	}
	for _, transport := range []benchmarkTransport{
		transportH2C,
		transportH2TLS,
		transportH2MTLS,
	} {
		transport := transport
		t.Run(string(transport), func(t *testing.T) {
			t.Parallel()
			result, err := runCase(config, http2DirectCase(transport, config.Concurrency, nil))
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

func TestHTTP2ExternalTargetCaseCompletesRealRPC(t *testing.T) {
	t.Parallel()

	target, err := startBenchmarkHTTP2Server(&atomic.Uint64{}, nil)
	if err != nil {
		t.Fatalf("startBenchmarkHTTP2Server() error = %v", err)
	}
	defer func() { _ = target.stop() }()
	result, err := runCase(
		RunConfig{Duration: 100 * time.Millisecond, Concurrency: 4},
		http2DirectCase(transportH2C, 4, &externalHTTP2Target{address: target.address}),
	)
	if err != nil {
		t.Fatalf("runCase(external h2c) error = %v", err)
	}
	if result.Mode != "external-direct-single-connection-h2c" {
		t.Fatalf("result mode = %q", result.Mode)
	}
	if result.Requests == 0 || result.Failures != 0 || result.Connections != 1 {
		t.Fatalf("unexpected external h2c workload result: %+v", result)
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

func TestConnectionSetupMeasuresHTTP2Transports(t *testing.T) {
	t.Parallel()

	for _, transport := range []benchmarkTransport{
		transportH2C,
		transportH2TLS,
		transportH2MTLS,
	} {
		transport := transport
		t.Run(string(transport), func(t *testing.T) {
			t.Parallel()
			result, err := measureHTTP2ConnectionSetup(transport, 5, nil)
			if err != nil {
				t.Fatalf("measureHTTP2ConnectionSetup(%s) error = %v", transport, err)
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

func TestExternalHTTP2TargetValidation(t *testing.T) {
	t.Parallel()

	if _, err := loadExternalHTTP2Target(
		[]benchmarkTransport{transportPlaintext},
		"127.0.0.1:8080",
		"", "", "", "", "1.2",
	); err == nil {
		t.Fatal("external TCP target was accepted")
	}
	if _, err := loadExternalHTTP2Target(
		[]benchmarkTransport{transportH2C, transportH2TLS},
		"127.0.0.1:8080",
		"", "", "", "", "1.2",
	); err == nil {
		t.Fatal("multiple external transports were accepted")
	}
	if _, err := loadExternalHTTP2Target(
		[]benchmarkTransport{transportH2C},
		"127.0.0.1:8080",
		"ca.pem", "", "", "", "1.2",
	); err == nil {
		t.Fatal("h2c external target accepted TLS options")
	}
	if _, err := loadExternalHTTP2Target(
		[]benchmarkTransport{transportH2MTLS},
		"127.0.0.1:8443",
		"", "", "", "localhost", "1.2",
	); err == nil {
		t.Fatal("external h2-mtls target accepted without a client certificate")
	}
	target, err := loadExternalHTTP2Target(
		[]benchmarkTransport{transportH2TLS},
		"127.0.0.1:8443",
		"", "", "", "localhost", "1.3",
	)
	if err != nil {
		t.Fatalf("loadExternalHTTP2Target(h2-tls) error = %v", err)
	}
	if target.address != "127.0.0.1:8443" || target.tlsConfig == nil {
		t.Fatalf("unexpected external h2-tls target: %+v", target)
	}
}

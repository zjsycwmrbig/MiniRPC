package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/governance"
	"github.com/zjsycwmrbig/mini-rpc/internal/observability"
	"github.com/zjsycwmrbig/mini-rpc/internal/registry"
	"github.com/zjsycwmrbig/mini-rpc/internal/security"
	"github.com/zjsycwmrbig/mini-rpc/internal/server"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func main() {
	address := flag.String("addr", "127.0.0.1:8080", "TCP listen address")
	shutdownTimeout := flag.Duration("shutdown-timeout", 5*time.Second, "graceful shutdown timeout")
	etcdEndpoints := flag.String("etcd-endpoints", "", "comma-separated etcd endpoints")
	instanceID := flag.String("instance-id", "", "registry instance ID")
	advertiseAddr := flag.String("advertise-addr", "", "address published to the registry")
	leaseTTL := flag.Duration("lease-ttl", 10*time.Second, "registry lease TTL")
	maxConnections := flag.Int("max-connections", 1024, "maximum simultaneous client connections")
	maxConcurrent := flag.Int("max-concurrent-requests", 1024, "maximum simultaneous handlers")
	maxRequestBody := flag.Int("max-request-body-bytes", 8<<20, "maximum decoded request body size")
	rate := flag.Float64("rate", 0, "server-side service rate limit in requests per second; 0 disables")
	burst := flag.Int("burst", 1, "server-side rate-limit burst capacity")
	observabilityEnabled := flag.Bool("observability", true, "enable structured logs, trace correlation, and metrics")
	debug := flag.Bool("debug", false, "emit verbose observability start events")
	metricsAddr := flag.String("metrics-addr", "127.0.0.1:9090", "Prometheus metrics listen address; empty disables")
	tlsCert := flag.String("tls-cert", "", "PEM server certificate; enables TLS when -tls-key is set")
	tlsKey := flag.String("tls-key", "", "PEM server private key")
	tlsClientCA := flag.String("tls-client-ca", "", "PEM client CA; enables mutual TLS when set")
	tlsMinVersion := flag.String("tls-min-version", "1.2", "minimum RPC TLS version: 1.2 or 1.3")
	tlsHandshakeTimeout := flag.Duration("tls-handshake-timeout", 10*time.Second, "maximum TLS handshake duration")
	tlsReloadInterval := flag.Duration("tls-reload-interval", security.DefaultReloadInterval, "TLS material polling interval; 0 disables polling")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/HTTP trace endpoint URL; empty disables trace export")
	otlpInsecure := flag.Bool("otlp-insecure", false, "allow plaintext OTLP/HTTP; required for an http endpoint")
	otlpHeaders := flag.String("otlp-headers", "", "comma-separated static OTLP HTTP headers in key=value form")
	otlpSampleRatio := flag.Float64("otlp-sample-ratio", 1, "root trace sampling ratio from 0 to 1")
	otlpExportTimeout := flag.Duration("otlp-export-timeout", 5*time.Second, "maximum OTLP batch export duration")
	flag.Parse()
	if *tlsHandshakeTimeout <= 0 {
		log.Fatal("tls-handshake-timeout must be positive")
	}
	if *tlsReloadInterval < 0 {
		log.Fatal("tls-reload-interval cannot be negative")
	}
	if *otlpSampleRatio < 0 || *otlpSampleRatio > 1 {
		log.Fatal("otlp-sample-ratio must be between 0 and 1")
	}
	if *otlpExportTimeout <= 0 {
		log.Fatal("otlp-export-timeout must be positive")
	}
	if *otlpEndpoint != "" && !*observabilityEnabled {
		log.Fatal("otlp trace export requires -observability=true")
	}
	minVersion, minVersionErr := security.ParseMinVersion(*tlsMinVersion)
	if minVersionErr != nil {
		log.Fatal(minVersionErr)
	}

	listener, err := net.Listen("tcp", *address)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	runtimeInstance := *instanceID
	if runtimeInstance == "" {
		runtimeInstance = defaultInstanceID(listener.Addr())
	}
	headers, err := observability.ParseOTLPHeaders(*otlpHeaders)
	if err != nil {
		_ = listener.Close()
		log.Fatal(err)
	}
	var traceExporter sdktrace.SpanExporter
	if *otlpEndpoint != "" {
		traceExporter, err = observability.NewOTLPTraceExporter(context.Background(), observability.OTLPExporterConfig{
			Endpoint: *otlpEndpoint,
			Insecure: *otlpInsecure,
			Headers:  headers,
			Timeout:  *otlpExportTimeout,
		})
		if err != nil {
			_ = listener.Close()
			log.Fatalf("configure OTLP trace exporter: %v", err)
		}
	}
	observer := observability.New(observability.Config{
		Enabled:            *observabilityEnabled,
		Debug:              *debug,
		Instance:           runtimeInstance,
		ServiceName:        "mini-rpc-echo-server",
		Output:             os.Stderr,
		TraceExporter:      traceExporter,
		TraceSampleRatio:   *otlpSampleRatio,
		TraceExportTimeout: *otlpExportTimeout,
	})
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), *otlpExportTimeout)
		defer cancel()
		if err := observer.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown trace exporter: %v", err)
		}
	}()
	observer.Metrics().SetRegistryInstances(echov1.EchoServiceName, 0)

	var metricsServer *http.Server
	var metricsDone <-chan error
	if observer.Enabled() && *metricsAddr != "" {
		metricsServer, metricsDone, err = startMetricsServer(*metricsAddr, observer)
		if err != nil {
			_ = listener.Close()
			log.Fatalf("start metrics server: %v", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tlsReloader, tlsErr := security.NewReloadableServerConfig(
		*tlsCert,
		*tlsKey,
		*tlsClientCA,
		minVersion,
		security.ReloadOptions{
			Interval: *tlsReloadInterval,
			OnReload: func(event security.ReloadEvent) {
				success := event.Error == nil
				observer.Metrics().RecordTLSReload("server", success)
				if success {
					observer.Metrics().SetTLSConfigVersion("server", event.Stats.Version)
					log.Printf(
						"TLS server material reloaded: version=%d attempts=%d",
						event.Stats.Version,
						event.Stats.Attempts,
					)
					return
				}
				log.Printf(
					"TLS server material reload failed; keeping version=%d: %v",
					event.Stats.Version,
					event.Error,
				)
			},
		},
	)
	if tlsErr != nil {
		_ = listener.Close()
		log.Fatalf("load TLS configuration: %v", tlsErr)
	}

	options := []server.Option{
		server.WithMaxConnections(*maxConnections),
		server.WithMaxConcurrentRequests(*maxConcurrent),
		server.WithMaxRequestBodyBytes(*maxRequestBody),
		server.WithObserver(observer),
		server.WithTLSHandshakeTimeout(*tlsHandshakeTimeout),
	}
	if tlsReloader != nil {
		observer.Metrics().SetTLSConfigVersion("server", tlsReloader.Stats().Version)
		options = append(options, server.WithTLSConfigSource(tlsReloader.Current))
		if *tlsReloadInterval > 0 {
			go tlsReloader.Run(ctx)
		}
	}
	if *rate > 0 {
		options = append(options, server.WithRateLimits(governance.RateLimitConfig{
			Services: map[string]governance.RateLimitRule{
				echov1.EchoServiceName: {Rate: *rate, Burst: *burst},
			},
		}))
	}
	rpcServer, err := server.NewWithOptions(options...)
	if err != nil {
		_ = listener.Close()
		log.Fatalf("configure server: %v", err)
	}
	if err := echov1.RegisterEchoServer(rpcServer, echoServer{}); err != nil {
		_ = listener.Close()
		log.Fatalf("register echo service: %v", err)
	}

	var backend *registry.EtcdRegistry
	var lease registry.Lease
	if *etcdEndpoints != "" {
		backend, err = registry.DialEtcd(
			ctx,
			splitEndpoints(*etcdEndpoints),
			3*time.Second,
		)
		if err != nil {
			log.Fatalf("connect etcd: %v", err)
		}
		defer func() { _ = backend.Close() }()

		id := *instanceID
		if id == "" {
			id = runtimeInstance
		}
		publishedAddress := *advertiseAddr
		if publishedAddress == "" {
			publishedAddress = listener.Addr().String()
		}
		lease, err = backend.Register(ctx, registry.Instance{
			ID:      id,
			Service: echov1.EchoServiceName,
			Address: publishedAddress,
			Weight:  1,
		}, *leaseTTL)
		if err != nil {
			log.Fatalf("register service: %v", err)
		}
		fmt.Printf(
			"Registered %s/%s at %s\n",
			echov1.EchoServiceName,
			id,
			publishedAddress,
		)
		observer.Metrics().SetRegistryInstances(echov1.EchoServiceName, 1)
	}

	fmt.Printf("Mini-RPC echo server listening on %s\n", listener.Addr())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- rpcServer.Serve(listener)
	}()

	select {
	case err := <-serveDone:
		if err != nil && !errors.Is(err, server.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	case <-ctx.Done():
		if lease != nil {
			revokeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := lease.Revoke(revokeCtx); err != nil {
				log.Printf("revoke registry lease: %v", err)
			}
			cancel()
		}
		observer.Metrics().SetRegistryInstances(echov1.EchoServiceName, 0)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
		defer cancel()
		if err := rpcServer.GracefulStop(shutdownCtx); err != nil {
			log.Printf("graceful stop: %v", err)
		}
		if err := <-serveDone; err != nil && !errors.Is(err, server.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
		stopMetricsServer(metricsServer, metricsDone)
	}
}

func startMetricsServer(
	address string,
	observer *observability.Runtime,
) (*http.Server, <-chan error, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", observability.PrometheusHandler(observer))
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Printf("Prometheus metrics listening on http://%s/metrics\n", listener.Addr())
	return server, done, nil
}

func stopMetricsServer(server *http.Server, done <-chan error) {
	if server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown metrics server: %v", err)
	}
	if done != nil {
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("metrics serve: %v", err)
			}
		case <-ctx.Done():
			log.Printf("metrics server shutdown timed out")
		}
	}
}

type echoServer struct{}

func (echoServer) Echo(_ context.Context, request *echov1.EchoRequest) (*echov1.EchoResponse, error) {
	return &echov1.EchoResponse{Message: request.GetMessage()}, nil
}

func splitEndpoints(value string) []string {
	parts := strings.Split(value, ",")
	endpoints := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			endpoints = append(endpoints, trimmed)
		}
	}
	return endpoints
}

func defaultInstanceID(address net.Addr) string {
	host, _ := os.Hostname()
	if host == "" {
		host = "echo-server"
	}
	return host + "-" + strings.ReplaceAll(address.String(), ":", "-")
}

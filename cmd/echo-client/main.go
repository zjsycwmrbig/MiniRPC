package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/client"
	"github.com/zjsycwmrbig/mini-rpc/internal/discovery"
	"github.com/zjsycwmrbig/mini-rpc/internal/governance"
	"github.com/zjsycwmrbig/mini-rpc/internal/observability"
	"github.com/zjsycwmrbig/mini-rpc/internal/registry"
	"github.com/zjsycwmrbig/mini-rpc/internal/resolver"
	"github.com/zjsycwmrbig/mini-rpc/internal/security"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
)

func main() {
	address := flag.String("addr", "127.0.0.1:8080", "TCP server address")
	message := flag.String("message", "hello from Mini-RPC", "message sent to the Echo service")
	timeout := flag.Duration("timeout", 5*time.Second, "request timeout")
	rate := flag.Float64("rate", 0, "client-side service rate limit in requests per second; 0 disables")
	burst := flag.Int("burst", 1, "client-side rate-limit burst capacity")
	observabilityEnabled := flag.Bool("observability", true, "enable structured logs and trace propagation")
	debug := flag.Bool("debug", false, "emit verbose observability start events")
	instance := flag.String("instance", "echo-client", "instance name included in structured logs")
	etcdEndpoints := flag.String("etcd-endpoints", "", "comma-separated etcd endpoints for discovery mode")
	retryAttempts := flag.Int("retry-attempts", 3, "maximum attempts for idempotent calls")
	retryBackoff := flag.Duration("retry-backoff", 50*time.Millisecond, "initial retry backoff")
	resolverBackoff := flag.Duration("resolver-backoff", 50*time.Millisecond, "initial watch reconnect backoff")
	tlsCA := flag.String("tls-ca", "", "PEM CA bundle used to verify RPC servers")
	tlsCert := flag.String("tls-cert", "", "PEM client certificate for mutual TLS")
	tlsKey := flag.String("tls-key", "", "PEM private key paired with -tls-cert")
	tlsServerName := flag.String("tls-server-name", "", "expected TLS server name; empty derives it from each instance address")
	tlsMinVersion := flag.String("tls-min-version", "1.2", "minimum RPC TLS version: 1.2 or 1.3")
	tlsReloadInterval := flag.Duration("tls-reload-interval", security.DefaultReloadInterval, "TLS material polling interval; 0 disables polling")
	flag.Parse()
	if *retryAttempts <= 0 || *retryBackoff <= 0 || *resolverBackoff <= 0 || *tlsReloadInterval < 0 {
		log.Fatal("retry-attempts, retry-backoff, and resolver-backoff must be positive; tls-reload-interval cannot be negative")
	}
	minVersion, minVersionErr := security.ParseMinVersion(*tlsMinVersion)
	if minVersionErr != nil {
		log.Fatal(minVersionErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	ctx = rpc.WithIdempotent(ctx)

	observer := observability.New(observability.Config{
		Enabled:  *observabilityEnabled,
		Debug:    *debug,
		Instance: *instance,
		Output:   os.Stderr,
	})
	tlsReloader, tlsErr := security.NewReloadableClientConfig(
		*tlsCA,
		*tlsCert,
		*tlsKey,
		*tlsServerName,
		minVersion,
		security.ReloadOptions{
			Interval: *tlsReloadInterval,
			OnReload: func(event security.ReloadEvent) {
				success := event.Error == nil
				observer.Metrics().RecordTLSReload("client", success)
				if success {
					observer.Metrics().SetTLSConfigVersion("client", event.Stats.Version)
					log.Printf(
						"TLS client material reloaded: version=%d attempts=%d",
						event.Stats.Version,
						event.Stats.Attempts,
					)
					return
				}
				log.Printf(
					"TLS client material reload failed; keeping version=%d: %v",
					event.Stats.Version,
					event.Error,
				)
			},
		},
	)
	if tlsErr != nil {
		log.Fatal(tlsErr)
	}
	retryPolicy := governance.RetryPolicy{
		MaxAttempts:    *retryAttempts,
		InitialBackoff: *retryBackoff,
		MaxBackoff:     maxDuration(*retryBackoff*8, time.Second),
		MaxElapsed:     2 * time.Second,
	}
	options := []client.Option{
		client.WithObserver(observer),
		client.WithRetryPolicy(retryPolicy),
	}
	if tlsReloader != nil {
		observer.Metrics().SetTLSConfigVersion("client", tlsReloader.Stats().Version)
		options = append(options, client.WithTLSConfigSource(tlsReloader.Current))
		if *tlsReloadInterval > 0 {
			go tlsReloader.Run(ctx)
		}
	}
	if *rate > 0 {
		options = append(options, client.WithRateLimits(governance.RateLimitConfig{
			Services: map[string]governance.RateLimitRule{
				echov1.EchoServiceName: {Rate: *rate, Burst: *burst},
			},
		}))
	}
	var clientConn rpc.ClientConn
	var err error
	cleanup := func() error { return nil }
	if *etcdEndpoints != "" {
		backend, err := registry.DialEtcd(ctx, splitEndpoints(*etcdEndpoints), 3*time.Second)
		if err != nil {
			log.Fatalf("connect etcd: %v", err)
		}
		serviceResolver := resolver.New(backend, resolver.Options{
			InitialBackoff: *resolverBackoff,
			MaxBackoff:     maxDuration(*resolverBackoff*8, time.Second),
		})
		discoveryClient, err := discovery.New(ctx, discovery.Config{
			Service:       echov1.EchoServiceName,
			Resolver:      serviceResolver,
			Balancer:      nil,
			Retry:         retryPolicy,
			CallTimeout:   *timeout,
			ClientOptions: options,
			Observer:      observer,
		})
		if err != nil {
			_ = serviceResolver.Close()
			_ = backend.Close()
			log.Fatalf("discover service: %v", err)
		}
		clientConn = discoveryClient
		cleanup = func() error {
			return errors.Join(
				discoveryClient.Close(),
				serviceResolver.Close(),
				backend.Close(),
			)
		}
	} else {
		clientConn, err = client.Dial(ctx, *address, options...)
		if err != nil {
			log.Fatalf("dial: %v", err)
		}
		cleanup = clientConn.Close
	}
	defer func() {
		if err := cleanup(); err != nil {
			log.Printf("close: %v", err)
		}
	}()

	echoClient := echov1.NewEchoClient(clientConn)
	response, err := echoClient.Echo(ctx, &echov1.EchoRequest{Message: *message})
	if err != nil {
		log.Fatalf("echo: %v", err)
	}
	fmt.Println(response.GetMessage())
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

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
}

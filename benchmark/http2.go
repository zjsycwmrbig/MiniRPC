package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/client"
	"github.com/zjsycwmrbig/mini-rpc/internal/security"
)

type externalHTTP2Target struct {
	address   string
	tlsConfig *tls.Config
}

func loadExternalHTTP2Target(
	transports []benchmarkTransport,
	address string,
	caFile string,
	certFile string,
	keyFile string,
	serverName string,
	minVersion string,
) (*externalHTTP2Target, error) {
	if address == "" {
		if caFile != "" || certFile != "" || keyFile != "" || serverName != "" {
			return nil, fmt.Errorf("external target TLS options require -target")
		}
		return nil, nil
	}
	if len(transports) != 1 || !isHTTP2Transport(transports[0]) {
		return nil, fmt.Errorf("-target requires exactly one HTTP/2 transport: h2c, h2-tls, or h2-mtls")
	}
	transport := transports[0]
	if transport == transportH2C {
		if caFile != "" || certFile != "" || keyFile != "" || serverName != "" {
			return nil, fmt.Errorf("h2c external target does not accept TLS options")
		}
		return &externalHTTP2Target{address: address}, nil
	}
	version, err := security.ParseMinVersion(minVersion)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := security.LoadClientConfig(caFile, certFile, keyFile, serverName, version)
	if err != nil {
		return nil, err
	}
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: version}
	}
	if transport == transportH2MTLS && len(tlsConfig.Certificates) == 0 {
		return nil, fmt.Errorf("h2-mtls external target requires -target-cert and -target-key")
	}
	return &externalHTTP2Target{address: address, tlsConfig: tlsConfig}, nil
}

func http2DirectCase(
	transport benchmarkTransport,
	concurrency int,
	external *externalHTTP2Target,
) Case {
	name := fmt.Sprintf("%s/serial/1conn/1024B", transport)
	mode := "direct-" + string(transport)
	if concurrency > 1 {
		name = fmt.Sprintf("%s/concurrent/1conn/1024B/c%d", transport, concurrency)
		mode = "direct-single-connection-" + string(transport)
	}
	if external != nil {
		mode = "external-" + mode
	}
	return Case{
		Name:        name,
		Mode:        mode,
		Transport:   transport,
		BodyBytes:   1024,
		Concurrency: concurrency,
		Instances:   1,
		Setup: func(ctx context.Context) (*benchmarkClient, error) {
			calls := &atomic.Uint64{}
			var target *benchmarkServer
			var clientTLS *tls.Config
			address := ""
			if external == nil {
				serverTLS, generatedClientTLS, tlsErr := benchmarkTLSConfigs(transport)
				if tlsErr != nil {
					return nil, tlsErr
				}
				clientTLS = generatedClientTLS
				var err error
				target, err = startBenchmarkHTTP2Server(calls, serverTLS)
				if err != nil {
					return nil, err
				}
				address = target.address
			} else {
				var err error
				clientTLS, err = configuredHTTP2TLS(transport, external)
				if err != nil {
					return nil, err
				}
				address = external.address
			}

			options := directClientOptions(1, concurrency, false)
			options = append(options, client.WithHTTP2())
			if clientTLS != nil {
				options = append(options, client.WithTLSConfig(clientTLS))
			}
			rpcClient, err := client.DialHTTP2(ctx, address, options...)
			if err != nil {
				if target != nil {
					_ = target.stop()
				}
				return nil, err
			}
			return &benchmarkClient{
				conn:        rpcClient,
				connections: func() int { return rpcClient.Stats().Connections },
				instances:   1,
				close: func() error {
					clientErr := rpcClient.Close()
					if target == nil {
						return clientErr
					}
					return errors.Join(clientErr, target.stop())
				},
			}, nil
		},
	}
}

func configuredHTTP2TLS(
	transport benchmarkTransport,
	external *externalHTTP2Target,
) (*tls.Config, error) {
	if transport == transportH2C {
		if external != nil && external.tlsConfig != nil {
			return nil, fmt.Errorf("h2c external target must not use TLS")
		}
		return nil, nil
	}
	if !transportUsesTLS(transport) {
		return nil, fmt.Errorf("unsupported HTTP/2 benchmark transport %q", transport)
	}
	if external == nil {
		_, clientTLS, err := benchmarkTLSConfigs(transport)
		return clientTLS, err
	}
	if external.tlsConfig == nil {
		return nil, fmt.Errorf("%s external target requires a TLS client configuration", transport)
	}
	return external.tlsConfig.Clone(), nil
}

func measureHTTP2ConnectionSetup(
	transport benchmarkTransport,
	samples int,
	external *externalHTTP2Target,
) (result HandshakeResult, returnErr error) {
	var target *benchmarkServer
	var clientTLS *tls.Config
	address := ""
	if external == nil {
		serverTLS, generatedClientTLS, tlsErr := benchmarkTLSConfigs(transport)
		if tlsErr != nil {
			return HandshakeResult{}, tlsErr
		}
		clientTLS = generatedClientTLS
		var err error
		target, err = startBenchmarkHTTP2Server(&atomic.Uint64{}, serverTLS)
		if err != nil {
			return HandshakeResult{}, fmt.Errorf("start %s setup server: %w", transport, err)
		}
		address = target.address
		defer func() {
			returnErr = errors.Join(returnErr, target.stop())
		}()
	} else {
		var err error
		clientTLS, err = configuredHTTP2TLS(transport, external)
		if err != nil {
			return HandshakeResult{}, err
		}
		address = external.address
	}

	latencies := make([]time.Duration, 0, samples)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for sample := 0; sample < samples; sample++ {
		options := []client.Option{
			client.WithDialTimeout(2 * time.Second),
			client.WithObserver(nil),
			client.WithHTTP2(),
		}
		if clientTLS != nil {
			options = append(options, client.WithTLSConfig(clientTLS))
		}
		started := time.Now()
		rpcClient, dialErr := client.DialHTTP2(ctx, address, options...)
		latency := time.Since(started)
		if dialErr != nil {
			return HandshakeResult{}, fmt.Errorf(
				"%s connection setup sample %d: %w",
				transport,
				sample,
				dialErr,
			)
		}
		latencies = append(latencies, latency)
		if _, callErr := echov1.NewEchoClient(rpcClient).Echo(
			ctx,
			&echov1.EchoRequest{Message: "connection-setup"},
		); callErr != nil {
			_ = rpcClient.Close()
			return HandshakeResult{}, fmt.Errorf(
				"%s connection setup verification %d: %w",
				transport,
				sample,
				callErr,
			)
		}
		_ = rpcClient.Close()
	}
	result = HandshakeResult{
		Transport: string(transport),
		Samples:   samples,
		P50:       percentile(latencies, 0.50),
		P95:       percentile(latencies, 0.95),
		P99:       percentile(latencies, 0.99),
	}
	for _, latency := range latencies {
		if latency > result.Max {
			result.Max = latency
		}
	}
	return result, nil
}

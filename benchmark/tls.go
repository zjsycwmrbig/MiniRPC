package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/client"
	"github.com/zjsycwmrbig/mini-rpc/internal/testkit"
)

func benchmarkTLSConfigs(transport benchmarkTransport) (*tls.Config, *tls.Config, error) {
	if transport == transportPlaintext {
		return nil, nil, nil
	}
	if transport != transportTLS && transport != transportMTLS {
		return nil, nil, fmt.Errorf("unsupported benchmark transport %q", transport)
	}

	material, err := testkit.NewTLSMaterial("127.0.0.1")
	if err != nil {
		return nil, nil, fmt.Errorf("generate benchmark TLS material: %w", err)
	}
	serverCertificate, err := tls.X509KeyPair(material.ServerCertPEM, material.ServerKeyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parse benchmark server certificate: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(material.CAPEM) {
		return nil, nil, fmt.Errorf("parse benchmark CA certificate")
	}
	serverConfig := &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		MinVersion:   tls.VersionTLS12,
	}
	clientConfig := &tls.Config{
		RootCAs:    caPool,
		MinVersion: tls.VersionTLS12,
	}
	if transport == transportMTLS {
		clientCertificate, err := tls.X509KeyPair(material.ClientCertPEM, material.ClientKeyPEM)
		if err != nil {
			return nil, nil, fmt.Errorf("parse benchmark client certificate: %w", err)
		}
		serverConfig.ClientAuth = tls.RequireAndVerifyClientCert
		serverConfig.ClientCAs = caPool
		clientConfig.Certificates = []tls.Certificate{clientCertificate}
	}
	return serverConfig, clientConfig, nil
}

func measureConnectionSetup(transport benchmarkTransport, samples int) (result HandshakeResult, returnErr error) {
	serverTLS, clientTLS, err := benchmarkTLSConfigs(transport)
	if err != nil {
		return HandshakeResult{}, err
	}
	target, err := startBenchmarkServerWithTLS(&atomic.Uint64{}, serverTLS)
	if err != nil {
		return HandshakeResult{}, fmt.Errorf("start %s setup server: %w", transport, err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, target.stop())
	}()

	latencies := make([]time.Duration, 0, samples)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for sample := 0; sample < samples; sample++ {
		options := []client.Option{
			client.WithDialTimeout(2 * time.Second),
			client.WithObserver(nil),
		}
		if clientTLS != nil {
			options = append(options, client.WithTLSConfig(clientTLS))
		}
		started := time.Now()
		rpcClient, dialErr := client.Dial(ctx, target.address, options...)
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

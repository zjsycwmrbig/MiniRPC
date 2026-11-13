package client_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/client"
	"github.com/zjsycwmrbig/mini-rpc/internal/security"
	"github.com/zjsycwmrbig/mini-rpc/internal/server"
	"github.com/zjsycwmrbig/mini-rpc/internal/testkit"
)

func TestTLSCallUsesVerifiedServerCertificate(t *testing.T) {
	t.Parallel()

	serverConfig, clientConfig := testTLSConfigs(t, false)
	address, _ := startTestServerWithOptions(
		t,
		echoServer{},
		server.WithTLSConfig(serverConfig),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	clientConn, err := client.Dial(ctx, address, client.WithTLSConfig(clientConfig))
	if err != nil {
		t.Fatalf("client.Dial(TLS) error = %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	response, err := echov1.NewEchoClient(clientConn).Echo(
		ctx,
		&echov1.EchoRequest{Message: "hello tls"},
	)
	if err != nil {
		t.Fatalf("Echo(TLS) error = %v", err)
	}
	if response.GetMessage() != "hello tls" {
		t.Fatalf("Echo(TLS) message = %q", response.GetMessage())
	}
}

func TestMutualTLSCallAndConcurrentMultiplexing(t *testing.T) {
	t.Parallel()

	serverConfig, clientConfig := testTLSConfigs(t, true)
	address, _ := startTestServerWithOptions(
		t,
		echoServer{},
		server.WithTLSConfig(serverConfig),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientConn, err := client.Dial(ctx, address, client.WithTLSConfig(clientConfig))
	if err != nil {
		t.Fatalf("client.Dial(mTLS) error = %v", err)
	}
	defer func() { _ = clientConn.Close() }()
	echoClient := echov1.NewEchoClient(clientConn)

	const calls = 64
	var waitGroup sync.WaitGroup
	errorsByCall := make([]error, calls)
	waitGroup.Add(calls)
	for index := range calls {
		index := index
		go func() {
			defer waitGroup.Done()
			message := fmt.Sprintf("mutual-%d", index)
			callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer callCancel()
			response, callErr := echoClient.Echo(callCtx, &echov1.EchoRequest{Message: message})
			if callErr == nil && response.GetMessage() != message {
				callErr = fmt.Errorf("message = %q, want %q", response.GetMessage(), message)
			}
			errorsByCall[index] = callErr
		}()
	}
	waitGroup.Wait()
	for index, callErr := range errorsByCall {
		if callErr != nil {
			t.Fatalf("mutual TLS call %d error = %v", index, callErr)
		}
	}
	if stats := clientConn.Stats(); stats.Connections != 1 {
		t.Fatalf("TLS connections = %d, want 1 multiplexed connection", stats.Connections)
	}
}

func TestMutualTLSRejectsClientWithoutCertificate(t *testing.T) {
	t.Parallel()

	serverConfig, clientConfig := testTLSConfigs(t, true)
	clientConfig.Certificates = nil
	address, testServer := startTestServerWithOptions(
		t,
		echoServer{},
		server.WithTLSConfig(serverConfig),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	clientConn, err := client.Dial(ctx, address, client.WithTLSConfig(clientConfig))
	if err != nil {
		waitForServerStats(t, testServer, func(stats server.Stats) bool {
			return stats.TLSHandshakeFailures >= 1
		})
		return
	}
	defer func() { _ = clientConn.Close() }()
	if _, err := echov1.NewEchoClient(clientConn).Echo(
		ctx,
		&echov1.EchoRequest{Message: "must fail"},
	); err == nil {
		t.Fatal("mTLS server accepted a client without a certificate")
	}
	waitForServerStats(t, testServer, func(stats server.Stats) bool {
		return stats.TLSHandshakeFailures >= 1
	})
}

func TestTLSServerRejectsPlaintextClient(t *testing.T) {
	t.Parallel()

	serverConfig, _ := testTLSConfigs(t, false)
	address, testServer := startTestServerWithOptions(
		t,
		echoServer{},
		server.WithTLSConfig(serverConfig),
		server.WithTLSHandshakeTimeout(time.Second),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	plainClient, err := client.Dial(ctx, address)
	if err != nil {
		t.Fatalf("plaintext Dial() error = %v", err)
	}
	defer func() { _ = plainClient.Close() }()
	_, err = echov1.NewEchoClient(plainClient).Echo(
		ctx,
		&echov1.EchoRequest{Message: "must fail"},
	)
	if err == nil {
		t.Fatal("plaintext Echo() succeeded against a TLS-only server")
	}
	waitForServerStats(t, testServer, func(stats server.Stats) bool {
		return stats.TLSHandshakeFailures >= 1
	})
}

func TestTLSRejectsUntrustedServerCertificate(t *testing.T) {
	t.Parallel()

	serverConfig, _ := testTLSConfigs(t, false)
	_, untrustedClientConfig := testTLSConfigs(t, false)
	address, testServer := startTestServerWithOptions(
		t,
		echoServer{},
		server.WithTLSConfig(serverConfig),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	clientConn, err := client.Dial(ctx, address, client.WithTLSConfig(untrustedClientConfig))
	if err != nil {
		waitForServerStats(t, testServer, func(stats server.Stats) bool {
			return stats.TLSHandshakeFailures >= 1
		})
		return
	}
	defer func() { _ = clientConn.Close() }()
	if _, err := echov1.NewEchoClient(clientConn).Echo(
		ctx,
		&echov1.EchoRequest{Message: "must fail"},
	); err == nil {
		t.Fatal("TLS client accepted a server certificate signed by an untrusted CA")
	}
	waitForServerStats(t, testServer, func(stats server.Stats) bool {
		return stats.TLSHandshakeFailures >= 1
	})
}

func TestTLSGracefulStopClosesActiveTransport(t *testing.T) {
	t.Parallel()

	serverConfig, clientConfig := testTLSConfigs(t, false)
	address, testServer := startTestServerWithOptions(
		t,
		echoServer{},
		server.WithTLSConfig(serverConfig),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	clientConn, err := client.Dial(ctx, address, client.WithTLSConfig(clientConfig))
	if err != nil {
		t.Fatalf("client.Dial(TLS) error = %v", err)
	}
	defer func() { _ = clientConn.Close() }()
	if _, err := echov1.NewEchoClient(clientConn).Echo(
		ctx,
		&echov1.EchoRequest{Message: "before stop"},
	); err != nil {
		t.Fatalf("Echo(TLS before stop) error = %v", err)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := testServer.GracefulStop(stopCtx); err != nil {
		t.Fatalf("GracefulStop(TLS) error = %v", err)
	}
}

func TestClientRejectsInsecureCertificateVerification(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	clientConn, err := client.Dial(
		ctx,
		"127.0.0.1:1",
		client.WithTLSConfig(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // verifies the API rejects this configuration.
	)
	if err == nil {
		_ = clientConn.Close()
		t.Fatal("client.Dial accepted InsecureSkipVerify")
	}
}

func TestReloadableTLSRotatesNewHandshakesWithoutInterruptingOldConnection(t *testing.T) {
	t.Parallel()

	first, err := testkit.NewTLSMaterial("localhost", "127.0.0.1")
	if err != nil {
		t.Fatalf("first NewTLSMaterial() error = %v", err)
	}
	second, err := testkit.NewTLSMaterial("localhost", "127.0.0.1")
	if err != nil {
		t.Fatalf("second NewTLSMaterial() error = %v", err)
	}
	directory := t.TempDir()
	serverCert := filepath.Join(directory, "server.crt")
	serverKey := filepath.Join(directory, "server.key")
	clientCA := filepath.Join(directory, "client-ca.crt")
	clientCert := filepath.Join(directory, "client.crt")
	clientKey := filepath.Join(directory, "client.key")
	writeTLSMaterial(t, first, serverCert, serverKey, clientCA, clientCert, clientKey)

	serverReloader, err := security.NewReloadableServerConfig(
		serverCert,
		serverKey,
		clientCA,
		security.DefaultMinVersion,
		security.ReloadOptions{Interval: time.Hour},
	)
	if err != nil {
		t.Fatalf("NewReloadableServerConfig() error = %v", err)
	}
	clientReloader, err := security.NewReloadableClientConfig(
		clientCA,
		clientCert,
		clientKey,
		"localhost",
		security.DefaultMinVersion,
		security.ReloadOptions{Interval: time.Hour},
	)
	if err != nil {
		t.Fatalf("NewReloadableClientConfig() error = %v", err)
	}
	address, _ := startTestServerWithOptions(
		t,
		echoServer{},
		server.WithTLSConfigSource(serverReloader.Current),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	oldClient, err := client.Dial(ctx, address, client.WithTLSConfigSource(clientReloader.Current))
	if err != nil {
		t.Fatalf("initial Dial(reloadable TLS) error = %v", err)
	}
	defer func() { _ = oldClient.Close() }()
	if _, err := echov1.NewEchoClient(oldClient).Echo(
		ctx,
		&echov1.EchoRequest{Message: "before rotation"},
	); err != nil {
		t.Fatalf("Echo(before rotation) error = %v", err)
	}

	writeTLSMaterial(t, second, serverCert, serverKey, clientCA, clientCert, clientKey)
	if _, err := serverReloader.Poll(); err != nil {
		t.Fatalf("server Reloader.Poll() error = %v", err)
	}
	if _, err := clientReloader.Poll(); err != nil {
		t.Fatalf("client Reloader.Poll() error = %v", err)
	}
	newClient, err := client.Dial(ctx, address, client.WithTLSConfigSource(clientReloader.Current))
	if err != nil {
		t.Fatalf("post-rotation Dial(reloadable TLS) error = %v", err)
	}
	defer func() { _ = newClient.Close() }()
	if _, err := echov1.NewEchoClient(newClient).Echo(
		ctx,
		&echov1.EchoRequest{Message: "after rotation"},
	); err != nil {
		t.Fatalf("Echo(after rotation) error = %v", err)
	}
	if _, err := echov1.NewEchoClient(oldClient).Echo(
		ctx,
		&echov1.EchoRequest{Message: "old connection survives"},
	); err != nil {
		t.Fatalf("Echo(existing connection after rotation) error = %v", err)
	}

	oldDirectory := t.TempDir()
	oldCA := filepath.Join(oldDirectory, "ca.crt")
	oldCert := filepath.Join(oldDirectory, "client.crt")
	oldKey := filepath.Join(oldDirectory, "client.key")
	writeFilePair(t, oldCA, first.CAPEM)
	writeFilePair(t, oldCert, first.ClientCertPEM)
	writeFilePair(t, oldKey, first.ClientKeyPEM)
	oldIdentity, err := security.LoadClientConfig(oldCA, oldCert, oldKey, "localhost", security.DefaultMinVersion)
	if err != nil {
		t.Fatalf("LoadClientConfig(old material) error = %v", err)
	}
	if rejected, err := client.Dial(ctx, address, client.WithTLSConfig(oldIdentity)); err == nil {
		_ = rejected.Close()
		t.Fatal("server accepted a new handshake with the retired client CA")
	}
}

func writeTLSMaterial(
	t *testing.T,
	material *testkit.TLSMaterial,
	serverCert string,
	serverKey string,
	clientCA string,
	clientCert string,
	clientKey string,
) {
	t.Helper()
	writeFilePair(t, serverCert, material.ServerCertPEM)
	writeFilePair(t, serverKey, material.ServerKeyPEM)
	writeFilePair(t, clientCA, material.CAPEM)
	writeFilePair(t, clientCert, material.ClientCertPEM)
	writeFilePair(t, clientKey, material.ClientKeyPEM)
}

func writeFilePair(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func testTLSConfigs(t *testing.T, mutual bool) (*tls.Config, *tls.Config) {
	t.Helper()
	material, err := testkit.NewTLSMaterial("localhost", "127.0.0.1")
	if err != nil {
		t.Fatalf("NewTLSMaterial() error = %v", err)
	}
	serverCertificate, err := tls.X509KeyPair(material.ServerCertPEM, material.ServerKeyPEM)
	if err != nil {
		t.Fatalf("parse server certificate: %v", err)
	}
	clientCertificate, err := tls.X509KeyPair(material.ClientCertPEM, material.ClientKeyPEM)
	if err != nil {
		t.Fatalf("parse client certificate: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(material.CAPEM) {
		t.Fatal("append test CA")
	}
	serverConfig := &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		MinVersion:   tls.VersionTLS12,
	}
	if mutual {
		serverConfig.ClientAuth = tls.RequireAndVerifyClientCert
		serverConfig.ClientCAs = caPool
	}
	clientConfig := &tls.Config{
		RootCAs:    caPool,
		MinVersion: tls.VersionTLS12,
	}
	if mutual {
		clientConfig.Certificates = []tls.Certificate{clientCertificate}
	}
	return serverConfig, clientConfig
}

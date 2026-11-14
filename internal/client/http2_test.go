package client_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/client"
	"github.com/zjsycwmrbig/mini-rpc/internal/server"
	"github.com/zjsycwmrbig/mini-rpc/internal/testkit"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
)

func TestGeneratedClientCallsEchoServerOverH2C(t *testing.T) {
	t.Parallel()

	address := startHTTP2TestServer(t, echoServer{})
	clientConn := dialHTTP2TestClient(t, address)
	echoClient := echov1.NewEchoClient(clientConn)

	response, err := echoClient.Echo(
		context.Background(),
		&echov1.EchoRequest{Message: "hello h2c"},
	)
	if err != nil {
		t.Fatalf("Echo() error = %v", err)
	}
	if response.GetMessage() != "hello h2c" {
		t.Fatalf("Echo() message = %q, want %q", response.GetMessage(), "hello h2c")
	}
}

func TestHTTP2ConcurrentCallsShareOneConnection(t *testing.T) {
	t.Parallel()

	address, testServer := startHTTP2TestServerWithHandle(t, echoServer{})
	clientConn := dialHTTP2TestClient(t, address)

	const calls = 100
	var waitGroup sync.WaitGroup
	callErrors := make([]error, calls)
	waitGroup.Add(calls)
	for index := range calls {
		index := index
		go func() {
			defer waitGroup.Done()
			message := fmt.Sprintf("http2-%d", index)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			response, callErr := echov1.NewEchoClient(clientConn).Echo(
				ctx,
				&echov1.EchoRequest{Message: message},
			)
			if callErr == nil && response.GetMessage() != message {
				callErr = fmt.Errorf("message = %q, want %q", response.GetMessage(), message)
			}
			callErrors[index] = callErr
		}()
	}
	waitGroup.Wait()
	for index, callErr := range callErrors {
		if callErr != nil {
			t.Fatalf("call %d error = %v", index, callErr)
		}
	}
	if stats := clientConn.Stats(); stats.ConnectionsDialed != 1 {
		t.Fatalf("client dialed %d connections, want 1", stats.ConnectionsDialed)
	}
	if stats := testServer.Stats(); stats.AcceptedConnections != 1 {
		t.Fatalf("server accepted %d connections, want 1", stats.AcceptedConnections)
	}
}

func TestHTTP2CancellationPropagatesToHandler(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	serverCanceled := make(chan error, 1)
	address := startHTTP2TestServer(t, blockingEchoServer{
		handler: func(ctx context.Context, _ *echov1.EchoRequest) (*echov1.EchoResponse, error) {
			close(started)
			<-ctx.Done()
			serverCanceled <- ctx.Err()
			return nil, ctx.Err()
		},
	})
	clientConn := dialHTTP2TestClient(t, address)

	ctx, cancel := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		_, err := echov1.NewEchoClient(clientConn).Echo(ctx, &echov1.EchoRequest{Message: "cancel"})
		callDone <- err
	}()
	waitForClosed(t, started, "HTTP/2 handler")
	cancel()

	if err := <-callDone; !isRPCCode(err, rpc.CodeCanceled) {
		t.Fatalf("Echo() error = %v, want CodeCanceled", err)
	}
	select {
	case err := <-serverCanceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("server context error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP/2 server context was not canceled")
	}
}

func TestHTTP2TLSALPNCarriesGeneratedCall(t *testing.T) {
	t.Parallel()

	material, err := testkit.NewTLSMaterial("localhost", "127.0.0.1")
	if err != nil {
		t.Fatalf("NewTLSMaterial() error = %v", err)
	}
	serverCertificate, err := tls.X509KeyPair(material.ServerCertPEM, material.ServerKeyPEM)
	if err != nil {
		t.Fatalf("server X509KeyPair() error = %v", err)
	}
	serverTLS := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{serverCertificate},
	}
	address, _ := startHTTP2TestServerWithHandle(
		t,
		echoServer{},
		server.WithTLSConfig(serverTLS),
	)

	rootCAs := x509.NewCertPool()
	if !rootCAs.AppendCertsFromPEM(material.CAPEM) {
		t.Fatal("append test CA")
	}
	clientConn := dialHTTP2TestClient(t, address, client.WithTLSConfig(&tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    rootCAs,
		ServerName: "localhost",
	}))
	response, err := echov1.NewEchoClient(clientConn).Echo(
		context.Background(),
		&echov1.EchoRequest{Message: "h2 over tls"},
	)
	if err != nil {
		t.Fatalf("Echo() error = %v", err)
	}
	if response.GetMessage() != "h2 over tls" {
		t.Fatalf("Echo() message = %q, want %q", response.GetMessage(), "h2 over tls")
	}
}

func startHTTP2TestServer(t *testing.T, implementation echov1.EchoServer) string {
	t.Helper()
	address, _ := startHTTP2TestServerWithHandle(t, implementation)
	return address
}

func startHTTP2TestServerWithHandle(
	t *testing.T,
	implementation echov1.EchoServer,
	options ...server.Option,
) (string, *server.Server) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	testServer, err := server.NewWithOptions(options...)
	if err != nil {
		_ = listener.Close()
		t.Fatalf("server.NewWithOptions() error = %v", err)
	}
	if err := echov1.RegisterEchoServer(testServer, implementation); err != nil {
		_ = listener.Close()
		t.Fatalf("RegisterEchoServer() error = %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- testServer.ServeHTTP2(listener) }()
	t.Cleanup(func() {
		_ = testServer.Close()
		select {
		case err := <-serveDone:
			if !errors.Is(err, server.ErrServerClosed) {
				t.Errorf("ServeHTTP2() error = %v, want ErrServerClosed", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("ServeHTTP2() did not stop")
		}
	})
	return listener.Addr().String(), testServer
}

func dialHTTP2TestClient(t *testing.T, address string, options ...client.Option) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	options = append(options, client.WithHTTP2())
	clientConn, err := client.Dial(ctx, address, options...)
	if err != nil {
		t.Fatalf("client.Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	return clientConn
}

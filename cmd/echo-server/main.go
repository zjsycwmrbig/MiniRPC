package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/registry"
	"github.com/zjsycwmrbig/mini-rpc/internal/server"
)

func main() {
	address := flag.String("addr", "127.0.0.1:8080", "TCP listen address")
	shutdownTimeout := flag.Duration("shutdown-timeout", 5*time.Second, "graceful shutdown timeout")
	etcdEndpoints := flag.String("etcd-endpoints", "", "comma-separated etcd endpoints")
	instanceID := flag.String("instance-id", "", "registry instance ID")
	advertiseAddr := flag.String("advertise-addr", "", "address published to the registry")
	leaseTTL := flag.Duration("lease-ttl", 10*time.Second, "registry lease TTL")
	flag.Parse()

	listener, err := net.Listen("tcp", *address)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	rpcServer := server.New()
	if err := echov1.RegisterEchoServer(rpcServer, echoServer{}); err != nil {
		_ = listener.Close()
		log.Fatalf("register echo service: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
			id = defaultInstanceID(listener.Addr())
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
		defer cancel()
		if err := rpcServer.GracefulStop(shutdownCtx); err != nil {
			log.Printf("graceful stop: %v", err)
		}
		if err := <-serveDone; err != nil && !errors.Is(err, server.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
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

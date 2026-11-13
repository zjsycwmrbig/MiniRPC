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
	"syscall"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/server"
)

func main() {
	address := flag.String("addr", "127.0.0.1:8080", "TCP listen address")
	shutdownTimeout := flag.Duration("shutdown-timeout", 5*time.Second, "graceful shutdown timeout")
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

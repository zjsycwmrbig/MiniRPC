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

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/server"
)

func main() {
	address := flag.String("addr", "127.0.0.1:8080", "TCP listen address")
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
	go func() {
		<-ctx.Done()
		_ = rpcServer.Close()
	}()

	fmt.Printf("Mini-RPC echo server listening on %s\n", listener.Addr())
	if err := rpcServer.Serve(listener); err != nil && !errors.Is(err, server.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
}

type echoServer struct{}

func (echoServer) Echo(_ context.Context, request *echov1.EchoRequest) (*echov1.EchoResponse, error) {
	return &echov1.EchoResponse{Message: request.GetMessage()}, nil
}

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	echov1 "github.com/zjsycwmrbig/mini-rpc/api/echo/v1"
	"github.com/zjsycwmrbig/mini-rpc/internal/client"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
)

func main() {
	address := flag.String("addr", "127.0.0.1:8080", "TCP server address")
	message := flag.String("message", "hello from Mini-RPC", "message sent to the Echo service")
	timeout := flag.Duration("timeout", 5*time.Second, "request timeout")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	ctx = rpc.WithIdempotent(ctx)

	clientConn, err := client.Dial(ctx, *address)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer func() {
		if err := clientConn.Close(); err != nil {
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

package echov1

import (
	"context"
	"errors"
	"testing"

	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
	"google.golang.org/protobuf/proto"
)

func TestGeneratedClientInvokesExpectedMethod(t *testing.T) {
	t.Parallel()

	clientConn := &recordingClientConn{}
	client := NewEchoClient(clientConn)

	response, err := client.Echo(context.Background(), &EchoRequest{Message: "hello"})
	if err != nil {
		t.Fatalf("Echo() error = %v", err)
	}
	if response.GetMessage() != "hello" {
		t.Fatalf("Echo() message = %q, want %q", response.GetMessage(), "hello")
	}
	if clientConn.service != EchoServiceName {
		t.Errorf("service = %q, want %q", clientConn.service, EchoServiceName)
	}
	if clientConn.method != "Echo" {
		t.Errorf("method = %q, want %q", clientConn.method, "Echo")
	}
}

func TestGeneratedServerRegistrationAndDispatch(t *testing.T) {
	t.Parallel()

	implementation := echoServerFunc(func(_ context.Context, request *EchoRequest) (*EchoResponse, error) {
		return &EchoResponse{Message: request.GetMessage()}, nil
	})
	registrar := &recordingRegistrar{}

	if err := RegisterEchoServer(registrar, implementation); err != nil {
		t.Fatalf("RegisterEchoServer() error = %v", err)
	}
	if registrar.desc == nil {
		t.Fatal("RegisterEchoServer() did not provide a service descriptor")
	}
	if registrar.desc.ServiceName != EchoServiceName {
		t.Errorf("service name = %q, want %q", registrar.desc.ServiceName, EchoServiceName)
	}
	if len(registrar.desc.Methods) != 1 {
		t.Fatalf("method count = %d, want 1", len(registrar.desc.Methods))
	}

	response, err := registrar.desc.Methods[0].Handler(
		registrar.implementation,
		context.Background(),
		&EchoRequest{Message: "generated"},
	)
	if err != nil {
		t.Fatalf("handler error = %v", err)
	}
	if response.(*EchoResponse).GetMessage() != "generated" {
		t.Errorf("handler message = %q, want %q", response.(*EchoResponse).GetMessage(), "generated")
	}
}

func TestGeneratedMessageRoundTrip(t *testing.T) {
	t.Parallel()

	input := &EchoRequest{Message: "protobuf"}
	encoded, err := proto.Marshal(input)
	if err != nil {
		t.Fatalf("proto.Marshal() error = %v", err)
	}

	var decoded EchoRequest
	if err := proto.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("proto.Unmarshal() error = %v", err)
	}
	if decoded.GetMessage() != input.GetMessage() {
		t.Fatalf("decoded message = %q, want %q", decoded.GetMessage(), input.GetMessage())
	}
}

type recordingClientConn struct {
	service string
	method  string
}

func (conn *recordingClientConn) Invoke(
	_ context.Context,
	service string,
	method string,
	request proto.Message,
	response proto.Message,
) error {
	conn.service = service
	conn.method = method

	input, ok := request.(*EchoRequest)
	if !ok {
		return errors.New("unexpected request type")
	}
	output, ok := response.(*EchoResponse)
	if !ok {
		return errors.New("unexpected response type")
	}
	output.Message = input.GetMessage()
	return nil
}

func (*recordingClientConn) Close() error {
	return nil
}

type recordingRegistrar struct {
	desc           *rpc.ServiceDesc
	implementation any
}

func (registrar *recordingRegistrar) RegisterService(desc *rpc.ServiceDesc, implementation any) error {
	registrar.desc = desc
	registrar.implementation = implementation
	return nil
}

type echoServerFunc func(context.Context, *EchoRequest) (*EchoResponse, error)

func (handler echoServerFunc) Echo(ctx context.Context, request *EchoRequest) (*EchoResponse, error) {
	return handler(ctx, request)
}

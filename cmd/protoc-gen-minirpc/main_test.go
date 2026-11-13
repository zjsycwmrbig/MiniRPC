package main

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

func TestGenerateFileCreatesUnaryAdapters(t *testing.T) {
	t.Parallel()

	plugin := newTestPlugin(t, false)
	file := plugin.Files[0]
	if err := generateFile(plugin, file); err != nil {
		t.Fatalf("generateFile() error = %v", err)
	}

	response := plugin.Response()
	if len(response.File) != 1 {
		t.Fatalf("generated files = %d, want 1", len(response.File))
	}

	content := response.File[0].GetContent()
	for _, expected := range []string{
		"const EchoServiceName = \"rpc.echo.v1.Echo\"",
		"type EchoServer interface",
		"type EchoClient interface",
		"func RegisterEchoServer(",
		"\"SayHello\"",
		"NewRequest: func() proto.Message",
	} {
		if !strings.Contains(content, expected) {
			t.Errorf("generated content does not contain %q\n%s", expected, content)
		}
	}
}

func TestGenerateFileRejectsStreamingRPC(t *testing.T) {
	t.Parallel()

	plugin := newTestPlugin(t, true)

	err := generateFile(plugin, plugin.Files[0])
	if err == nil {
		t.Fatal("generateFile() error = nil, want streaming error")
	}
	if !strings.Contains(err.Error(), "streaming RPC is not supported") {
		t.Fatalf("generateFile() error = %v", err)
	}
}

func newTestPlugin(t *testing.T, clientStreaming bool) *protogen.Plugin {
	t.Helper()

	streamingFalse := false
	request := &pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{"echo.proto"},
		Parameter:      proto.String("paths=source_relative"),
		ProtoFile: []*descriptorpb.FileDescriptorProto{{
			Name:    proto.String("echo.proto"),
			Package: proto.String("rpc.echo.v1"),
			Syntax:  proto.String("proto3"),
			Options: &descriptorpb.FileOptions{
				GoPackage: proto.String("example.com/echo;echov1"),
			},
			MessageType: []*descriptorpb.DescriptorProto{
				{Name: proto.String("SayHelloRequest")},
				{Name: proto.String("SayHelloResponse")},
			},
			Service: []*descriptorpb.ServiceDescriptorProto{{
				Name: proto.String("Echo"),
				Method: []*descriptorpb.MethodDescriptorProto{{
					Name:            proto.String("SayHello"),
					InputType:       proto.String(".rpc.echo.v1.SayHelloRequest"),
					OutputType:      proto.String(".rpc.echo.v1.SayHelloResponse"),
					ServerStreaming: &streamingFalse,
					ClientStreaming: &clientStreaming,
				}},
			}},
		}},
	}

	plugin, err := protogen.Options{}.New(request)
	if err != nil {
		t.Fatalf("protogen.Options.New() error = %v", err)
	}
	return plugin
}

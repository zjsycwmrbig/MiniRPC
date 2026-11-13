package registry

import (
	"context"
	"errors"
	"testing"
)

func TestValidateInstance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		instance Instance
		wantErr  bool
	}{
		{
			name: "valid",
			instance: Instance{
				ID:      "echo-a",
				Service: "rpc.echo.v1.Echo",
				Address: "127.0.0.1:8080",
				Weight:  1,
			},
		},
		{name: "missing id", instance: Instance{Service: "svc", Address: "addr"}, wantErr: true},
		{name: "missing service", instance: Instance{ID: "id", Address: "addr"}, wantErr: true},
		{name: "missing address", instance: Instance{ID: "id", Service: "svc"}, wantErr: true},
		{name: "negative weight", instance: Instance{ID: "id", Service: "svc", Address: "addr", Weight: -1}, wantErr: true},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateInstance(test.instance)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidInstance) {
					t.Fatalf("validateInstance() error = %v, want ErrInvalidInstance", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateInstance() error = %v", err)
			}
		})
	}
}

func TestLeaseTTLValidation(t *testing.T) {
	t.Parallel()

	registry := &EtcdRegistry{
		prefix: "/test",
		leases: make(map[*etcdLease]struct{}),
	}
	_, err := registry.Register(context.Background(), Instance{
		ID:      "a",
		Service: "svc",
		Address: "addr",
	}, 0)
	if !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("Register() error = %v, want ErrInvalidTTL", err)
	}
}

func TestEventTypesAreStable(t *testing.T) {
	t.Parallel()

	if EventPut != "put" || EventDelete != "delete" {
		t.Fatalf("event types = %q, %q", EventPut, EventDelete)
	}
}

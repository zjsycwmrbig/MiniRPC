// Package registry defines service registration and discovery contracts and
// provides the etcd-backed implementation.
package registry

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrInvalidInstance indicates incomplete registration data.
	ErrInvalidInstance = errors.New("registry: invalid instance")
	// ErrInvalidTTL indicates a non-positive lease duration.
	ErrInvalidTTL = errors.New("registry: invalid lease TTL")
)

// Instance describes one discoverable service endpoint.
type Instance struct {
	ID       string            `json:"id"`
	Service  string            `json:"service"`
	Address  string            `json:"address"`
	Weight   int               `json:"weight,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// EventType identifies an instance snapshot mutation.
type EventType string

const (
	// EventPut adds or updates an instance.
	EventPut EventType = "put"
	// EventDelete removes an instance.
	EventDelete EventType = "delete"
)

// Event is one registry watch update. Err is set for a watch failure and is
// followed by the output channel closing.
type Event struct {
	Type     EventType
	Instance Instance
	Err      error
}

// Lease represents an instance registration lifetime.
type Lease interface {
	Done() <-chan struct{}
	Revoke(ctx context.Context) error
}

// Registry stores discoverable service instances.
type Registry interface {
	Register(ctx context.Context, instance Instance, ttl time.Duration) (Lease, error)
	Deregister(ctx context.Context, instance Instance) error
	List(ctx context.Context, service string) ([]Instance, error)
	Watch(ctx context.Context, service string) (<-chan Event, error)
	Close() error
}

func validateInstance(instance Instance) error {
	switch {
	case instance.ID == "":
		return fmt.Errorf("%w: missing id", ErrInvalidInstance)
	case instance.Service == "":
		return fmt.Errorf("%w: missing service", ErrInvalidInstance)
	case instance.Address == "":
		return fmt.Errorf("%w: missing address", ErrInvalidInstance)
	case instance.Weight < 0:
		return fmt.Errorf("%w: negative weight", ErrInvalidInstance)
	default:
		return nil
	}
}

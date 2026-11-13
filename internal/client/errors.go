package client

import "errors"

var (
	// ErrPoolClosed indicates that Invoke was attempted after Close.
	ErrPoolClosed = errors.New("client: connection pool closed")

	// ErrDialTimeout indicates that establishing a new connection exceeded its
	// context or configured dial deadline.
	ErrDialTimeout = errors.New("client: connection establishment timed out")
)

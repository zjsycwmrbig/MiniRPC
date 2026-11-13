package client

import "errors"

// ErrPoolClosed indicates that Invoke was attempted after Close.
var ErrPoolClosed = errors.New("client: connection pool closed")

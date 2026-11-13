package transport

import "errors"

var (
	// ErrConnClosed indicates that a connection was closed while a request was
	// pending or before a write could be completed.
	ErrConnClosed = errors.New("transport: connection closed")

	// ErrNilContext indicates that a caller supplied no context.
	ErrNilContext = errors.New("transport: nil context")

	// ErrNotRequest indicates that Call received a frame that is not a request.
	ErrNotRequest = errors.New("transport: frame is not a request")

	// ErrOneWayUnsupported indicates that one-way delivery is not implemented.
	ErrOneWayUnsupported = errors.New("transport: one-way calls are not implemented")
)

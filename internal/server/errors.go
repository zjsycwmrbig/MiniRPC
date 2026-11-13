package server

import "errors"

var (
	// ErrServerClosed indicates that Serve stopped because the server closed.
	ErrServerClosed = errors.New("server: closed")
	// ErrAlreadyServing indicates a second Serve call on one server.
	ErrAlreadyServing = errors.New("server: already serving")
	// ErrInvalidListener indicates a nil listener.
	ErrInvalidListener = errors.New("server: nil listener")
	// ErrInvalidService indicates incomplete generated service metadata.
	ErrInvalidService = errors.New("server: invalid service descriptor")
	// ErrServiceRegistered indicates a duplicate service registration.
	ErrServiceRegistered = errors.New("server: service already registered")
	// ErrInvalidImplementation indicates the implementation does not satisfy
	// the generated server interface.
	ErrInvalidImplementation = errors.New("server: invalid service implementation")
	// ErrInvalidConfig indicates an invalid resource or rate-limit policy.
	ErrInvalidConfig = errors.New("server: invalid configuration")
)

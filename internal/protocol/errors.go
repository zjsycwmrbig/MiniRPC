package protocol

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalidMagic indicates that the byte stream is not Mini-RPC.
	ErrInvalidMagic = errors.New("protocol: invalid magic")
	// ErrUnsupportedVersion indicates an unknown protocol version.
	ErrUnsupportedVersion = errors.New("protocol: unsupported version")
	// ErrInvalidFrameType indicates an unknown frame operation.
	ErrInvalidFrameType = errors.New("protocol: invalid frame type")
	// ErrInvalidFlags indicates reserved flag bits are set.
	ErrInvalidFlags = errors.New("protocol: invalid flags")
	// ErrInvalidLimits indicates an unusable codec limit configuration.
	ErrInvalidLimits = errors.New("protocol: invalid limits")
	// ErrHeaderTooLarge indicates encoded Metadata exceeds its limit.
	ErrHeaderTooLarge = errors.New("protocol: header too large")
	// ErrBodyTooLarge indicates Body exceeds its configured limit.
	ErrBodyTooLarge = errors.New("protocol: body too large")
	// ErrTruncatedFrame indicates a stream ended inside a frame.
	ErrTruncatedFrame = errors.New("protocol: truncated frame")
	// ErrMalformedMetadata indicates invalid length-prefixed Metadata.
	ErrMalformedMetadata = errors.New("protocol: malformed metadata")
	// ErrEmptyMetadataKey indicates a metadata entry without a key.
	ErrEmptyMetadataKey = errors.New("protocol: empty metadata key")
	// ErrDuplicateMetadataKey indicates ambiguous repeated metadata keys.
	ErrDuplicateMetadataKey = errors.New("protocol: duplicate metadata key")
)

// Error classifies a protocol failure while preserving user-facing context.
type Error struct {
	Kind   error
	Detail string
}

// Error implements error.
func (protocolError *Error) Error() string {
	if protocolError.Detail == "" {
		return protocolError.Kind.Error()
	}
	return fmt.Sprintf("%s: %s", protocolError.Kind, protocolError.Detail)
}

// Unwrap exposes the protocol error category to errors.Is and errors.As.
func (protocolError *Error) Unwrap() error {
	return protocolError.Kind
}

func newProtocolError(kind error, format string, arguments ...any) error {
	return &Error{
		Kind:   kind,
		Detail: fmt.Sprintf(format, arguments...),
	}
}

package rpc

import (
	"fmt"
	"strconv"
)

// ErrorCode is a stable RPC status code carried in protocol Metadata.
type ErrorCode uint32

const (
	CodeOK                ErrorCode = 0
	CodeInvalidArgument   ErrorCode = 1
	CodeNotFound          ErrorCode = 2
	CodeDeadlineExceeded  ErrorCode = 3
	CodeUnavailable       ErrorCode = 4
	CodeResourceExhausted ErrorCode = 5
	CodeInternal          ErrorCode = 6
	CodeCanceled          ErrorCode = 7
	CodeProtocolError     ErrorCode = 8
)

// String returns a stable lowercase error code for logs.
func (code ErrorCode) String() string {
	switch code {
	case CodeOK:
		return "ok"
	case CodeInvalidArgument:
		return "invalid_argument"
	case CodeNotFound:
		return "not_found"
	case CodeDeadlineExceeded:
		return "deadline_exceeded"
	case CodeUnavailable:
		return "unavailable"
	case CodeResourceExhausted:
		return "resource_exhausted"
	case CodeInternal:
		return "internal"
	case CodeCanceled:
		return "canceled"
	case CodeProtocolError:
		return "protocol_error"
	default:
		return "unknown"
	}
}

// Error is an RPC status returned by a remote server.
type Error struct {
	Code    ErrorCode
	Message string
}

// NewError constructs an RPC status.
func NewError(code ErrorCode, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Error implements error.
func (rpcError *Error) Error() string {
	if rpcError.Message == "" {
		return fmt.Sprintf("rpc: %s", rpcError.Code)
	}
	return fmt.Sprintf("rpc: %s: %s", rpcError.Code, rpcError.Message)
}

// ParseErrorCode decodes the decimal Metadata representation.
func ParseErrorCode(value string) (ErrorCode, bool) {
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return CodeInternal, false
	}
	return ErrorCode(parsed), true
}

// FormatErrorCode encodes a code for protocol Metadata.
func FormatErrorCode(code ErrorCode) string {
	return strconv.FormatUint(uint64(code), 10)
}

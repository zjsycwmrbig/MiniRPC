package rpc

import (
	"errors"
	"testing"
)

func TestErrorCodeFormattingAndParsing(t *testing.T) {
	t.Parallel()

	codes := []ErrorCode{
		CodeOK,
		CodeInvalidArgument,
		CodeNotFound,
		CodeDeadlineExceeded,
		CodeUnavailable,
		CodeResourceExhausted,
		CodeInternal,
		CodeCanceled,
		CodeProtocolError,
	}
	for _, code := range codes {
		encoded := FormatErrorCode(code)
		decoded, ok := ParseErrorCode(encoded)
		if !ok || decoded != code {
			t.Errorf("ParseErrorCode(%q) = %v, %v; want %v, true", encoded, decoded, ok, code)
		}
	}
	if _, ok := ParseErrorCode("not-a-number"); ok {
		t.Fatal("ParseErrorCode() accepted invalid input")
	}
}

func TestRemoteErrorCanBeInspected(t *testing.T) {
	t.Parallel()

	err := error(NewError(CodeUnavailable, "try another instance"))
	var remoteError *Error
	if !errors.As(err, &remoteError) {
		t.Fatalf("errors.As() = false for %v", err)
	}
	if remoteError.Code != CodeUnavailable || remoteError.Message != "try another instance" {
		t.Fatalf("remote error = %#v", remoteError)
	}
}

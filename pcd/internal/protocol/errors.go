package protocol

import (
	"errors"
	"fmt"
)

// ErrorCode is a JSON-RPC error code.
type ErrorCode int

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     ErrorCode = -32700
	CodeInvalidRequest ErrorCode = -32600
	CodeMethodNotFound ErrorCode = -32601
	CodeInvalidParams  ErrorCode = -32602
	CodeInternalError  ErrorCode = -32603
)

// Application error codes (spec §7).
const (
	CodeUnknownSource       ErrorCode = -32001
	CodeSourceUnavailable   ErrorCode = -32002
	CodeHelloRequired       ErrorCode = -32003
	CodeUnsupportedProtocol ErrorCode = -32004
	CodeUnknownSubscription ErrorCode = -32005
	CodeNoValue             ErrorCode = -32006
)

// Name returns the symbolic name used in error.data.name.
//
// The spec only defines names for the application codes (-32001..-32006).
// For the standard JSON-RPC codes we use the snake_case form of the names in
// the JSON-RPC 2.0 specification so that every error can carry data.name.
func (c ErrorCode) Name() string {
	switch c {
	case CodeParseError:
		return "parse_error"
	case CodeInvalidRequest:
		return "invalid_request"
	case CodeMethodNotFound:
		return "method_not_found"
	case CodeInvalidParams:
		return "invalid_params"
	case CodeInternalError:
		return "internal_error"
	case CodeUnknownSource:
		return "unknown_source"
	case CodeSourceUnavailable:
		return "source_unavailable"
	case CodeHelloRequired:
		return "hello_required"
	case CodeUnsupportedProtocol:
		return "unsupported_protocol"
	case CodeUnknownSubscription:
		return "unknown_subscription"
	case CodeNoValue:
		return "no_value"
	}
	return ""
}

func (c ErrorCode) String() string {
	if n := c.Name(); n != "" {
		return fmt.Sprintf("%d (%s)", int(c), n)
	}
	return fmt.Sprintf("%d", int(c))
}

// ErrorData is the "data" member of an error object.
type ErrorData struct {
	// Name is the symbolic error name, e.g. "unknown_source".
	Name string `json:"name,omitempty"`
	// Supported lists supported protocol versions (unsupported_protocol only).
	Supported []string `json:"supported,omitempty"`
}

// Error is a JSON-RPC error object. It implements the error interface so it
// can be returned directly from client calls.
type Error struct {
	Code    ErrorCode  `json:"code"`
	Message string     `json:"message"`
	Data    *ErrorData `json:"data,omitempty"`
}

// NewError returns an Error with data.name set from the code.
func NewError(code ErrorCode, format string, args ...any) *Error {
	e := &Error{Code: code, Message: fmt.Sprintf(format, args...)}
	if n := code.Name(); n != "" {
		e.Data = &ErrorData{Name: n}
	}
	return e
}

func (e *Error) Error() string {
	name := ""
	if e.Data != nil && e.Data.Name != "" {
		name = " " + e.Data.Name
	} else if n := e.Code.Name(); n != "" {
		name = " " + n
	}
	return fmt.Sprintf("phonetopc: error %d%s: %s", int(e.Code), name, e.Message)
}

// Is reports whether target is an *Error with the same code, so that
// errors.Is(err, &protocol.Error{Code: protocol.CodeNoValue}) works.
func (e *Error) Is(target error) bool {
	var t *Error
	if errors.As(target, &t) {
		return t.Code == e.Code
	}
	return false
}

// ErrorCodeOf returns the code of err if it is (or wraps) an *Error.
func ErrorCodeOf(err error) (ErrorCode, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, true
	}
	return 0, false
}

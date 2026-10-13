package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
)

// Request is an outgoing JSON-RPC request (client to server).
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  any             `json:"params,omitempty"`
}

// NewRequest builds a request. params may be nil.
func NewRequest(id json.RawMessage, method string, params any) *Request {
	return &Request{JSONRPC: JSONRPCVersion, ID: id, Method: method, Params: params}
}

// Response is an outgoing JSON-RPC response (server to client). Exactly one
// of Result and Error must be set. A nil ID is encoded as null, which is what
// JSON-RPC requires when the request id could not be determined.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// NewResult builds a success response. result must not be nil; use
// struct{}{} (or UnsubscribeResult{}) for an empty object.
func NewResult(id json.RawMessage, result any) *Response {
	return &Response{JSONRPC: JSONRPCVersion, ID: id, Result: result}
}

// NewErrorResponse builds an error response.
func NewErrorResponse(id json.RawMessage, err *Error) *Response {
	return &Response{JSONRPC: JSONRPCVersion, ID: id, Error: err}
}

// Notification is an outgoing JSON-RPC notification (no id).
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// NewEventNotification wraps an event in an "event" notification.
func NewEventNotification(ev *Event) *Notification {
	return &Notification{JSONRPC: JSONRPCVersion, Method: MethodEvent, Params: ev}
}

// Kind classifies an incoming message.
type Kind int

const (
	KindInvalid Kind = iota
	KindRequest
	KindResponse
	KindNotification
)

func (k Kind) String() string {
	switch k {
	case KindRequest:
		return "request"
	case KindResponse:
		return "response"
	case KindNotification:
		return "notification"
	}
	return "invalid"
}

// Message is a generic decoded incoming message. Absent members are nil;
// members that are present but JSON null hold the literal "null".
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Kind reports what kind of message m is, based only on which members are
// present. It does not validate the members' values.
func (m *Message) Kind() Kind {
	switch {
	case m.Method != "" && m.ID != nil && m.Result == nil && m.Error == nil:
		return KindRequest
	case m.Method != "" && m.ID == nil && m.Result == nil && m.Error == nil:
		return KindNotification
	case m.Method == "" && m.ID != nil && (m.Result != nil) != (m.Error != nil):
		return KindResponse
	}
	return KindInvalid
}

// IntID encodes an integer request id.
func IntID(n int64) json.RawMessage {
	return json.RawMessage(strconv.FormatInt(n, 10))
}

// ParseIntID decodes an integer id. It returns false for string ids, null,
// or anything that is not an integer.
func ParseIntID(id json.RawMessage) (int64, bool) {
	n, err := strconv.ParseInt(string(bytes.TrimSpace(id)), 10, 64)
	return n, err == nil
}

// MaxSafeID is the largest magnitude of an integer request id: 2^53-1, so
// that every JSON library represents the id exactly (spec §2).
const MaxSafeID = 1<<53 - 1

// ValidRequestID reports whether id is acceptable as a request id: a JSON
// string or an integer in [-MaxSafeID, MaxSafeID]. Fractions, exponents,
// out-of-range integers, null, booleans, arrays and objects are rejected.
func ValidRequestID(id json.RawMessage) bool {
	id = bytes.TrimSpace(id)
	if len(id) == 0 {
		return false
	}
	switch id[0] {
	case '"':
		var s string
		return json.Unmarshal(id, &s) == nil
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		var n json.Number
		if json.Unmarshal(id, &n) != nil {
			return false
		}
		// JSON Schema "integer" accepts 1.0 too, but JSON-RPC ids SHOULD NOT
		// have fractional parts and echoing such ids back is error-prone, so
		// only plain integer literals are accepted.
		if bytes.ContainsAny(id, ".eE") {
			return false
		}
		i, err := strconv.ParseInt(string(id), 10, 64)
		return err == nil && i >= -MaxSafeID && i <= MaxSafeID
	}
	return false
}

// ParseRequest applies the framing checks of PROTOCOL.md §2, steps 1-4, to
// one text frame: strict JSON (-32700), a JSON object (-32600; a batch array
// gets its own message, §1), then the id, jsonrpc and method members
// (-32600). It returns an error response with the first failure, or the
// request's parts. id is nil (and errResp nil) for a client notification,
// which gets no reply (§2 step 3), whatever else the object contains.
//
// The pairing listener frames its messages the same way (SECURITY.md §4.1,
// "(1)–(4) as in PROTOCOL.md §2"), so both servers share this.
func ParseRequest(data []byte) (id json.RawMessage, method string, params json.RawMessage, errResp *Response) {
	fail := func(id json.RawMessage, code ErrorCode, format string, args ...any) (json.RawMessage, string, json.RawMessage, *Response) {
		return nil, "", nil, NewErrorResponse(id, NewError(code, format, args...))
	}
	if !json.Valid(data) {
		return fail(nil, CodeParseError, "parse error: frame is not valid JSON")
	}
	switch bytes.TrimLeft(data, " \t\r\n")[0] {
	case '{':
	case '[':
		return fail(nil, CodeInvalidRequest, "batch requests are not supported")
	default:
		return fail(nil, CodeInvalidRequest, "request must be a JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fail(nil, CodeInvalidRequest, "invalid request: %v", err)
	}
	id, hasID := fields["id"]
	if !hasID {
		return nil, "", nil, nil
	}
	// Only an invalid id is answered with "id": null; later step-4 errors
	// echo it.
	if !ValidRequestID(id) {
		return fail(nil, CodeInvalidRequest, "id must be a string or an integer within ±(2^53-1)")
	}
	var version string
	if json.Unmarshal(fields["jsonrpc"], &version) != nil || version != JSONRPCVersion {
		return fail(id, CodeInvalidRequest, `jsonrpc must be "2.0"`)
	}
	// Any string passes this step, even ""; it is then an unknown method.
	if m := bytes.TrimSpace(fields["method"]); len(m) == 0 || m[0] != '"' || json.Unmarshal(m, &method) != nil {
		return fail(id, CodeInvalidRequest, "method must be a string")
	}
	return id, method, fields["params"], nil
}

// DecodeParams decodes a request's params into v (PROTOCOL.md §2): they
// must be an object if present, and absent or null params decode as {}.
// Unknown members are ignored. Every failure is -32602.
func DecodeParams(params json.RawMessage, v any) *Error {
	p := bytes.TrimSpace(params)
	if len(p) == 0 || string(p) == "null" {
		p = []byte("{}")
	}
	if p[0] != '{' {
		return NewError(CodeInvalidParams, "params must be an object")
	}
	if err := json.Unmarshal(p, v); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			return NewError(CodeInvalidParams, "params.%s has the wrong type (%s)", te.Field, te.Value)
		}
		return NewError(CodeInvalidParams, "invalid params: %v", err)
	}
	return nil
}

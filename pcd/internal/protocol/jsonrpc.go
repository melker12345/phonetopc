package protocol

import (
	"bytes"
	"encoding/json"
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

// Package protocol defines the wire types of the phonetopc/1 protocol
// (draft 4, see spec/PROTOCOL.md, spec/SECURITY.md, spec/APPROVAL.md,
// spec/PANELS.md and spec/schemas/phonetopc-1.schema.json).
//
// The package has no I/O. It is shared by the client library, the mock phone
// server and (later) the phonetopcd daemon, so it only contains message
// types, the sensor catalog, error codes and small pure helpers.
package protocol

// Version is the protocol version string exchanged in hello.
const Version = "phonetopc/1"

// SupportedVersions lists every protocol version this implementation speaks.
var SupportedVersions = []string{Version}

// JSONRPCVersion is the value of the "jsonrpc" member of every message.
const JSONRPCVersion = "2.0"

// WSPath is the HTTP path of the WebSocket endpoint.
const WSPath = "/v1/ws"

// DefaultPort is the default TCP port of a phone server.
const DefaultPort = 8765

// Method names.
const (
	MethodHello       = "hello"
	MethodListSensors = "list_sensors"
	MethodSubscribe   = "subscribe"
	MethodUnsubscribe = "unsubscribe"
	MethodGet         = "get"

	// MethodEvent is the only server-to-client notification.
	MethodEvent = "event"
)

// Capabilities advertised in HelloResult.Capabilities.
const (
	CapabilityEvents = "events"
	CapabilityGet    = "get"
)

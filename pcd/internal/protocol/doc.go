// Package protocol defines the wire types of the phonetopc/1 protocol
// (draft 4, see spec/PROTOCOL.md, spec/SECURITY.md, spec/APPROVAL.md,
// spec/PANELS.md and spec/schemas/phonetopc-1.schema.json).
//
// The package has no I/O: it is shared by the client library, the mock
// phone and the daemon.
package protocol

// Exchanged in hello.
const Version = "phonetopc/1"

var SupportedVersions = []string{Version}

const JSONRPCVersion = "2.0"

const WSPath = "/v1/ws"

const DefaultPort = 8765

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

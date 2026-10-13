// Package version holds the version of the pcd module's programs and Go
// client: what `phonetopc -version` prints and what the client and the mock
// phone report in hello (client.version, server.version; PROTOCOL.md §4.1).
// It is not the protocol version (protocol.Version).
package version

// Version is the release version. It is a variable so that release builds
// can set it:
//
//	go build -ldflags "-X github.com/melker12345/phonetopc/pcd/internal/version.Version=1.2.3" ./cmd/...
var Version = "0.1.0"

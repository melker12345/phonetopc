package daemon

import (
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

type connKey struct{}

// program names the program at the other end of r's connection for the
// phone (APPROVAL.md §4): its executable's file name, where the daemon can
// find it, or protocol.DefaultProgram.
func program(r *http.Request) string {
	c, _ := r.Context().Value(connKey{}).(net.Conn)
	if c == nil {
		return protocol.DefaultProgram
	}
	return programName(peerExecutable(c))
}

// programName makes a name the phone accepts from an executable's path.
func programName(path string) string {
	name := filepath.Base(strings.TrimSuffix(path, " (deleted)"))
	if utf8.RuneCountInString(name) > protocol.MaxProgramLen {
		name = strings.TrimSpace(string([]rune(name)[:protocol.MaxProgramLen]))
	}
	if path == "" || protocol.CheckProgram(name) != nil {
		return protocol.DefaultProgram
	}
	return name
}

package daemon

import (
	"strings"
	"testing"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

func TestProgramName(t *testing.T) {
	long := strings.Repeat("p", 70)
	for path, want := range map[string]string{
		"/usr/bin/python3.12":          "python3.12",
		"/home/u/bin/deploy (deleted)": "deploy",
		"curl":                         "curl",
		"/opt/" + long:                 strings.Repeat("p", 60),
		"":                             protocol.DefaultProgram,
		"/tmp/evil\u202egnp.exe":       protocol.DefaultProgram,
		"/tmp/tab\there":               protocol.DefaultProgram,
	} {
		if got := programName(path); got != want {
			t.Errorf("programName(%q) = %q, want %q", path, got, want)
		}
	}
}

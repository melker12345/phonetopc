package systemd

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnits(t *testing.T) {
	u := Units("/home/a b/go/bin/phonetopcd")
	for _, want := range []string{"ListenStream=%t/phonetopc.sock", "SocketMode=0600", "ListenStream=127.0.0.1:8767", "WantedBy=sockets.target"} {
		if !strings.Contains(u[Socket], want+"\n") {
			t.Errorf("socket unit lacks %q", want)
		}
	}
	for _, want := range []string{`ExecStart="/home/a b/go/bin/phonetopcd"`, "Requires=phonetopcd.socket", "Also=phonetopcd.socket"} {
		if !strings.Contains(u[Service], want+"\n") {
			t.Errorf("service unit lacks %q:\n%s", want, u[Service])
		}
	}
}

var update = flag.Bool("update", false, "rewrite testdata/ from the units")

// TestSystemUnits compares the system service's files with testdata/, so
// that a change to them is a visible change to the golden files too.
func TestSystemUnits(t *testing.T) {
	u := SystemUnits("/opt/phone topc/phonetopcd")
	if len(u) != 3 {
		t.Fatalf("got %d files", len(u))
	}
	for name, content := range u {
		golden := filepath.Join("testdata", "system", name)
		if *update {
			if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if content != string(want) {
			t.Errorf("%s differs from %s (go test -update rewrites it):\n%s", name, golden, content)
		}
	}
	for _, want := range []string{`ExecStart="/opt/phone topc/phonetopcd" --system`, "User=phonetopc", "ProtectHome=yes", "CapabilityBoundingSet=\n"} {
		if !strings.Contains(u[Service], want) {
			t.Errorf("service lacks %q", want)
		}
	}
	for _, want := range []string{"SocketGroup=phonetopc-users", "SocketMode=0660"} {
		if !strings.Contains(u[Socket], want) {
			t.Errorf("socket lacks %q", want)
		}
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/bin/phonetopcd":  "/usr/bin/phonetopcd",
		"/opt/100%/phonetopcd": "/opt/100%%/phonetopcd",
		`/a "b"/c`:             `"/a \"b\"/c"`,
	} {
		if got := quote(in); got != want {
			t.Errorf("quote(%q) = %q, want %q", in, got, want)
		}
	}
}

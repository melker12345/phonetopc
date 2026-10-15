package systemd

import (
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

package daemon_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/melker12345/phonetopc/pcd/internal/daemon"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// TestApprovalMatchComesFirst: the number arrives at once, while the phone
// still waits for the user, and the outcome follows on the next line
// (LOCAL-API.md §5).
func TestApprovalMatchComesFirst(t *testing.T) {
	seen := make(chan mockphone.ApprovalRequest, 1)
	release := make(chan protocol.Why)
	sp := newSecurePhone(t, func(o *mockphone.Options) {
		o.Approve = func(ctx context.Context, req mockphone.ApprovalRequest) protocol.Why {
			seen <- req
			return <-release
		}
	})
	d, url := startDaemon(t, sp.dial)
	if err := d.WaitConnected(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(testCtx(t), "POST", url+"/v1/approvals", strings.NewReader(`{"reason":"Deploy?"}`))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("%d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	lines := bufio.NewScanner(res.Body)
	var first daemon.MatchLine
	if !lines.Scan() || json.Unmarshal(lines.Bytes(), &first) != nil {
		t.Fatalf("first line: %q %v", lines.Bytes(), lines.Err())
	}
	shown := <-seen
	if first.Match != shown.Match || first.Program != shown.Program {
		t.Fatalf("the program got %+v, the phone shows %+v", first, shown)
	}
	release <- protocol.WhyMismatch
	var last daemon.ApprovalResponse
	if !lines.Scan() || json.Unmarshal(lines.Bytes(), &last) != nil || last.Approved || last.Why != "mismatch" {
		t.Fatalf("last line: %q", lines.Bytes())
	}
	if lines.Scan() {
		t.Errorf("a third line: %q", lines.Bytes())
	}
}

// TestApprovalWithoutPhoneRequest: an outcome known before anything is
// sent is the only line.
func TestApprovalWithoutPhoneRequest(t *testing.T) {
	sp := newSecurePhone(t, func(o *mockphone.Options) { o.ApprovalKey = func() *ecdsa.PrivateKey { return nil } })
	d, url := startDaemon(t, sp.dial)
	if err := d.WaitConnected(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	r := do(t, "POST", url+"/v1/approvals", `{"reason":"Deploy?"}`)
	if r.status != 200 || len(r.lines) != 1 || r.body["why"] != "unavailable" {
		t.Fatalf("%d %s", r.status, r.raw)
	}
}

// TestApprovalNamesTheProgram: the phone is told the file name of the
// program at the other end, over TCP and the Unix socket.
func TestApprovalNamesTheProgram(t *testing.T) {
	programs := make(chan string, 4)
	sp := newSecurePhone(t, func(o *mockphone.Options) {
		o.Approve = func(_ context.Context, req mockphone.ApprovalRequest) protocol.Why {
			programs <- req.Program
			return ""
		}
	})
	d, _ := startDaemon(t, sp.dial)
	if err := d.WaitConnected(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	want := protocol.DefaultProgram
	if runtime.GOOS == "linux" {
		exe, _ := os.Executable()
		want = filepath.Base(exe)
	}

	tcp := httptest.NewUnstartedServer(d.Handler())
	tcp.Config.ConnContext = daemon.ConnContext
	tcp.Start()
	defer tcp.Close()

	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	unix := &http.Server{Handler: d.Handler(), ConnContext: daemon.ConnContext}
	go unix.Serve(ln)
	defer unix.Close()
	unixClient := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}

	for name, post := range map[string]func() (*http.Response, error){
		"tcp": func() (*http.Response, error) {
			return http.Post(tcp.URL+"/v1/approvals", "application/json", strings.NewReader(`{"reason":"Deploy?"}`))
		},
		"unix": func() (*http.Response, error) {
			return unixClient.Post("http://phonetopc/v1/approvals", "application/json", strings.NewReader(`{"reason":"Deploy?"}`))
		},
	} {
		res, err := post()
		if err != nil {
			t.Fatal(err)
		}
		var first daemon.MatchLine
		json.NewDecoder(res.Body).Decode(&first)
		res.Body.Close()
		if got := <-programs; got != want || first.Program != want {
			t.Errorf("%s: the phone was told %q, the program %q; want %q", name, got, first.Program, want)
		}
	}
}

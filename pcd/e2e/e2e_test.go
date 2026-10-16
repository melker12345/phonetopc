//go:build unix

// Package e2e runs the real binaries as separate processes: phonetopcd
// against phonetopc-mock, paired with phonetopc pair.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/melker12345/phonetopc/pcd/internal/pairing"
)

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ptc-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	build := exec.Command("go", "build", "-o", dir+string(filepath.Separator), "./cmd/phonetopc", "./cmd/phonetopcd", "./cmd/phonetopc-mock")
	build.Dir = ".."
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building the binaries:", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	bin = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const timeout = 10 * time.Second

// sameUserStatus is the answer to a token-less TCP request from this user:
// only Linux can tell who is at the other end.
func sameUserStatus() int {
	if runtime.GOOS == "linux" {
		return 200
	}
	return 401
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// shortDir returns a directory whose paths fit in a Unix socket address.
func shortDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "ptc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// testEnv is a whole environment, so nothing of the user's leaks in.
type testEnv struct {
	rt   string
	vars []string
}

func newEnv(t *testing.T) testEnv {
	rt := shortDir(t)
	return testEnv{rt: rt, vars: []string{
		"HOME=" + t.TempDir(),
		"XDG_RUNTIME_DIR=" + rt,
		"XDG_CONFIG_HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
		"PHONETOPC_NO_MDNS=1",
	}}
}

func (e testEnv) socket() string { return filepath.Join(e.rt, "phonetopc.sock") }

type proc struct {
	name string
	cmd  *exec.Cmd
	out  syncBuffer
	done chan struct{}
}

func start(t *testing.T, env testEnv, name string, args ...string) *proc {
	t.Helper()
	p := &proc{name: name, cmd: exec.Command(filepath.Join(bin, name), args...), done: make(chan struct{})}
	p.cmd.Env = env.vars
	p.cmd.Stdout, p.cmd.Stderr = &p.out, &p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		p.kill()
		if t.Failed() {
			t.Logf("%s %v:\n%s", name, args, p.out.String())
		}
	})
	return p
}

func (p *proc) kill() {
	p.cmd.Process.Kill()
	<-p.done
}

func (p *proc) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-p.done:
		return p.cmd.ProcessState.ExitCode()
	case <-time.After(timeout):
		t.Fatalf("%s did not exit:\n%s", p.name, p.out.String())
		return -1
	}
}

// poll calls cond until it holds, failing if p exits first.
func poll(t *testing.T, p *proc, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		select {
		case <-p.done:
			t.Fatalf("%s exited waiting for %s:\n%s", p.name, what, p.out.String())
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s within %v", what, timeout)
		}
	}
}

// freeAddr returns a free loopback address below the ephemeral port range,
// so that no outgoing connection or ":0" listener takes it before the
// mock listens on it (again).
func freeAddr(t *testing.T) string {
	t.Helper()
	for range 100 {
		addr := "127.0.0.1:" + strconv.Itoa(20000+rand.IntN(12000))
		if ln, err := net.Listen("tcp", addr); err == nil {
			ln.Close()
			return addr
		}
	}
	t.Fatal("no free port")
	return ""
}

func startMock(t *testing.T, env testEnv, addr string, args ...string) *proc {
	t.Helper()
	args = append([]string{"--addr", addr, "--tap-every", "0", "--nfc-every", "0", "--user-every", "0", "--seed", "1"}, args...)
	p := start(t, env, "phonetopc-mock", args...)
	poll(t, p, "mock listening", func() bool {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	return p
}

// startDaemon starts phonetopcd and waits until it is connected to the
// phone.
func startDaemon(t *testing.T, env testEnv, args ...string) (*proc, *http.Client) {
	t.Helper()
	p := start(t, env, "phonetopcd", args...)
	uc := unixClient(env.socket())
	poll(t, p, "connection to the phone", func() bool {
		r, err := tryGet(uc, "http://phonetopc/v1/status", "")
		return err == nil && r.body["connected"] == true
	})
	return p, uc
}

func unixClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
}

type response struct {
	status int
	body   map[string]any
	raw    string
}

func (r response) errorCode() string {
	e, _ := r.body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func tryDo(c *http.Client, method, url, token, body string) (response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		return response{}, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := c.Do(req)
	if err != nil {
		return response{}, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	r := response{status: res.StatusCode, raw: string(b)}
	json.Unmarshal(b, &r.body)
	return r, err
}

func tryGet(c *http.Client, url, token string) (response, error) {
	return tryDo(c, "GET", url, token, "")
}

func do(t *testing.T, c *http.Client, method, url, token, body string) response {
	t.Helper()
	r, err := tryDo(c, method, url, token, body)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type stream struct {
	t  *testing.T
	ch chan map[string]any
}

// openStream opens an NDJSON event stream; it ends when the server ends
// it or the test does.
func openStream(t *testing.T, c *http.Client, url string) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	res, err := c.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		cancel()
		t.Fatalf("GET %s: %d %s", url, res.StatusCode, b)
	}
	s := &stream{t: t, ch: make(chan map[string]any, 1000)}
	go func() {
		defer close(s.ch)
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("bad line %q: %v", sc.Text(), err)
				return
			}
			s.ch <- m
		}
	}()
	t.Cleanup(func() { cancel(); res.Body.Close() })
	return s
}

func (s *stream) next() map[string]any {
	s.t.Helper()
	select {
	case m, ok := <-s.ch:
		if !ok {
			s.t.Fatal("stream ended")
		}
		return m
	case <-time.After(timeout):
		s.t.Fatalf("no event within %v", timeout)
		return nil
	}
}

// rest waits for the stream to end and returns the events it had left.
func (s *stream) rest() []map[string]any {
	s.t.Helper()
	var evs []map[string]any
	deadline := time.After(timeout)
	for {
		select {
		case m, ok := <-s.ch:
			if !ok {
				return evs
			}
			evs = append(evs, m)
		case <-deadline:
			s.t.Fatalf("stream didn't end within %v", timeout)
		}
	}
}

// nextFrom skips events of other sources.
func (s *stream) nextFrom(source string) map[string]any {
	s.t.Helper()
	for {
		if m := s.next(); m["source"] == source {
			return m
		}
	}
}

func wantDaemonEvent(t *testing.T, s *stream, typ string) {
	t.Helper()
	if ev := s.nextFrom("phonetopcd"); ev["type"] != typ {
		t.Fatalf("want phonetopcd/%s, got %v", typ, ev)
	}
}

func wantGyro(t *testing.T, s *stream, n int) {
	t.Helper()
	for range n {
		if ev := s.nextFrom("gyro"); ev["type"] != "rotation" {
			t.Fatalf("event: %v", ev)
		}
	}
}

// devDaemon runs a dev-mode mock and phonetopcd --insecure-dev on it.
func devDaemon(t *testing.T, args ...string) (env testEnv, mockAddr string, mock, d *proc, uc *http.Client) {
	t.Helper()
	env = newEnv(t)
	mockAddr = freeAddr(t)
	mock = startMock(t, env, mockAddr)
	d, uc = startDaemon(t, env, append([]string{"--insecure-dev", "--addr", mockAddr}, args...)...)
	return env, mockAddr, mock, d, uc
}

func TestDevModeOverSocketAndTCP(t *testing.T) {
	t.Parallel()
	tcp := freeAddr(t)
	env, _, _, _, uc := devDaemon(t, "--listen", tcp)

	r := do(t, uc, "GET", "http://phonetopc/v1/status", "", "")
	if dev, _ := r.body["device"].(map[string]any); r.status != 200 || dev["id"] != "6d6f636b00000001" {
		t.Fatalf("status: %s", r.raw)
	}
	r = do(t, uc, "GET", "http://phonetopc/v1/sensors/battery", "", "")
	if r.status != 200 || r.body["source"] != "battery" || r.body["value"] == nil {
		t.Fatalf("battery: %s", r.raw)
	}
	wantGyro(t, openStream(t, uc, "http://phonetopc/v1/events?source=gyro&rate=50"), 3)

	tok, err := os.ReadFile(filepath.Join(env.rt, "phonetopc.token"))
	if err != nil {
		t.Fatal(err)
	}
	if r := do(t, http.DefaultClient, "GET", "http://"+tcp+"/v1/status", "", ""); r.status != sameUserStatus() {
		t.Errorf("this user, no token: %s", r.raw)
	}
	if r := do(t, http.DefaultClient, "GET", "http://"+tcp+"/v1/status", strings.TrimSpace(string(tok)), ""); r.status != 200 || r.body["connected"] != true {
		t.Errorf("with token: %s", r.raw)
	}
}

func TestStreamSurvivesPhoneRestart(t *testing.T) {
	t.Parallel()
	env, mockAddr, mock, _, uc := devDaemon(t, "--listen", "off")
	s := openStream(t, uc, "http://phonetopc/v1/events?source=gyro&rate=50")
	wantGyro(t, s, 1)

	mock.kill()
	wantDaemonEvent(t, s, "disconnected")
	startMock(t, env, mockAddr)
	wantDaemonEvent(t, s, "connected")
	wantGyro(t, s, 3)
}

func TestSIGTERMEndsStreamsAndRemovesSocket(t *testing.T) {
	t.Parallel()
	env, _, _, d, uc := devDaemon(t, "--listen", "off")
	s := openStream(t, uc, "http://phonetopc/v1/events?source=gyro&rate=50")
	wantGyro(t, s, 1)

	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := d.wait(t); code != 0 {
		t.Fatalf("exit code %d:\n%s", code, d.out.String())
	}
	if _, err := os.Stat(env.socket()); !os.IsNotExist(err) {
		t.Errorf("socket left behind: %v", err)
	}
	rest := s.rest()
	if n := len(rest); n == 0 || rest[n-1]["source"] != "phonetopcd" || rest[n-1]["type"] != "error" {
		t.Errorf("events at the end %v, want phonetopcd/error last", rest)
	}
}

var uriRE = regexp.MustCompile(`phonetopc://pair\?\S+`)

// pairPhone pairs a mock with state in stateDir with this PC, the way a
// user does: phonetopc pair shows a URI, the phone scans it. It returns
// the PC's id.
func pairPhone(t *testing.T, env testEnv, stateDir, mockAddr string) string {
	t.Helper()
	pc := start(t, env, "phonetopc", "pair", "--yes", "--listen", "127.0.0.1:0", "--host", "127.0.0.1", "--timeout", "30s")
	var uri string
	poll(t, pc, "pairing URI", func() bool { uri = uriRE.FindString(pc.out.String()); return uri != "" })
	u, err := pairing.ParseURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	scan := exec.Command(filepath.Join(bin, "phonetopc-mock"), "pair", "--state", stateDir, "--addr", mockAddr, "--name", "E2E Phone", uri)
	scan.Env = env.vars
	if out, err := scan.CombinedOutput(); err != nil {
		t.Fatalf("phonetopc-mock pair: %v\n%s", err, out)
	}
	if code := pc.wait(t); code != 0 {
		t.Fatalf("phonetopc pair exited %d:\n%s", code, pc.out.String())
	}
	return u.ID
}

func TestPairedModeStreamsApprovesAndSeesRevocation(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	stateDir, mockAddr := t.TempDir(), freeAddr(t)
	pcID := pairPhone(t, env, stateDir, mockAddr)
	startMock(t, env, mockAddr, "--state", stateDir, "--approval", "auto", "--approval-delay", "1ms")
	d, uc := startDaemon(t, env, "--listen", "off")

	r := do(t, uc, "GET", "http://phonetopc/v1/status", "", "")
	dev, _ := r.body["device"].(map[string]any)
	caps, _ := r.body["capabilities"].([]any)
	if dev["name"] != "E2E Phone" || !slices.Contains(caps, any("approval")) {
		t.Fatalf("status: %s", r.raw)
	}
	s := openStream(t, uc, "http://phonetopc/v1/events?source=gyro&rate=50")
	wantGyro(t, s, 3)
	r = do(t, uc, "POST", "http://phonetopc/v1/approvals", "", `{"reason":"Deploy?"}`)
	if r.status != 200 || r.body["approved"] != true {
		t.Fatalf("approval: %s", r.raw)
	}

	revoke := exec.Command(filepath.Join(bin, "phonetopc-mock"), "revoke", "--state", stateDir, pcID)
	revoke.Env = env.vars
	if out, err := revoke.CombinedOutput(); err != nil {
		t.Fatalf("revoke: %v\n%s", err, out)
	}
	wantDaemonEvent(t, s, "disconnected")
	poll(t, d, "503 unauthorized", func() bool {
		r := do(t, uc, "GET", "http://phonetopc/v1/sensors/battery", "", "")
		return r.status == 503 && r.errorCode() == "unauthorized"
	})
}

// Command phonetopcd keeps one connection to the paired phone open and
// offers it to programs on this PC as a plain local HTTP API
// (spec/LOCAL-API.md):
//
//	curl --unix-socket $XDG_RUNTIME_DIR/phonetopc.sock http://phonetopc/v1/status
//	curl -N -H "Authorization: Bearer $(cat $XDG_RUNTIME_DIR/phonetopc.token)" \
//		'http://127.0.0.1:8767/v1/events?source=gyro'
//
// Pairing, TLS pinning, tokens, discovery and reconnecting stay in here.
package main

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/daemon"
	"github.com/melker12345/phonetopc/pcd/internal/hints"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/store"
	"github.com/melker12345/phonetopc/pcd/internal/version"
)

const DefaultListen = "127.0.0.1:8767"

// Defaults of --system.
const (
	SystemStore  = "/var/lib/phonetopc"
	SystemSocket = "/run/phonetopc/phonetopc.sock"
	SystemGroup  = "phonetopc-users"
)

const usage = `Usage: phonetopcd [flags]

Keeps a connection to the paired phone and serves the local API
(spec/LOCAL-API.md) on a Unix socket and on 127.0.0.1.

Started by systemd socket activation (phonetopc daemon install), it serves
the sockets systemd passes in, and --socket and --listen are ignored.

Flags:
  --system             run as the system service (phonetopc daemon install
                       --system): the pairings in /var/lib/phonetopc, the
                       socket /run/phonetopc/phonetopc.sock, members of
                       --group may use the API, root pairs; no token
  --store DIR          the pairings (default $XDG_CONFIG_HOME/phonetopc; with
                       --system, $STATE_DIRECTORY or /var/lib/phonetopc)
  --group G            members of group G (a name or gid) may use the API
                       (default with --system: phonetopc-users)
  --device D           paired phone to use, by name or id ($PHONETOPC_DEVICE);
                       needed only when several are paired
  --addr host:port     connect to this address instead of looking for the phone
                       ($PHONETOPC_ADDR)
  --insecure-dev       plaintext ws:// to a phone in developer mode (or the mock
                       without --state); default address 127.0.0.1:8765
                       ($PHONETOPC_INSECURE_DEV=1)
  --no-mdns            don't look for the phone with mDNS ($PHONETOPC_NO_MDNS=1)
  --socket PATH        Unix socket (default $XDG_RUNTIME_DIR/phonetopc.sock);
                       "off" turns it off
  --listen ADDR        loopback TCP address (default 127.0.0.1:8767); "off"
                       turns it off. Requests from web pages, from other
                       users, and on systems other than Linux need the token
                       from --token-file
  --token-file PATH    where the TCP token is kept (default
                       $XDG_RUNTIME_DIR/phonetopc.token)
  -v                   log every reconnect attempt, not only new errors
  --version            print the version

The phone's approval and decryption keys must be attested by its hardware
(phonetopc approval-key). $PHONETOPC_REQUIRE_STRONGBOX=1 also requires
StrongBox, $PHONETOPC_CHECK_REVOCATION=1 checks Google's revocation list
(online), and $PHONETOPC_APP_SIGNING_DIGEST=<hex SHA-256> accepts only an
app signed with that certificate.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stderr, os.Getenv, nil)
	stop()
	os.Exit(code)
}

type config struct {
	device, addr       string
	dev, noMDNS        bool
	system             bool
	storeDir, group    string
	socket, listen     string
	tokenFile          string
	verbose, showVersn bool
}

// run is main without process globals. ready, if set, gets the addresses
// it serves on once it does (for tests).
func run(ctx context.Context, args []string, stderr io.Writer, getenv func(string) string, ready func(socket, tcp string)) int {
	logger := log.New(stderr, "phonetopcd: ", 0)
	c := config{
		device: getenv("PHONETOPC_DEVICE"),
		addr:   getenv("PHONETOPC_ADDR"),
		dev:    envBool(getenv("PHONETOPC_INSECURE_DEV")),
		noMDNS: envBool(getenv("PHONETOPC_NO_MDNS")),
	}
	fs := flag.NewFlagSet("phonetopcd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.device, "device", c.device, "")
	fs.StringVar(&c.addr, "addr", c.addr, "")
	fs.BoolVar(&c.dev, "insecure-dev", c.dev, "")
	fs.BoolVar(&c.noMDNS, "no-mdns", c.noMDNS, "")
	fs.BoolVar(&c.system, "system", false, "")
	fs.StringVar(&c.storeDir, "store", "", "")
	fs.StringVar(&c.group, "group", "", "")
	fs.StringVar(&c.socket, "socket", "", "")
	fs.StringVar(&c.listen, "listen", DefaultListen, "")
	fs.StringVar(&c.tokenFile, "token-file", "", "")
	fs.BoolVar(&c.verbose, "v", false, "")
	fs.BoolVar(&c.showVersn, "version", false, "")
	if err := fs.Parse(args); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "phonetopcd:", err)
		}
		fmt.Fprint(stderr, usage)
		return 2
	}
	c.defaults(getenv)
	if c.showVersn {
		fmt.Fprintln(stderr, "phonetopcd", version.Version)
		return 0
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "phonetopcd: unexpected argument %q\n%s", fs.Arg(0), usage)
		return 2
	}
	if off(c.socket) && off(c.listen) {
		fmt.Fprintln(stderr, "phonetopcd: --socket and --listen are both off; nothing to serve on")
		return 2
	}

	access := daemon.UserAccess()
	if c.group != "" {
		gid, err := lookupGroup(c.group)
		if err != nil {
			logger.Print(err)
			return 1
		}
		access.Group = gid
	}
	explain := hints.Explain
	if c.system {
		explain = func(err error) string {
			return strings.ReplaceAll(hints.Explain(err), "`phonetopc ", "`sudo phonetopc ")
		}
	}
	dial, st, err := c.dialer(getenv)
	if err != nil {
		logger.Print(explain(err))
		return 1
	}
	d := daemon.New(daemon.Options{Dial: dial, Logf: logger.Printf, Explain: explain, Verbose: c.verbose, Store: st, Device: c.device})

	var (
		servers         []*http.Server
		socketAt, tcpAt string
	)
	serve := func(ln net.Listener, h http.Handler) {
		srv := &http.Server{
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
			ErrorLog:          log.New(io.Discard, "", 0),
			ConnContext:       access.ConnContext,
		}
		servers = append(servers, srv)
		go srv.Serve(ln)
	}
	serveUnix := func(ln net.Listener) {
		serve(ln, daemon.RequirePeer(d.Handler()))
		socketAt = ln.Addr().String()
	}
	serveTCP := func(ln net.Listener) bool {
		token := ""
		if !c.system {
			var err error
			if token, err = loadToken(c.tokenFile); err != nil {
				logger.Print(err)
				return false
			}
		}
		serve(ln, daemon.RequireToken(token, d.Handler()))
		tcpAt = ln.Addr().String()
		return true
	}
	inherited, err := activated(getenv)
	if err != nil {
		logger.Print(err)
		return 1
	}
	for _, ln := range inherited {
		switch ln.Addr().Network() {
		case "unix":
			serveUnix(ln)
		case "tcp":
			if !serveTCP(ln) {
				return 1
			}
		default:
			logger.Printf("ignoring a %s socket passed in by systemd", ln.Addr().Network())
			ln.Close()
		}
	}
	if len(inherited) == 0 && !off(c.socket) {
		ln, err := listenUnix(c.socket, access.Group)
		if err != nil {
			logger.Print(err)
			return 1
		}
		defer os.Remove(c.socket)
		serveUnix(ln)
	}
	if len(inherited) == 0 && !off(c.listen) {
		if err := checkLoopback(c.listen); err != nil {
			logger.Print(err)
			return 1
		}
		ln, err := net.Listen("tcp", c.listen)
		if err != nil {
			logger.Printf("%v (is phonetopcd already running? --listen picks another address)", err)
			return 1
		}
		if !serveTCP(ln) {
			return 1
		}
	}
	if socketAt != "" {
		logger.Printf("serving on unix:%s", socketAt)
	}
	switch {
	case tcpAt != "" && c.system:
		logger.Printf("serving on http://%s", tcpAt)
	case tcpAt != "":
		logger.Printf("serving on http://%s (token in %s)", tcpAt, c.tokenFile)
	}
	if ready != nil {
		ready(socketAt, tcpAt)
	}

	d.Run(ctx)

	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, srv := range servers {
		srv.Shutdown(sctx)
	}
	return 0
}

func off(s string) bool { return s == "" || s == "off" }

func envBool(s string) bool {
	b, err := strconv.ParseBool(s)
	return err == nil && b
}

// runtimeDir is $XDG_RUNTIME_DIR, or a private directory under the temp
// directory where that isn't set.
func runtimeDir(getenv func(string) string) string {
	if x := getenv("XDG_RUNTIME_DIR"); x != "" && filepath.IsAbs(x) {
		return x
	}
	dir := filepath.Join(os.TempDir(), "phonetopc-"+strconv.Itoa(os.Getuid()))
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// dialer fails early if there is no paired phone to connect to, except in
// the system service.
func (c config) dialer(getenv func(string) string) (daemon.Dialer, *store.Store, error) {
	info := client.WithClientInfo("phonetopcd", version.Version)
	if c.dev {
		addr := c.addr
		if addr == "" {
			addr = fmt.Sprintf("127.0.0.1:%d", protocol.DefaultPort)
		}
		return func(ctx context.Context) (*client.Client, error) {
			dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			return client.Dial(dctx, client.URL(addr), info)
		}, nil, nil
	}
	dir := c.storeDir
	if dir == "" {
		var err error
		if dir, err = store.DefaultDir(getenv); err != nil {
			return nil, nil, err
		}
	}
	st := store.Open(dir)
	if _, err := st.Find(c.device); err != nil && !c.system {
		return nil, nil, err
	}
	opts := []client.Option{info, client.WithConfigDir(dir), client.WithDiscovery(!c.noMDNS, 0)}
	if c.addr != "" {
		opts = append(opts, client.WithAddress(c.addr))
	}
	aopts, err := client.AttestationOptionsFromEnv(getenv)
	if err != nil {
		return nil, nil, err
	}
	opts = append(opts, aopts...)
	if attestationRoots != nil {
		opts = append(opts, client.WithAttestationRoots(attestationRoots...))
	}
	return func(ctx context.Context) (*client.Client, error) {
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return client.DialPaired(dctx, c.device, opts...)
	}, st, nil
}

// attestationRoots replaces Google's attestation roots; only tests set it.
var attestationRoots []*x509.Certificate

// listenUnix listens on a Unix socket only this user can use, and members
// of group gid if it isn't -1. A socket file left behind by a daemon that
// died is replaced; one that answers means another daemon is running.
func listenUnix(path string, gid int) (net.Listener, error) {
	if len(path) > 107 {
		return nil, fmt.Errorf("socket path %s is too long for a Unix socket (%d bytes, at most 107); pick a shorter one with --socket", path, len(path))
	}
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		conn.Close()
		return nil, fmt.Errorf("another phonetopcd is already serving on %s", path)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != os.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket; not touching it", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	perm, dirPerm := os.FileMode(0o600), os.FileMode(0o700)
	if gid >= 0 {
		perm, dirPerm = 0o660, 0o755
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, err
	}
	ln, err := listenUnixMode(path, perm)
	if err != nil {
		return nil, err
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false) // removed by run, after the servers stop
	if gid >= 0 {
		if err := os.Chown(path, -1, gid); err != nil {
			ln.Close()
			os.Remove(path)
			return nil, fmt.Errorf("giving the socket to group %d: %w", gid, err)
		}
	}
	return ln, nil
}

// lookupGroup returns the gid of group, a name or a number.
func lookupGroup(group string) (int, error) {
	if gid, err := strconv.Atoi(group); err == nil {
		return gid, nil
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return 0, fmt.Errorf("--group %s: %w", group, err)
	}
	return strconv.Atoi(g.Gid)
}

// defaults fills in what the flags left out, which depends on --system.
func (c *config) defaults(getenv func(string) string) {
	if c.system {
		if c.storeDir == "" {
			c.storeDir = SystemStore
			if dirs := getenv("STATE_DIRECTORY"); dirs != "" {
				c.storeDir, _, _ = strings.Cut(dirs, ":")
			}
		}
		if c.socket == "" {
			c.socket = SystemSocket
		}
		if c.group == "" {
			c.group = SystemGroup
		}
		return
	}
	rt := ""
	if c.socket == "" || c.tokenFile == "" {
		rt = runtimeDir(getenv)
	}
	if c.socket == "" {
		c.socket = filepath.Join(rt, "phonetopc.sock")
	}
	if c.tokenFile == "" {
		c.tokenFile = filepath.Join(rt, "phonetopc.token")
	}
}

// checkLoopback refuses to serve the API beyond this machine: it controls
// the phone.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--listen %q: want host:port", addr)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("--listen %q: only loopback addresses (127.0.0.1, [::1]) are allowed", addr)
	}
	return nil
}

// loadToken returns the token in path, creating it (mode 0600) if it
// doesn't exist yet. Keeping it across restarts lets running programs keep
// using it.
func loadToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("%s can be read by other users (mode %v); remove it and restart", path, fi.Mode().Perm())
		}
		if tok := strings.TrimSpace(string(b)); len(tok) >= 32 {
			return tok, nil
		}
	}
	tok := security.NewToken()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	os.Remove(tmp)
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tok, nil
}

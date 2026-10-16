//go:build linux

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSystemService runs phonetopcd as the system service does, minus the
// separate user: it owns its store, and phonetopc pairs, accepts the key
// and asks for approvals through it.
func TestSystemService(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	sock := filepath.Join(env.rt, "sys.sock")
	env.vars = append(env.vars, "PHONETOPC_MODE=system", "PHONETOPC_SOCKET="+sock)
	storeDir, stateDir, mockAddr := filepath.Join(t.TempDir(), "phonetopc"), t.TempDir(), freeAddr(t)
	d := start(t, env, "phonetopcd", "--system", "--store", storeDir, "--socket", sock, "--group", strconv.Itoa(os.Getgid()), "--listen", "off")
	uc := unixClient(sock)
	poll(t, d, "the socket", func() bool {
		r, err := tryGet(uc, "http://phonetopc/v1/status", "")
		return err == nil && r.status == 200
	})

	pairPhone(t, env, stateDir, mockAddr)
	if _, err := os.Stat(filepath.Join(storeDir, "devices.json")); err != nil {
		t.Fatalf("the daemon's store: %v", err)
	}
	startMock(t, env, mockAddr, "--state", stateDir, "--approval", "auto", "--approval-delay", "1ms")
	poll(t, d, "connection to the phone", func() bool {
		r, err := tryGet(uc, "http://phonetopc/v1/status", "")
		return err == nil && r.body["connected"] == true
	})

	cli := func(args ...string) (string, error) {
		cmd := exec.Command(filepath.Join(bin, "phonetopc"), args...)
		cmd.Env = env.vars
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// The mock's attestation comes from a test root.
	if out, err := cli("approval-key", "--accept", "--unattested"); err != nil || !strings.Contains(out, "accepted now") {
		t.Fatalf("approval-key --accept --unattested: %v\n%s", err, out)
	}
	if out, err := cli("approve", "Deploy?"); err != nil || !strings.Contains(out, "approved on") || !strings.Contains(out, "pick ") {
		t.Fatalf("approve: %v\n%s", err, out)
	}
	if out, err := cli("unpair", "E2E Phone"); err != nil || !strings.Contains(out, "Forgot") {
		t.Fatalf("unpair: %v\n%s", err, out)
	}
	poll(t, d, "the phone forgotten", func() bool {
		r, err := tryGet(uc, "http://phonetopc/v1/status", "")
		return err == nil && r.body["connected"] == false
	})
}

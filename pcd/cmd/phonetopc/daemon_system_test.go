package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHost replaces what install --system does to the machine: files go
// below a temporary root, and commands and chowns are only recorded.
type fakeHostRun struct {
	root, home string
	calls      []string
	chowns     []string
}

func fakeHost(t *testing.T, euid int) *fakeHostRun {
	t.Helper()
	f := &fakeHostRun{root: t.TempDir(), home: t.TempDir()}
	exe := filepath.Join(t.TempDir(), "phonetopcd")
	if err := os.WriteFile(exe, []byte("the daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldHost, oldFind := host, findDaemon
	host = sysHost{
		root: f.root,
		euid: func() int { return euid },
		run: func(_ context.Context, name string, args ...string) error {
			f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
			return nil
		},
		lookupUser: func(name string) (int, int, string, error) {
			switch name {
			case "phonetopc":
				return 990, 990, "/var/lib/phonetopc", nil
			case "alice":
				return 1000, 1000, f.home, nil
			}
			return 0, 0, "", errors.New("unknown user " + name)
		},
		chown: func(path string, uid, gid int) error {
			rel, _ := filepath.Rel(f.root, path)
			f.chowns = append(f.chowns, fmt.Sprintf("/%s %d:%d", rel, uid, gid))
			return nil
		},
	}
	findDaemon = func() (string, error) { return exe, nil }
	t.Cleanup(func() { host, findDaemon = oldHost, oldFind })
	return f
}

func (f *fakeHostRun) userStore() string { return filepath.Join(f.home, ".config", "phonetopc") }

func (f *fakeHostRun) writeUserStore(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(f.userStore(), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pc.json", "pc-key.pem", "devices.json", ".lock"} {
		if err := os.WriteFile(filepath.Join(f.userStore(), name), []byte(name+" content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInstallSystem(t *testing.T) {
	f := fakeHost(t, 0)
	f.writeUserStore(t)
	env := testEnv(t, map[string]string{"SUDO_USER": "alice"})
	code, out, errs := runCLIWith(t, context.Background(), env, strings.NewReader("y\ny\ny\n"), "daemon", "install", "--system")
	if code != 0 {
		t.Fatalf("code %d\n%s\n%s", code, out, errs)
	}
	want := []string{
		"systemd-sysusers /etc/sysusers.d/phonetopc.conf",
		"usermod -a -G phonetopc-users alice",
		"systemctl daemon-reload",
		"systemctl enable --now phonetopcd.socket",
		"systemctl try-restart phonetopcd.service",
	}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Errorf("commands %q, want %q", f.calls, want)
	}

	bin, err := os.ReadFile(filepath.Join(f.root, "usr/local/bin/phonetopcd"))
	if fi, _ := os.Stat(filepath.Join(f.root, "usr/local/bin/phonetopcd")); err != nil || string(bin) != "the daemon" || fi.Mode().Perm() != 0o755 {
		t.Errorf("binary: %q %v", bin, err)
	}
	svc, err := os.ReadFile(filepath.Join(f.root, "etc/systemd/system/phonetopcd.service"))
	if err != nil || !strings.Contains(string(svc), "ExecStart=/usr/local/bin/phonetopcd --system\n") {
		t.Errorf("service (%v):\n%s", err, svc)
	}
	for _, p := range []string{"etc/systemd/system/phonetopcd.socket", "etc/sysusers.d/phonetopc.conf"} {
		if _, err := os.Stat(filepath.Join(f.root, p)); err != nil {
			t.Error(err)
		}
	}

	// The pairings moved, owned by the service's user.
	for _, name := range storeFiles {
		p := filepath.Join(f.root, "var/lib/phonetopc", name)
		b, err := os.ReadFile(p)
		if fi, _ := os.Stat(p); err != nil || string(b) != name+" content" || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %q %v", name, b, err)
		}
		if _, err := os.Stat(filepath.Join(f.userStore(), name)); !os.IsNotExist(err) {
			t.Errorf("%s left in the user's store: %v", name, err)
		}
	}
	wantChowns := []string{"/var/lib/phonetopc 990:990", "/var/lib/phonetopc/pc.json 990:990", "/var/lib/phonetopc/pc-key.pem 990:990", "/var/lib/phonetopc/devices.json 990:990"}
	if strings.Join(f.chowns, "|") != strings.Join(wantChowns, "|") {
		t.Errorf("chowns %q, want %q", f.chowns, wantChowns)
	}
	if fi, err := os.Stat(filepath.Join(f.root, "var/lib/phonetopc")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("state directory: %v %v", fi.Mode(), err)
	}
	if _, err := os.Stat(f.userStore()); !os.IsNotExist(err) {
		t.Errorf("empty user store left: %v", err)
	}

	// Again: the service's store has pairings now, so nothing is moved.
	f.writeUserStore(t)
	f.calls = nil
	code, out, errs = runCLIWith(t, context.Background(), env, strings.NewReader("n\n"), "daemon", "install", "--system")
	if code != 0 || !strings.Contains(out, "Not moving") {
		t.Fatalf("reinstall: %d\n%s\n%s", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(f.userStore(), "devices.json")); err != nil {
		t.Errorf("the user's pairings: %v", err)
	}

	f.calls = nil
	code, out, errs = runCLI(t, context.Background(), env, "daemon", "uninstall", "--system")
	if code != 0 || !strings.Contains(out, "Kept the pairings in /var/lib/phonetopc") {
		t.Fatalf("uninstall: %d\n%s\n%s", code, out, errs)
	}
	want = []string{"systemctl disable --now phonetopcd.socket phonetopcd.service", "systemctl daemon-reload"}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Errorf("commands %q, want %q", f.calls, want)
	}
	for _, p := range []string{"etc/systemd/system/phonetopcd.socket", "etc/systemd/system/phonetopcd.service", "etc/sysusers.d/phonetopc.conf"} {
		if _, err := os.Stat(filepath.Join(f.root, p)); !os.IsNotExist(err) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, "var/lib/phonetopc/devices.json")); err != nil {
		t.Errorf("uninstall removed the pairings: %v", err)
	}
	if _, out, _ := runCLI(t, context.Background(), env, "daemon", "uninstall", "--system"); !strings.Contains(out, "isn't installed") {
		t.Errorf("second uninstall: %s", out)
	}
}

func TestInstallSystemKeepsTheUsersCopyIfAsked(t *testing.T) {
	f := fakeHost(t, 0)
	f.writeUserStore(t)
	env := testEnv(t, map[string]string{"SUDO_USER": "alice"})
	code, out, errs := runCLIWith(t, context.Background(), env, strings.NewReader("y\nn\nn\n"), "daemon", "install", "--system")
	if code != 0 || !strings.Contains(out, "can still read its token and key") {
		t.Fatalf("code %d\n%s\n%s", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(f.userStore(), "devices.json")); err != nil {
		t.Error(err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "usermod") {
			t.Errorf("added to the group after no: %s", c)
		}
	}
}

func TestInstallSystemRefusals(t *testing.T) {
	f := fakeHost(t, 1000)
	if code, _, errs := runCLI(t, context.Background(), nil, "daemon", "install", "--system"); code != 1 || !strings.Contains(errs, "sudo phonetopc daemon install --system") {
		t.Errorf("not root: %d %s", code, errs)
	}
	if code, _, errs := runCLI(t, context.Background(), nil, "daemon", "uninstall", "--system"); code != 1 || !strings.Contains(errs, "sudo") {
		t.Errorf("not root: %d %s", code, errs)
	}

	f = fakeHost(t, 0)
	units := filepath.Join(f.home, ".config", "systemd", "user")
	os.MkdirAll(units, 0o755)
	os.WriteFile(filepath.Join(units, "phonetopcd.socket"), nil, 0o644)
	env := testEnv(t, map[string]string{"SUDO_USER": "alice"})
	if code, _, errs := runCLI(t, context.Background(), env, "daemon", "install", "--system"); code != 1 || !strings.Contains(errs, "phonetopc daemon uninstall") {
		t.Errorf("user units: %d %s", code, errs)
	}
	if len(f.calls) != 0 {
		t.Errorf("went on: %q", f.calls)
	}
}

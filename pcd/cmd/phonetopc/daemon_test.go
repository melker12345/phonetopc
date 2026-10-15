package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSystemd replaces systemctl and findDaemon, recording the calls.
func fakeSystemd(t *testing.T, fail map[string]error) *[]string {
	t.Helper()
	var calls []string
	oldCtl, oldFind := systemctl, findDaemon
	systemctl = func(_ context.Context, args ...string) error {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		return fail[call]
	}
	findDaemon = func() (string, error) { return "/opt/ptc/phonetopcd", nil }
	t.Cleanup(func() { systemctl, findDaemon = oldCtl, oldFind })
	return &calls
}

func TestDaemonInstallAndUninstall(t *testing.T) {
	calls := fakeSystemd(t, nil)
	env := testEnv(t, nil)
	units := filepath.Join(env["XDG_CONFIG_HOME"], "systemd", "user")

	code, out, errs := runCLI(t, context.Background(), env, "daemon", "install")
	if code != 0 || !strings.Contains(out, "curl localhost:8767/v1/status") {
		t.Fatalf("code %d\nstdout %s\nstderr %s", code, out, errs)
	}
	svc, err := os.ReadFile(filepath.Join(units, "phonetopcd.service"))
	if err != nil || !strings.Contains(string(svc), "ExecStart=/opt/ptc/phonetopcd\n") {
		t.Fatalf("service unit (%v):\n%s", err, svc)
	}
	if _, err := os.Stat(filepath.Join(units, "phonetopcd.socket")); err != nil {
		t.Fatal(err)
	}
	want := []string{"show-environment", "daemon-reload", "enable --now phonetopcd.socket"}
	if strings.Join(*calls, "|") != strings.Join(want, "|") {
		t.Fatalf("systemctl calls %q, want %q", *calls, want)
	}

	*calls = nil
	if code, out, errs := runCLI(t, context.Background(), env, "daemon", "uninstall"); code != 0 {
		t.Fatalf("uninstall: code %d\n%s%s", code, out, errs)
	}
	want = []string{"disable --now phonetopcd.socket phonetopcd.service", "daemon-reload"}
	if strings.Join(*calls, "|") != strings.Join(want, "|") {
		t.Fatalf("systemctl calls %q, want %q", *calls, want)
	}
	if entries, _ := os.ReadDir(units); len(entries) != 0 {
		t.Fatalf("units left behind: %v", entries)
	}
	if _, out, _ := runCLI(t, context.Background(), env, "daemon", "uninstall"); !strings.Contains(out, "isn't installed") {
		t.Fatalf("second uninstall: %s", out)
	}
}

func TestDaemonInstallWithoutSystemd(t *testing.T) {
	calls := fakeSystemd(t, map[string]error{"show-environment": errNoSystemd})
	env := testEnv(t, nil)
	code, _, errs := runCLI(t, context.Background(), env, "daemon", "install")
	if code != 1 || !strings.Contains(errs, "Start /opt/ptc/phonetopcd yourself") {
		t.Fatalf("code %d: %s", code, errs)
	}
	if len(*calls) != 1 {
		t.Fatalf("went on after show-environment failed: %q", *calls)
	}
	if _, err := os.Stat(filepath.Join(env["XDG_CONFIG_HOME"], "systemd")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrote units anyway: %v", err)
	}
}

func TestDaemonInstallSocketBusy(t *testing.T) {
	fakeSystemd(t, map[string]error{"enable --now phonetopcd.socket": errors.New("Job failed")})
	code, _, errs := runCLI(t, context.Background(), nil, "daemon", "install")
	if code != 1 || !strings.Contains(errs, "a phonetopcd you started yourself") {
		t.Fatalf("code %d: %s", code, errs)
	}
}

func TestDaemonUsage(t *testing.T) {
	fakeSystemd(t, nil)
	for _, args := range [][]string{{"daemon"}, {"daemon", "start"}} {
		if code, _, errs := runCLI(t, context.Background(), nil, args...); code != 2 {
			t.Errorf("%v: code %d: %s", args, code, errs)
		}
	}
}

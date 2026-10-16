package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/melker12345/phonetopc/pcd/systemd"
)

// systemctl runs `systemctl --user args...`; tests replace it.
var systemctl = func(ctx context.Context, args ...string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errNoSystemd
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return nil
}

var errNoSystemd = errors.New("systemctl not found")

// findDaemon returns the phonetopcd to install: the one next to this
// phonetopc, else the one on PATH. Tests replace it.
var findDaemon = func() (string, error) {
	if exe, err := os.Executable(); err == nil {
		if exe, err := filepath.EvalSymlinks(exe); err == nil {
			p := filepath.Join(filepath.Dir(exe), "phonetopcd")
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p, nil
			}
		}
	}
	p, err := exec.LookPath("phonetopcd")
	if err != nil {
		return "", errors.New("phonetopcd isn't next to phonetopc or on PATH; build it first (go build ./cmd/...)")
	}
	return filepath.Abs(p)
}

func (c *cli) daemon(ctx context.Context, args []string) error {
	fs := c.flagSet("daemon")
	system := fs.Bool("system", false, "")
	rest, err := parse(fs, args, 1, "of install or uninstall")
	if err != nil {
		return err
	}
	switch {
	case rest[0] == "install" && *system:
		return c.installSystem(ctx)
	case rest[0] == "uninstall" && *system:
		return c.uninstallSystem(ctx)
	case rest[0] == "install":
		return c.installDaemon(ctx)
	case rest[0] == "uninstall":
		return c.uninstallDaemon(ctx)
	}
	return usageErr{fmt.Errorf("unknown daemon command %q (want install or uninstall)", rest[0])}
}

func (c *cli) unitDir() string {
	base := c.getenv("XDG_CONFIG_HOME")
	if base == "" || !filepath.IsAbs(base) {
		base = filepath.Join(c.getenv("HOME"), ".config")
	}
	return filepath.Join(base, "systemd", "user")
}

func (c *cli) daemonInstalled() bool {
	_, err := os.Stat(filepath.Join(c.unitDir(), systemd.Socket))
	return err == nil
}

func (c *cli) installDaemon(ctx context.Context) error {
	exe, err := findDaemon()
	if err != nil {
		return err
	}
	if api, err := c.system(); err == nil && api != nil {
		return fmt.Errorf("the phonetopcd system service is installed (%s); a user service would only get in its way", api.Socket())
	}
	if err := systemctl(ctx, "show-environment"); err != nil {
		fmt.Fprintf(c.stderr, "phonetopc: no systemd user manager here (%v).\n"+
			"Start %s yourself instead, e.g. from your session's autostart.\n", err, exe)
		return exitError{code: 1}
	}
	dir := c.unitDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, content := range systemd.Units(exe) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	if err := systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if err := systemctl(ctx, "enable", "--now", systemd.Socket); err != nil {
		return fmt.Errorf("%w\n(a phonetopcd you started yourself holds the socket: stop it, then run `phonetopc daemon install` again)", err)
	}
	fmt.Fprintf(c.stdout, "Installed %s and %s in %s.\n"+
		"phonetopcd now starts when a program connects, e.g.: curl localhost:8767/v1/status\n"+
		"Safer: `sudo phonetopc daemon install --system` runs it as its own user, so other programs\n"+
		"running as you can't stop it, take its place or read its keys.\n", systemd.Socket, systemd.Service, dir)
	return nil
}

func (c *cli) uninstallDaemon(ctx context.Context) error {
	if !c.daemonInstalled() {
		fmt.Fprintln(c.stdout, "phonetopcd isn't installed.")
		return nil
	}
	if err := systemctl(ctx, "disable", "--now", systemd.Socket, systemd.Service); err != nil && !errors.Is(err, errNoSystemd) {
		return err
	}
	for _, name := range []string{systemd.Socket, systemd.Service} {
		if err := os.Remove(filepath.Join(c.unitDir(), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := systemctl(ctx, "daemon-reload"); err != nil && !errors.Is(err, errNoSystemd) {
		return err
	}
	fmt.Fprintln(c.stdout, "Uninstalled phonetopcd's units; it no longer starts by itself.")
	return nil
}

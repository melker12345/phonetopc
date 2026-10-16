package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/melker12345/phonetopc/pcd/internal/store"
	"github.com/melker12345/phonetopc/pcd/systemd"
)

// The system service (`daemon install --system`): phonetopcd runs as its
// own user, so programs running as you can't stop it, take its place, or
// read its keys and tokens.

const (
	systemUser    = "phonetopc"
	systemGroup   = "phonetopc-users"
	systemStore   = "/var/lib/phonetopc"
	systemUnitDir = "/etc/systemd/system"
	sysusersDir   = "/etc/sysusers.d"
	systemBinary  = "/usr/local/bin/phonetopcd"
)

// sysHost is what install --system changes on the machine; tests replace
// it, and nothing here runs without root.
type sysHost struct {
	root       string // the files are written below it
	euid       func() int
	run        func(ctx context.Context, name string, args ...string) error
	lookupUser func(name string) (uid, gid int, home string, err error)
	chown      func(path string, uid, gid int) error
}

var host = sysHost{
	root: "/",
	euid: os.Geteuid,
	run: func(ctx context.Context, name string, args ...string) error {
		var out bytes.Buffer
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
		}
		return nil
	},
	lookupUser: func(name string) (int, int, string, error) {
		u, err := user.Lookup(name)
		if err != nil {
			return 0, 0, "", err
		}
		uid, err1 := strconv.Atoi(u.Uid)
		gid, err2 := strconv.Atoi(u.Gid)
		return uid, gid, u.HomeDir, errors.Join(err1, err2)
	},
	chown: os.Chown,
}

func (h sysHost) path(p string) string { return filepath.Join(h.root, p) }

// The files that make up a credential store (SECURITY.md §6).
var storeFiles = []string{store.PCFile, store.PCKeyFile, store.DevicesFile}

func (c *cli) installSystem(ctx context.Context) error {
	if host.euid() != 0 {
		return errors.New("installing a system service needs root: run `sudo phonetopc daemon install --system`")
	}
	exe, err := findDaemon()
	if err != nil {
		return err
	}
	// The user who ran sudo: whose pairings to move, and who may use it.
	who, home := c.getenv("SUDO_USER"), ""
	if who == "root" {
		who = ""
	}
	if who != "" {
		if _, _, home, err = host.lookupUser(who); err != nil {
			return fmt.Errorf("looking up %s: %w", who, err)
		}
		if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", systemd.Socket)); err == nil {
			return fmt.Errorf("%s has phonetopcd as a user service, which holds localhost:8767: first run `phonetopc daemon uninstall` as %s, without sudo", who, who)
		}
	}

	if exe, err = installBinary(exe); err != nil {
		return err
	}
	units := systemd.SystemUnits(exe)
	for name, content := range units {
		dir := systemUnitDir
		if name == systemd.Sysusers {
			dir = sysusersDir
		}
		if err := writeFile(host.path(filepath.Join(dir, name)), []byte(content), 0o644); err != nil {
			return err
		}
	}
	if err := host.run(ctx, "systemd-sysusers", filepath.Join(sysusersDir, systemd.Sysusers)); err != nil {
		return err
	}
	prompt := newTermPrompter(c.stdin, c.stdout, false, isTerminal(c.stdin))
	if home != "" {
		if err := c.moveStore(ctx, prompt, filepath.Join(home, ".config", "phonetopc")); err != nil {
			return err
		}
		if ok, _ := prompt.ask(ctx, fmt.Sprintf("Let %s use the phone (add them to the group %s)? [Y/n]", who, systemGroup), true); ok {
			if err := host.run(ctx, "usermod", "-a", "-G", systemGroup, who); err != nil {
				return err
			}
			fmt.Fprintf(c.stdout, "Added %s to %s; it takes effect at their next login.\n", who, systemGroup)
		}
	}
	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", "--now", systemd.Socket},
		{"try-restart", systemd.Service}, // a new binary, if it was running
	} {
		if err := host.run(ctx, "systemctl", args...); err != nil {
			return err
		}
	}
	fmt.Fprintf(c.stdout, "Installed the system service phonetopcd (%s, %s in %s).\n"+
		"It runs as the user %s; members of %s may use it (sudo usermod -aG %s <user>).\n"+
		"Pair a phone with: sudo phonetopc pair\n", systemd.Socket, systemd.Service, systemUnitDir, systemUser, systemGroup, systemGroup)
	return nil
}

// installBinary returns the phonetopcd for the service to run: exe if root
// already installed it there, else a copy in /usr/local/bin. One that you
// could replace would make the service yours again.
func installBinary(exe string) (string, error) {
	if exe == "/usr/bin/phonetopcd" || exe == systemBinary {
		return exe, nil
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		return "", err
	}
	if err := writeFile(host.path(systemBinary), b, 0o755); err != nil {
		return "", err
	}
	return systemBinary, nil
}

// writeFile replaces path atomically, creating its directory.
func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// moveStore offers to move the pairings in from (a user's own store) to
// the service's, which must have none yet.
func (c *cli) moveStore(ctx context.Context, prompt *termPrompter, from string) error {
	if !hasStore(from) {
		return nil
	}
	to := host.path(systemStore)
	if hasStore(to) {
		fmt.Fprintf(c.stdout, "Not moving the pairings in %s: %s has its own.\n", from, systemStore)
		return nil
	}
	if ok, _ := prompt.ask(ctx, fmt.Sprintf("Move the pairings in %s to %s, so the service uses them? [Y/n]", from, systemStore), true); !ok {
		return nil
	}
	uid, gid, _, err := host.lookupUser(systemUser)
	if err != nil {
		return fmt.Errorf("looking up %s: %w", systemUser, err)
	}
	if err := os.MkdirAll(to, 0o700); err != nil {
		return err
	}
	if err := host.chown(to, uid, gid); err != nil {
		return err
	}
	for _, name := range storeFiles {
		b, err := os.ReadFile(filepath.Join(from, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		dst := filepath.Join(to, name)
		if err := writeFile(dst, b, 0o600); err != nil {
			return err
		}
		if err := host.chown(dst, uid, gid); err != nil {
			return err
		}
	}
	fmt.Fprintf(c.stdout, "Copied the pairings to %s.\n", systemStore)
	// A copy left in the user's home would still be readable by programs
	// running as them, which is what the service is for.
	if ok, _ := prompt.ask(ctx, fmt.Sprintf("Remove them from %s? [Y/n]", from), true); !ok {
		fmt.Fprintf(c.stdout, "Kept %s; programs running as you can still read its token and key.\n", from)
		return nil
	}
	for _, name := range append(storeFiles, ".lock") {
		if err := os.Remove(filepath.Join(from, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	os.Remove(from) // only if empty
	fmt.Fprintf(c.stdout, "Removed the pairings from %s.\n", from)
	return nil
}

func hasStore(dir string) bool {
	for _, name := range storeFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

func (c *cli) uninstallSystem(ctx context.Context) error {
	if host.euid() != 0 {
		return errors.New("removing the system service needs root: run `sudo phonetopc daemon uninstall --system`")
	}
	if _, err := os.Stat(host.path(filepath.Join(systemUnitDir, systemd.Socket))); err != nil {
		fmt.Fprintln(c.stdout, "The phonetopcd system service isn't installed.")
		return nil
	}
	if err := host.run(ctx, "systemctl", "disable", "--now", systemd.Socket, systemd.Service); err != nil {
		return err
	}
	for _, p := range []string{filepath.Join(systemUnitDir, systemd.Socket), filepath.Join(systemUnitDir, systemd.Service), filepath.Join(sysusersDir, systemd.Sysusers)} {
		if err := os.Remove(host.path(p)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := host.run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "Removed the phonetopcd system service. Kept the pairings in %s, the user %s and the group %s;\n"+
		"to remove them too: sudo rm -r %s; sudo userdel %s; sudo groupdel %s\n",
		systemStore, systemUser, systemGroup, systemStore, systemUser, systemGroup)
	return nil
}

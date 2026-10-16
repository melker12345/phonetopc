// Package systemd holds phonetopcd's systemd units, for `phonetopc daemon
// install` and for packages: user units in this directory, and the system
// service in system/.
package systemd

import (
	"embed"
	"strings"
)

//go:embed phonetopcd.service phonetopcd.socket system
var files embed.FS

const (
	Socket  = "phonetopcd.socket"
	Service = "phonetopcd.service"
	// Sysusers is the sysusers.d(5) file of the system service.
	Sysusers = "phonetopc.conf"
)

// Units returns the user units by name, with the service starting the
// phonetopcd binary at exe.
func Units(exe string) map[string]string {
	return units("", exe, "/usr/bin/phonetopcd")
}

// SystemUnits returns the system service's units and Sysusers by name,
// with the service starting the phonetopcd binary at exe.
func SystemUnits(exe string) map[string]string {
	u := units("system/", exe, "/usr/local/bin/phonetopcd")
	u[Sysusers] = read("system/" + Sysusers)
	return u
}

func units(dir, exe, placeholder string) map[string]string {
	u := map[string]string{}
	for _, name := range []string{Socket, Service} {
		u[name] = strings.Replace(read(dir+name), "ExecStart="+placeholder, "ExecStart="+quote(exe), 1)
	}
	return u
}

func read(name string) string {
	b, err := files.ReadFile(name)
	if err != nil {
		panic(err) // embedded above
	}
	return string(b)
}

// quote writes path as one systemd command line word: % starts a specifier
// and spaces split words.
func quote(path string) string {
	path = strings.ReplaceAll(path, "%", "%%")
	if !strings.ContainsAny(path, " \t\"'\\") {
		return path
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(path) + `"`
}

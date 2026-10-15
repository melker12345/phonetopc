// Package systemd holds phonetopcd's systemd user units, for `phonetopc
// daemon install` and for packages.
package systemd

import (
	"embed"
	"strings"
)

//go:embed phonetopcd.service phonetopcd.socket
var files embed.FS

const (
	Socket  = "phonetopcd.socket"
	Service = "phonetopcd.service"
)

// Units returns the unit files by name, with the service starting the
// phonetopcd binary at exe.
func Units(exe string) map[string]string {
	units := map[string]string{}
	for _, name := range []string{Socket, Service} {
		b, err := files.ReadFile(name)
		if err != nil {
			panic(err) // embedded above
		}
		units[name] = strings.Replace(string(b), "ExecStart=/usr/bin/phonetopcd", "ExecStart="+quote(exe), 1)
	}
	return units
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

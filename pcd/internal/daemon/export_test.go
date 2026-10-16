package daemon

// Routes lists the API's requests as "METHOD /path", as LOCAL-API.md writes them.
func Routes() []string {
	var out []string
	d := &Daemon{}
	for _, rt := range append(d.routes(), d.adminRoutes()...) {
		out = append(out, rt.method+" "+rt.path)
	}
	return out
}

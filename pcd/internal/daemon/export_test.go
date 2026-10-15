package daemon

// Routes lists the API's requests as "METHOD /path", as LOCAL-API.md writes them.
func Routes() []string {
	var out []string
	for _, rt := range (&Daemon{}).routes() {
		out = append(out, rt.method+" "+rt.path)
	}
	return out
}

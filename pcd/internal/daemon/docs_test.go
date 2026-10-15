package daemon_test

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/melker12345/phonetopc/pcd/internal/daemon"
)

type documented struct{ method, path string }

var (
	requestLineRE = regexp.MustCompile("(GET|POST|PUT|PATCH|DELETE) (/v1/[^\\s`|]*)")
	exampleURLRE  = regexp.MustCompile(`(?:(?:https?|wss?)://)?(?:phonetopc|127\.0\.0\.1:8767|localhost:8767)(/v1/[^\s'"` + "`" + `]*)`)
	placeholderRE = regexp.MustCompile(`\{(\w+)\}`)
)

// documentedRequests finds the request lines ("GET /v1/events?source=gyro")
// and the paths of the examples' URLs in LOCAL-API.md. An example URL's
// method isn't parsed: it is "" there.
func documentedRequests(t *testing.T) []documented {
	t.Helper()
	b, err := os.ReadFile("../../../spec/LOCAL-API.md")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[documented]bool{}
	var out []documented
	add := func(d documented) {
		d.path, _, _ = strings.Cut(d.path, "${") // a JS template's placeholder
		d.path = placeholderRE.ReplaceAllStringFunc(d.path, func(p string) string {
			if p == "{source}" {
				return "battery"
			}
			return "x"
		})
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, m := range requestLineRE.FindAllStringSubmatch(string(b), -1) {
		add(documented{m[1], m[2]})
	}
	for _, m := range exampleURLRE.FindAllStringSubmatch(string(b), -1) {
		add(documented{"", m[1]})
	}
	return out
}

// The documented GETs are routes that take the documented query, and the
// other documented requests are routes with that method.
func TestDocumentedRequestsAreRoutes(t *testing.T) {
	_, _, url := connectedDaemon(t, quietOptions())
	reqs := documentedRequests(t)
	if len(reqs) < 20 {
		t.Fatalf("found only %d requests in LOCAL-API.md; is the parser broken? %v", len(reqs), reqs)
	}
	for _, d := range reqs {
		if d.method != "" && d.method != "GET" {
			// No body: 400 is fine, an unknown panel too.
			if r := do(t, d.method, url+d.path, ""); r.status == http.StatusMethodNotAllowed || r.errorCode() == "not_found" {
				t.Errorf("%s %s: %s", d.method, d.path, r.raw)
			}
			continue
		}
		// An example URL's method isn't known: 405 for a GET still shows
		// the path is a route. A path without its query (a heading) may
		// lack a required one.
		code := getStatus(t, url+d.path)
		if code == http.StatusNotFound || (code == http.StatusBadRequest && strings.Contains(d.path, "?")) || (d.method == "GET" && code == http.StatusMethodNotAllowed) {
			t.Errorf("GET %s: %d", d.path, code)
		}
	}
}

// getStatus doesn't wait for the body: event streams don't end.
func getStatus(t *testing.T, url string) int {
	t.Helper()
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestEveryRouteIsDocumented(t *testing.T) {
	b, err := os.ReadFile("../../../spec/LOCAL-API.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range daemon.Routes() {
		if !regexp.MustCompile(regexp.QuoteMeta(r) + `(?:[^/\w]|$)`).Match(b) {
			t.Errorf("%s is not in LOCAL-API.md", r)
		}
	}
}

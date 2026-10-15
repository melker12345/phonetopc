package daemon_test

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/melker12345/phonetopc/pcd/internal/daemon"
)

func TestThisUserNeedsNoTokenButWebPagesDo(t *testing.T) {
	_, d, _ := connectedDaemon(t, quietOptions())
	hs := httptest.NewUnstartedServer(daemon.RequireToken("s3cret-token", d.Handler()))
	hs.Config.ConnContext = daemon.ConnContext
	hs.Start()
	defer hs.Close()

	thisUser := 200
	if runtime.GOOS != "linux" {
		thisUser = 401
	}
	for _, tc := range []struct {
		name   string
		header http.Header
		want   int
	}{
		{"plain", nil, thisUser},
		{"typed into the address bar", http.Header{"Sec-Fetch-Site": {"none"}}, thisUser},
		{"web page", http.Header{"Origin": {"https://example.com"}}, 401},
		{"cross-site no-cors", http.Header{"Sec-Fetch-Site": {"cross-site"}}, 401},
		{"same-site", http.Header{"Sec-Fetch-Site": {"same-site"}}, 401},
		{"web page with the token", http.Header{"Origin": {"https://example.com"}, "Authorization": {"Bearer s3cret-token"}}, 200},
	} {
		req, _ := http.NewRequest("GET", hs.URL+"/v1/status", nil)
		for k, v := range tc.header {
			req.Header[k] = v
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, res.StatusCode, tc.want)
		}
	}
}

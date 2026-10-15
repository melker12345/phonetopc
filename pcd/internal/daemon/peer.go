package daemon

import (
	"context"
	"net"
	"net/http"
)

type sameUserKey struct{}

// ConnContext is an http.Server ConnContext for the TCP listener: it notes
// whether the program at the other end runs as this user, which RequireToken
// then lets in without a token.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	if peerIsSameUser(c) {
		return context.WithValue(ctx, sameUserKey{}, true)
	}
	return ctx
}

func sameUser(r *http.Request) bool {
	ok, _ := r.Context().Value(sameUserKey{}).(bool)
	return ok
}

// fromBrowser reports requests a web page may have sent: browsers run as
// this user too, but must not get in without the token.
func fromBrowser(r *http.Request) bool {
	if r.Header.Get("Origin") != "" {
		return true
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "none", "same-origin":
		return false
	}
	return true
}

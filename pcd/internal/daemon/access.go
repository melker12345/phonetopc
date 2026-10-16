package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/user"
	"slices"
	"strconv"
)

// Access decides who may use the API and who may administer the daemon:
// pair, unpair and accept keys (LOCAL-API.md §1).
type Access struct {
	// Self is the user the daemon runs as. It and root may do everything.
	Self int
	// Group is the group whose members may use the API, -1 for none.
	Group int
	// GroupsOf returns a user's groups. It is needed for TCP peers, whose
	// groups the kernel doesn't report. Default: the user database.
	GroupsOf func(uid int) ([]int, error)
}

// UserAccess lets only this process's user (and root) in: the daemon of a
// single user.
func UserAccess() *Access { return &Access{Self: os.Getuid(), Group: -1} }

// Peer is the program at the other end of a local connection.
type Peer struct {
	UID int
	// Groups are its primary and supplementary groups; nil if the kernel
	// didn't say (TCP).
	Groups []int
}

func (a *Access) MayAdmin(p Peer) bool { return p.UID == 0 || p.UID == a.Self }

func (a *Access) MayUse(p Peer) bool {
	if a.MayAdmin(p) {
		return true
	}
	if a.Group < 0 {
		return false
	}
	groups := p.Groups
	if groups == nil {
		lookup := a.GroupsOf
		if lookup == nil {
			lookup = userGroups
		}
		var err error
		if groups, err = lookup(p.UID); err != nil {
			return false
		}
	}
	return slices.Contains(groups, a.Group)
}

func userGroups(uid int) ([]int, error) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return nil, err
	}
	ids, err := u.GroupIds()
	if err != nil {
		return nil, err
	}
	var gids []int
	for _, id := range ids {
		if gid, err := strconv.Atoi(id); err == nil {
			gids = append(gids, gid)
		}
	}
	return gids, nil
}

type peerKey struct{}

type peerAccess struct {
	known      bool
	uid        int
	use, admin bool
}

// ConnContext is an http.Server ConnContext for the daemon's listeners: it
// notes the connection, whose program the phone is told about for
// approvals, and finds out who is at the other end, for RequirePeer,
// RequireToken and the administration endpoints.
func (a *Access) ConnContext(ctx context.Context, c net.Conn) context.Context {
	ctx = context.WithValue(ctx, connKey{}, c)
	p, ok := connPeer(c)
	if !ok {
		return ctx
	}
	return a.WithPeer(ctx, p)
}

// WithPeer is what ConnContext returns for a connection from p.
func (a *Access) WithPeer(ctx context.Context, p Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, peerAccess{known: true, uid: p.UID, use: a.MayUse(p), admin: a.MayAdmin(p)})
}

var userAccess = UserAccess()

// ConnContext is UserAccess().ConnContext.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	return userAccess.ConnContext(ctx, c)
}

func peerOf(r *http.Request) peerAccess {
	p, _ := r.Context().Value(peerKey{}).(peerAccess)
	return p
}

// RequirePeer guards h for the Unix socket. Its file mode lets only the
// right users connect; where the kernel also says who connected
// (SO_PEERCRED), that is checked again here.
func RequirePeer(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := peerOf(r); p.known && !p.use {
			writeError(w, http.StatusForbidden, "forbidden", "user "+strconv.Itoa(p.uid)+" may not use phonetopcd (LOCAL-API.md §1)")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// admin reports whether the request may administer the daemon, and
// answers 403 if not. Only peers the kernel identified qualify.
func admin(w http.ResponseWriter, r *http.Request) bool {
	if peerOf(r).admin {
		return true
	}
	writeError(w, http.StatusForbidden, "forbidden", "only root may do this: run it with sudo")
	return false
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

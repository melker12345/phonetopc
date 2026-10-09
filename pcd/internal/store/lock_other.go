//go:build !unix

package store

// lockPath is a no-op where flock is unavailable; updates within one
// process are still serialised by Store.mu.
func lockPath(string) (func(), error) { return func() {}, nil }

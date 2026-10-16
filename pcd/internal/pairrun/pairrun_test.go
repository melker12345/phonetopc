package pairrun

import (
	"net"
	"testing"
)

func TestDefaultPortFallback(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	old := DefaultListen
	DefaultListen = busy.Addr().String()
	defer func() { DefaultListen = old }()

	ln, random, err := listen("")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if !random || ln.Addr().String() == busy.Addr().String() {
		t.Errorf("want a random port, got %v (random %v)", ln.Addr(), random)
	}
	ln.Close()

	// The default port itself when it is free.
	port := busy.Addr().(*net.TCPAddr).Port
	busy.Close()
	ln, random, err = listen("")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if random || ln.Addr().(*net.TCPAddr).Port != port {
		t.Errorf("want port %d, got %v (random %v)", port, ln.Addr(), random)
	}

	// An explicit address that fails is an error, without fallback.
	if _, _, err := listen(ln.Addr().String()); err == nil {
		t.Error("explicit busy address succeeded")
	}
}

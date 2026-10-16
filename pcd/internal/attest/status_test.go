package attest_test

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/melker12345/phonetopc/pcd/internal/attest"
)

// The example list of the Android documentation.
const exampleStatus = `{
  "entries": {
    "2c8cdddfd5e03bfc": {"status": "REVOKED", "expires": "2020-11-13", "reason": "KEY_COMPROMISE", "comment": "Key stored on unsecure system"},
    "c8966fcb2fbb0d7a": {"status": "SUSPENDED", "reason": "SOFTWARE_FLAW", "comment": "Bug in keystore causes this key malfunction b/555555"}
  }
}`

func TestStatusList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(exampleStatus))
	}))
	defer srv.Close()
	l, err := attest.FetchStatusList(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := new(big.Int).SetString("c8966fcb2fbb0d7a", 16)
	if e, ok := l.Lookup(serial); !ok || e.Status != "SUSPENDED" {
		t.Fatalf("%+v %v", e, ok)
	}
	if _, ok := l.Lookup(big.NewInt(1)); ok {
		t.Fatal("serial 1 listed")
	}
	if _, err := attest.ParseStatusList([]byte(`{}`)); err == nil {
		t.Fatal("a list without entries accepted")
	}

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer fail.Close()
	if _, err := attest.FetchStatusList(context.Background(), fail.Client(), fail.URL); err == nil {
		t.Fatal("a 500 accepted")
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

// esc is the escape termSafe writes for a UTF-16 unit, spelled without a
// literal backslash-u so editors and tools leave these tests alone.
func esc(units ...string) string {
	var s string
	for _, u := range units {
		s += `\` + "u" + u
	}
	return s
}

func TestTermSafe(t *testing.T) {
	for in, want := range map[string]string{
		"plain\tname\n": "plain\tname\n",
		"S25 \xf0\x9f\x91\xa9\xe2\x80\x8d\xf0\x9f\x92\xbb": "S25 \xf0\x9f\x91\xa9\xe2\x80\x8d\xf0\x9f\x92\xbb", // ZWJ emoji
		"evil\x1b[2J":         "evil" + esc("001b") + "[2J",
		"c1\xc2\x9b31m":       "c1" + esc("009b") + "31m",  // C1 CSI
		"rlo\xe2\x80\xaetxt":  "rlo" + esc("202e") + "txt", // U+202E
		"zw\xe2\x80\x8bsp":    "zw" + esc("200b") + "sp",   // U+200B
		"tag\xf3\xa0\x81\x81": "tag" + esc("db40", "dc41"), // U+E0041
		"bad\xffbyte":         "bad\xef\xbf\xbdbyte",       // U+FFFD
	} {
		var out bytes.Buffer
		if _, err := (&termSafe{w: &out}).Write([]byte(in)); err != nil {
			t.Fatal(err)
		}
		if out.String() != want {
			t.Errorf("%q: got %q, want %q", in, out.String(), want)
		}
	}
}

func TestTermSafeSplitRune(t *testing.T) {
	var out bytes.Buffer
	w := &termSafe{w: &out}
	b := []byte("\xc3\xa4\xe2\x80\xae") // ä U+202E
	for i := range b {
		if n, err := w.Write(b[i : i+1]); n != 1 || err != nil {
			t.Fatal(n, err)
		}
	}
	if want := "\xc3\xa4" + esc("202e"); out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}

func TestTermSafeKeepsJSON(t *testing.T) {
	name := "x\xc2\x9b\xe2\x80\xae\xf3\xa0\x81\x81y"
	in, _ := json.Marshal(map[string]string{"name": name})
	var out bytes.Buffer
	(&termSafe{w: &out}).Write(in)
	var back map[string]string
	if err := json.Unmarshal(out.Bytes(), &back); err != nil || back["name"] != name {
		t.Errorf("%s: %v %q", out.Bytes(), err, back["name"])
	}
}

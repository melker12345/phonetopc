package main

import (
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// termSafe escapes what a phone could use to take over the terminal or
// disguise text: control characters other than newline and tab (ESC, C1
// CSI), and protocol.IsHidden characters. Escapes are JSON-style \uXXXX,
// so --json output stays valid JSON with the same content. Invalid UTF-8
// becomes U+FFFD.
type termSafe struct {
	w    io.Writer
	tail []byte // an incomplete rune from the end of the last Write
}

func (t *termSafe) Write(p []byte) (int, error) {
	buf := append(t.tail, p...)
	t.tail = nil
	var b strings.Builder
	for len(buf) > 0 {
		r, size := utf8.DecodeRune(buf)
		if r == utf8.RuneError && size <= 1 && !utf8.FullRune(buf) {
			t.tail = append([]byte(nil), buf...)
			break
		}
		buf = buf[size:]
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case unicode.Is(unicode.Cc, r) || protocol.IsHidden(r):
			if r > 0xFFFF {
				hi, lo := utf16.EncodeRune(r)
				fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteRune(r) // RuneError for invalid bytes
		}
	}
	if _, err := io.WriteString(t.w, b.String()); err != nil {
		return 0, err
	}
	return len(p), nil
}

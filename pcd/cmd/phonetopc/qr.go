package main

import (
	"io"
	"os"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// writeQR renders content as a QR code with Unicode half blocks, two
// modules per character cell vertically, quiet zone included.
//
// With color, light modules are drawn white on black explicitly (ANSI), so
// the code scans whatever the terminal's theme. Without color, light
// modules are drawn as blocks in the foreground color, which is right for
// the usual light-on-dark terminal.
func writeQR(w io.Writer, content string, color bool) error {
	q, err := qrcode.New(content, qrcode.Low)
	if err != nil {
		return err
	}
	bm := q.Bitmap() // true = dark
	var b strings.Builder
	for y := 0; y < len(bm); y += 2 {
		b.WriteString("    ")
		if color {
			b.WriteString("\x1b[97;40m") // bright white on black
		}
		for x := range bm[y] {
			top := !bm[y][x]                        // light
			bottom := y+1 >= len(bm) || !bm[y+1][x] // past the end: quiet zone
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		if color {
			b.WriteString("\x1b[0m")
		}
		b.WriteString("\n")
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// writePNG writes content as a QR code PNG.
func writePNG(path, content string) error {
	png, err := qrcode.Encode(content, qrcode.Medium, 512)
	if err != nil {
		return err
	}
	return os.WriteFile(path, png, 0o644)
}

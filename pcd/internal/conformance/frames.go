// Package conformance reads the client frames of spec/conformance, to seed
// fuzz tests.
package conformance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

// Frames returns every frame a client sends in the transcripts and server
// cases under dir (spec/conformance): the msg of each c2s line, and each
// send, raw text as is.
func Frames(dir string) ([][]byte, error) {
	var frames [][]byte
	lines, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	for _, name := range lines {
		b, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var line struct {
				Dir string          `json:"dir"`
				Msg json.RawMessage `json:"msg"`
			}
			if json.Unmarshal(sc.Bytes(), &line) == nil && line.Dir == "c2s" && line.Msg != nil {
				frames = append(frames, line.Msg)
			}
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	cases, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	for _, name := range cases {
		b, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		frames = appendSends(frames, v)
	}
	return frames, nil
}

func appendSends(frames [][]byte, v any) [][]byte {
	switch v := v.(type) {
	case []any:
		for _, x := range v {
			frames = appendSends(frames, x)
		}
	case map[string]any:
		for k, x := range v {
			if k != "send" {
				frames = appendSends(frames, x)
				continue
			}
			if m, ok := x.(map[string]any); ok && len(m) == 1 {
				if raw, ok := m["raw"].(string); ok {
					frames = append(frames, []byte(raw))
					continue
				}
				if _, ok := m["binary"]; ok {
					continue
				}
				if _, ok := m["wait"]; ok {
					continue
				}
			}
			if b, err := json.Marshal(x); err == nil {
				frames = append(frames, b)
			}
		}
	}
	return frames
}

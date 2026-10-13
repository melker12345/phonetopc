package mockphone_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Runs spec/conformance/panel-server-cases.json (format:
// spec/conformance/README.md, "Panel server cases") against the mock, with
// a fresh server per case.

type panelCases struct {
	Fixture struct {
		Mode        string          `json:"mode"`
		Permissions map[string]bool `json:"permissions"`
	} `json:"fixture"`
	Cases []struct {
		Name        string     `json:"name"`
		Description string     `json:"description"`
		Hello       bool       `json:"hello"`
		Permissions string     `json:"permissions"`
		Setup       []caseStep `json:"setup"`
		Send        json.RawMessage
		Expect      struct {
			caseExpect
			ResultHas          map[string]json.RawMessage `json:"result_has"`
			ResultCapabilities []string                   `json:"result_capabilities"`
		} `json:"expect"`
	} `json:"cases"`
}

func TestPanelServerCases(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(specDir, "conformance", "panel-server-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pc panelCases
	if err := json.Unmarshal(b, &pc); err != nil {
		t.Fatal(err)
	}
	if pc.Fixture.Mode != "dev" || !pc.Fixture.Permissions["notify"] || !pc.Fixture.Permissions["panels"] || len(pc.Cases) == 0 {
		t.Fatalf("unexpected fixture %+v (%d cases)", pc.Fixture, len(pc.Cases))
	}
	seen := map[string]bool{}
	for _, c := range pc.Cases {
		if seen[c.Name] {
			t.Fatalf("duplicate case %q", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			opts := quietOptions()
			switch c.Permissions {
			case "":
			case "off":
				opts.NoNotify, opts.NoPanels = true, true
			default:
				t.Fatalf("unknown permissions %q", c.Permissions)
			}
			_, url := start(t, opts)
			r := dialRaw(t, url, nil)
			if c.Hello {
				r.sendRaw(caseHello)
				if m := r.response(); m.Error != nil || string(m.ID) != `"conformance-hello"` {
					t.Fatalf("hello failed: %+v", m.Error)
				}
			}
			for i, s := range c.Setup {
				runCaseStep(t, r, fmt.Sprintf("setup[%d]", i), s)
			}
			ex := c.Expect
			m := runCaseStep(t, r, "send", caseStep{Send: c.Send, Expect: ex.caseExpect})
			if m == nil || m.Result == nil {
				return
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(m.Result, &got); err != nil {
				t.Fatal(err)
			}
			for k, want := range ex.ResultHas {
				wantSameJSON(t, "result."+k, got[k], want)
			}
			var caps []string
			_ = json.Unmarshal(got["capabilities"], &caps)
			for _, want := range ex.ResultCapabilities {
				if !slices.Contains(caps, want) {
					t.Errorf("capabilities %v lack %q", caps, want)
				}
			}
		})
	}
}

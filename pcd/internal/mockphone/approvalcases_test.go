package mockphone_test

import (
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/coder/websocket"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/mockphone"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// Runs spec/conformance/approval-server-cases.json (format:
// spec/conformance/README.md) against the secure mock with approvals.

type approvalCases struct {
	Fixture struct {
		Key string `json:"key"`
		Pin string `json:"pin"`
		PCs []struct {
			ID, Name, Token string
		} `json:"pcs"`
		ApprovalKey string                `json:"approval_key"`
		Approval    protocol.ApprovalInfo `json:"approval"`
		Decision    string                `json:"decision"`
	} `json:"fixture"`
	Cases []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Hello       bool            `json:"hello"`
		Send        json.RawMessage `json:"send"`
		Expect      struct {
			caseExpect
			ResultHas          map[string]json.RawMessage `json:"result_has"`
			ResultCapabilities []string                   `json:"result_capabilities"`
			ApprovalValid      bool                       `json:"approval_valid"`
		} `json:"expect"`
	} `json:"cases"`
}

func readPEMKey(t *testing.T, rel string) *ecdsa.PrivateKey {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(specDir, "conformance", rel))
	if err != nil {
		t.Fatal(err)
	}
	k, err := security.ParseKeyPEM(b)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestApprovalServerCases(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(specDir, "conformance", "approval-server-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ac approvalCases
	if err := json.Unmarshal(b, &ac); err != nil {
		t.Fatal(err)
	}
	if ac.Fixture.Decision != "approve" {
		t.Fatalf("fixture decision %q", ac.Fixture.Decision)
	}
	cert, err := security.SelfSigned(readPEMKey(t, ac.Fixture.Key), "conformance")
	if err != nil {
		t.Fatal(err)
	}
	if pin, _ := security.CertPin(cert); pin != ac.Fixture.Pin {
		t.Fatalf("fixture pin %s, key gives %s", ac.Fixture.Pin, pin)
	}
	approvalKey := readPEMKey(t, ac.Fixture.ApprovalKey)
	if k, pin, _ := security.EncodeApprovalKey(&approvalKey.PublicKey); k != ac.Fixture.Approval.Key || pin != ac.Fixture.Approval.Pin {
		t.Fatalf("fixture approval %+v, key gives %s %s", ac.Fixture.Approval, k, pin)
	}
	pubKey, err := security.ParseApprovalKey(ac.Fixture.Approval.Key)
	if err != nil {
		t.Fatal(err)
	}

	opts := quietOptions()
	opts.MaxAuthFailures = -1
	opts.Authenticate = func(token string) (protocol.PCInfo, bool) {
		for _, pc := range ac.Fixture.PCs {
			if security.Equal(pc.Token, token) {
				return protocol.PCInfo{ID: pc.ID, Name: pc.Name}, true
			}
		}
		return protocol.PCInfo{}, false
	}
	opts.ApprovalKey = func() *ecdsa.PrivateKey { return approvalKey }
	opts.Approve = mockphone.ApproveAll
	_, addr := serveTLS(t, opts, cert)
	url := client.SecureURL(addr)
	pc := ac.Fixture.PCs[0]
	hello := `{"jsonrpc":"2.0","id":"conformance-hello","method":"hello","params":{"protocol":"phonetopc/1","client":{"name":"conformance","version":"1"},"auth":{"token":"` + pc.Token + `"}}}`

	seen := map[string]bool{}
	for _, c := range ac.Cases {
		if seen[c.Name] {
			t.Fatalf("duplicate case %q", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			// Not parallel: one pending approval per PC.
			hc := &http.Client{Transport: &http.Transport{TLSClientConfig: security.PinnedClientTLSConfig(ac.Fixture.Pin)}}
			r := dialRaw(t, url, &websocket.DialOptions{HTTPClient: hc})
			var deviceID string
			if c.Hello {
				r.sendRaw(hello)
				m := r.response()
				var h protocol.HelloResult
				if m.Error != nil || string(m.ID) != `"conformance-hello"` || json.Unmarshal(m.Result, &h) != nil {
					t.Fatalf("hello failed: %+v", m.Error)
				}
				deviceID = h.Device.ID
			}
			ex := c.Expect
			m := runCaseStep(t, r, "send", caseStep{Send: c.Send, Expect: ex.caseExpect})
			if m == nil {
				return
			}
			var got map[string]json.RawMessage
			if m.Result != nil {
				if err := json.Unmarshal(m.Result, &got); err != nil {
					t.Fatal(err)
				}
			}
			for k, want := range ex.ResultHas {
				wantSameJSON(t, "result."+k, got[k], want)
			}
			if ex.ResultCapabilities != nil {
				var caps []string
				json.Unmarshal(got["capabilities"], &caps)
				for _, want := range ex.ResultCapabilities {
					if !slices.Contains(caps, want) {
						t.Errorf("capabilities %v lack %q", caps, want)
					}
				}
			}
			if ex.ApprovalValid {
				var res protocol.RequestApprovalResult
				var sent struct {
					Params protocol.RequestApprovalParams `json:"params"`
				}
				if err := json.Unmarshal(m.Result, &res); err != nil || !res.Approved {
					t.Fatalf("want an approval, got %s", m.Result)
				}
				if err := json.Unmarshal(c.Send, &sent); err != nil {
					t.Fatal(err)
				}
				msg := security.ApprovalMessage(deviceID, pc.ID, sent.Params.Nonce, sent.Params.Reason)
				if err := security.VerifyApproval(pubKey, res.Signature, msg); err != nil {
					t.Errorf("signature: %v", err)
				}
			}
		})
	}
}

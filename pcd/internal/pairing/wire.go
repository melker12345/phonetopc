// Package pairing implements pairing (SECURITY.md §4) on both sides: the
// PC's pairing listener (Listener, used by `phonetopc pair`) and the phone's
// pairing client (PairQR, PairSAS, used by the mock phone and tests).
//
// The wire format is JSON-RPC 2.0 over a TLS WebSocket at /v1/pair, with
// protocol name phonetopc-pair/1.
package pairing

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// Protocol is the pairing protocol name.
const Protocol = "phonetopc-pair/1"

// SupportedProtocols lists the pairing protocols this implementation speaks.
var SupportedProtocols = []string{Protocol}

// DefaultPort is the default port of the pairing listener (§4.1).
const DefaultPort = 8766

// Path is the HTTP path of the pairing listener.
const Path = "/v1/pair"

// Method names (§4.2-§4.4).
const (
	MethodQR         = "pair_qr"
	MethodSASBegin   = "pair_sas_begin"
	MethodSASNonce   = "pair_sas_nonce"
	MethodSASConfirm = "pair_sas_confirm"
	MethodFinish     = "pair_finish"
)

// PhoneInfo is the phone member of pair_qr and pair_sas_begin.
type PhoneInfo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Pin       string   `json:"pin"`
	Port      int      `json:"port,omitempty"`
	Addresses []string `json:"addresses"`
}

// Limits on what a phone may send, so a hostile one can't bloat the store.
const (
	maxNameLen    = 256
	maxAddresses  = 16
	maxAddressLen = 255
)

// idRE is what a phone id must look like (SECURITY.md §2): 64 to 256
// random bits as lowercase hex, as phones generate them. Nothing else can
// sneak into the store, the terminal or `--device`.
var idRE = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

// ValidID reports whether id is a well-formed phone id.
func ValidID(id string) bool { return idRE.MatchString(id) }

// validate checks a phone's self-description and fills defaults. The error
// becomes the message of an invalid_params error.
func (p *PhoneInfo) validate() error {
	switch {
	case !ValidID(p.ID):
		return fmt.Errorf("params.phone.id must be 16 to 64 lowercase hex digits")
	case len(p.Name) > maxNameLen:
		return fmt.Errorf("params.phone.name is too long")
	case !security.ValidPin(p.Pin):
		return fmt.Errorf("params.phone.pin must be sha256/<base64url of 32 bytes>")
	case p.Port < 0 || p.Port > 65535:
		return fmt.Errorf("params.phone.port must be 1-65535")
	case len(p.Addresses) > maxAddresses:
		return fmt.Errorf("params.phone.addresses has more than %d entries", maxAddresses)
	}
	for _, a := range p.Addresses {
		if a == "" || len(a) > maxAddressLen || strings.ContainsAny(a, "/ \t\r\n") {
			return fmt.Errorf("params.phone.addresses contains an invalid entry")
		}
	}
	if p.Name == "" {
		p.Name = p.ID
	}
	if p.Port == 0 {
		p.Port = protocol.DefaultPort
	}
	if p.Addresses == nil {
		p.Addresses = []string{}
	}
	return nil
}

// QRParams are the params of pair_qr.
type QRParams struct {
	Protocol string    `json:"protocol"`
	Secret   string    `json:"secret"`
	Phone    PhoneInfo `json:"phone"`
}

// PCResult is the result of pair_qr and pair_finish: the PC's identity.
type PCResult struct {
	PC protocol.PCInfo `json:"pc"`
}

// SASBeginParams are the params of pair_sas_begin.
type SASBeginParams struct {
	Protocol string    `json:"protocol"`
	Phone    PhoneInfo `json:"phone"`
}

// SASBeginResult is the result of pair_sas_begin.
type SASBeginResult struct {
	PC         protocol.PCInfo `json:"pc"`
	Commitment string          `json:"commitment"`
}

// NonceMessage is the params and the result of pair_sas_nonce.
type NonceMessage struct {
	Nonce string `json:"nonce"`
}

// SASConfirmParams are the params of pair_sas_confirm.
type SASConfirmParams struct {
	Match *bool `json:"match"`
}

// FinishParams are the params of pair_finish.
type FinishParams struct {
	Token string `json:"token"`
}

// URI is the content of the pairing QR code (§4.2).
type URI struct {
	ID     string   // PC id
	Name   string   // PC name
	Hosts  []string // LAN addresses, in the order to try
	Port   int
	Pin    string // PC pin
	Secret string // base64url of 16 random bytes
}

// String renders the URI with its parameters in the order of §4.2. The name
// is percent-encoded (spaces as %20); hosts, pin and secret only contain
// characters that are safe in a query.
func (u URI) String() string {
	var b strings.Builder
	b.WriteString("phonetopc://pair?v=1")
	b.WriteString("&id=" + url.QueryEscape(u.ID))
	b.WriteString("&n=" + strings.ReplaceAll(url.QueryEscape(u.Name), "+", "%20"))
	hosts := make([]string, len(u.Hosts))
	for i, h := range u.Hosts {
		hosts[i] = url.QueryEscape(h)
		hosts[i] = strings.ReplaceAll(hosts[i], "%3A", ":") // IPv6, readable
	}
	b.WriteString("&h=" + strings.Join(hosts, ","))
	b.WriteString("&p=" + strconv.Itoa(u.Port))
	b.WriteString("&pin=" + u.Pin)
	b.WriteString("&s=" + u.Secret)
	return b.String()
}

// ParseURI parses and checks a pairing URI.
func ParseURI(s string) (URI, error) {
	pu, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return URI{}, fmt.Errorf("pairing URI: %w", err)
	}
	if pu.Scheme != "phonetopc" || pu.Host != "pair" {
		return URI{}, fmt.Errorf("pairing URI: want phonetopc://pair?..., got %s://%s", pu.Scheme, pu.Host)
	}
	q, err := url.ParseQuery(pu.RawQuery)
	if err != nil {
		return URI{}, fmt.Errorf("pairing URI: %w", err)
	}
	if v := q.Get("v"); v != "1" {
		return URI{}, fmt.Errorf("pairing URI: unsupported version v=%q", v)
	}
	u := URI{ID: q.Get("id"), Name: q.Get("n"), Pin: q.Get("pin"), Secret: q.Get("s")}
	for _, h := range strings.Split(q.Get("h"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			u.Hosts = append(u.Hosts, strings.Trim(h, "[]"))
		}
	}
	u.Port, err = strconv.Atoi(q.Get("p"))
	switch {
	case err != nil || u.Port < 1 || u.Port > 65535:
		return URI{}, fmt.Errorf("pairing URI: bad port %q", q.Get("p"))
	case u.ID == "":
		return URI{}, fmt.Errorf("pairing URI: no id")
	case len(u.Hosts) == 0:
		return URI{}, fmt.Errorf("pairing URI: no hosts")
	case !security.ValidPin(u.Pin):
		return URI{}, fmt.Errorf("pairing URI: bad pin %q", u.Pin)
	}
	if b, err := security.DecodeB64(u.Secret); err != nil || len(b) == 0 {
		return URI{}, fmt.Errorf("pairing URI: bad secret")
	}
	return u, nil
}

package attest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
)

// StatusURL is Google's attestation certificate revocation list, from
// https://developer.android.com/privacy-and-security/security-key-attestation#certificate_status.
const StatusURL = "https://android.googleapis.com/attestation/status"

// StatusList is the revocation list: entries by certificate serial number
// in lowercase hex.
type StatusList struct {
	Entries map[string]StatusEntry `json:"entries"`
}

type StatusEntry struct {
	Status  string `json:"status"` // REVOKED or SUSPENDED
	Expires string `json:"expires,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Comment string `json:"comment,omitempty"`
}

func ParseStatusList(b []byte) (*StatusList, error) {
	var l StatusList
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("attestation status list: %w", err)
	}
	if l.Entries == nil {
		return nil, fmt.Errorf("attestation status list: no entries member")
	}
	return &l, nil
}

// Lookup reports whether the certificate with this serial number is on
// the list.
func (l *StatusList) Lookup(serial *big.Int) (StatusEntry, bool) {
	e, ok := l.Entries[serial.Text(16)]
	return e, ok
}

// FetchStatusList downloads the list from url (StatusURL in production).
func FetchStatusList(ctx context.Context, hc *http.Client, url string) (*StatusList, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the attestation status list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching the attestation status list: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("fetching the attestation status list: %w", err)
	}
	return ParseStatusList(b)
}

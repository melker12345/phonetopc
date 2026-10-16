package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/melker12345/phonetopc/pcd/internal/daemon"
	"github.com/melker12345/phonetopc/pcd/internal/localapi"
	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// With phonetopcd running as a system service, the pairings are its own,
// and the plugin asks it (LOCAL-API.md §6) instead of the phone.

func systemAPI() (*localapi.Client, error) {
	sock, err := localapi.Socket(os.Getenv)
	if err != nil || sock == "" {
		return nil, err
	}
	return localapi.New(sock), nil
}

// daemonPhone returns the phone phonetopcd is connected to, which must be
// the one selected (by id or name, "" for any).
func daemonPhone(ctx context.Context, api *localapi.Client, selector string) (daemon.Status, error) {
	var st daemon.Status
	if err := api.Get(ctx, "/v1/status", &st); err != nil {
		return st, err
	}
	if !st.Connected || st.Device == nil {
		return st, fmt.Errorf("phonetopcd has no phone: %s", st.Error)
	}
	if selector != "" && selector != st.Device.ID && selector != st.Device.Name {
		return st, fmt.Errorf("phonetopcd is connected to %q (%s), not %s", st.Device.Name, st.Device.ID, selector)
	}
	return st, nil
}

func generateViaDaemon(ctx context.Context, api *localapi.Client, selector string, unattested bool) (id, name string, k daemon.DecryptKeyResponse, err error) {
	st, err := daemonPhone(ctx, api, selector)
	if err != nil {
		return "", "", k, err
	}
	path := "/v1/decryption-key"
	if unattested {
		path += "?unattested=true"
	}
	if err := api.Get(ctx, path, &k); err != nil {
		if localapi.IsCode(err, "decrypt_key_unattested") {
			return "", "", k, fmt.Errorf("%w.\nWhat is encrypted to this key is only as safe as the key. To use it anyway: age-plugin-phonetopc --generate --unattested", err)
		}
		return "", "", k, err
	}
	return st.Device.ID, st.Device.Name, k, nil
}

func decryptViaDaemon(ctx context.Context, api *localapi.Client, deviceID, keyPin string, ciphertext []byte, show func(msg string)) ([]byte, error) {
	st, err := daemonPhone(ctx, api, deviceID)
	if err != nil {
		return nil, err
	}
	var res daemon.DecryptResponse
	req := daemon.DecryptRequest{Reason: reason(parentCommand()), KeyPin: keyPin, Ciphertext: security.B64(ciphertext)}
	if err := api.Ask(ctx, "/v1/decryptions", req, func(match int, _ string) { show(matchMessage(st.Device.Name, match)) }, &res); err != nil {
		return nil, err
	}
	if !res.Decrypted {
		return nil, fmt.Errorf("the phone did not decrypt: %s", res.Why)
	}
	pt, err := security.DecodeB64(res.Plaintext)
	if err != nil {
		return nil, errors.New("phonetopcd sent a plaintext that isn't base64url")
	}
	return pt, nil
}

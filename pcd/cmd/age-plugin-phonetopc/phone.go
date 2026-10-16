package main

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"unicode/utf8"

	"filippo.io/age"

	"github.com/melker12345/phonetopc/pcd/internal/security"
)

// The age format of DECRYPT.md §5.

const (
	name       = "phonetopc"
	stanzaType = "phonetopc"
	fileKeyLen = 16
)

// recipient encrypts age file keys to the phone's decryption key.
type recipient struct {
	spki []byte
	pub  *rsa.PublicKey
}

func parseRecipient(spki []byte) (*recipient, error) {
	pub, err := security.ParseDecryptKeyDER(spki)
	if err != nil {
		return nil, err
	}
	return &recipient{spki: spki, pub: pub}, nil
}

// tag is the stanza argument that says which key a stanza is for.
func (r *recipient) tag() string {
	sum := sha256.Sum256(r.spki)
	return base64.RawStdEncoding.EncodeToString(sum[:4])
}

func (r *recipient) Wrap(fileKey []byte) ([]*age.Stanza, error) {
	ct, err := security.EncryptToPhone(r.pub, fileKey)
	if err != nil {
		return nil, err
	}
	return []*age.Stanza{{Type: stanzaType, Args: []string{r.tag()}, Body: ct}}, nil
}

// askPhone sends decrypt to the phone with this device id and returns the
// plaintext, or an error that says why not.
type askPhone func(ctx context.Context, deviceID, keyPin string, ciphertext []byte) ([]byte, error)

// identity decrypts with the phone that holds the key. It holds no secret.
type identity struct {
	deviceID string
	*recipient
	ask askPhone
}

func encodeIdentity(deviceID string, spki []byte) ([]byte, error) {
	if len(deviceID) == 0 || len(deviceID) > 255 {
		return nil, fmt.Errorf("device id %q is not 1 to 255 bytes", deviceID)
	}
	return append(append([]byte{byte(len(deviceID))}, deviceID...), spki...), nil
}

func parseIdentity(data []byte) (string, *recipient, error) {
	if len(data) < 1 || len(data) < 1+int(data[0]) || data[0] == 0 {
		return "", nil, errors.New("malformed phonetopc identity")
	}
	id := data[1 : 1+int(data[0])]
	if !utf8.Valid(id) {
		return "", nil, errors.New("malformed phonetopc identity: the device id is not UTF-8")
	}
	r, err := parseRecipient(data[1+int(data[0]):])
	if err != nil {
		return "", nil, fmt.Errorf("phonetopc identity: %w", err)
	}
	return string(id), r, nil
}

func (i *identity) Unwrap(stanzas []*age.Stanza) ([]byte, error) {
	tag := i.tag()
	for _, s := range stanzas {
		if s.Type != stanzaType || len(s.Args) != 1 || s.Args[0] != tag {
			continue
		}
		if len(s.Body) != i.pub.Size() {
			return nil, fmt.Errorf("phonetopc stanza body is %d bytes, not %d", len(s.Body), i.pub.Size())
		}
		pt, err := i.ask(context.Background(), i.deviceID, security.PinSPKI(i.spki), s.Body)
		if err != nil {
			return nil, err
		}
		if len(pt) != fileKeyLen {
			return nil, fmt.Errorf("the phone returned %d bytes, not a %d-byte file key", len(pt), fileKeyLen)
		}
		return pt, nil
	}
	return nil, age.ErrIncorrectIdentity
}

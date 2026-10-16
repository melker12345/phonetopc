package protocol

// Decryption with the phone's key (spec/DECRYPT.md).

// §4.
const MethodDecrypt = "decrypt"

// CapabilityDecrypt is listed in hello when the phone can decrypt (§1).
const CapabilityDecrypt = "decrypt"

// DecryptInfo is the decrypt member of the hello result (§1): the
// decryption key as base64url DER SubjectPublicKeyInfo, its pin, and its
// attestation chain, as in ApprovalInfo.
type DecryptInfo struct {
	Key         string   `json:"key"`
	Pin         string   `json:"pin"`
	Attestation []string `json:"attestation,omitempty"`
}

// §4.
type DecryptParams struct {
	Reason     string `json:"reason"`
	KeyPin     string `json:"key_pin"`
	Ciphertext string `json:"ciphertext"`
	// TimeoutMS is optional (default 30000).
	TimeoutMS *int64 `json:"timeout_ms,omitempty"`
}

// §4.
type DecryptResult struct {
	Decrypted bool   `json:"decrypted"`
	Plaintext string `json:"plaintext,omitempty"`
	Why       Why    `json:"why,omitempty"`
}

// WhyFailed: the user confirmed, but the ciphertext doesn't decrypt with
// the phone's key (§4).
const WhyFailed Why = "failed"

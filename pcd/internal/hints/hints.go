// Package hints turns errors of the client library into messages that say
// what to do, for the phonetopc CLI and phonetopcd.
package hints

import (
	"errors"
	"fmt"
	"strings"

	"github.com/melker12345/phonetopc/pcd/client"
)

// Explain turns an error into a message that says what to do.
func Explain(err error) string {
	var pm *client.PinMismatchError
	var amb *client.AmbiguousError
	switch {
	case errors.As(err, &pm):
		return fmt.Sprintf("phone identity changed — re-pair with `phonetopc pair`.\n"+
			"  The phone presented key %s, but %s was paired.\n"+
			"  If you didn't reinstall or reset the phone app, something may be impersonating it.", pm.Got, pm.Want)
	case client.IsUnauthorized(err):
		return "this PC was revoked on the phone — run `phonetopc pair` to pair it again"
	case errors.Is(err, client.ErrNoDevices):
		return "no paired phone — run `phonetopc pair`, or use --insecure-dev --addr HOST:PORT for a phone in developer mode"
	case errors.As(err, &amb):
		return amb.Error()
	case errors.Is(err, client.ErrApprovalKeyChanged):
		return strings.TrimPrefix(err.Error(), "phonetopc: ") + ".\n" +
			"  Typical cause: fingerprints were added or removed on the phone. If so, accept the new key with\n" +
			"  `phonetopc approval-key --accept`; otherwise someone may be tampering with the phone app. Nothing was sent."
	case errors.Is(err, client.ErrApprovalInvalid):
		return strings.TrimPrefix(err.Error(), "phonetopc: ") + ".\n" +
			"  This is NOT an approval. The phone app may be broken, or someone may be tampering with it."
	}
	return strings.TrimPrefix(err.Error(), "phonetopc: ")
}

// Command age-plugin-phonetopc lets age encrypt files so that reading them
// needs a fingerprint on a paired phone (spec/DECRYPT.md §5).
package main

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"filippo.io/age"
	"filippo.io/age/plugin"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
	"github.com/melker12345/phonetopc/pcd/internal/security"
	"github.com/melker12345/phonetopc/pcd/internal/version"
)

const usage = `age-plugin-phonetopc: age files that need a fingerprint on your phone to decrypt.

  age-plugin-phonetopc --generate [--device D] [--unattested] > identity.txt
      print an identity for the paired phone, with its recipient in a comment
  age -r age1phonetopc1… -r <recovery recipient> -o secrets.age secrets
  age -d -i identity.txt secrets.age

The identity holds no secret: the key stays on the phone, which asks for a fingerprint
every time. Adding or removing a fingerprint replaces that key, so also encrypt to a
recovery key kept offline.

--generate needs the phone's hardware to prove (attest) that the key needs a fingerprint
for every use; --unattested accepts a key without that proof.

Uses the pairing of phonetopc; $PHONETOPC_DEVICE, $PHONETOPC_ADDR, $PHONETOPC_NO_MDNS,
$PHONETOPC_REQUIRE_STRONGBOX, $PHONETOPC_CHECK_REVOCATION and $PHONETOPC_APP_SIGNING_DIGEST
work as there.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	p, err := plugin.New(name)
	if err != nil {
		fmt.Fprintln(stderr, "age-plugin-phonetopc:", err)
		return 1
	}
	p.HandleRecipient(func(data []byte) (age.Recipient, error) { return parseRecipient(data) })
	p.HandleIdentityAsRecipient(func(data []byte) (age.Recipient, error) {
		_, r, err := parseIdentity(data)
		return r, err
	})
	p.HandleIdentity(func(data []byte) (age.Identity, error) {
		id, r, err := parseIdentity(data)
		if err != nil {
			return nil, err
		}
		return &identity{deviceID: id, recipient: r, ask: askWith(p)}, nil
	})

	fs := flag.NewFlagSet("age-plugin-phonetopc", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	gen := fs.Bool("generate", false, "")
	unattested := fs.Bool("unattested", false, "")
	device := fs.String("device", os.Getenv("PHONETOPC_DEVICE"), "")
	help := fs.Bool("help", false, "")
	p.RegisterFlags(fs)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stderr, "age-plugin-phonetopc: %v\n\n%s", err, usage)
		return 2
	}
	switch {
	case *gen:
		if err := generate(context.Background(), *device, *unattested, stdout); err != nil {
			fmt.Fprintln(stderr, "age-plugin-phonetopc:", err)
			return 1
		}
		return 0
	case *help || !strings.HasPrefix(strings.Join(args, " "), "--age-plugin"):
		fmt.Fprint(stdout, usage)
		if *help {
			return 0
		}
		return 2
	}
	return p.Main()
}

func generate(ctx context.Context, device string, unattested bool, out io.Writer) error {
	api, err := systemAPI()
	if err != nil {
		return err
	}
	if api != nil {
		id, name, k, err := generateViaDaemon(ctx, api, device, unattested)
		if err != nil {
			return err
		}
		spki, err := security.DecodeB64(k.Key)
		if err != nil {
			return fmt.Errorf("phonetopcd sent a key that isn't base64url: %w", err)
		}
		return writeIdentity(out, id, name, k.Attestation, spki)
	}
	var opts []client.Option
	if unattested {
		opts = append(opts, client.WithUnattestedDecryptKey())
	}
	cl, err := dial(ctx, device, opts...)
	if err != nil {
		return err
	}
	defer cl.Close()
	k, err := cl.DecryptKey()
	if errors.Is(err, client.ErrKeyAttestation) {
		return fmt.Errorf("%w.\nWhat is encrypted to this key is only as safe as the key. To use it anyway: age-plugin-phonetopc --generate --unattested", err)
	}
	if err != nil {
		return err
	}
	dev := cl.Server().Device
	return writeIdentity(out, dev.ID, dev.Name, k.Attestation, k.SPKI)
}

func writeIdentity(out io.Writer, id, phone, attestation string, spki []byte) error {
	if _, err := parseRecipient(spki); err != nil {
		return err
	}
	data, err := encodeIdentity(id, spki)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "# phone: %q (%s)\n", phone, id)
	fmt.Fprintf(out, "# created: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(out, "# attestation: %s\n", attestation)
	fmt.Fprintf(out, "# recipient: %s\n", plugin.EncodeRecipient(name, spki))
	fmt.Fprintln(out, plugin.EncodeIdentity(name, data))
	return nil
}

func dial(ctx context.Context, selector string, extra ...client.Option) (*client.Client, error) {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	noMDNS := os.Getenv("PHONETOPC_NO_MDNS")
	opts := []client.Option{
		client.WithClientInfo("age-plugin-phonetopc", version.Version),
		client.WithDiscovery(noMDNS == "" || noMDNS == "0", 0),
	}
	if a := os.Getenv("PHONETOPC_ADDR"); a != "" {
		opts = append(opts, client.WithAddress(a))
	}
	aopts, err := client.AttestationOptionsFromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}
	opts = append(opts, aopts...)
	if attestationRoots != nil {
		opts = append(opts, client.WithAttestationRoots(attestationRoots...))
	}
	return client.DialPaired(dctx, selector, append(opts, extra...)...)
}

// attestationRoots replaces Google's attestation roots; only tests set it.
var attestationRoots []*x509.Certificate

// askWith asks the phone over a paired connection, telling the user through
// age to look at it.
func askWith(p *plugin.Plugin) askPhone {
	return func(ctx context.Context, deviceID, keyPin string, ciphertext []byte) ([]byte, error) {
		api, err := systemAPI()
		if err != nil {
			return nil, err
		}
		if api != nil {
			return decryptViaDaemon(ctx, api, deviceID, keyPin, ciphertext, func(msg string) {
				if p == nil || p.DisplayMessage(msg) != nil {
					fmt.Fprintln(os.Stderr, "age-plugin-phonetopc:", msg)
				}
			})
		}
		cl, err := dial(ctx, deviceID)
		if err != nil {
			return nil, err
		}
		defer cl.Close()
		show := func(match int) {
			msg := matchMessage(cl.Server().Device.Name, match)
			if p == nil || p.DisplayMessage(msg) != nil {
				fmt.Fprintln(os.Stderr, "age-plugin-phonetopc:", msg)
			}
		}
		res, err := cl.Decrypt(ctx, client.DecryptRequest{
			Reason: reason(parentCommand()), Program: "age-plugin-phonetopc", KeyPin: keyPin, Ciphertext: ciphertext, ShowMatch: show,
		})
		if err != nil {
			return nil, err
		}
		if !res.Decrypted() {
			return nil, fmt.Errorf("the phone did not decrypt: %s", res.Why)
		}
		return res.Plaintext, nil
	}
}

// matchMessage tells the user what to do on the phone (DECRYPT.md §5).
func matchMessage(phone string, match int) string {
	return fmt.Sprintf("confirm with your fingerprint on %q; pick %d", phone, match)
}

const plainReason = "Decrypt a file with age"

// reason is what the phone shows: the command that runs age, when known,
// cut to fit, or plainReason.
func reason(cmd string) string {
	cmd = strings.Join(strings.Fields(cmd), " ")
	if cmd == "" {
		return plainReason
	}
	r := "Decrypt: " + cmd
	if utf8.RuneCountInString(r) > 120 {
		r = strings.TrimSpace(string([]rune(r)[:119])) + "…"
	}
	if protocol.CheckReason(r) != nil {
		return plainReason
	}
	return r
}

// parentCommand is the command line of the process that ran the plugin
// (age, or a tool built on it), or "" where it can't be read.
func parentCommand() string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", os.Getppid()))
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(strings.TrimRight(string(b), "\x00"), "\x00", " ")
}

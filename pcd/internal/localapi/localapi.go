// Package localapi is how phonetopc's own tools find and use a phonetopcd
// that runs as a system service (LOCAL-API.md §1). That daemon owns the
// credential store, so the tools ask it instead of reading the store.
package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"

	"github.com/coder/websocket"
)

// SystemSocket is where the system service listens.
const SystemSocket = "/run/phonetopc/phonetopc.sock"

// Socket returns the system phonetopcd's socket if the tools should use it,
// or "" if they use the user's own store. $PHONETOPC_MODE=system or user
// decides; unset, the system service is used if its socket exists.
// $PHONETOPC_SOCKET replaces SystemSocket.
func Socket(getenv func(string) string) (string, error) {
	path := getenv("PHONETOPC_SOCKET")
	if path == "" {
		path = SystemSocket
	}
	switch mode := getenv("PHONETOPC_MODE"); mode {
	case "system":
		return path, nil
	case "user":
		return "", nil
	case "":
		if fi, err := os.Stat(path); err == nil && fi.Mode().Type() == os.ModeSocket {
			return path, nil
		}
		return "", nil
	default:
		return "", fmt.Errorf("$PHONETOPC_MODE is %q; want system or user", mode)
	}
}

type Client struct {
	socket string
	hc     *http.Client
}

func New(socket string) *Client {
	return &Client{socket: socket, hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

func (c *Client) Socket() string { return c.socket }

// Error is an error answer of the API (LOCAL-API.md §2).
type Error struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
	RPCCode int    `json:"rpc_code,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// IsCode reports whether err is an API error with this code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// reachErr explains why the daemon can't be reached.
func (c *Client) reachErr(err error) error {
	switch {
	case errors.Is(err, syscall.EACCES):
		return fmt.Errorf("you may not use phonetopcd (%s): add yourself to the phonetopc-users group with `sudo usermod -aG phonetopc-users $USER`, then log in again", c.socket)
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("phonetopcd isn't running at %s (`systemctl status phonetopcd.socket`; PHONETOPC_MODE=user uses your own pairings instead)", c.socket)
	}
	return fmt.Errorf("phonetopcd at %s: %w", c.socket, err)
}

// Do sends in (if not nil) as JSON and decodes the answer into out (if not
// nil). An error answer is an *Error.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	res, err := c.send(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("phonetopcd's answer to %s %s: %w", method, path, err)
	}
	return nil
}

func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodGet, path, nil, out)
}

// Ask posts an approval or decryption (LOCAL-API.md §5, §6): it calls show
// with the number the user must pick on the phone and the program the
// phone names, then decodes the outcome into out.
func (c *Client) Ask(ctx context.Context, path string, in any, show func(match int, program string), out any) error {
	res, err := c.send(ctx, http.MethodPost, path, in)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	dec := json.NewDecoder(res.Body)
	for {
		var line json.RawMessage
		if err := dec.Decode(&line); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("phonetopcd's answer to POST %s: %w", path, err)
		}
		var probe struct {
			Match   *int   `json:"match"`
			Program string `json:"program"`
			Error   *Error `json:"error"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			return fmt.Errorf("phonetopcd's answer to POST %s: %w", path, err)
		}
		switch {
		case probe.Error != nil:
			probe.Error.Status = res.StatusCode
			return probe.Error
		case probe.Match != nil:
			show(*probe.Match, probe.Program)
		default:
			return json.Unmarshal(line, out)
		}
	}
}

// Stream opens a GET whose answer doesn't end, such as /v1/events.
func (c *Client) Stream(ctx context.Context, path string) (io.ReadCloser, error) {
	res, err := c.send(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

func (c *Client) send(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://phonetopc"+path, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, c.reachErr(err)
	}
	if res.StatusCode/100 != 2 {
		defer res.Body.Close()
		var e struct{ Error Error }
		b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if json.Unmarshal(b, &e) != nil || e.Error.Code == "" {
			return nil, &Error{Status: res.StatusCode, Code: "http_error", Message: fmt.Sprintf("phonetopcd answered %s", res.Status)}
		}
		e.Error.Status = res.StatusCode
		return nil, &e.Error
	}
	return res, nil
}

// WebSocket opens a WebSocket request, such as /v1/pairing.
func (c *Client) WebSocket(ctx context.Context, path string) (*websocket.Conn, error) {
	conn, res, err := websocket.Dial(ctx, "ws://phonetopc"+path, &websocket.DialOptions{HTTPClient: c.hc})
	if err == nil {
		return conn, nil
	}
	if res != nil && res.StatusCode/100 != 2 && res.StatusCode != http.StatusSwitchingProtocols {
		var e struct{ Error Error }
		if res.Body != nil {
			b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			if json.Unmarshal(b, &e) == nil && e.Error.Code != "" {
				e.Error.Status = res.StatusCode
				return nil, &e.Error
			}
		}
		return nil, &Error{Status: res.StatusCode, Code: "http_error", Message: fmt.Sprintf("phonetopcd answered %s", res.Status)}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return nil, c.reachErr(ue.Err)
	}
	return nil, c.reachErr(err)
}

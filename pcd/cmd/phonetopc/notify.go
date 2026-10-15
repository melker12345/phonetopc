package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/melker12345/phonetopc/pcd/client"
	"github.com/melker12345/phonetopc/pcd/internal/protocol"
)

// actionFlags collects repeated --action id:Label (or just Label) flags.
type actionFlags []client.NotifyAction

func (a *actionFlags) String() string { return fmt.Sprint(*a) }

func (a *actionFlags) Set(s string) error {
	id, label, ok := strings.Cut(s, ":")
	if !ok {
		label = s
		id = strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
				return r
			case r >= 'A' && r <= 'Z':
				return r + 'a' - 'A'
			}
			return '_'
		}, s)
	}
	*a = append(*a, client.NotifyAction{ID: id, Label: label})
	return nil
}

// PANELS.md §3.
func (c *cli) notify(ctx context.Context, args []string) error {
	fs := c.flagSet("notify")
	var opts client.NotifyOptions
	var actions actionFlags
	fs.StringVar(&opts.Body, "body", "", "")
	fs.StringVar(&opts.ID, "id", "", "")
	fs.BoolVar(&opts.High, "high", false, "")
	fs.Var(&actions, "action", "")
	wait := fs.Bool("wait", false, "")
	pos, err := parse(fs, args, 1, "title")
	if err != nil {
		return err
	}
	opts.Actions = actions
	opts.Body = strings.ReplaceAll(opts.Body, `\n`, "\n")
	if err := protocol.CheckNotify(protocol.NotifyParams{Title: pos[0], Body: opts.Body, ID: opts.ID, Actions: opts.Actions}); err != nil {
		return usageErr{fmt.Errorf("notify: %w", err)}
	}
	cl, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer cl.Close()
	var sub *client.Subscription
	if *wait {
		// Subscribe first, so an answer can't come before we listen.
		if sub, err = cl.Subscribe(ctx, client.SourceNotification, client.SubscribeOptions{}); err != nil {
			return explainNotAllowed(err)
		}
	}
	id, err := cl.Notify(ctx, pos[0], opts)
	if err != nil {
		return explainNotAllowed(err)
	}
	if !*wait {
		if c.json {
			return c.printJSON(map[string]string{"id": id})
		}
		_, err := fmt.Fprintln(c.stdout, id)
		return err
	}
	fmt.Fprintf(c.stderr, "phonetopc: posted notification %q; waiting for the user (Ctrl-C to stop)…\n", id)
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-sub.Events():
			if !ok {
				return sub.Err()
			}
			n, err := ev.Notification()
			if err != nil || n.Notification != id {
				continue
			}
			if c.json {
				return c.printJSON(map[string]string{"id": id, "type": ev.Type, "action": n.Action})
			}
			answer := ev.Type
			if ev.Type == client.TypeAction {
				answer = n.Action
			}
			_, err = fmt.Fprintln(c.stdout, answer)
			return err
		}
	}
}

// explainNotAllowed says where to turn a feature on.
func explainNotAllowed(err error) error {
	var pe *protocol.Error
	if client.IsNotAllowed(err) && errors.As(err, &pe) && pe.Data != nil {
		what := "notifications"
		if pe.Data.Permission == protocol.PermissionPanels {
			what = "screen panels"
		}
		return fmt.Errorf("the phone doesn't allow %s for this PC yet: turn them on for it in the phonetopc app", what)
	}
	if code, ok := protocol.ErrorCodeOf(err); ok && code == protocol.CodeSourceUnavailable {
		return errors.New("the phone doesn't allow notifications for this PC yet: turn them on for it in the phonetopc app")
	}
	return err
}

// formatEvent renders panel and notification event values, which are too
// nested for formatValue; ok is false for other sources. The panel's pseq
// is p<N>:
//
//	deck p3 mute
//	deck p4 vol = 42 (final)
//	pad p7 ink stroke 12 move finger 2 pt (0.4120,0.2331)…(0.4153,0.2402)
//	claude open
func formatEvent(r client.Reading) (string, bool) {
	switch r.Source {
	case client.SourceNotification:
		n, err := r.Notification()
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(n.Notification + " " + n.Action), true
	case client.SourcePanel:
	default:
		return "", false
	}
	p, err := r.Panel()
	if err != nil {
		return "", false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s p%d", p.Panel, p.Pseq)
	switch r.Type {
	case client.TypeState:
		b.WriteString(" " + p.State)
		if p.Reason != "" {
			b.WriteString(" (" + p.Reason + ")")
		}
	case client.TypeStroke:
		fmt.Fprintf(&b, " %s stroke %d %s %s %d pt", p.Widget, deref(p.Stroke), p.Phase, p.Tool, len(p.Points))
		if n := len(p.Points); n > 0 {
			fmt.Fprintf(&b, " (%.4f,%.4f)", p.Points[0].X(), p.Points[0].Y())
			if n > 1 {
				fmt.Fprintf(&b, "…(%.4f,%.4f)", p.Points[n-1].X(), p.Points[n-1].Y())
			}
		}
		if p.Replayed {
			b.WriteString(" replayed")
		}
	default:
		b.WriteString(" " + p.Widget)
		if len(p.Value) > 0 {
			b.WriteString(" = " + string(p.Value))
		}
		if p.Final {
			b.WriteString(" (final)")
		}
	}
	return b.String(), true
}

func deref(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

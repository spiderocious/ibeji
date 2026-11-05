package cli

import (
	"context"
	"flag"
	"fmt"
	"text/tabwriter"
	"time"
)

// Pairing from the CLI.
//
// Until this existed, two machines could only be paired through the Electron
// window — so a headless Mac, a machine reached over SSH, or an agent setting
// itself up had no route in at all. The daemon has always exposed the whole
// ceremony; only the window spoke it.
//
// The ceremony still requires a human to compare six digits on two screens.
// That is the security property, not an interface limitation: the code is what
// proves no machine in the middle substituted its own key. So this prints the
// digits and waits to be told they match; it never confirms on its own.

var cmdPair = &Command{
	Name:    "pair",
	Usage:   "ibeji pair [<address>] | ibeji pair confirm | ibeji pair cancel | ibeji pair status",
	Summary: "Pair with your other Mac: start a ceremony, or confirm the one in flight",
	Run: func(ctx context.Context, env *Env, args []string) error {
		if len(args) > 0 {
			switch args[0] {
			case "confirm", "yes", "match":
				return pairConfirm(ctx, env)
			case "cancel", "no":
				return pairCancel(ctx, env)
			case "status":
				return pairStatus(ctx, env)
			case "scan", "devices":
				return pairScan(ctx, env)
			}
		}

		fs := flag.NewFlagSet("pair", flag.ContinueOnError)
		fs.SetOutput(env.Err)
		wait := fs.Bool("wait", false, "wait for the other machine to confirm, then confirm here")
		if err := fs.Parse(args); err != nil {
			return err
		}
		rest := fs.Args()

		// No address: either a ceremony is already in flight (the other machine
		// dialled us), or we have nothing to do but show what is on the LAN.
		if len(rest) == 0 {
			if v, _ := pending(ctx, env); v != nil {
				return showCeremony(env, v)
			}
			fmt.Fprintln(env.Out, "No pairing in flight. Give an address to start one:")
			fmt.Fprintln(env.Out, "  ibeji pair 192.168.1.39:7749")
			fmt.Fprintln(env.Out)
			return pairScan(ctx, env)
		}

		var view pairingView
		if err := env.Client.PostJSON(ctx, "/v1/pair/start",
			map[string]any{"address": rest[0]}, &view); err != nil {
			return err
		}
		if err := showCeremony(env, &view); err != nil {
			return err
		}
		if *wait {
			return pairWaitAndConfirm(ctx, env, &view)
		}
		return nil
	},
}

type pairingView struct {
	ID              string `json:"id"`
	Code            string `json:"code"`
	CodeFormatted   string `json:"codeFormatted"`
	PeerName        string `json:"peerName"`
	PeerAddress     string `json:"peerAddress"`
	PeerFingerprint string `json:"peerFingerprint"`
}

func pending(ctx context.Context, env *Env) (*pairingView, error) {
	var out struct {
		Pairing *pairingView `json:"pairing"`
	}
	if err := env.Client.GetJSON(ctx, "/v1/pair", &out); err != nil {
		return nil, err
	}
	return out.Pairing, nil
}

// showCeremony prints the six digits the way they must be read: grouped, on
// their own line, with the peer's fingerprint beside them.
func showCeremony(env *Env, v *pairingView) error {
	code := v.CodeFormatted
	if code == "" {
		code = v.Code
	}
	fmt.Fprintf(env.Out, "\n  %s\n\n", code)
	fmt.Fprintf(env.Out, "  Peer   %s\n", v.PeerName)
	fmt.Fprintf(env.Out, "  At     %s\n", v.PeerAddress)
	fmt.Fprintf(env.Out, "  Key    %s\n\n", v.PeerFingerprint)
	fmt.Fprintln(env.Out, "The same six digits must be showing on the other Mac.")
	fmt.Fprintln(env.Out, "If they match, run `ibeji pair confirm` on BOTH machines.")
	fmt.Fprintln(env.Out, "If they do not, run `ibeji pair cancel` — something is wrong.")
	return nil
}

func pairConfirm(ctx context.Context, env *Env) error {
	v, err := pending(ctx, env)
	if err != nil {
		return err
	}
	if v == nil {
		return fmt.Errorf("no pairing is in flight on this machine")
	}
	var out struct {
		Device struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"device"`
	}
	if err := env.Client.PostJSON(ctx, "/v1/pair/confirm",
		map[string]any{"sessionId": v.ID}, &out); err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "paired with %s at %s\n", out.Device.Name, out.Device.Address)
	fmt.Fprintln(env.Out, "Run `ibeji devices` to see the link come up.")
	return nil
}

func pairCancel(ctx context.Context, env *Env) error {
	v, err := pending(ctx, env)
	if err != nil {
		return err
	}
	if v == nil {
		fmt.Fprintln(env.Out, "Nothing to cancel.")
		return nil
	}
	if err := env.Client.PostJSON(ctx, "/v1/pair/cancel",
		map[string]any{"sessionId": v.ID}, nil); err != nil {
		return err
	}
	fmt.Fprintln(env.Out, "cancelled")
	return nil
}

func pairStatus(ctx context.Context, env *Env) error {
	v, err := pending(ctx, env)
	if err != nil {
		return err
	}
	if v == nil {
		fmt.Fprintln(env.Out, "No pairing in flight.")
		return nil
	}
	return showCeremony(env, v)
}

// pairWaitAndConfirm polls until the peer has pinned us, then confirms here.
//
// It still does not decide that the digits matched — the caller passed --wait
// having read them. What it removes is the race of having to press confirm on
// two machines at once.
func pairWaitAndConfirm(ctx context.Context, env *Env, v *pairingView) error {
	fmt.Fprintln(env.Out, "Waiting for the other machine to confirm…")
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		time.Sleep(2 * time.Second)
		cur, err := pending(ctx, env)
		if err != nil {
			continue
		}
		// The session going away means it was consumed — the ceremony is over
		// one way or the other.
		if cur == nil || cur.ID != v.ID {
			break
		}
	}
	return pairConfirm(ctx, env)
}

func pairScan(ctx context.Context, env *Env) error {
	var out struct {
		Candidates []struct {
			Name       string `json:"name"`
			Address    string `json:"address"`
			Port       int    `json:"port"`
			Compatible bool   `json:"compatible"`
		} `json:"candidates"`
		Hint string `json:"hint"`
	}
	if err := env.Client.GetJSON(ctx, "/v1/discovery", &out); err != nil {
		return err
	}
	if len(out.Candidates) == 0 {
		fmt.Fprintln(env.Out, "Nothing found on the LAN.")
		if out.Hint != "" {
			fmt.Fprintf(env.Out, "%s\n", out.Hint)
		}
		// The address this machine listens on is what the other one must be
		// told, so print it rather than making the user go and find it.
		var self struct {
			Addresses []string `json:"addresses"`
			Port      int      `json:"port"`
		}
		if err := env.Client.GetJSON(ctx, "/v1/self", &self); err == nil && len(self.Addresses) > 0 {
			fmt.Fprintf(env.Out, "\nThis Mac is at %s:%d — type that on the other one.\n",
				self.Addresses[0], self.Port)
		}
		return nil
	}
	w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tADDRESS\tCOMPATIBLE")
	for _, c := range out.Candidates {
		yes := "yes"
		if !c.Compatible {
			yes = "no — version mismatch"
		}
		fmt.Fprintf(w, "%s\t%s:%d\t%s\n", c.Name, c.Address, c.Port, yes)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "\nPair with:  ibeji pair <address>\n")
	return nil
}

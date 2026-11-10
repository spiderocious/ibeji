package cli

import (
	"context"
	"fmt"
	"text/tabwriter"
)

// `ibeji sweep` — find a daemon on this network.
//
// `ibeji reconnect` takes an address, and the whole difficulty is that when a
// peer moves you do not have one. mDNS is the intended answer and plenty of
// routers block it; the written-down address is the fallback, and a DHCP
// lease expiring makes that fallback wrong overnight.
//
// The failure is silent and badly signposted. Nothing answers at the old
// address, which reports as "the paired device is not reachable" — the same
// words a machine that is switched off produces, and the same words a denied
// macOS permission produces. One of those sent a day into checking a
// permission that was fine, when the other Mac had simply moved from
// 192.168.1.147 to .39 after the network changed subnets.
//
// A sweep settles it in under two seconds: probe the /24 around this machine
// for daemons on the Ìbejì port and name what answers.

var cmdSweep = &Command{
	Name:    "sweep",
	Usage:   "ibeji sweep",
	Summary: "Look for Ìbejì daemons on this network, for a peer that changed address",
	Run: func(ctx context.Context, env *Env, args []string) error {
		var out struct {
			Subnet  string `json:"subnet"`
			Port    int    `json:"port"`
			Scanned int    `json:"scanned"`
			Took    string `json:"took"`
			Hint    string `json:"hint"`
			Found   []struct {
				Address string `json:"address"`
				Name    string `json:"name"`
				Self    bool   `json:"self"`
				Ibeji   bool   `json:"ibeji"`
			} `json:"found"`
		}
		if err := env.Client.PostJSON(ctx, "/v1/pair/sweep", map[string]any{}, &out); err != nil {
			return err
		}

		w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SUBNET\tPORT\tSCANNED\tTOOK")
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", out.Subnet, out.Port, out.Scanned, out.Took)
		if err := w.Flush(); err != nil {
			return err
		}

		if len(out.Found) == 0 {
			fmt.Fprintf(env.Out, "\n%s\n", out.Hint)
			return nil
		}

		fmt.Fprintln(env.Out)
		w = tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tADDRESS\tNOTE")
		for _, f := range out.Found {
			name := f.Name
			if name == "" {
				name = "—"
			}
			// Mark this machine rather than hiding it: a sweep that silently
			// drops a result is harder to trust than one that explains it,
			// and seeing your own Mac listed confirms the sweep works.
			note := ""
			switch {
			case f.Self:
				note = "this Mac"
			case !f.Ibeji:
				note = "answered, but not as Ìbejì"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", name, f.Address, note)
		}
		if err := w.Flush(); err != nil {
			return err
		}

		fmt.Fprintf(env.Out, "\n%s\n", out.Hint)
		// Say the next command outright. The sweep's whole purpose is to feed
		// an address to one of these two, and making the reader work out
		// which is a small cruelty at the end of a long debugging session.
		for _, f := range out.Found {
			if !f.Self && f.Ibeji {
				fmt.Fprintf(env.Out, "\n  already paired:  ibeji reconnect %s\n", f.Address)
				fmt.Fprintf(env.Out, "  not yet paired:  ibeji pair %s\n", f.Address)
				break
			}
		}
		return nil
	},
}

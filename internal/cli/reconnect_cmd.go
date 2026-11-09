package cli

import (
	"context"
	"fmt"
)

// `ibeji reconnect` — tell the daemon where the paired machine went.
//
// Until this existed, a peer that changed address and could not be found over
// mDNS left unpairing as the only available action, which throws away a
// working pairing to work around a network problem. The keys are already
// pinned; a new IP is not a trust event.

var cmdReconnect = &Command{
	Name:    "reconnect",
	Usage:   "ibeji reconnect <address>",
	Summary: "Point the daemon at the paired machine's new address, without re-pairing",
	Run: func(ctx context.Context, env *Env, args []string) error {
		if len(args) < 1 {
			// Say what this machine is, so the other end can be told too.
			var self struct {
				Addresses []string `json:"addresses"`
				Port      int      `json:"port"`
			}
			if err := env.Client.GetJSON(ctx, "/v1/self", &self); err == nil && len(self.Addresses) > 0 {
				fmt.Fprintf(env.Err, "usage: ibeji reconnect <address>\n\n")
				fmt.Fprintf(env.Err, "This Mac is at %s:%d — run `ibeji reconnect` on the other one with that.\n",
					self.Addresses[0], self.Port)
				return fmt.Errorf("no address given")
			}
			return fmt.Errorf("usage: ibeji reconnect <address>")
		}

		var out struct {
			Device struct {
				Name    string `json:"name"`
				Address string `json:"address"`
				Status  string `json:"status"`
			} `json:"device"`
		}
		if err := env.Client.PostJSON(ctx, "/v1/devices/reconnect",
			map[string]any{"address": args[0]}, &out); err != nil {
			return err
		}
		fmt.Fprintf(env.Out, "reconnected to %s at %s (%s)\n",
			out.Device.Name, out.Device.Address, out.Device.Status)
		return nil
	},
}

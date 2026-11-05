package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

// Space management from the CLI.
//
// These exist because the daemon has implemented every one of these actions
// since M3 and no client exposed them: the window could not create a space,
// the CLI had no subcommand for it, and `ibeji spaces` on an empty daemon
// printed "Create one in the Ìbejì window" — pointing at a window that could
// not do it either. An agent following that instruction has nowhere to go.
//
// The whole product is one daemon API with thin clients over it. A capability
// reachable from none of them is not shipped.

// spaceSub dispatches `ibeji spaces <verb> ...`, falling through to the
// listing when the first argument is not a verb (so `ibeji spaces atlas`
// still shows one space).
func spaceSub(ctx context.Context, env *Env, args []string) (handled bool, err error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "create", "new":
		return true, spaceCreate(ctx, env, args[1:])
	case "pause":
		return true, spaceSimple(ctx, env, "pause", args[1:], "paused")
	case "resume":
		return true, spaceSimple(ctx, env, "resume", args[1:], "resumed")
	case "leave":
		return true, spaceSimple(ctx, env, "leave", args[1:], "left")
	case "delete", "rm":
		return true, spaceSimple(ctx, env, "delete", args[1:], "deleted")
	case "wait":
		return true, spaceWait(ctx, env, args[1:])
	}
	return false, nil
}

func spaceCreate(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("spaces create", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	role := fs.String("role", "master", "master or replica")
	mode := fs.String("mode", "two-way", "two-way, or mirror for master-to-replica only")
	ignore := fs.String("ignore", "", "extra ignore patterns, comma separated")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("usage: ibeji spaces create <name> <folder> [--role master|replica] [--mode two-way|mirror]")
	}
	name := rest[0]

	// An absolute path, because the daemon is a different process with its own
	// working directory: a relative path would resolve somewhere the user did
	// not mean, and silently create a space over the wrong folder.
	path, err := filepath.Abs(rest[1])
	if err != nil {
		return fmt.Errorf("resolve %s: %w", rest[1], err)
	}
	if info, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	} else if !info.IsDir() {
		return fmt.Errorf("%s is not a folder", path)
	}

	if *role != "master" && *role != "replica" {
		return fmt.Errorf("--role must be master or replica, got %q", *role)
	}
	if *mode != "two-way" && *mode != "mirror" {
		return fmt.Errorf("--mode must be two-way or mirror, got %q", *mode)
	}

	var patterns []string
	for _, p := range strings.Split(*ignore, ",") {
		if p = strings.TrimSpace(p); p != "" {
			patterns = append(patterns, p)
		}
	}

	body := map[string]any{"name": name, "path": path, "role": *role, "mode": *mode}
	if len(patterns) > 0 {
		body["ignore"] = patterns
	}

	var out struct {
		Space struct {
			Name string `json:"name"`
			Path string `json:"path"`
			Role string `json:"role"`
			Mode string `json:"mode"`
		} `json:"space"`
		Warning string `json:"warning"`
	}
	if err := env.Client.PostJSON(ctx, "/v1/spaces/create", body, &out); err != nil {
		return err
	}

	fmt.Fprintf(env.Out, "created %s · %s · %s\n", out.Space.Name, out.Space.Role, out.Space.Path)
	// The .git warning is the daemon's, not ours to decide: it travels with
	// the response so whoever removed the ignore is the one who hears it.
	if out.Warning != "" {
		fmt.Fprintf(env.Err, "\nwarning  %s\n", out.Warning)
	}
	fmt.Fprintf(env.Out, "\nOn the other machine, run:\n  ibeji spaces create %s <folder> --role %s\n",
		name, opposite(out.Space.Role))
	return nil
}

// opposite names the role the peer must take: a space has exactly one master.
func opposite(role string) string {
	if role == "master" {
		return "replica"
	}
	return "master"
}

func spaceSimple(ctx context.Context, env *Env, verb string, args []string, done string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: ibeji spaces %s <name>", verb)
	}
	name := args[0]
	if err := env.Client.PostJSON(ctx, "/v1/spaces/"+verb, map[string]any{"name": name}, nil); err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "%s %s\n", done, name)
	return nil
}

func spaceWait(ctx context.Context, env *Env, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: ibeji spaces wait <name>")
	}
	name := args[0]
	var out struct {
		State string `json:"state"`
	}
	if err := env.Client.PostJSON(ctx, "/v1/spaces/wait", map[string]any{"name": name}, &out); err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "%s · %s\n", name, out.State)
	return nil
}

// --- rules -----------------------------------------------------------------

var cmdRules = &Command{
	Name:    "rules",
	Usage:   "ibeji rules [revoke <id>]",
	Summary: "Remembered permission rules, and how to revoke one",
	Run: func(ctx context.Context, env *Env, args []string) error {
		if len(args) >= 2 && (args[0] == "revoke" || args[0] == "rm") {
			if err := env.Client.PostJSON(ctx, "/v1/rules/revoke",
				map[string]any{"id": args[1]}, nil); err != nil {
				return err
			}
			fmt.Fprintf(env.Out, "revoked %s\n", args[1])
			return nil
		}

		var out struct {
			Rules []struct {
				ID      string `json:"id"`
				Command string `json:"command"`
				Cwd     string `json:"cwd"`
				Device  string `json:"device"`
				Hits    int    `json:"hits"`
				Created string `json:"createdAt"`
			} `json:"rules"`
		}
		if err := env.Client.GetJSON(ctx, "/v1/rules", &out); err != nil {
			return err
		}
		if len(out.Rules) == 0 {
			fmt.Fprintln(env.Out, "No remembered rules. Every command asks first.")
			return nil
		}
		w := tabwriter.NewWriter(env.Out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tHITS\tDEVICE\tCOMMAND\tIN")
		for _, r := range out.Rules {
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", r.ID, r.Hits, r.Device, r.Command, r.Cwd)
		}
		return w.Flush()
	},
}

// --- prompts ---------------------------------------------------------------

var cmdPrompts = &Command{
	Name:    "prompts",
	Usage:   "ibeji prompts [allow|deny <id>] [--remember]",
	Summary: "Requests waiting for your approval on this machine",
	Run: func(ctx context.Context, env *Env, args []string) error {
		if len(args) >= 2 && (args[0] == "allow" || args[0] == "deny") {
			fs := flag.NewFlagSet("prompts", flag.ContinueOnError)
			fs.SetOutput(env.Err)
			remember := fs.Bool("remember", false, "always allow this exact command in this folder")
			if err := fs.Parse(args[2:]); err != nil {
				return err
			}
			accept := args[0] == "allow"
			if err := env.Client.PostJSON(ctx, "/v1/prompts/answer", map[string]any{
				"id": args[1], "accept": accept, "remember": *remember,
			}, nil); err != nil {
				return err
			}
			word := "declined"
			if accept {
				word = "allowed"
			}
			if accept && *remember {
				word = "allowed, and remembered"
			}
			fmt.Fprintf(env.Out, "%s %s\n", word, args[1])
			return nil
		}

		var out struct {
			Prompts []struct {
				ID      string `json:"id"`
				Device  string `json:"device"`
				Action  string `json:"action"`
				Command string `json:"command"`
				Cwd     string `json:"cwd"`
			} `json:"prompts"`
		}
		if err := env.Client.GetJSON(ctx, "/v1/prompts", &out); err != nil {
			return err
		}
		if len(out.Prompts) == 0 {
			fmt.Fprintln(env.Out, "Nothing is waiting for approval.")
			return nil
		}
		for _, p := range out.Prompts {
			fmt.Fprintf(env.Out, "%s  %s wants to %s\n", p.ID, p.Device, p.Action)
			if p.Command != "" {
				fmt.Fprintf(env.Out, "     %s\n", p.Command)
			}
			if p.Cwd != "" {
				fmt.Fprintf(env.Out, "     in %s\n", p.Cwd)
			}
		}
		fmt.Fprintf(env.Out, "\nAnswer with: ibeji prompts allow <id> [--remember]\n")
		return nil
	},
}

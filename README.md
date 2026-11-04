# Ìbejì

A LAN-only companion app that links **exactly two** of your own computers into a
trusted pair: shared folders, remote port tunnels, file drop, and a CLI / HTTP
API / MCP surface so both you and your AI agents can work across machines.

*Ìbejì* (ee-BAY-jee) is Yoruba for **twins** — two machines acting as one pair.

**macOS only.** The daemon is built on launchd, the macOS keychain and a zsh
login shell; it does not run on Windows or Linux and does not cross-compile to
them. Both machines must be Macs.

Nothing here touches the internet. No accounts, no cloud, no relay, no web app.
If the two machines are not on the same LAN, Ìbejì does nothing. It also never
shares, bridges or forwards a VPN — the VPN stays on one machine and what
crosses the LAN is a command, a tunnel or a URL.

## The thing it is for

```sh
ibeji run desktop --cwd ~/projects/medcord --after-sync medcord -- git push
```

You edit on the Laptop. The push leaves from the Desktop, with Desktop's
credentials on Desktop's VPN. `--after-sync` waits for your last save to land
first, so the commit never captures a half-synced tree.

## Documents

| | |
|---|---|
| [`docs/prd.md`](docs/prd.md) | What the product is. Nine modules, the permission model, the deferred list |
| [`docs/build-plan.html`](docs/build-plan.html) | How it was built. Module by module, four milestones, drawn in the product's own design system |

The design system lives outside this repo at
`dockito/design-system/projects/ibeji` — 113 specimens, frozen as built. The app
vendors its two stylesheets via `app/scripts/sync-design-system.sh`.

## Layout

One repo, one version: a mismatch between the app and the API it consumes is
impossible by construction.

```
cmd/ibeji-daemon    the daemon — identity, link, permissions, spaces, trace, API
cmd/ibeji           the CLI — a thin client over that API
cmd/ibeji-mcp       the MCP server — the same API, for agents
internal/
  identity          Ed25519 keys, pinned peers, the macOS keychain
  link              the pinned-key TLS transport, and an in-memory twin for tests
  discovery         mDNS, and manual entry by IP — both first class
  device            the daemon's core: state, supervisor, stream routing
  api               the local HTTP surface every client speaks
  permission        the chokepoint every side effect passes through
  remote            exec, search, read, write, over the link
  space             shared folders, master-wins sync, the barrier
  ports             the port survey and tunnels
  transfer          file drop, clipboard, handoff, the scratchpad
  trace             what the daemon did, with payloads, capped
  doctor            six checks, each failure naming its fix
  surface           the API described once, for agents
app/                the Electron window — also a client of that API
docs/               the PRD and the build plan
test/               end-to-end runs against two real daemons
```

## Building

```sh
make build      # daemon, CLI and MCP server into bin/
make check      # what CI runs: vet plus the suite under the race detector
make test       # just the Go suite
make e2e        # four end-to-end runs against two real daemons
make smoke      # the Electron window, with its assertions
```

`make help` lists every target.

## Installing

Ìbejì is **macOS only**, and goes on **both** of your Macs. On each one:

```sh
curl -fsSL https://raw.githubusercontent.com/spiderocious/ibeji/main/scripts/install.sh | sh
```

That is the whole install. It picks the right build for your Mac, checks the
download against the release's checksums, installs to `/Applications`, starts
the daemon so it comes back at login, and puts `ibeji` on your PATH. No
password, no Gatekeeper prompt, nothing to click.

Then open Ìbejì on both machines and pair them: one shows six digits, the
other must show the same six. Both have to be on the same LAN or nothing
happens — that is the whole security model.

To remove it: `ibeji install --uninstall`, then drag the app to the Bin. Your
identity stays in the keychain until you unpair.

<details>
<summary>Prefer to download by hand?</summary>

| Your Mac | Download |
|---|---|
| Apple Silicon (M1–M4) | [**Ibeji-0.1.2-arm64.dmg**](https://github.com/spiderocious/ibeji/releases/latest/download/Ibeji-0.1.2-arm64.dmg) |
| Intel | [**Ibeji-0.1.2.dmg**](https://github.com/spiderocious/ibeji/releases/latest/download/Ibeji-0.1.2.dmg) |

` > About This Mac`. "Apple M…" means Apple Silicon.

Drag Ibeji to Applications and open it — the app installs its own daemon and
CLI on first launch.

A browser marks everything it downloads, so macOS will refuse the first open
with *"Apple could not verify Ibeji is free of malware"*. Clear it once in
**System Settings → Privacy & Security → Open Anyway**, or with:

```sh
xattr -dr com.apple.quarantine /Applications/Ibeji.app
```

The `curl` installer above avoids this entirely — not by defeating Gatekeeper,
but because the flag that triggers it is attached by browsers, and `curl` does
not set it.

(Right-click → Open does *not* work on current macOS. If you ever see "Ibeji
is **damaged**", that is a different and genuine fault — please file an issue.)

</details>

### Why macOS complains

The app is **ad-hoc signed**: a real signature that carries no certificate, so
the bundle is internally consistent and macOS will run it. What it is not is
**notarized** — that needs a paid Apple Developer account, so macOS cannot
check it against Apple's malware service and says so once, until you clear the
quarantine flag above.

If you would rather not trust a binary from the internet — a fair instinct for
something that runs commands on your machines — build it yourself:

```sh
git clone https://github.com/spiderocious/ibeji && cd ibeji
make build && cd app && npm install && npm run dist:arm64   # or dist:x64
```

The DMG lands in `dist/`, and `npm run dist:*` verifies that every binary
inside it matches the architecture on the label before it finishes.

### Just the command line

If you only want the CLI and the MCP server — for a headless machine, or for an
agent — the release also carries
[`ibeji-cli-0.1.2-darwin-arm64.tar.gz`](https://github.com/spiderocious/ibeji/releases/latest/download/ibeji-cli-0.1.2-darwin-arm64.tar.gz)
and
[`ibeji-cli-0.1.2-darwin-x64.tar.gz`](https://github.com/spiderocious/ibeji/releases/latest/download/ibeji-cli-0.1.2-darwin-x64.tar.gz):
the three binaries, nothing else. Extract them somewhere on your PATH and run
`ibeji install` from that directory. Checksums are in `SHA256SUMS.txt`.

## The commands

```
ibeji devices                            the paired device, its address and mode
ibeji ls desktop:~/projects              list a directory over there
ibeji cat desktop:~/notes.md             read a file
ibeji grep desktop:~/p TODO --glob '*.ts'  search contents and names
ibeji run desktop -- git push            run a command, with its exit code
ibeji spaces                             shared folders and their state
ibeji ports                              what is listening on both machines
ibeji tunnel desktop:3000                make it answer on localhost here
ibeji cp build.zip desktop:              send a file to the inbox
ibeji open desktop https://gitlab.com    open a URL in its browser
ibeji pad                                the shared note
ibeji ps desktop / kill desktop <id>     processes you started over there
ibeji check desktop https://gitlab.com   whose network is the problem
ibeji install                            set up the daemon and the CLI
ibeji doctor                             diagnose a broken install
ibeji trace --errors                     what happened, and why it failed
ibeji surface --json                     the whole API, for an agent to read
```

## For agents

```sh
ibeji surface            # every call, with what it does
ibeji surface --json     # parameters, examples and error codes
ibeji-mcp --tools        # the MCP tool list, without needing a daemon
```

The MCP tool descriptions, `ibeji surface --json` and the human help all
generate from one definition in `internal/surface`, so they cannot drift — a
drifted tool description is worse than a missing one, because the agent trusts
it. Every call documents the failures it can return and what to do about each.

Register the MCP server with a Claude Code–compatible agent by pointing it at
`bin/ibeji-mcp`; it speaks stdio and needs no configuration beyond a running
daemon.

## Status

All four milestones built and tested. `make check` runs `go vet` and the full
suite under the race detector — 446 tests across 19 packages. `make e2e` runs
four suites against two real daemons: pairing and unpairing; remote exec and
the CLI; spaces, conflicts, the barrier and the agent surface; and the Electron
window itself, opened against a live paired daemon so all four of its tabs draw
real data. 45 checks, all passing.

Every bug worth remembering in this codebase was found by running the thing,
not by a unit test — two daemons displacing each other 46,251 times in fifteen
seconds, a sync that silently dropped same-second edits, a plist launchd
rejected without naming the key. Each one is now a test with a comment saying
what would be lost if it failed.

## License

MIT. See [LICENSE](LICENSE).

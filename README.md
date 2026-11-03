# Ìbejì

A LAN-only companion app that links **exactly two** of your own computers into a
trusted pair: shared folders, remote port tunnels, file drop, and a CLI / HTTP
API / MCP surface so both you and your AI agents can work across machines.

*Ìbejì* (ee-BAY-jee) is Yoruba for **twins** — two machines acting as one pair.

Nothing here touches the internet. No accounts, no cloud, no relay, no web app.
If the two machines are not on the same LAN, Ìbejì does nothing. It also never
shares, bridges or forwards a VPN — the VPN stays on one machine and what
crosses the LAN is a command, a tunnel or a URL.

## Documents

| | |
|---|---|
| [`docs/prd.md`](docs/prd.md) | What the product is. Nine modules, the permission model, the deferred list |
| [`docs/build-plan.html`](docs/build-plan.html) | How it gets built. Module by module, four milestones, drawn in the product's own design system |

The design system lives outside this repo at
`dockito/design-system/projects/ibeji` — 113 specimens, frozen as built. The app
vendors its two stylesheets via `app/scripts/sync-design-system.sh`.

## Layout

One repo, one version: a mismatch between the app and the API it consumes is
impossible by construction.

```
cmd/ibeji-daemon    the daemon — identity, link, permissions, trace, local API
cmd/ibeji           the CLI — a thin client over that API
cmd/ibeji-mcp       the MCP server — the same API, for agents
internal/           the daemon's packages
app/                the Electron window — also a client of that API
docs/               the PRD and the build plan
```

## Building

```sh
make build      # daemon, CLI and MCP server into bin/
make test       # the Go suite, with the race detector
make check      # what CI runs: gofmt, vet, race tests
cd app && npm install && npm run smoke   # the window, with its assertions
```

`make help` lists every target.

## Status

Milestone 1 in progress. See the build plan for what each milestone means and
what "done" is defined as.

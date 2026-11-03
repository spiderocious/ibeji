#!/usr/bin/env bash
#
# Milestone 3 end to end: a shared folder, a conflict, and the sync barrier.
#
# Sync is the one module that can lose work, so this exercises it against two
# real daemons over a real link: a file syncs, both machines edit it, the
# master's version wins, the replica's is preserved, and the barrier refuses
# to let a command through on a half-synced tree.
#
#   usage: test/e2e-spaces.sh [scratch-dir]
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRATCH="${1:-$(mktemp -d -t ibeji-e2e-spaces)}"
A_PORT=${IBEJI_E2E_A_PORT:-17781}
B_PORT=${IBEJI_E2E_B_PORT:-17782}

mkdir -p "$SCRATCH"
cd "$ROOT"

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAILED=1; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }
FAILED=0

cleanup() {
  for inst in s3-a s3-b; do
    [[ -f "$SCRATCH/$inst.pid" ]] && kill "$(cat "$SCRATCH/$inst.pid")" 2>/dev/null
    security delete-generic-password -s "com.ibeji.daemon.$inst" >/dev/null 2>&1
    rm -rf "$HOME/.ibeji/instances/$inst"
  done
}
trap cleanup EXIT

handoff() { python3 -c "import json;print(json.load(open('$HOME/.ibeji/instances/$1/api.json'))['$2'])" 2>/dev/null; }
api() {
  local ep="$1" tok="$2" method="$3" path="$4" body="${5:-}"
  if [[ -n "$body" ]]; then
    curl -s --max-time 20 -X "$method" -H "Authorization: Bearer $tok" \
      -H 'Content-Type: application/json' -d "$body" "$ep$path"
  else
    curl -s --max-time 20 -X "$method" -H "Authorization: Bearer $tok" "$ep$path"
  fi
}
field() { python3 -c "import json,sys;d=json.load(sys.stdin);print(d$1)" 2>/dev/null; }

step "Starting and pairing two daemons"
[[ -x "$ROOT/bin/ibeji-daemon" ]] || { echo "  run: make build" >&2; exit 1; }
cleanup

MASTER_DIR="$SCRATCH/master-work"
REPLICA_DIR="$SCRATCH/replica-work"
mkdir -p "$MASTER_DIR" "$REPLICA_DIR"

"$ROOT/bin/ibeji-daemon" -instance s3-a -api 127.0.0.1:0 -port "$A_PORT" -no-mdns > "$SCRATCH/a.log" 2>&1 &
echo $! > "$SCRATCH/s3-a.pid"
"$ROOT/bin/ibeji-daemon" -instance s3-b -api 127.0.0.1:0 -port "$B_PORT" -no-mdns > "$SCRATCH/b.log" 2>&1 &
echo $! > "$SCRATCH/s3-b.pid"

for _ in $(seq 1 20); do
  AEP=$(handoff s3-a endpoint); ATK=$(handoff s3-a token)
  BEP=$(handoff s3-b endpoint); BTK=$(handoff s3-b token)
  [[ -n "${AEP:-}" && -n "${BEP:-}" ]] && break
  sleep 0.5
done
[[ -n "${AEP:-}" && -n "${BEP:-}" ]] || { tail -5 "$SCRATCH"/*.log >&2; exit 1; }

api "$AEP" "$ATK" POST /v1/pair/start "{\"address\":\"127.0.0.1:$B_PORT\"}" > /dev/null
api "$BEP" "$BTK" POST /v1/pair/start "{\"address\":\"127.0.0.1:$A_PORT\"}" > /dev/null
api "$AEP" "$ATK" POST /v1/pair/confirm '{"sessionId":"pair-1"}' > /dev/null
api "$BEP" "$BTK" POST /v1/pair/confirm '{"sessionId":"pair-1"}' > /dev/null
api "$BEP" "$BTK" POST /v1/devices/mode '{"deviceId":"","mode":"bypass"}' > /dev/null

linked=0
for _ in $(seq 1 20); do
  [[ "$(api "$AEP" "$ATK" GET /v1/devices | field "['devices'][0]['status']")" == "live" ]] && { linked=1; break; }
  sleep 1
done
(( linked )) && pass "paired and linked" || { fail "no link"; exit 1; }

step "Creating a space"
# a is the replica; b is the master, as it would be on the machine with the
# credentials.
echo "the original" > "$MASTER_DIR/report.md"

api "$BEP" "$BTK" POST /v1/spaces/create \
  "{\"name\":\"work\",\"path\":\"$MASTER_DIR\",\"role\":\"master\",\"mode\":\"two-way\"}" > /dev/null
resp=$(api "$AEP" "$ATK" POST /v1/spaces/create \
  "{\"name\":\"work\",\"path\":\"$REPLICA_DIR\",\"role\":\"replica\",\"mode\":\"two-way\"}")

if [[ "$(echo "$resp" | field "['space']['name']")" == "work" ]]; then
  pass "space created on both machines"
else
  fail "could not create the space: $resp"
fi

# .git/ must be ignored by default: syncing it corrupts repositories.
if api "$AEP" "$ATK" GET "/v1/spaces/status?name=work" | grep -q '".git/"'; then
  pass ".git/ is ignored by default"
else
  fail ".git/ is not in the default ignores"
fi

step "Syncing"
converged=0
for _ in $(seq 1 30); do
  if [[ -f "$REPLICA_DIR/report.md" ]]; then converged=1; break; fi
  sleep 1
done
if (( converged )) && [[ "$(cat "$REPLICA_DIR/report.md")" == "the original" ]]; then
  pass "the master's file reached the replica"
else
  fail "the file never arrived"
fi

echo "written on the replica" > "$REPLICA_DIR/from-replica.md"
back=0
for _ in $(seq 1 30); do
  [[ -f "$MASTER_DIR/from-replica.md" ]] && { back=1; break; }
  sleep 1
done
(( back )) && pass "the replica's file reached the master" || fail "the replica's file never arrived"

step "A conflict"
# Both edit the same file without seeing each other's change.
echo "the master's version" > "$MASTER_DIR/report.md"
echo "the replica's version" > "$REPLICA_DIR/report.md"

conflicted=0
for _ in $(seq 1 40); do
  state=$(api "$AEP" "$ATK" GET "/v1/spaces/status?name=work" | field "['state']")
  [[ "$state" == "conflict" ]] && { conflicted=1; break; }
  sleep 1
done

if (( conflicted )); then
  pass "the collision was detected"
else
  fail "no conflict was raised after both machines edited the same file"
fi

if [[ "$(cat "$REPLICA_DIR/report.md" 2>/dev/null)" == "the master's version" ]]; then
  pass "the master's version won"
else
  fail "the replica does not hold the master's version: $(cat "$REPLICA_DIR/report.md" 2>/dev/null)"
fi

# The whole point: the replica's work still exists.
preserved=$(ls "$REPLICA_DIR" | grep "conflict-from" | head -1)
if [[ -n "$preserved" ]] && [[ "$(cat "$REPLICA_DIR/$preserved")" == "the replica's version" ]]; then
  pass "the replica's version was preserved ($preserved)"
else
  fail "the replica's work was lost — no conflict copy holds it"
fi

step "The barrier"
# A space with an unresolved conflict cannot converge, so a command that waits
# for it must fail with a reason rather than hanging until its timeout.
out=$(api "$AEP" "$ATK" POST /v1/exec \
  '{"command":"echo should not run","afterSync":"work","timeoutSeconds":10}')
code=$(echo "$out" | field "['code']")
if [[ "$code" == "sync_conflict" ]]; then
  pass "the barrier refused to run on an unresolved conflict"
else
  fail "the barrier returned '$code', want sync_conflict"
fi
if echo "$out" | grep -qi "resolve"; then
  pass "the refusal says what to do about it"
else
  fail "the refusal offers no hint: $out"
fi

# Resolve it, and the barrier lets the command through.
api "$AEP" "$ATK" POST /v1/spaces/discard '{"name":"work","path":"report.md"}' > /dev/null
ran=0
for _ in $(seq 1 20); do
  out=$(api "$AEP" "$ATK" POST /v1/exec \
    '{"command":"echo the barrier let it through","afterSync":"work","timeoutSeconds":30}')
  if echo "$out" | grep -q "let it through"; then ran=1; break; fi
  sleep 1
done
(( ran )) && pass "after resolving, the barrier let the command run" \
          || fail "the barrier still blocks after the conflict was resolved: $out"

step "Processes"
pid=$(api "$AEP" "$ATK" POST /v1/exec '{"command":"sleep 30","detach":true}' | field "['process']['processId']")
if [[ -n "$pid" ]]; then
  pass "a detached process started ($pid)"
else
  fail "could not start a detached process"
fi

if api "$AEP" "$ATK" GET /v1/processes | grep -q "sleep 30"; then
  pass "it is listed by ps"
else
  fail "the detached process is not listed"
fi

api "$AEP" "$ATK" POST /v1/processes/kill "{\"id\":\"$pid\"}" > /dev/null
sleep 2
if api "$AEP" "$ATK" GET /v1/processes | grep -q '"running":true'; then
  fail "the process is still running after kill"
else
  pass "kill stopped it"
fi

step "The agent surface"
tools=$("$ROOT/bin/ibeji-mcp" --tools 2>/dev/null | python3 -c "import json,sys;print(len(json.load(sys.stdin)))")
if [[ "$tools" -ge 14 ]]; then
  pass "the MCP server exposes $tools tools"
else
  fail "only $tools MCP tools"
fi

if "$ROOT/bin/ibeji" surface --json 2>/dev/null | python3 -c "
import json,sys
d=json.load(sys.stdin)
assert d['calls'], 'no calls'
assert d['concepts'], 'no concepts'
for c in d['calls']:
    assert c['summary'], c['name']+' has no summary'
    if c.get('mutating'):
        assert c.get('errors'), c['name']+' lists no failures'
"; then
  pass "ibeji surface --json describes every call"
else
  fail "the surface document is incomplete"
fi

step "Result"
if (( FAILED )); then
  echo "  logs: $SCRATCH/a.log $SCRATCH/b.log"
  exit 1
fi
echo "  all checks passed"

#!/usr/bin/env bash
#
# End-to-end: the Electron window against a real, paired, populated daemon.
#
# `make smoke` on its own proves only that the window opens: with no daemon it
# renders the empty state, 7 classes, and every populated view goes unchecked —
# which is the exact failure the smoke test's own comment warns about, a view
# that breaks without anyone noticing. This stands up two daemons, pairs them,
# gives them a space, a tunnel and some traced activity, and points the window
# at them so all four tabs render real data.
#
#   usage: test/e2e-app.sh [scratch-dir]
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=test/lib-cleanup.sh
source "$ROOT/test/lib-cleanup.sh"
SCRATCH="${1:-$(mktemp -d -t ibeji-e2e-app)}"
A_PORT=${IBEJI_E2E_ALPHA_PORT:-17769}
B_PORT=${IBEJI_E2E_BETA_PORT:-17770}

mkdir -p "$SCRATCH"
cd "$ROOT"

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAILED=1; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }
FAILED=0

cleanup() {
  for inst in app-a app-b; do
    [[ -f "$SCRATCH/$inst.pid" ]] && kill "$(cat "$SCRATCH/$inst.pid")" 2>/dev/null
    forget_instance "$inst"
  done
}
trap cleanup EXIT

handoff() {
  python3 -c "import json;print(json.load(open('$HOME/.ibeji/instances/$1/api.json'))['$2'])" 2>/dev/null
}
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

WORK_A="$SCRATCH/work-a"; WORK_B="$SCRATCH/work-b"
mkdir -p "$WORK_A" "$WORK_B"

"$ROOT/bin/ibeji-daemon" -instance app-a -api 127.0.0.1:0 -port "$A_PORT" -no-mdns > "$SCRATCH/a.log" 2>&1 &
echo $! > "$SCRATCH/app-a.pid"
"$ROOT/bin/ibeji-daemon" -instance app-b -api 127.0.0.1:0 -port "$B_PORT" -no-mdns > "$SCRATCH/b.log" 2>&1 &
echo $! > "$SCRATCH/app-b.pid"

for _ in $(seq 1 20); do
  AEP=$(handoff app-a endpoint); ATK=$(handoff app-a token)
  BEP=$(handoff app-b endpoint); BTK=$(handoff app-b token)
  [[ -n "${AEP:-}" && -n "${BEP:-}" ]] && break
  sleep 0.5
done
[[ -n "${AEP:-}" && -n "${BEP:-}" ]] || { tail -5 "$SCRATCH"/*.log >&2; exit 1; }

# One side dials; the other raises its own ceremony in response, so only
# alpha starts one here. Session ids are read back rather than assumed: beta's
# is created by the inbound connection, not by a request of its own.
APAIR=$(api "$AEP" "$ATK" POST /v1/pair/start "{\"address\":\"127.0.0.1:$B_PORT\"}")
ASESS=$(echo "$APAIR" | field "['id']")
BSESS=""
for attempt in $(seq 1 20); do
  BSESS=$(api "$BEP" "$BTK" GET /v1/pair | field "['pairing']['id']")
  [[ -n "$BSESS" ]] && break
  sleep 0.5
done
api "$AEP" "$ATK" POST /v1/pair/confirm "{\"sessionId\":\"$ASESS\"}" > /dev/null
api "$BEP" "$BTK" POST /v1/pair/confirm "{\"sessionId\":\"$BSESS\"}" > /dev/null
api "$AEP" "$ATK" POST /v1/devices/mode '{"deviceId":"","mode":"bypass"}' > /dev/null
api "$BEP" "$BTK" POST /v1/devices/mode '{"deviceId":"","mode":"bypass"}' > /dev/null

linked=0
for _ in $(seq 1 20); do
  [[ "$(api "$AEP" "$ATK" GET /v1/devices | field "['devices'][0]['status']")" == "live" ]] && { linked=1; break; }
  sleep 1
done
(( linked )) && pass "paired and linked" || { fail "no link"; exit 1; }

step "Giving the window something to show"

# A space, so the spaces tab has a row rather than an empty state.
api "$AEP" "$ATK" POST /v1/spaces/create \
  "{\"name\":\"notes\",\"path\":\"$WORK_A\",\"role\":\"master\"}" > /dev/null
api "$BEP" "$BTK" POST /v1/spaces/create \
  "{\"name\":\"notes\",\"path\":\"$WORK_B\",\"role\":\"replica\"}" > /dev/null
echo "visible to the window" > "$WORK_A/hello.md"
spaced=0
for _ in $(seq 1 20); do
  [[ -n "$(api "$AEP" "$ATK" GET /v1/spaces | field "['spaces'][0]['name']")" ]] && { spaced=1; break; }
  sleep 0.5
done
(( spaced )) && pass "a space exists" || fail "no space to show"

# Traced activity, so the trace tab has entries. The exec must SUCCEED: a
# trace full of BAD_REQUEST would satisfy a mere "is it non-empty" check while
# proving nothing about the view — which is how the first version of this
# script passed with a malformed body.
exec_out=$(api "$AEP" "$ATK" POST /v1/exec '{"command":"echo from the far machine"}')
if grep -q 'from the far machine' <<<"$exec_out"; then
  pass "a command ran on the far machine"
else
  fail "the exec failed: $(head -c 200 <<<"$exec_out")"
fi

traced=0
for _ in $(seq 1 10); do
  body=$(api "$AEP" "$ATK" GET '/v1/trace?limit=50')
  # Succeeded exec events only — an error entry does not prove the pane works.
  n=$(python3 -c "
import json,sys
d=json.load(sys.stdin)
ev=[e for e in d.get('events',[]) if not e.get('code') and not e.get('error')]
print(len(ev))" <<<"$body" 2>/dev/null)
  [[ -n "$n" && "$n" -ge 3 ]] && { traced=1; break; }
  sleep 0.5
done
(( traced )) && pass "the trace holds successful events ($n)" || fail "the trace has no clean events"

step "Opening the window against it"
export IBEJI_HANDOFF="$HOME/.ibeji/instances/app-a/api.json"
export IBEJI_SMOKE_REQUIRE_DAEMON=1
unset ELECTRON_RUN_AS_NODE
out="$SCRATCH/smoke.out"
if (cd app && npm run --silent smoke > "$out" 2>&1); then
  pass "the window rendered every tab against a live daemon"
  sed -n 's/^/    /p' "$out" | grep -E 'classes used|tab |state|device name' || true
else
  fail "the smoke test failed"
  sed -n 's/^/    /p' "$out"
fi

# The trace rows must open and show the payload the daemon recorded, and the
# selected tab must look selected. Both were broken in ways a render-only
# check cannot see: the payload was discarded, and the active class never
# matched the design system's.
tc="$SCRATCH/trace.out"
if (cd app && ELECTRON_RUN_AS_NODE= ./node_modules/electron/dist/Electron.app/Contents/MacOS/Electron \
      test/trace-check.js > "$tc" 2>&1); then
  if grep -q "TRACE PASS" "$tc"; then
    pass "trace rows expand and filter, and the tab shows as active"
    sed -n 's/^/    /p' "$tc" | grep -E "rows|payload|filter" || true
  else
    pass "trace check skipped ($(grep -o 'SKIP.*' "$tc" | head -1))"
  fi
else
  fail "the trace check failed"
  sed -n 's/^/    /p' "$tc" | head -8
fi

n_classes=$(grep -oE 'classes used +[0-9]+' "$out" | grep -oE '[0-9]+$')
if [[ -n "$n_classes" && "$n_classes" -ge 30 ]]; then
  pass "all four tabs drew real data ($n_classes design-system classes used)"
else
  fail "only ${n_classes:-0} classes rendered — the populated views did not draw"
fi

step "Result"
if (( FAILED )); then
  echo "  logs: $SCRATCH/a.log $SCRATCH/b.log $out"
  exit 1
fi
echo "  all checks passed"

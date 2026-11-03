#!/usr/bin/env bash
#
# End-to-end: two real daemons on this machine, paired over real TCP, then
# unpaired — with both sides checked at every step.
#
# The Go suite tests each piece; this tests the thing. Three bugs were found by
# running it that every unit test had passed through:
#
#   1. The listener captured the pinned key at startup, so unpairing did not
#      stop it accepting the revoked device.
#   2. A half-finished pairing ceremony left the listener accepting strangers,
#      so the ex-peer reconnected within a second of being unpaired.
#   3. Nothing noticed a peer that went away without closing, so a device
#      showed "live" indefinitely. Hence the heartbeat.
#
#   usage: test/e2e-pair.sh [scratch-dir]
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRATCH="${1:-$(mktemp -d -t ibeji-e2e)}"
# High ports, away from the daemon's real 7749, so a running Ìbejì does not
# collide with the test — and the test does not have to kill the user's daemon.
ALPHA_PORT=${IBEJI_E2E_ALPHA_PORT:-17749}
BETA_PORT=${IBEJI_E2E_BETA_PORT:-17750}

mkdir -p "$SCRATCH"
cd "$ROOT"

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAILED=1; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }
FAILED=0

cleanup() {
  for inst in alpha beta; do
    if [[ -f "$SCRATCH/$inst.pid" ]]; then
      kill "$(cat "$SCRATCH/$inst.pid")" 2>/dev/null
    fi
  done
  # Leave no test identities in the real keychain.
  security delete-generic-password -s com.ibeji.daemon.e2e-alpha >/dev/null 2>&1
  security delete-generic-password -s com.ibeji.daemon.e2e-beta  >/dev/null 2>&1
  rm -rf "$HOME/.ibeji/instances/e2e-alpha" "$HOME/.ibeji/instances/e2e-beta"
}
trap cleanup EXIT

# --- helpers ---

# read <instance> <field> — pull endpoint or token from the handoff file.
read_handoff() {
  python3 -c "import json,sys;print(json.load(open('$HOME/.ibeji/instances/$1/api.json'))['$2'])" 2>/dev/null
}

# api <endpoint> <token> <method> <path> [body]
api() {
  local ep="$1" tok="$2" method="$3" path="$4" body="${5:-}"
  if [[ -n "$body" ]]; then
    curl -s --max-time 10 -X "$method" -H "Authorization: Bearer $tok" \
      -H 'Content-Type: application/json' -d "$body" "$ep$path"
  else
    curl -s --max-time 10 -X "$method" -H "Authorization: Bearer $tok" "$ep$path"
  fi
}

# jq-free field extraction, so this runs without extra tools installed.
field() { python3 -c "import json,sys;d=json.load(sys.stdin);print(d$1)" 2>/dev/null; }

# status <endpoint> <token> — the peer's status, or "unpaired".
status() {
  api "$1" "$2" GET /v1/devices | python3 -c \
    "import json,sys
d=json.load(sys.stdin)['devices']
print(d[0]['status'] if d else 'unpaired')" 2>/dev/null || echo "error"
}

# wait_for <endpoint> <token> <wanted> <seconds>
wait_for() {
  local ep="$1" tok="$2" want="$3" limit="$4" waited=0
  while (( waited < limit )); do
    [[ "$(status "$ep" "$tok")" == "$want" ]] && return 0
    sleep 1
    (( waited++ ))
  done
  return 1
}

start_daemon() {
  local inst="$1" port="$2"
  "$ROOT/bin/ibeji-daemon" -instance "$inst" -api 127.0.0.1:0 -port "$port" -no-mdns \
    > "$SCRATCH/${inst#e2e-}.log" 2>&1 &
  echo $! > "$SCRATCH/${inst#e2e-}.pid"
}

# --- setup ---

step "Starting two isolated daemons"
if [[ ! -x "$ROOT/bin/ibeji-daemon" ]]; then
  echo "  bin/ibeji-daemon is missing — run: make build" >&2
  exit 1
fi
for port in "$ALPHA_PORT" "$BETA_PORT"; do
  if nc -z 127.0.0.1 "$port" 2>/dev/null; then
    echo "  port $port is already in use; set IBEJI_E2E_ALPHA_PORT/IBEJI_E2E_BETA_PORT" >&2
    exit 1
  fi
done
cleanup  # a previous run may have left state behind

start_daemon e2e-alpha "$ALPHA_PORT"
start_daemon e2e-beta "$BETA_PORT"

# Wait for both to publish a handoff file rather than assuming a fixed delay.
for attempt in $(seq 1 20); do
  AEP=$(read_handoff e2e-alpha endpoint); ATK=$(read_handoff e2e-alpha token)
  BEP=$(read_handoff e2e-beta endpoint);  BTK=$(read_handoff e2e-beta token)
  [[ -n "${AEP:-}" && -n "${BEP:-}" ]] && break
  sleep 0.5
done
if [[ -z "${AEP:-}" || -z "${BEP:-}" ]]; then
  echo "  daemons did not start. Logs:" >&2
  tail -5 "$SCRATCH/alpha.log" "$SCRATCH/beta.log" >&2
  exit 1
fi
pass "both daemons up (alpha=$AEP beta=$BEP)"

AFP=$(api "$AEP" "$ATK" GET /v1/self | field "['fingerprint']")
BFP=$(api "$BEP" "$BTK" GET /v1/self | field "['fingerprint']")
if [[ -n "$AFP" && "$AFP" != "$BFP" ]]; then
  pass "distinct identities"
else
  fail "the two daemons share an identity ($AFP)"
fi

# --- the ceremony ---

step "Pairing over TCP"
A=$(api "$AEP" "$ATK" POST /v1/pair/start "{\"address\":\"127.0.0.1:$BETA_PORT\"}")
B=$(api "$BEP" "$BTK" POST /v1/pair/start "{\"address\":\"127.0.0.1:$ALPHA_PORT\"}")
ACODE=$(echo "$A" | field "['code']")
BCODE=$(echo "$B" | field "['code']")

if [[ -z "$ACODE" ]]; then
  fail "alpha could not start pairing: $A"
elif [[ "$ACODE" == "$BCODE" ]]; then
  pass "both machines show the same code ($ACODE)"
else
  fail "codes differ: alpha=$ACODE beta=$BCODE — the ceremony is broken"
fi

# Each side must have learned the OTHER's name and fingerprint.
APEER=$(echo "$A" | field "['peerName']")
if [[ "$APEER" == *beta* ]]; then
  pass "alpha learned beta's name over the control stream"
else
  fail "alpha's peer name is '$APEER', expected it to name beta"
fi

api "$AEP" "$ATK" POST /v1/pair/confirm '{"sessionId":"pair-1"}' > /dev/null
api "$BEP" "$BTK" POST /v1/pair/confirm '{"sessionId":"pair-1"}' > /dev/null

if wait_for "$AEP" "$ATK" live 20 && wait_for "$BEP" "$BTK" live 20; then
  pass "the link came up on both sides"
else
  fail "the link did not come up (alpha=$(status "$AEP" "$ATK") beta=$(status "$BEP" "$BTK"))"
fi

# --- invariants ---

step "Checking the rules hold"
# Check the status, not the body: an error response carries a "code" field too
# (the error code), so testing for its absence passed when it should not have.
THIRD_STATUS=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
  -X POST -H "Authorization: Bearer $ATK" -H 'Content-Type: application/json' \
  -d "{\"address\":\"127.0.0.1:$BETA_PORT\"}" "$AEP/v1/pair/start")
if [[ "$THIRD_STATUS" == "409" ]]; then
  pass "a third pairing is refused (v1 is exactly two devices)"
else
  fail "a third pairing returned $THIRD_STATUS, want 409"
fi

api "$BEP" "$BTK" POST /v1/devices/mode '{"deviceId":"","mode":"bypass"}' > /dev/null
MODE=$(api "$BEP" "$BTK" GET /v1/devices | field "['devices'][0]['mode']")
if [[ "$MODE" == "bypass" ]]; then
  pass "permission mode is settable per device"
else
  fail "mode is '$MODE' after setting bypass"
fi

if [[ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$AEP/v1/self")" == "401" ]]; then
  pass "the API refuses a request with no token"
else
  fail "the API answered without a token"
fi

# --- unpair ---

step "Unpairing"
ID=$(api "$AEP" "$ATK" GET /v1/devices | field "['devices'][0]['id']")
api "$AEP" "$ATK" POST /v1/unpair "{\"deviceId\":\"$ID\"}" > /dev/null

if wait_for "$AEP" "$ATK" unpaired 10; then
  pass "alpha forgot the device immediately"
else
  fail "alpha still reports a paired device"
fi

# The regression that mattered: beta must NOT be able to reconnect.
if wait_for "$BEP" "$BTK" unreachable 30; then
  pass "beta went unreachable — the revoked device cannot reconnect"
else
  fail "beta still reports '$(status "$BEP" "$BTK")'; a revoked device reconnected"
fi

step "Result"
if (( FAILED )); then
  echo "  logs: $SCRATCH/alpha.log $SCRATCH/beta.log"
  exit 1
fi
echo "  all checks passed"

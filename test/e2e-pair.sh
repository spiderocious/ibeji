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
# shellcheck source=test/lib-cleanup.sh
source "$ROOT/test/lib-cleanup.sh"
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
  forget_instance e2e-alpha
  forget_instance e2e-beta
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
# Only ONE side is told to pair, because that is all a user does: they type an
# IP (or click Pair) on one machine and walk to the other. Driving both sides
# here is what hid the v0.1.3 bug — the daemon that was dialled never raised a
# ceremony of its own, so it showed nothing, and a test that started one for it
# could not tell. Beta's code must come from beta reacting to alpha.
A=$(api "$AEP" "$ATK" POST /v1/pair/start "{\"address\":\"127.0.0.1:$BETA_PORT\"}")
ACODE=$(echo "$A" | field "['code']")

if [[ -z "$ACODE" ]]; then
  fail "alpha could not start pairing: $A"
fi

# Poll beta's pending ceremony — the window does exactly this on a timer.
BCODE=""
for attempt in $(seq 1 20); do
  BCODE=$(api "$BEP" "$BTK" GET /v1/pair | field "['pairing']['code']")
  [[ -n "$BCODE" ]] && break
  sleep 0.5
done

if [[ -z "$BCODE" ]]; then
  fail "beta shows no code: the machine that was dialled raised no ceremony, so there is nothing to compare against"
elif [[ "$ACODE" == "$BCODE" ]]; then
  pass "both machines show the same code ($ACODE), beta without being asked"
else
  fail "codes differ: alpha=$ACODE beta=$BCODE — the ceremony is broken"
fi

# Beta must have learned alpha's name too, not just a bare address.
BPEER=$(api "$BEP" "$BTK" GET /v1/pair | field "['pairing']['peerName']")
if [[ "$BPEER" == *alpha* ]]; then
  pass "beta learned alpha's name without being told to pair"
else
  fail "beta's peer name is '$BPEER', expected it to name alpha"
fi

# Each side must have learned the OTHER's name and fingerprint.
APEER=$(echo "$A" | field "['peerName']")
if [[ "$APEER" == *beta* ]]; then
  pass "alpha learned beta's name over the control stream"
else
  fail "alpha's peer name is '$APEER', expected it to name beta"
fi

ASESS=$(echo "$A" | field "['id']")
BSESS=$(api "$BEP" "$BTK" GET /v1/pair | field "['pairing']['id']")
api "$AEP" "$ATK" POST /v1/pair/confirm "{\"sessionId\":\"$ASESS\"}" > /dev/null
api "$BEP" "$BTK" POST /v1/pair/confirm "{\"sessionId\":\"$BSESS\"}" > /dev/null

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

# The link must be stable, not merely established. Both sides dialling at once
# made each connection displace the previous one — 46,000 drops in fifteen
# seconds, and a pegged core on both machines.
drops() { grep -c "link dropped" "$SCRATCH/alpha.log" 2>/dev/null | head -1; }
DROPS_BEFORE=$(drops); DROPS_BEFORE=${DROPS_BEFORE:-0}
sleep 8
DROPS_AFTER=$(drops); DROPS_AFTER=${DROPS_AFTER:-0}
CHURN=$(( DROPS_AFTER - DROPS_BEFORE ))
if (( CHURN <= 1 )); then
  pass "the link is stable ($CHURN drops in 8s)"
else
  fail "the link churned $CHURN times in 8s; both sides are probably dialling"
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

step "Diagnosing a failure that cannot be reached"

# The thing this covers is not a crash — it is silence. A failed pairing used
# to produce one sentence: "the paired device is not reachable". No step, no
# error, nothing to copy. That silence let a stale IP address look like a macOS
# permission problem for over a day, because nothing in the product ever said
# "the address you are dialling may have changed".
#
# So dial an address in the documentation-only range (RFC 5737), which is
# guaranteed not to route, and assert the daemon writes down WHERE it broke and
# WHY — not merely that it did.
#
# Checked on beta, which is unpaired by this point in the run. Alpha is paired,
# and a paired daemon refuses to pair again, which would exercise a different
# path entirely.
DEAD_ADDR="192.0.2.123:7749"

# A paired daemon refuses to pair again, and that refusal is a real failure
# with its own step. Whether beta still holds the pairing here depends on how
# quickly it notices alpha's revocation, so check it only when it does rather
# than racing that timing.
if [[ "$(status "$BEP" "$BTK")" != "unpaired" ]]; then
  api "$BEP" "$BTK" POST /v1/pair/start "{\"address\":\"$DEAD_ADDR\"}" >/dev/null 2>&1
  WHILE_PAIRED="$(api "$BEP" "$BTK" GET /v1/pair/attempt | field "['attempt']['failed']")"
  if [[ "$WHILE_PAIRED" == "ready" ]]; then
    pass "pairing while already paired fails at its own step, not as a bad address"
  else
    fail "expected the ready step to catch an already-paired daemon, got '$WHILE_PAIRED'"
  fi
  BDEV=$(api "$BEP" "$BTK" GET /v1/devices | field "['devices'][0]['id']")
  api "$BEP" "$BTK" POST /v1/unpair "{\"deviceId\":\"$BDEV\"}" >/dev/null 2>&1
fi

api "$BEP" "$BTK" POST /v1/pair/start "{\"address\":\"$DEAD_ADDR\"}" >/dev/null 2>&1

ATTEMPT="$(api "$BEP" "$BTK" GET /v1/pair/attempt)"

# Which step broke. "connect" is the answer for an unroutable address: the
# daemon got as far as opening a socket and nothing answered.
FAILED_STEP="$(printf '%s' "$ATTEMPT" | field "['attempt']['failed']")"
if [[ "$FAILED_STEP" == "connect" ]]; then
  pass "the daemon records which step failed (connect)"
else
  fail "expected the connect step to be flagged, got '$FAILED_STEP'"
fi

# Every step is present, including the ones never reached — a timeline that
# only lists what happened cannot show how far there was left to go.
STEP_COUNT="$(printf '%s' "$ATTEMPT" | field "['attempt']['steps']" | python3 -c "import sys;print(sys.stdin.read().count('key'))" 2>/dev/null || echo 0)"
STEP_N="$(printf '%s' "$ATTEMPT" | python3 -c "import json,sys;print(len(json.load(sys.stdin)['attempt']['steps']))" 2>/dev/null || echo 0)"
if (( STEP_N >= 7 )); then
  pass "the whole ceremony is recorded, not just the step that broke ($STEP_N steps)"
else
  fail "expected at least 7 steps, got $STEP_N"
fi

# The steps after the failure must say they were never attempted. Leaving them
# "pending" makes a timeline that looks like it is still working.
SKIPPED="$(printf '%s' "$ATTEMPT" | python3 -c "
import json,sys
st=json.load(sys.stdin)['attempt']['steps']
print(sum(1 for s in st if s['state']=='skipped'))" 2>/dev/null || echo 0)"
if (( SKIPPED >= 1 )); then
  pass "steps after the failure are marked never-attempted ($SKIPPED skipped)"
else
  fail "expected the steps after the failure to be skipped, got $SKIPPED"
fi

# The cause, in plain words. This is the part that was missing, so assert it
# says something substantial rather than merely existing.
CAUSE="$(printf '%s' "$ATTEMPT" | python3 -c "
import json,sys
st=json.load(sys.stdin)['attempt']['steps']
print(next((s.get('cause','') for s in st if s['state']=='failed'), ''))" 2>/dev/null)"
if (( ${#CAUSE} > 24 )); then
  pass "the failure carries a cause in plain words"
else
  fail "the failing step has no usable cause: '$CAUSE'"
fi

# And a fix, and the offer to sweep — a timeout is the one failure the daemon
# can resolve by itself, and not offering that is what cost the day.
FIX="$(printf '%s' "$ATTEMPT" | python3 -c "
import json,sys
st=json.load(sys.stdin)['attempt']['steps']
print(next((s.get('fix','') for s in st if s['state']=='failed'), ''))" 2>/dev/null)"
SCAN="$(printf '%s' "$ATTEMPT" | python3 -c "
import json,sys
st=json.load(sys.stdin)['attempt']['steps']
print(next((str(s.get('scan',False)) for s in st if s['state']=='failed'), 'False'))" 2>/dev/null)"
if (( ${#FIX} > 16 )); then
  pass "the failure carries a concrete next move"
else
  fail "the failing step has no usable fix: '$FIX'"
fi
if [[ "$SCAN" == "True" ]]; then
  pass "a timeout offers the subnet sweep, since a moved peer is the likeliest cause"
else
  fail "a timeout should offer the sweep, got scan=$SCAN"
fi

# The attempt's own log, which is what the Copy button hands over.
LOGLEN="$(printf '%s' "$ATTEMPT" | python3 -c "
import json,sys
print(len(json.load(sys.stdin)['attempt']['log']))" 2>/dev/null || echo 0)"
if (( LOGLEN >= 3 )); then
  pass "the attempt keeps its own log ($LOGLEN lines)"
else
  fail "expected a log for this attempt, got $LOGLEN lines"
fi

# A malformed address must be named as such at once, rather than surfacing
# fifteen seconds later as a timeout.
api "$BEP" "$BTK" POST /v1/pair/start '{"address":"not an address at all"}' >/dev/null 2>&1
BAD="$(api "$BEP" "$BTK" GET /v1/pair/attempt | field "['attempt']['failed']")"
if [[ "$BAD" == "address" ]]; then
  pass "a malformed address fails at the address step, not as a timeout"
else
  fail "expected the address step to catch a bad address, got '$BAD'"
fi

# Pairing with this very machine.
#
# The sweep lists every daemon on the subnet, including this one, so a Pair
# button pointing at ourselves is one click away. Without a guard the daemon
# dialled itself, completed the handshake, and showed six digits to compare
# against the same six digits on the same screen. A screenshot of the sweep
# caught it; no assertion had.
SELF_ADDR="$(api "$BEP" "$BTK" GET /v1/self | field "['addresses'][0]")"
if [[ -n "$SELF_ADDR" ]]; then
  api "$BEP" "$BTK" POST /v1/pair/start "{\"address\":\"$SELF_ADDR:$BETA_PORT\"}" >/dev/null 2>&1
  SELF_FAIL="$(api "$BEP" "$BTK" GET /v1/pair/attempt | field "['attempt']['failed']")"
  SELF_CAUSE="$(api "$BEP" "$BTK" GET /v1/pair/attempt | python3 -c "
import json,sys
st=json.load(sys.stdin)['attempt']['steps']
print(next((s.get('cause','') for s in st if s['state']=='failed'), ''))" 2>/dev/null)"
  if [[ "$SELF_FAIL" == "ready" ]]; then
    pass "pairing with this Mac's own address is refused"
  else
    fail "a daemon paired with itself: failed step was '$SELF_FAIL'"
  fi
  if [[ "$SELF_CAUSE" == *"itself"* || "$SELF_CAUSE" == *"own address"* || "$SELF_CAUSE" == *"this Mac"* ]]; then
    pass "and it says so, rather than failing cryptically"
  else
    fail "the self-pair refusal is not explained: '$SELF_CAUSE'"
  fi
fi

step "Finding a peer that moved"

# The sweep: the answer to "where did my other Mac go?". Alpha is listening on
# a high test port, so sweeping beta's subnet on the default port will not
# find it — what is asserted here is that the sweep runs, reports what it
# covered, and says something useful about the result. Finding alpha would
# require both daemons on 7749, which collides with the user's real one.
SWEEP="$(api "$BEP" "$BTK" POST /v1/pair/sweep '{}')"
SCANNED="$(printf '%s' "$SWEEP" | field "['scanned']")"
SUBNET="$(printf '%s' "$SWEEP" | field "['subnet']")"
HINT="$(printf '%s' "$SWEEP" | field "['hint']")"
if [[ "$SCANNED" =~ ^[0-9]+$ ]] && (( SCANNED > 1 )); then
  pass "the sweep probes this Mac's subnet ($SCANNED addresses on $SUBNET)"
else
  fail "the sweep covered nothing: scanned=$SCANNED"
fi
if (( ${#HINT} > 20 )); then
  pass "the sweep says what to do with what it found"
else
  fail "the sweep gave no usable hint: '$HINT'"
fi

step "Result"
if (( FAILED )); then
  echo "  logs: $SCRATCH/alpha.log $SCRATCH/beta.log"
  exit 1
fi
echo "  all checks passed"

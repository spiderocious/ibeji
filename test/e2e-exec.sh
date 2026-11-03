#!/usr/bin/env bash
#
# Milestone 2 end to end: two real daemons, and the CLI doing the thing the
# product exists for — running a command on the other machine.
#
# `make check` tests each piece. This tests the lever: a command leaves this
# machine, runs over there in a login shell, and its output and exit code come
# back. Three bugs were found by running it that the unit tests passed through:
#
#   1. Only the accepting side served streams, so the dialer answered nothing
#      and even the heartbeat timed out, dropping the link every 20 seconds.
#   2. The request decoder read ahead past the JSON frame, swallowing the
#      first bytes of a file write's content — so every write hung forever.
#   3. Arguments after `--` were joined with spaces, so `sh -c "exit 42"`
#      became `sh -c exit 42`: a silent wrong answer rather than an error.
#
#   usage: test/e2e-exec.sh [scratch-dir]
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRATCH="${1:-$(mktemp -d -t ibeji-e2e-exec)}"
A_PORT=${IBEJI_E2E_A_PORT:-17771}
B_PORT=${IBEJI_E2E_B_PORT:-17772}

mkdir -p "$SCRATCH"
cd "$ROOT"

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAILED=1; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }
FAILED=0

cleanup() {
  for inst in x2-a x2-b; do
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
    curl -s --max-time 15 -X "$method" -H "Authorization: Bearer $tok" \
      -H 'Content-Type: application/json' -d "$body" "$ep$path"
  else
    curl -s --max-time 15 -X "$method" -H "Authorization: Bearer $tok" "$ep$path"
  fi
}

step "Starting and pairing two daemons"
if [[ ! -x "$ROOT/bin/ibeji-daemon" || ! -x "$ROOT/bin/ibeji" ]]; then
  echo "  bin/ missing — run: make build" >&2
  exit 1
fi
cleanup

"$ROOT/bin/ibeji-daemon" -instance x2-a -api 127.0.0.1:0 -port "$A_PORT" -no-mdns > "$SCRATCH/a.log" 2>&1 &
echo $! > "$SCRATCH/x2-a.pid"
"$ROOT/bin/ibeji-daemon" -instance x2-b -api 127.0.0.1:0 -port "$B_PORT" -no-mdns > "$SCRATCH/b.log" 2>&1 &
echo $! > "$SCRATCH/x2-b.pid"

for _ in $(seq 1 20); do
  AEP=$(handoff x2-a endpoint); ATK=$(handoff x2-a token)
  BEP=$(handoff x2-b endpoint); BTK=$(handoff x2-b token)
  [[ -n "${AEP:-}" && -n "${BEP:-}" ]] && break
  sleep 0.5
done
if [[ -z "${AEP:-}" || -z "${BEP:-}" ]]; then
  echo "  daemons did not start:" >&2; tail -5 "$SCRATCH/a.log" "$SCRATCH/b.log" >&2; exit 1
fi

api "$AEP" "$ATK" POST /v1/pair/start "{\"address\":\"127.0.0.1:$B_PORT\"}" > /dev/null
api "$BEP" "$BTK" POST /v1/pair/start "{\"address\":\"127.0.0.1:$A_PORT\"}" > /dev/null
api "$AEP" "$ATK" POST /v1/pair/confirm '{"sessionId":"pair-1"}' > /dev/null
api "$BEP" "$BTK" POST /v1/pair/confirm '{"sessionId":"pair-1"}' > /dev/null
# B runs commands for A without prompting, which is what Bypass is for
# between two machines the same person owns.
api "$BEP" "$BTK" POST /v1/devices/mode '{"deviceId":"","mode":"bypass"}' > /dev/null

linked=0
for _ in $(seq 1 20); do
  status=$(api "$AEP" "$ATK" GET /v1/devices | python3 -c "import json,sys;d=json.load(sys.stdin)['devices'];print(d[0]['status'] if d else '')" 2>/dev/null)
  [[ "$status" == "live" ]] && { linked=1; break; }
  sleep 1
done
(( linked )) && pass "paired and linked" || { fail "the link never came up"; exit 1; }

export IBEJI_INSTANCE=x2-a
IB="$ROOT/bin/ibeji"

step "Running a command on the other machine"
out=$("$IB" run x2-b -- echo hello-from-over-there 2>&1)
if [[ "$out" == *"hello-from-over-there"* ]]; then
  pass "output came back"
else
  fail "no output: $out"
fi

"$IB" run x2-b -- exit 42 >/dev/null 2>&1
code=$?
if [[ $code -eq 42 ]]; then
  pass "the remote exit code passes through ($code)"
else
  fail "exit code was $code, want 42 — `ibeji run … && deploy` would not mean what it says"
fi

# The bug that returned 0 for a failing command.
"$IB" run x2-b -- sh -c "exit 7" >/dev/null 2>&1
code=$?
if [[ $code -eq 7 ]]; then
  pass "quoted arguments survive (sh -c \"exit 7\" → $code)"
else
  fail "sh -c \"exit 7\" exited $code; argument boundaries were lost"
fi

err=$("$IB" run x2-b -- sh -c "echo to-stderr 1>&2" 2>&1 >/dev/null)
if [[ "$err" == *"to-stderr"* ]]; then
  pass "stderr stays on stderr"
else
  fail "stderr did not arrive separately: $err"
fi

step "Reading the other machine"
work="$SCRATCH/project/src"
mkdir -p "$work"
printf 'package main\n// TODO: handle this\nfunc main() {}\n' > "$work/main.go"

[[ "$("$IB" ls "x2-b:$SCRATCH/project" 2>&1)" == *src* ]] \
  && pass "ls" || fail "ls did not list the directory"

[[ "$("$IB" cat "x2-b:$work/main.go" 2>&1)" == *"package main"* ]] \
  && pass "cat" || fail "cat did not return the file"

[[ "$("$IB" grep "x2-b:$SCRATCH/project" TODO --glob '*.go' 2>&1)" == *"TODO: handle this"* ]] \
  && pass "grep" || fail "grep found nothing"

[[ "$("$IB" info x2-b 2>&1)" == *"shell"* ]] \
  && pass "info reports the far machine's toolchain" || fail "info returned nothing useful"

step "Diagnostics"
# A command must leave a trace: that is what an agent reads when something
# fails, and it has to be recorded from the first call rather than retrofitted.
if [[ "$("$IB" trace --since 5m -n 20 2>&1)" == *"api.exec"* ]]; then
  pass "the exec was traced"
else
  fail "nothing about the exec is in the trace"
fi

if [[ "$("$IB" trace --since 5m -n 20 2>&1)" == *"cli"* ]]; then
  pass "provenance distinguishes the CLI from an agent"
else
  fail "the trace does not record who asked"
fi

step "Permissions"
# Back to Prompt with nobody answering: the command must be refused, not run.
api "$BEP" "$BTK" POST /v1/devices/mode '{"deviceId":"","mode":"prompt"}' > /dev/null
marker="$SCRATCH/should-not-exist"
timeout_out=$("$IB" run x2-b -- touch "$marker" 2>&1)
code=$?
if [[ -e "$marker" ]]; then
  fail "an unapproved command ran anyway"
elif [[ $code -eq 124 ]]; then
  pass "an unanswered command times out and does not run (exit $code)"
else
  fail "expected exit 124 for a timeout, got $code: $timeout_out"
fi

step "Result"
if (( FAILED )); then
  echo "  logs: $SCRATCH/a.log $SCRATCH/b.log"
  exit 1
fi
echo "  all checks passed"

#!/usr/bin/env bash
#
# End-to-end: the Shell tab, against two real daemons over a real link.
#
# The point of a shell session is that state carries between commands. Exec
# cannot do it — each call spawns a fresh `zsh -lc`, so `cd` and `export` are
# forgotten the moment it returns — and a terminal that forgets where it is
# between lines is not a terminal. These checks are what distinguish the two.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=test/lib-cleanup.sh
source "$ROOT/test/lib-cleanup.sh"
SCRATCH="${1:-$(mktemp -d -t ibeji-e2e-shell)}"
A_PORT=${IBEJI_E2E_SH_A_PORT:-17841}
B_PORT=${IBEJI_E2E_SH_B_PORT:-17842}

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAILED=1; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }
FAILED=0

PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do [ -n "$pid" ] && kill "$pid" 2>/dev/null; done
  pkill -f "ibeji-daemon -instance sh-" 2>/dev/null
  for _ in $(seq 1 20); do pgrep -f "ibeji-daemon -instance sh-" >/dev/null || break; sleep 0.25; done
  for i in sh-a sh-b; do forget_instance "$i"; done
}
trap cleanup EXIT
cleanup

h(){ python3 -c "import json;print(json.load(open('$HOME/.ibeji/instances/$1/api.json'))['$2'])" 2>/dev/null; }
f(){ python3 -c "import json,sys;d=json.load(sys.stdin);print(d$1)" 2>/dev/null; }
api(){ local e="$1" t="$2" m="$3" p="$4" b="${5:-}"
  if [ -n "$b" ]; then curl -s --max-time 30 -X "$m" -H "Authorization: Bearer $t" -H 'Content-Type: application/json' -d "$b" "$e$p"
  else curl -s --max-time 30 -X "$m" -H "Authorization: Bearer $t" "$e$p"; fi; }
# read_shell <endpoint> <token> <id> <since> — ONE read, returning the text
# and the next sequence on separate lines.
#
# Reading twice to get the text and then the cursor consumes the stream: the
# first call takes the output and the second returns nothing, so an assertion
# on the text sees an empty window and reports a working feature as broken.
read_shell(){
  api "$1" "$2" POST /v1/shells/read "{\"id\":\"$3\",\"since\":$4,\"waitMs\":2500}" \
    | python3 -c "
import json,sys
d=json.load(sys.stdin)
print(''.join(o['data'] for o in d.get('output',[])).replace(chr(10), ' '))
print(d.get('seq', $4))
" 2>/dev/null
}

step "Starting and pairing two daemons"
"$ROOT/bin/ibeji-daemon" -instance sh-a -api 127.0.0.1:0 -port "$A_PORT" -no-mdns > "$SCRATCH/a.log" 2>&1 &
PIDS+=($!)
"$ROOT/bin/ibeji-daemon" -instance sh-b -api 127.0.0.1:0 -port "$B_PORT" -no-mdns > "$SCRATCH/b.log" 2>&1 &
PIDS+=($!)
sleep 4
AE=$(h sh-a endpoint); AT=$(h sh-a token); BE=$(h sh-b endpoint); BT=$(h sh-b token)
[ -z "${AE:-}" ] && { fail "the daemons did not start"; exit 1; }

P=$(api "$AE" "$AT" POST /v1/pair/start "{\"address\":\"127.0.0.1:$B_PORT\"}"); AS=$(echo "$P" | f "['id']")
for _ in $(seq 1 20); do BS=$(api "$BE" "$BT" GET /v1/pair | f "['pairing']['id']"); [ -n "$BS" ] && break; sleep 0.5; done
api "$AE" "$AT" POST /v1/pair/confirm "{\"sessionId\":\"$AS\"}" >/dev/null
api "$BE" "$BT" POST /v1/pair/confirm "{\"sessionId\":\"$BS\"}" >/dev/null
api "$BE" "$BT" POST /v1/devices/mode '{"deviceId":"","mode":"bypass"}' >/dev/null
linked=0
for _ in $(seq 1 20); do [ "$(api "$AE" "$AT" GET /v1/devices | f "['devices'][0]['status']")" = "live" ] && { linked=1; break; }; sleep 1; done
(( linked )) && pass "paired and linked" || { fail "the link never came up"; exit 1; }

step "Opening a shell on the other machine"
OPEN=$(api "$AE" "$AT" POST /v1/shells/open '{"cwd":""}')
ID=$(echo "$OPEN" | f "['shell']['id']")
SPID=$(echo "$OPEN" | f "['shell']['pid']")
if [ -n "$ID" ] && [ -n "$SPID" ]; then pass "a shell is running on the far machine (pid $SPID)"
else fail "could not open a shell: $(head -c 160 <<<"$OPEN")"; exit 1; fi
SEQ=$(tail -1 <<<"$(read_shell "$AE" "$AT" "$ID" 0)")

step "State carries between commands"
api "$AE" "$AT" POST /v1/shells/write "{\"id\":\"$ID\",\"data\":\"cd /tmp\"}" >/dev/null
api "$AE" "$AT" POST /v1/shells/write "{\"id\":\"$ID\",\"data\":\"pwd\"}" >/dev/null
R=$(read_shell "$AE" "$AT" "$ID" "$SEQ"); OUT=$(head -1 <<<"$R"); SEQ=$(tail -1 <<<"$R")
if grep -q "/tmp" <<<"$OUT"; then pass "cd persists to the next command"
else fail "cd did not persist: $(tr '\n' ' ' <<<"$OUT" | head -c 120)"; fi

api "$AE" "$AT" POST /v1/shells/write "{\"id\":\"$ID\",\"data\":\"export E2E_VAR=held\"}" >/dev/null
api "$AE" "$AT" POST /v1/shells/write "{\"id\":\"$ID\",\"data\":\"echo \\\"[\$E2E_VAR]\\\"\"}" >/dev/null
R=$(read_shell "$AE" "$AT" "$ID" "$SEQ"); OUT=$(head -1 <<<"$R"); SEQ=$(tail -1 <<<"$R")
if grep -q "\[held\]" <<<"$OUT"; then pass "an exported variable survives to the next command"
else fail "export did not persist: $(tr '\n' ' ' <<<"$OUT" | head -c 120)"; fi

step "Interrupting a long-running command"
api "$AE" "$AT" POST /v1/shells/write "{\"id\":\"$ID\",\"data\":\"echo running && sleep 30 && echo SHOULD-NOT-APPEAR\"}" >/dev/null
started=0
for _ in $(seq 1 20); do ps -o command= -g "$SPID" 2>/dev/null | grep -q "sleep 30" && { started=1; break; }; sleep 0.3; done
(( started )) || fail "the long-running command never started"
api "$AE" "$AT" POST /v1/shells/interrupt "{\"id\":\"$ID\"}" >/dev/null
stopped=0
for _ in $(seq 1 20); do ps -o command= -g "$SPID" 2>/dev/null | grep -q "sleep 30" || { stopped=1; break; }; sleep 0.3; done
(( stopped )) && pass "the interrupt stopped the running command" || fail "the command kept running after the interrupt"

api "$AE" "$AT" POST /v1/shells/write "{\"id\":\"$ID\",\"data\":\"echo alive-after-interrupt\"}" >/dev/null
R=$(read_shell "$AE" "$AT" "$ID" "$SEQ"); OUT=$(head -1 <<<"$R"); SEQ=$(tail -1 <<<"$R")
if grep -q "alive-after-interrupt" <<<"$OUT"; then pass "the shell survived its own interrupt"
else fail "the shell stopped responding after the interrupt"; fi
if grep -q "SHOULD-NOT-APPEAR" <<<"$OUT"; then fail "the interrupted chain ran on to completion"; fi

step "Several shells at once"
api "$AE" "$AT" POST /v1/shells/open '{"cwd":"/tmp"}' >/dev/null
N=$(api "$AE" "$AT" GET /v1/shells | python3 -c "import json,sys; print(len(json.load(sys.stdin)['shells']))" 2>/dev/null)
[ "$N" = "2" ] && pass "two shells run side by side" || fail "expected 2 shells, got ${N:-none}"

step "Closing a shell ends it"
api "$AE" "$AT" POST /v1/shells/close "{\"id\":\"$ID\"}" >/dev/null
gone=0
for _ in $(seq 1 20); do ps -p "$SPID" >/dev/null 2>&1 || { gone=1; break; }; sleep 0.3; done
(( gone )) && pass "the shell process is gone" || fail "the shell is still running after being closed"

step "Result"
if (( FAILED )); then echo "  logs: $SCRATCH/a.log $SCRATCH/b.log"; exit 1; fi
echo "  all checks passed"

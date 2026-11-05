#!/usr/bin/env bash
#
# End-to-end: does a pairing survive a daemon restart?
#
# It did not. Store.Pin wrote the pinned peer to a struct field and nothing
# else, so the pairing lived exactly as long as the process. Every unit test
# shared one long-lived store and passed; the product lost its pairing on the
# first restart, and it reached a user as "I suddenly cannot connect to my
# other Mac this morning".
#
# So this kills both daemons and starts them again, which is the one thing
# no other test did.
set -uo pipefail
ROOT=/Users/feranmi/codebases/2026/ibeji
# shellcheck source=test/lib-cleanup.sh
source "$ROOT/test/lib-cleanup.sh"
S=$(mktemp -d); A=17811; B=17812
# Kill by instance flag AND wait: this script starts a second pair of daemons
# after the first, and an earlier version's trap raced them — it returned
# while the restarted daemons were still coming up, leaving one orphaned on
# port 17812. The next script in `make e2e` then failed to pair, which looked
# like a product bug and was not.
cleanup(){
  for pid in "${PIDS[@]:-}"; do [ -n "$pid" ] && kill "$pid" 2>/dev/null; done
  pkill -f "ibeji-daemon -instance rst-" 2>/dev/null
  for _ in $(seq 1 20); do
    pgrep -f "ibeji-daemon -instance rst-" >/dev/null 2>&1 || break
    sleep 0.25
  done
  pkill -9 -f "ibeji-daemon -instance rst-" 2>/dev/null
  for i in rst-a rst-b; do
    forget_instance "$i"
  done
}
trap cleanup EXIT; cleanup
hand(){ python3 -c "import json;print(json.load(open('$HOME/.ibeji/instances/$1/api.json'))['$2'])" 2>/dev/null; }
f(){ python3 -c "import json,sys;d=json.load(sys.stdin);print(d$1)" 2>/dev/null; }
api(){ local e="$1" t="$2" m="$3" p="$4" b="${5:-}"; if [ -n "$b" ]; then curl -s -X "$m" -H "Authorization: Bearer $t" -H 'Content-Type: application/json' -d "$b" "$e$p"; else curl -s -X "$m" -H "Authorization: Bearer $t" "$e$p"; fi; }
PIDS=()
start(){
  "$ROOT/bin/ibeji-daemon" -instance "$1" -api 127.0.0.1:0 -port "$2" -no-mdns > "$S/$1.log" 2>&1 &
  PIDS+=($!)
}

start rst-a $A; start rst-b $B; sleep 4
AE=$(hand rst-a endpoint); AT=$(hand rst-a token); BE=$(hand rst-b endpoint); BT=$(hand rst-b token)
[ -z "$AE" ] && { echo "FAIL: daemons did not start"; exit 1; }

P=$(api "$AE" "$AT" POST /v1/pair/start "{\"address\":\"127.0.0.1:$B\"}")
AS=$(echo "$P" | f "['id']")
for _ in $(seq 1 20); do BS=$(api "$BE" "$BT" GET /v1/pair | f "['pairing']['id']"); [ -n "$BS" ] && break; sleep 0.5; done
api "$AE" "$AT" POST /v1/pair/confirm "{\"sessionId\":\"$AS\"}" >/dev/null
api "$BE" "$BT" POST /v1/pair/confirm "{\"sessionId\":\"$BS\"}" >/dev/null
sleep 2
echo "before restart: A paired=$(api "$AE" "$AT" GET /v1/self | f "['paired']")  B paired=$(api "$BE" "$BT" GET /v1/self | f "['paired']")"

echo "--- killing both daemons ---"
for pid in "${PIDS[@]}"; do kill "$pid" 2>/dev/null; done
for _ in $(seq 1 20); do pgrep -f "ibeji-daemon -instance rst-" >/dev/null || break; sleep 0.25; done
PIDS=()
start rst-a $A; start rst-b $B; sleep 5
AE=$(hand rst-a endpoint); AT=$(hand rst-a token); BE=$(hand rst-b endpoint); BT=$(hand rst-b token)
AP=$(api "$AE" "$AT" GET /v1/self | f "['paired']"); BP=$(api "$BE" "$BT" GET /v1/self | f "['paired']")
echo "after  restart: A paired=$AP  B paired=$BP"
echo "A sees: $(api "$AE" "$AT" GET /v1/devices | f "['devices'][0]['name']") at $(api "$AE" "$AT" GET /v1/devices | f "['devices'][0]['address']")"
echo "--- restored log line ---"; grep "pairing restored" "$S/rst-a.log" | tail -1
for _ in $(seq 1 15); do [ "$(api "$AE" "$AT" GET /v1/devices | f "['devices'][0]['status']")" = "live" ] && break; sleep 1; done
echo "link after restart: $(api "$AE" "$AT" GET /v1/devices | f "['devices'][0]['status']")"
[ "$AP" = "True" ] && [ "$BP" = "True" ] && echo "RESULT: PASS" || echo "RESULT: FAIL"

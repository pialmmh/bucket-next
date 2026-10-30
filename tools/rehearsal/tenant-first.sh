#!/bin/bash
#
# tenant-first.sh — rehearse the bed's move on this machine, without root: the bed's binary (OLD_REF, default fc779e7)
# writes a pre-tenant state like the bed's, then this tree's binary carries it over — killed with SIGKILL at random
# moments of its first start, again and again — and must never hand out an id twice.
#
#   tools/rehearsal/tenant-first.sh [--kills 40] [--port 7094]
#
#   1  the bed today: partner.btcl (int) registered at 2007 like the bed's, ids minted; tok (uuid12) and ev (snowflake) too
#   2  the carry-over under SIGKILL: N times, a copy of that state, the new binary started and killed at a random moment
#      0-150 ms in; then a clean start must serve partner.btcl above every old id and tok with no old string
#   3  after the move, one counter for both roads: 4 workers on v1 and 4 on v2, partner.btcl, every id distinct
#
# Exit 0 = every check held. Needs: go, curl, jq. Binds 127.0.0.1:<port> only.
set -euo pipefail

KILLS=40; PORT=7094; OLD_REF="${OLD_REF:-fc779e7}"
while [ $# -gt 0 ]; do
    case "$1" in
        --kills) KILLS="$2"; shift 2 ;;
        --port)  PORT="$2"; shift 2 ;;
        *) echo "unknown option: $1"; exit 2 ;;
    esac
done
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK=$(mktemp -d)
PID=""
FAILS=0; CHECKS=0
pass() { CHECKS=$((CHECKS + 1)); echo "  ok    $*"; }
fail() { CHECKS=$((CHECKS + 1)); FAILS=$((FAILS + 1)); echo "  FAIL  $*"; }
check() { local what="$1"; shift; if "$@"; then pass "$what"; else fail "$what"; fi; }
stop() { [ -z "$PID" ] || { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; PID=""; }; }
cleanup() { stop; rm -rf "$WORK"; }
trap cleanup EXIT

URL="http://127.0.0.1:$PORT"
get() { curl -s "$URL$1"; }
post() { curl -s -X POST -H 'Content-Type: application/json' -d "$2" "$URL$1"; }
# start <binary> <config> [-init]: in the background, waits for /health
start() {
    "$1" -config "$2" ${3:-} >> "$WORK/service.log" 2>&1 &
    PID=$!
    for _ in $(seq 1 100); do curl -s -o /dev/null "$URL/health" && return 0; sleep 0.05; done
    return 1
}

echo "=== build: the bed's binary ($OLD_REF) and this tree's"
mkdir -p "$WORK/old-src" "$WORK/bin"
git -C "$REPO" archive "$OLD_REF" | tar -x -C "$WORK/old-src"
(cd "$WORK/old-src" && go build -o "$WORK/bin/old" ./cmd/bucket-next)
(cd "$REPO" && go build -o "$WORK/bin/new" ./cmd/bucket-next && go build -o "$WORK/bin/cli" ./cmd/bucket-next-cli)
common="shard_id: 1
total_shards: 2
listen_address: 127.0.0.1
listen_port: $PORT
segment_size: 1000
segment_refill_watermark: 0.5"
printf '%s\nstate_path: %s/shard-1/state.json\n' "$common" "$WORK" > "$WORK/old.yaml"
printf '%s\nstate_dir: %s/shard-1\ntenants: [btcl]\nlegacy_tenant: btcl\n' "$common" "$WORK" > "$WORK/new.yaml"

echo "=== 1 · the bed today"
start "$WORK/bin/old" "$WORK/old.yaml" -init || fail "1: the old binary did not start"
post /api/init/partner.btcl '{"dataType":"int","startValue":2007}' > /dev/null
OLD_IDS=(); OLD_TOK=()
for _ in 1 2 3 4 5; do OLD_IDS+=("$(get '/api/next-id/partner.btcl?dataType=int' | jq -r .value)"); done
for _ in 1 2 3; do OLD_TOK+=("$(get '/api/next-id/tok?dataType=uuid12' | jq -r .value)"); done
get '/api/next-id/ev?dataType=snowflake' > /dev/null
stop
check "1: the old binary minted ${OLD_IDS[*]}" test "${OLD_IDS[*]}" = "2007 2009 2011 2013 2015"
OLD_MAX=2015
cp -a "$WORK/shard-1" "$WORK/pristine"

echo "=== 2 · the carry-over killed at random moments of the first start ($KILLS times)"
declare -A SEEN_AT_KILL=()
REPEATS=0; BAD_STARTS=0
for ((k = 1; k <= KILLS; k++)); do
    rm -rf "$WORK/shard-1"; cp -a "$WORK/pristine" "$WORK/shard-1"
    "$WORK/bin/new" -config "$WORK/new.yaml" >> "$WORK/service.log" 2>&1 &
    victim=$!
    sleep "0.$(printf "%03d" $((RANDOM % 150)))"
    kill -9 "$victim" 2>/dev/null || true
    wait "$victim" 2>/dev/null || true
    d="$WORK/shard-1"
    if [ -f "$d/shard.json" ] && [ ! -f "$d/state.json" ]; then at="done"
    elif [ -f "$d/shard.json" ]; then at="shard.json written, state.json not yet renamed"
    elif [ -f "$d/tenants/btcl.json" ]; then at="tenant file written, no shard.json"
    else at="nothing written"; fi
    SEEN_AT_KILL[$at]=$(( ${SEEN_AT_KILL[$at]:-0} + 1 ))
    if ! start "$WORK/bin/new" "$WORK/new.yaml"; then BAD_STARTS=$((BAD_STARTS + 1)); stop; continue; fi
    id=$(get '/api/v2/btcl/next-id/partner.btcl?dataType=int' | jq -r .value)
    tok=$(get '/api/next-id/tok?dataType=uuid12' | jq -r .value)
    if ! [ "$id" -gt "$OLD_MAX" ] 2>/dev/null; then REPEATS=$((REPEATS + 1)); echo "    kill $k ($at): partner.btcl gave $id"; fi
    for t in "${OLD_TOK[@]}"; do [ "$t" != "$tok" ] || { REPEATS=$((REPEATS + 1)); echo "    kill $k: tok repeated $tok"; }; done
    stop
done
for at in "${!SEEN_AT_KILL[@]}"; do echo "    killed with: $at — ${SEEN_AT_KILL[$at]} times"; done
check "2: every start after a kill came up ($((KILLS - BAD_STARTS))/$KILLS)" test "$BAD_STARTS" -eq 0
check "2: no id or string repeated after any kill" test "$REPEATS" -eq 0
check "2: the kills hit more than one step of the carry-over (${#SEEN_AT_KILL[@]} kinds)" test "${#SEEN_AT_KILL[@]}" -ge 2

echo "=== 3 · after the move: 4 workers on v1, 4 on v2, one counter"
rm -rf "$WORK/shard-1"; cp -a "$WORK/pristine" "$WORK/shard-1"
start "$WORK/bin/new" "$WORK/new.yaml" || fail "3: the new binary did not start"
WORKERS=()
for w in 1 2 3 4; do
    (for _ in $(seq 1 250); do get '/api/next-id/partner.btcl?dataType=int' | jq -r .value; done > "$WORK/v1-$w") &
    WORKERS+=($!)
    (for _ in $(seq 1 250); do get '/api/v2/btcl/next-id/partner.btcl?dataType=int' | jq -r .value; done > "$WORK/v2-$w") &
    WORKERS+=($!)
done
wait "${WORKERS[@]}"
total=$(cat "$WORK"/v1-* "$WORK"/v2-* | wc -l)
distinct=$(cat "$WORK"/v1-* "$WORK"/v2-* | sort -u | wc -l)
min=$(cat "$WORK"/v1-* "$WORK"/v2-* | sort -n | sed -n 1p)
check "3: $total ids, $distinct distinct" test "$total" -eq 2000 -a "$distinct" -eq 2000
check "3: the lowest ($min) is above every old id" test "$min" -gt "$OLD_MAX"
info=$(get /shard-info)
check "3: /shard-info counted the v1 calls ($(echo "$info" | jq .v1Calls))" test "$(echo "$info" | jq .v1Calls)" -eq 1000
stop
check "3: the CLI reads the state the service left (info)" bash -c "'$WORK/bin/cli' -config '$WORK/new.yaml' info | grep -q 'Carried over:     state.json (format 2) into tenant btcl'"

echo
echo "=== $((CHECKS - FAILS))/$CHECKS checks held"
[ "$FAILS" -eq 0 ]

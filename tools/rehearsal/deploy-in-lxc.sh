#!/bin/bash
#
# deploy-in-lxc.sh — rehearse the bed's move to the tenant-first service through the REAL deploy path
# (tools/deploy/dependencies/remote-setup.sh under systemd, the unit's sandbox included) in a throwaway LXD container.
# Nothing outside the container is touched: no ssh, no server, no bucket-next/bin/.
#
#   tools/rehearsal/deploy-in-lxc.sh [--image debian-12] [--name bn-tenant-rehearsal] [--keep]
#
#   A  the bed today: the pre-tenant binary (OLD_REF, default fc779e7 = what the bed runs) installed by ITS remote-setup,
#      a state file created, partner.btcl registered at 2007 like the bed's, ids minted on the pre-tenant roads
#   B  the move: this tree's remote-setup with a profile like the bed's (tenants [btcl], legacy_tenant btcl): the new
#      binary checks the state read-only BEFORE installing, then carries state.json over at its first start; v1 and v2
#      mint from ONE counter, every id above every earlier one
#   C  a refused deploy (a config of another cluster size): nothing installed, the running shard keeps serving
#   D  the pre-tenant roads switched off (legacy_tenant removed): 400 "Tenant is required"; v2 serves
#   E  the route back and forward again: export-v2 → the old binary continues above → the new binary carries the file
#      over again, marks only forward
#
# Exit 0 = every check held. Needs: lxc (LXD) with a Debian 12 image, go, jq.
set -euo pipefail

IMAGE=debian-12; NAME=bn-tenant-rehearsal; KEEP=false; OLD_REF="${OLD_REF:-fc779e7}"
while [ $# -gt 0 ]; do
    case "$1" in
        --image) IMAGE="$2"; shift 2 ;;
        --name)  NAME="$2"; shift 2 ;;
        --keep)  KEEP=true; shift ;;
        *) echo "unknown option: $1"; exit 2 ;;
    esac
done
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK=$(mktemp -d)
FAILS=0; CHECKS=0
pass() { CHECKS=$((CHECKS + 1)); echo "  ok    $*"; }
fail() { CHECKS=$((CHECKS + 1)); FAILS=$((FAILS + 1)); echo "  FAIL  $*"; }
check() { local what="$1"; shift; if "$@"; then pass "$what"; else fail "$what"; fi; }
cleanup() {
    rm -rf "$WORK"
    if [ "$KEEP" = false ]; then lxc delete --force "$NAME" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

in_ct() { lxc exec "$NAME" -- "$@"; }
# api METHOD PATH [BODY] → "<status> <json body>" from the shard inside the container
api() { in_ct /root/http.sh "$@"; }
status_of() { echo "$1" | cut -d' ' -f1; }
body_of() { echo "$1" | cut -d' ' -f2-; }
val() { body_of "$1" | jq -r "$2"; }

echo "=== build: the bed's binary ($OLD_REF) and this tree's, static linux/amd64"
mkdir -p "$WORK/old/src" "$WORK/old/stage" "$WORK/new/stage"
git -C "$REPO" archive "$OLD_REF" | tar -x -C "$WORK/old/src"
(cd "$WORK/old/src" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/old/stage/bucket-next" ./cmd/bucket-next \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/old/stage/bucket-next-cli" ./cmd/bucket-next-cli)
cp "$WORK/old/src/tools/deploy/dependencies/remote-setup.sh" "$WORK/old/src/tools/deploy/dependencies/bucket-next@.service" "$WORK/old/stage/"
(cd "$REPO" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/new/stage/bucket-next" ./cmd/bucket-next \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/new/stage/bucket-next-cli" ./cmd/bucket-next-cli)
cp "$REPO/tools/deploy/dependencies/remote-setup.sh" "$REPO/tools/deploy/dependencies/bucket-next@.service" "$WORK/new/stage/"

common='total_shards: 2
listen_address: "127.0.0.1"
listen_port: 7001
segment_size: 1000
segment_refill_watermark: 0.5
clock_drift_tolerance_ms: 0
snowflake_epoch_ms: 1704067200000'
printf 'shard_id: 1\n%s\nstate_path: /var/lib/bucket-next/shard-1/state.json\n' "$common" > "$WORK/old/stage/config.yaml"
printf 'shard_id: 1\n%s\nstate_dir: /var/lib/bucket-next/shard-1\ntenants: [btcl]\nlegacy_tenant: btcl\n' "$common" > "$WORK/new/config-legacy.yaml"
printf 'shard_id: 1\n%s\nstate_dir: /var/lib/bucket-next/shard-1\ntenants: [btcl]\n' "$common" > "$WORK/new/config-strict.yaml"
sed 's/^total_shards: 2/total_shards: 3/' "$WORK/new/config-legacy.yaml" > "$WORK/new/config-wrong.yaml"

cat > "$WORK/http.sh" <<'EOF'
#!/bin/bash
# http.sh METHOD PATH [BODY] -> "<status> <body>" (the image has no curl)
m=$1; p=$2; b=${3:-}
exec 3<>/dev/tcp/127.0.0.1/7001 || { echo "000 {}"; exit 0; }
if [ -n "$b" ]; then
    printf '%s %s HTTP/1.0\r\nHost: shard\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s' "$m" "$p" "${#b}" "$b" >&3
else
    printf '%s %s HTTP/1.0\r\nHost: shard\r\n\r\n' "$m" "$p" >&3
fi
resp=$(cat <&3)
echo "$(echo "$resp" | head -1 | awk '{print $2}') $(echo "$resp" | sed '1,/^\r$/d' | tr -d '\n')"
EOF
chmod +x "$WORK/http.sh"

echo "=== container $NAME from image $IMAGE"
lxc delete --force "$NAME" >/dev/null 2>&1 || true
lxc launch "$IMAGE" "$NAME" >/dev/null
for _ in $(seq 1 60); do in_ct systemctl is-system-running 2>/dev/null | grep -qE 'running|degraded' && break; sleep 1; done
in_ct mkdir -p /root/old /root/new
lxc file push "$WORK/http.sh" "$NAME/root/http.sh" >/dev/null
for f in bucket-next bucket-next-cli remote-setup.sh bucket-next@.service config.yaml; do lxc file push "$WORK/old/stage/$f" "$NAME/root/old/$f" >/dev/null; done
for f in bucket-next bucket-next-cli remote-setup.sh bucket-next@.service; do lxc file push "$WORK/new/stage/$f" "$NAME/root/new/$f" >/dev/null; done
# stage <config> → /root/new/config.yaml (what the deploy renders per shard)
stage() { lxc file push "$WORK/new/$1" "$NAME/root/new/config.yaml" >/dev/null; }
setup_new() { in_ct bash /root/new/remote-setup.sh /root/new 1 /var/lib/bucket-next/shard-1 0; }
setup_old() { in_ct bash /root/old/remote-setup.sh /root/old 1 /var/lib/bucket-next/shard-1/state.json "${1:-0}"; }
wait_up() { for _ in $(seq 1 50); do [ "$(status_of "$(api GET /health)")" = 200 ] && return 0; sleep 0.2; done; return 1; }
ISSUED=()
mint() { # mint <label> <path>: one id, kept in ISSUED
    local r; r=$(api GET "$2")
    if [ "$(status_of "$r")" = 200 ]; then ISSUED+=("$(val "$r" .value)"); else fail "$1: $(echo "$r" | cut -c1-200)"; fi
}
above_all_before() { # above_all_before <n-earlier>: every id after the first n is above the earlier max
    local n=$1 max=0 i
    for ((i = 0; i < n; i++)); do [ "${ISSUED[$i]}" -gt "$max" ] && max=${ISSUED[$i]}; done
    for ((i = n; i < ${#ISSUED[@]}; i++)); do [ "${ISSUED[$i]}" -gt "$max" ] || return 1; done
}
distinct() { [ "$(printf '%s\n' "${ISSUED[@]}" | sort -u | wc -l)" -eq "${#ISSUED[@]}" ]; }

echo "=== A · the bed today: $OLD_REF, its remote-setup, partner.btcl at 2007"
setup_old 1 | sed 's/^/    /' || fail "A: the old remote-setup failed"
wait_up && pass "A: the pre-tenant shard serves" || fail "A: the pre-tenant shard is not up"
r=$(api POST /api/init/partner.btcl '{"dataType":"int","startValue":2007}')
check "A: partner.btcl registered at 2007 (iteration 1003, like the bed)" test "$(val "$r" .nextValue)" = 2007
for _ in 1 2 3; do mint "A: v1 next-id" "/api/next-id/partner.btcl?dataType=int"; done
check "A: the old binary minted 2007 2009 2011" test "${ISSUED[*]}" = "2007 2009 2011"
NA=${#ISSUED[@]}

echo "=== B · the move: this tree's remote-setup, tenants [btcl], legacy_tenant btcl"
stage config-legacy.yaml
out=$(setup_new 2>&1) || fail "B: the new remote-setup failed"; echo "$out" | sed 's/^/    /'
check "B: the read-only check said it will carry state.json over into btcl" grep -q "carry the pre-tenant state.json (format 2, 1 entities) over into tenant btcl" <<< "$out"
wait_up && pass "B: the tenant-first shard serves" || fail "B: the tenant-first shard is not up"
info=$(api GET /shard-info)
check "B: /shard-info tenants [btcl], legacyTenant btcl" test "$(val "$info" '.tenants|join(",")') $(val "$info" .legacyTenant)" = "btcl btcl"
check "B: shard.json, tenants/btcl.json and state.json.migrated-v2 on disk" in_ct test -f /var/lib/bucket-next/shard-1/shard.json -a -f /var/lib/bucket-next/shard-1/tenants/btcl.json -a -f /var/lib/bucket-next/shard-1/state.json.migrated-v2 -a ! -e /var/lib/bucket-next/shard-1/state.json
check "B: the journal says what the start did" bash -c "lxc exec $NAME -- journalctl -u bucket-next@1 --no-pager | grep -q 'carry the pre-tenant state.json'"
for _ in 1 2 3; do mint "B: v1" "/api/next-id/partner.btcl?dataType=int"; mint "B: v2" "/api/v2/btcl/next-id/partner.btcl?dataType=int"; done
check "B: v1 and v2 draw from one counter, every id above the old binary's (${ISSUED[*]:$NA})" above_all_before "$NA"
check "B: no id issued twice" distinct
r=$(api GET /api/v2/BTCL/next-id/partner.btcl?dataType=int); check "B: a capitalised tenant is 400 Invalid tenant" test "$(status_of "$r") $(val "$r" .error)" = "400 Invalid tenant"
r=$(api GET /api/v2/nobody/status/partner.btcl); check "B: an unknown tenant is 400 Unknown tenant" test "$(status_of "$r") $(val "$r" .error)" = "400 Unknown tenant"
r=$(api GET /api/v2/btcl/status/missing); check "B: a missing entity is 404 Entity not found" test "$(status_of "$r") $(val "$r" .error)" = "404 Entity not found"
r=$(api GET /shard-info); check "B: v1Calls counts the pre-tenant calls ($(val "$r" .v1Calls))" test "$(val "$r" .v1Calls)" -ge 3
NB=${#ISSUED[@]}

echo "=== C · a refused deploy: a config of another cluster size"
stage config-wrong.yaml
before=$(in_ct sha256sum /opt/bucket-next/bucket-next /etc/bucket-next/shard-1.yaml)
if out=$(setup_new 2>&1); then fail "C: the deploy went through"; else pass "C: the deploy stopped"; fi
echo "$out" | sed 's/^/    /'
check "C: it said why (total_shards file=2 config=3)" grep -q "total_shards: file=2 config=3" <<< "$out"
check "C: nothing was installed" test "$before" = "$(in_ct sha256sum /opt/bucket-next/bucket-next /etc/bucket-next/shard-1.yaml)"
n=${#ISSUED[@]}; mint "C: v2 after the refused deploy" "/api/v2/btcl/next-id/partner.btcl?dataType=int"
check "C: the running shard kept serving (${ISSUED[-1]})" test "${#ISSUED[@]}" -gt "$n"

echo "=== D · the pre-tenant roads switched off: legacy_tenant removed"
stage config-strict.yaml
setup_new | sed 's/^/    /' || fail "D: the new remote-setup failed"
wait_up || fail "D: not up"
r=$(api GET "/api/next-id/partner.btcl?dataType=int"); check "D: a v1 call is 400 Tenant is required" test "$(status_of "$r") $(val "$r" .error)" = "400 Tenant is required"
r=$(api GET /shard-info); check "D: /shard-info legacyTenant null" test "$(val "$r" .legacyTenant)" = null
mint "D: v2" "/api/v2/btcl/next-id/partner.btcl?dataType=int"
check "D: v2 serves, above everything before" above_all_before "$NB"
ND=${#ISSUED[@]}

echo "=== E · the route back (export-v2 → the old binary) and forward again"
in_ct systemctl stop bucket-next@1
out=$(in_ct runuser -u bucket-next -- /opt/bucket-next/bucket-next-cli -config /etc/bucket-next/shard-1.yaml -tenant btcl export-v2 2>&1) || fail "E: export-v2 failed"; echo "$out" | sed 's/^/    /'
check "E: export-v2 wrote state.json and retired shard.json" in_ct test -f /var/lib/bucket-next/shard-1/state.json -a ! -e /var/lib/bucket-next/shard-1/shard.json
setup_old 0 | sed 's/^/    /' || fail "E: the old remote-setup failed"
wait_up || fail "E: the old binary is not up"
for _ in 1 2; do mint "E: the old binary, v1" "/api/next-id/partner.btcl?dataType=int"; done
check "E: the old binary continues above every id the new one issued" above_all_before "$ND"
NE=${#ISSUED[@]}
stage config-legacy.yaml
out=$(setup_new 2>&1) || fail "E: the new remote-setup failed"; echo "$out" | sed 's/^/    /'
check "E: the check says it will carry state.json over again" grep -q "carry the pre-tenant state.json" <<< "$out"
wait_up || fail "E: the new binary is not up again"
for _ in 1 2; do mint "E: forward again, v2" "/api/v2/btcl/next-id/partner.btcl?dataType=int"; done
check "E: forward again, above the old binary's ids" above_all_before "$NE"
check "E: no id issued twice across A-E (${#ISSUED[@]} ids)" distinct

echo
echo "ids in order: ${ISSUED[*]}"
echo "=== $((CHECKS - FAILS))/$CHECKS checks held"
[ "$FAILS" -eq 0 ]

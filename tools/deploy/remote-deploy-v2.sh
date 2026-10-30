#!/bin/bash
#
# remote-deploy-v2.sh — build & deploy bucket-next for ONE tenant profile (the routesphere convention).
#
#   tools/deploy/remote-deploy-v2.sh <tenant> <profile> --dry-run        the plan + the rendered configs; no ssh, no build
#   tools/deploy/remote-deploy-v2.sh <tenant> <profile> --init-state     FIRST deploy of a shard: create its missing state
#   tools/deploy/remote-deploy-v2.sh <tenant> <profile>                  every deployed shard of the profile
#   tools/deploy/remote-deploy-v2.sh <tenant> <profile> --shard N        one shard
#   tools/deploy/remote-deploy-v2.sh <tenant> <profile> --skip-build     ship the binaries already in bin/
#   tools/deploy/remote-deploy-v2.sh --list                              tenants and their profiles
#   ... --yes                                                            do not ask before touching the server
#
# Three trees, each with one concern:
#   config/tenants/<tenant>/<profile>/profile-<profile>.yml   WHAT: the service's own config, the same for every shard
#   tools/deploy/tenant-conf-v2/<tenant>.conf  [<profile>]    WHERE: inventory_tenant, server, which shards are deployed
#   the SHARED ssh inventory (by reference, never a copy)     HOW TO REACH IT: servers/<inventory_tenant>/hosts/<server>
#     default /home/mustafa/telcobright-projects/routesphere/routesphere-core/tools/ssh-automation/servers
#     override: BUCKET_NEXT_SSH_INVENTORY=<path>
#
# <tenant> here is the INSTALLATION (whose boxes; the routesphere convention). The tenants the service SERVES are the
# profile's own key: tenants: [btcl, ...] - one service per installation serves every tenant, the tenant a key on every call.
#
# Each shard runs as the systemd unit bucket-next@<shard_id> with its own config (/etc/bucket-next/shard-<id>.yaml) and its
# own state directory (shard.json + tenants/<tenant>.json), so several shards can share a host. A shard's state is created
# ONLY by --init-state and only when it has none: after a shard has minted, its state is never initialised again (that would
# re-issue ids). A new tenant needs no --init-state: the shard creates its file at the next start.
#
# Phases: 1 args · 2 validate · 3 tenant conf · 4 the service's config · 5 ssh · 6 plan · 7 build · 8 ship + check (the new
# binary reads the shard's state BEFORE anything is installed; a refusal leaves the running shard untouched) + install +
# (first time) init-state + start · 9 health (/health, and /shard-info must report the shard and the tenants we deployed).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
CONF_DIR="$SCRIPT_DIR/tenant-conf-v2"
APP_CONFIG_DIR="$PROJECT_DIR/config/tenants"
DEPS_DIR="$SCRIPT_DIR/dependencies"
BIN_DIR="$PROJECT_DIR/bin"
INVENTORY_DIR="${BUCKET_NEXT_SSH_INVENTORY:-/home/mustafa/telcobright-projects/routesphere/routesphere-core/tools/ssh-automation/servers}"
# every key the service's profile YAML may state (shard_id and uuid_key are written by this script, never by the file)
PROFILE_KEYS="total_shards listen_address listen_port state_dir tenants legacy_tenant segment_size segment_refill_watermark clock_drift_tolerance_ms snowflake_epoch_ms"
TENANT_RE='[a-z][a-z0-9]{1,15}'   # tenant-master's tenant id: the service refuses any other spelling

# ---------- 1 · args ----------

TENANT=""; PROFILE=""; ONLY_SHARD=""; SKIP_BUILD=false; DRY_RUN=false; INIT_STATE=0; YES=false

usage() { sed -n '3,12p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

list_tenants() {
    echo "Tenants under $CONF_DIR:"
    for f in "$CONF_DIR"/*.conf; do
        [ -f "$f" ] || continue
        echo "  $(basename "$f" .conf)"
        grep -E '^\[' "$f" | sed -E 's/^\[(.+)\]$/    profile: \1/'
    done
}

[ $# -gt 0 ] || { usage; exit 2; }
case "$1" in --list|-l) list_tenants; exit 0 ;; -h|--help) usage; exit 0 ;; esac
TENANT="${1:-}"; PROFILE="${2:-}"; shift 2 || true
while [ $# -gt 0 ]; do
    case "$1" in
        --shard)      ONLY_SHARD="${2:-}"; shift 2 ;;
        --skip-build) SKIP_BUILD=true; shift ;;
        --init-state) INIT_STATE=1; shift ;;
        --dry-run)    DRY_RUN=true; shift ;;
        --yes)        YES=true; shift ;;
        -h|--help)    usage; exit 0 ;;
        *)            echo "unknown option: $1"; usage; exit 2 ;;
    esac
done
[ -n "$TENANT" ] && [ -n "$PROFILE" ] || { usage; exit 2; }

die() { echo "ERROR: $*" >&2; exit 1; }

# ---------- 2 · validate: the three files of this tenant + profile exist ----------

CONFIG_FILE="$CONF_DIR/$TENANT.conf"
PROFILE_YML="$APP_CONFIG_DIR/$TENANT/$PROFILE/profile-$PROFILE.yml"
[ -f "$CONFIG_FILE" ] || { echo "ERROR: tenant conf not found: $CONFIG_FILE" >&2; list_tenants; exit 1; }
grep -qE "^\[${PROFILE}\]" "$CONFIG_FILE" || die "profile '$PROFILE' not found in $CONFIG_FILE (it has: $(grep -E '^\[' "$CONFIG_FILE" | tr -d '[]' | tr '\n' ' '))"
[ -f "$PROFILE_YML" ] || die "the service's config for $TENANT/$PROFILE is missing: $PROFILE_YML"
[ -d "$INVENTORY_DIR" ] || die "the shared ssh inventory is not at $INVENTORY_DIR (set BUCKET_NEXT_SSH_INVENTORY)"

# ---------- 3 · the tenant conf (INI): WHERE ----------

# parse_conf <key>  →  the value of key in [PROFILE] (empty when absent); a trailing "; comment" is dropped
parse_conf() {
    awk -v section="[$PROFILE]" -v key="$1" '
        $0 == section { in_section=1; next }
        /^\[/ { in_section=0 }
        in_section {
            line=$0; gsub(/^[ \t]+/, "", line)
            if (line ~ "^" key "[ \t]*=") {
                value=substr(line, index(line, "=")+1)
                sub(/[ \t]+;.*$/, "", value); gsub(/^[ \t]+|[ \t]+$/, "", value)
                print value; exit
            }
        }' "$CONFIG_FILE"
}
# parse_kv <file> <key>  →  key=value of a flat host file; empty when absent
parse_kv() { { grep -E "^${2}=" "$1" 2>/dev/null || true; } | head -1 | cut -d= -f2-; }

DESCRIPTION=$(parse_conf description)
INVENTORY_TENANT=$(parse_conf inventory_tenant)
SERVER=$(parse_conf server)
SHARD_LIST=$(parse_conf shards)
UUID_KEY_FILE=$(parse_conf uuid_key_file)
DEPLOY_ALLOWED=$(parse_conf deploy)
[ -n "$INVENTORY_TENANT" ] || die "[$PROFILE].inventory_tenant is required in $CONFIG_FILE"
[ -n "$SERVER" ] || die "[$PROFILE].server is required in $CONFIG_FILE"
case "${DEPLOY_ALLOWED:-true}" in true|false) ;; *) die "[$PROFILE].deploy must be true or false, not '$DEPLOY_ALLOWED'" ;; esac

# ---------- 4 · the service's own config (flat YAML): WHAT ----------

# yaml_get <key>  →  the value of a top-level "key: value" line, comment and quotes removed
yaml_get() {
    awk -v key="$1" '
        /^[ \t]*#/ { next }
        $0 ~ "^" key "[ \t]*:" {
            v=substr($0, index($0, ":")+1)
            sub(/[ \t]+#.*$/, "", v); gsub(/^[ \t]+|[ \t]+$/, "", v)
            if (v ~ /^".*"$/ || v ~ /^\047.*\047$/) v=substr(v, 2, length(v)-2)
            print v; exit
        }' "$PROFILE_YML"
}
grep -qE '^[[:space:]]*-' "$PROFILE_YML" && die "$PROFILE_YML: write a list on one line, e.g. tenants: [btcl, other] (a block list is not read)"
for k in $(awk '/^[ \t]*#/ || /^[ \t]*$/ { next } /^[A-Za-z_][A-Za-z0-9_]*[ \t]*:/ { sub(/[ \t]*:.*$/, ""); print; next } { print "?" $0 }' "$PROFILE_YML"); do
    case " $PROFILE_KEYS " in
        *" $k "*) ;;
        *) case "$k" in
               shard_id|uuid_key) die "$PROFILE_YML states $k: the deploy writes it per shard (the shard list is in the tenant conf; the uuid key comes from uuid_key_file)" ;;
               state_path) die "$PROFILE_YML states state_path: it is replaced by state_dir (the shard's directory, e.g. /var/lib/bucket-next/shard-{shard_id}; a state.json in it is carried over into legacy_tenant)" ;;
               *) die "$PROFILE_YML: '$k' is not a key bucket-next reads (known: $PROFILE_KEYS)" ;;
           esac ;;
    esac
done
TOTAL_SHARDS=$(yaml_get total_shards)
LISTEN_ADDRESS=$(yaml_get listen_address)
LISTEN_PORT=$(yaml_get listen_port)
STATE_DIR_TMPL=$(yaml_get state_dir)
SERVED_TENANTS_RAW=$(yaml_get tenants)
LEGACY_TENANT=$(yaml_get legacy_tenant)
[[ "$TOTAL_SHARDS" =~ ^[0-9]+$ ]] && [ "$TOTAL_SHARDS" -ge 1 ] && [ "$TOTAL_SHARDS" -le 1024 ] || die "$PROFILE_YML: total_shards must be 1..1024 (got '${TOTAL_SHARDS:-unset}')"
: "${LISTEN_PORT:=7001}"
: "${STATE_DIR_TMPL:=/var/lib/bucket-next/shard-{shard_id}}"
# the tenants the service serves: one line, tenants: [a, b]; each tenant-master's tenant id; the list the same on every shard
[[ "$SERVED_TENANTS_RAW" =~ ^\[[[:space:]]*${TENANT_RE}([[:space:]]*,[[:space:]]*${TENANT_RE})*[[:space:]]*\]$ ]] \
    || die "$PROFILE_YML: tenants must be one line of tenant ids, e.g. tenants: [btcl] (got '${SERVED_TENANTS_RAW:-unset}'; a tenant id is ^${TENANT_RE}\$)"
mapfile -t SERVED_TENANTS < <(echo "$SERVED_TENANTS_RAW" | tr -d '[] ' | tr ',' '\n')
[ "$(printf '%s\n' "${SERVED_TENANTS[@]}" | sort | uniq -d)" = "" ] || die "$PROFILE_YML: a tenant is listed twice in tenants"
if [ -n "$LEGACY_TENANT" ]; then
    printf '%s\n' "${SERVED_TENANTS[@]}" | grep -qx "$LEGACY_TENANT" || die "$PROFILE_YML: legacy_tenant '$LEGACY_TENANT' is not one of tenants ${SERVED_TENANTS_RAW}"
fi
# what /shard-info must report: the served tenants, sorted, as JSON
EXPECT_TENANTS_JSON="\"tenants\":[$(printf '%s\n' "${SERVED_TENANTS[@]}" | sort | sed 's/.*/"&"/' | paste -sd,)]"
[[ "$LISTEN_PORT" =~ ^[0-9]+$ ]] || die "$PROFILE_YML: listen_port must be a number"

# the uuid key is the same on every shard; it lives in a file outside git, named by the tenant conf
UUID_KEY=""
if [ -n "$UUID_KEY_FILE" ]; then
    case "$UUID_KEY_FILE" in /*) key_path="$UUID_KEY_FILE" ;; *) key_path="$PROJECT_DIR/$UUID_KEY_FILE" ;; esac
    [ -f "$key_path" ] || die "uuid_key_file not found: $key_path"
    UUID_KEY=$(tr -d ' \t\r\n' < "$key_path")
    [[ "$UUID_KEY" =~ ^[0-9a-fA-F]+$ ]] && { [ ${#UUID_KEY} -eq 32 ] || [ ${#UUID_KEY} -eq 48 ] || [ ${#UUID_KEY} -eq 64 ]; } \
        || die "$key_path must hold 32, 48 or 64 hex characters"
fi

# which shards of the cluster this profile deploys; the others are declared in total_shards and deployed nowhere yet
SHARDS=()
if [ -n "$SHARD_LIST" ]; then
    IFS=', ' read -r -a SHARDS <<< "$SHARD_LIST"
else
    for ((i = 1; i <= TOTAL_SHARDS; i++)); do SHARDS+=("$i"); done
fi
mapfile -t SHARDS < <(printf '%s\n' "${SHARDS[@]}" | sort -n | uniq)
[ "${#SHARDS[@]}" -ge 1 ] && [ -n "${SHARDS[0]}" ] || die "[$PROFILE].shards names no shard"
NOT_DEPLOYED=()
for sid in "${SHARDS[@]}"; do
    [[ "$sid" =~ ^[0-9]+$ ]] && [ "$sid" -ge 1 ] && [ "$sid" -le "$TOTAL_SHARDS" ] \
        || die "[$PROFILE].shards: '$sid' is not a shard of this cluster (1..$TOTAL_SHARDS, total_shards of $PROFILE_YML)"
done
for ((i = 1; i <= TOTAL_SHARDS; i++)); do
    printf '%s\n' "${SHARDS[@]}" | grep -qx "$i" || NOT_DEPLOYED+=("$i")
done

# ---------- 5 · ssh: every deployed shard's host, from the shared inventory ----------

declare -A S_SERVER S_HOST S_PORT S_USER S_KEY S_LPORT S_STATE S_ADDR
for sid in "${SHARDS[@]}"; do
    server=$(parse_conf "shard${sid}.server"); : "${server:=$SERVER}"
    host_file="$INVENTORY_DIR/$INVENTORY_TENANT/hosts/$server"
    [ -f "$host_file" ] || die "shard $sid: no host file for server '$server' in the shared inventory: $host_file"
    S_SERVER[$sid]="$server"
    S_HOST[$sid]=$(parse_kv "$host_file" host); S_PORT[$sid]=$(parse_kv "$host_file" port)
    S_USER[$sid]=$(parse_kv "$host_file" user); S_KEY[$sid]=$(parse_kv "$host_file" key)
    [ -n "${S_HOST[$sid]}" ] && [ -n "${S_PORT[$sid]}" ] && [ -n "${S_USER[$sid]}" ] && [ -n "${S_KEY[$sid]}" ] \
        || die "shard $sid: $host_file lacks host/port/user/key"
    lport=$(parse_conf "shard${sid}.listen_port"); S_LPORT[$sid]="${lport:-$LISTEN_PORT}"
    S_STATE[$sid]="${STATE_DIR_TMPL//\{shard_id\}/$sid}"
    addr="${LISTEN_ADDRESS//\{private_ip\}/$(parse_kv "$host_file" private_ip)}"
    [[ "$LISTEN_ADDRESS" != *"{private_ip}"* ]] || [ -n "$(parse_kv "$host_file" private_ip)" ] \
        || die "shard $sid: listen_address says {private_ip} and $host_file has no private_ip="
    [ -z "$addr" ] || [[ "$addr" =~ ^[0-9a-fA-F:.]+$ ]] || die "shard $sid: listen_address '$addr' is not an IP address"
    S_ADDR[$sid]="$addr"
done
# two shards on one host must not share a port or a state directory: one would read the other's counters
declare -A SEEN_PORT SEEN_STATE
for sid in "${SHARDS[@]}"; do
    pk="${S_HOST[$sid]}:${S_LPORT[$sid]}"; sk="${S_HOST[$sid]}:${S_STATE[$sid]}"
    [ -z "${SEEN_PORT[$pk]:-}" ] || die "shards ${SEEN_PORT[$pk]} and $sid both listen on $pk - give one a shard<N>.listen_port in $CONFIG_FILE"
    [ -z "${SEEN_STATE[$sk]:-}" ] || die "shards ${SEEN_STATE[$sk]} and $sid share the state directory ${S_STATE[$sid]} on ${S_HOST[$sid]} - put {shard_id} in state_dir"
    SEEN_PORT[$pk]=$sid; SEEN_STATE[$sk]=$sid
done
if [ -n "$ONLY_SHARD" ]; then
    printf '%s\n' "${SHARDS[@]}" | grep -qx "$ONLY_SHARD" || die "--shard $ONLY_SHARD is not a deployed shard of $TENANT/$PROFILE (deployed: ${SHARDS[*]})"
    SHARDS=("$ONLY_SHARD")
fi

# ---------- 6 · the plan ----------

cat <<EOF
========================================================
  bucket-next V2 deploy
========================================================
Tenant/profile: $TENANT / $PROFILE   ($DESCRIPTION)
Config (what):  ${PROFILE_YML#"$PROJECT_DIR"/}
Conf (where):   ${CONFIG_FILE#"$PROJECT_DIR"/}   inventory $INVENTORY_DIR/$INVENTORY_TENANT
Cluster:        total_shards=$TOTAL_SHARDS   deployed here: ${SHARDS[*]}$( [ "${#NOT_DEPLOYED[@]}" -gt 0 ] && echo "   DECLARED, NOT DEPLOYED: ${NOT_DEPLOYED[*]}" )
Bind:           $( [ -n "$LISTEN_ADDRESS" ] && echo "listen_address=$LISTEN_ADDRESS" || echo "ALL interfaces (set listen_address: the API has no authentication)" )
Serves tenants: ${SERVED_TENANTS[*]}   (every call /api/v2/{tenant}/...)
Pre-tenant API: $( [ -n "$LEGACY_TENANT" ] && echo "ON, acting on legacy_tenant $LEGACY_TENANT (remove legacy_tenant once every caller is on /api/v2/)" || echo "OFF - /api/next-id/... without a tenant answers 400 \"Tenant is required\"" )
uuid key:       $( [ -n "$UUID_KEY" ] && echo "from $UUID_KEY_FILE" || echo "not set - the built-in key (unique, but predictable)" )
Build:          $( [ "$DRY_RUN" = true ] && echo "no (dry run)" || { [ "$SKIP_BUILD" = true ] && echo "SKIPPED" || echo "yes (GOOS=linux GOARCH=amd64, static)"; } )
Init state:     $( [ "$INIT_STATE" = 1 ] && echo "yes - create the state where a shard has none" || echo "no - a shard without state fails the deploy" )
Dry-run:        $DRY_RUN

EOF
for sid in "${SHARDS[@]}"; do
    printf "  shard %-3s -> %-12s %s@%s:%s   unit bucket-next@%s   listen %s:%s   state %s\n" "$sid" "${S_SERVER[$sid]}" \
        "${S_USER[$sid]}" "${S_HOST[$sid]}" "${S_PORT[$sid]}" "$sid" "${S_ADDR[$sid]:-*}" "${S_LPORT[$sid]}" "${S_STATE[$sid]}"
done
echo

# render_config <shard_id>  →  the shard's /etc/bucket-next/shard-<id>.yaml on stdout
render_config() {
    local sid="$1" k v
    cat <<EOF
# bucket-next config - rendered by tools/deploy/remote-deploy-v2.sh $TENANT $PROFILE
# from config/tenants/$TENANT/$PROFILE/profile-$PROFILE.yml; shard $sid of $TOTAL_SHARDS. Do not edit here: change the
# profile and deploy again.
shard_id: $sid
total_shards: $TOTAL_SHARDS
listen_address: "${S_ADDR[$sid]}"
listen_port: ${S_LPORT[$sid]}
state_dir: ${S_STATE[$sid]}
tenants: $SERVED_TENANTS_RAW
EOF
    [ -z "$LEGACY_TENANT" ] || echo "legacy_tenant: $LEGACY_TENANT"
    for k in segment_size segment_refill_watermark clock_drift_tolerance_ms snowflake_epoch_ms; do
        v=$(yaml_get "$k"); [ -z "$v" ] || echo "$k: $v"
    done
    [ -z "$UUID_KEY" ] || echo "uuid_key: $UUID_KEY"
}

if [ "$DRY_RUN" = true ]; then
    for sid in "${SHARDS[@]}"; do
        echo "  [dry run] shard $sid: /etc/bucket-next/shard-$sid.yaml would be:"
        render_config "$sid" | sed -E 's/^(uuid_key: ).*/\1<hidden>/; s/^/    | /'
        key_file="$INVENTORY_DIR/$INVENTORY_TENANT/keys/${S_KEY[$sid]}"
        [ -f "$key_file" ] && echo "  [dry run] shard $sid: ssh key present (${S_KEY[$sid]})" || echo "  [dry run] shard $sid: WARNING - ssh key missing: $key_file"
    done
    [ "${DEPLOY_ALLOWED:-true}" = true ] || echo "  [dry run] NOTE: [$PROFILE].deploy = false - this profile is declared, and a real deploy is refused"
    echo "========================================================"
    echo "  Dry run OK for ${#SHARDS[@]} shard(s): nothing was built, nothing was sent."
    echo "========================================================"
    exit 0
fi

[ "${DEPLOY_ALLOWED:-true}" = true ] || die "[$PROFILE].deploy = false in $CONFIG_FILE: $TENANT/$PROFILE is declared, not deployed (--dry-run shows its plan)"
if [ "$YES" = false ] && [ -t 0 ]; then
    read -r -p "Deploy bucket-next shard(s) ${SHARDS[*]} of $TENANT/$PROFILE (restarts bucket-next@<id>)? [y/N] " ans
    [ "$ans" = y ] || [ "$ans" = Y ] || { echo "aborted"; exit 1; }
fi

# ---------- 7 · build ----------

if [ "$SKIP_BUILD" = false ]; then
    echo "--- build ---"
    mkdir -p "$BIN_DIR"
    (cd "$PROJECT_DIR" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$BIN_DIR/bucket-next"     ./cmd/bucket-next)
    (cd "$PROJECT_DIR" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$BIN_DIR/bucket-next-cli" ./cmd/bucket-next-cli)
fi
[ -f "$BIN_DIR/bucket-next" ] && [ -f "$BIN_DIR/bucket-next-cli" ] || die "binaries missing under $BIN_DIR (run without --skip-build)"
GIT_COMMIT=$(cd "$PROJECT_DIR" && git rev-parse --short HEAD 2>/dev/null || echo unknown)
[ -z "$(cd "$PROJECT_DIR" && git status --porcelain 2>/dev/null)" ] || GIT_COMMIT="$GIT_COMMIT+dirty"
BIN_SHA=$(sha256sum "$BIN_DIR/bucket-next" | cut -c1-16)
echo "binary: bucket-next sha256 $BIN_SHA  commit $GIT_COMMIT"
echo

# ---------- 8 + 9 · per shard: ship, install, (first time) init-state, start, health ----------

deploy_one_shard() {
    local sid="$1"
    local host="${S_HOST[$sid]}" port="${S_PORT[$sid]}" user="${S_USER[$sid]}" lport="${S_LPORT[$sid]}"
    local ssh_key="$INVENTORY_DIR/$INVENTORY_TENANT/keys/${S_KEY[$sid]}"
    [ -f "$ssh_key" ] || { echo "  [shard $sid] ERROR: ssh key not found: $ssh_key"; return 1; }

    echo "========================================================"
    echo "  shard $sid -> ${S_SERVER[$sid]}  ($user@$host:$port, unit bucket-next@$sid, listen ${S_ADDR[$sid]:-*}:$lport)"
    echo "========================================================"
    local stage; stage=$(mktemp -d)
    render_config "$sid" > "$stage/config.yaml"
    printf 'project=bucket-next\ncommit=%s\ntenant=%s\nprofile=%s\nshard=%s of %s\nbinary_sha256=%s\ndeployed_at=%s\ndeployed_by=%s\n' \
        "$GIT_COMMIT" "$TENANT" "$PROFILE" "$sid" "$TOTAL_SHARDS" "$BIN_SHA" "$(date '+%Y-%m-%d %H:%M:%S %z')" "$(whoami)@$(hostname)" > "$stage/version.txt"
    cp "$DEPS_DIR/bucket-next@.service" "$BIN_DIR/bucket-next" "$BIN_DIR/bucket-next-cli" "$DEPS_DIR/remote-setup.sh" "$stage/"

    local ctrl="/tmp/bn-ssh-$$-$sid" server_addr="$user@$host"
    ssh -o ControlPath="$ctrl" -o ControlMaster=yes -o ControlPersist=300 -o StrictHostKeyChecking=accept-new \
        -o ConnectTimeout=10 -o BatchMode=yes -p "$port" -i "$ssh_key" -fN "$server_addr"
    # shellcheck disable=SC2064
    trap "ssh -O exit -o ControlPath='$ctrl' '$server_addr' 2>/dev/null || true; rm -rf '$stage'" RETURN

    local remote_tmp="/tmp/bn-deploy-$$-$sid"
    ssh -o ControlPath="$ctrl" "$server_addr" "mkdir -p $remote_tmp && chmod 700 $remote_tmp"
    scp -q -o ControlPath="$ctrl" -P "$port" "$stage"/* "$server_addr:$remote_tmp/"
    if ! ssh -o ControlPath="$ctrl" "$server_addr" \
        "bash $remote_tmp/remote-setup.sh $remote_tmp $sid '${S_STATE[$sid]}' $INIT_STATE; rc=\$?; rm -rf $remote_tmp; exit \$rc"; then
        echo "  [shard $sid] ERROR: remote setup failed"; return 1
    fi

    # health: the road answers, and the node reports exactly the identity we deployed (probed on the address it binds)
    local probe="${S_ADDR[$sid]}"
    case "$probe" in ""|0.0.0.0|::) probe=127.0.0.1 ;; esac
    [[ "$probe" == *:* ]] && probe="[$probe]"
    local health info
    health=$(ssh -o ControlPath="$ctrl" "$server_addr" "curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://$probe:$lport/health" || true)
    info=$(ssh -o ControlPath="$ctrl" "$server_addr" "curl -s --max-time 5 http://$probe:$lport/shard-info" || true)
    if [ "$health" = 200 ] && echo "$info" | grep -q "\"shardId\":$sid," && echo "$info" | grep -q "\"totalShards\":$TOTAL_SHARDS," \
        && echo "$info" | grep -qF "$EXPECT_TENANTS_JSON"; then
        echo "  [shard $sid] OK - /health 200; /shard-info $info"
    else
        echo "  [shard $sid] ERROR - /health ${health:-no answer}; /shard-info ${info:-<no answer>} (want shard $sid of $TOTAL_SHARDS, $EXPECT_TENANTS_JSON)"; return 1
    fi
}

echo "--- deploy ---"
FAILED=()
for sid in "${SHARDS[@]}"; do
    deploy_one_shard "$sid" || FAILED+=("$sid")
    echo
done

echo "========================================================"
if [ "${#FAILED[@]}" -eq 0 ]; then
    echo "  $TENANT/$PROFILE: ${#SHARDS[@]} shard(s) deployed (${SHARDS[*]})$( [ "${#NOT_DEPLOYED[@]}" -gt 0 ] && echo "; declared, not deployed: ${NOT_DEPLOYED[*]}" )."
else
    echo "  $TENANT/$PROFILE: ${#FAILED[@]} shard(s) FAILED: ${FAILED[*]}"
fi
echo "========================================================"
[ "${#FAILED[@]}" -eq 0 ]

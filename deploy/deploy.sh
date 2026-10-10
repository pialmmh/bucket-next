#!/bin/bash
#
# Build & deploy bucket-next to every shard in a tenant's profile.
#
# Usage:
#   deploy.sh <tenant> <profile>                  # build + deploy to all shards
#   deploy.sh <tenant> <profile> --shard N        # build + deploy to one shard
#   deploy.sh <tenant> <profile> --skip-build     # deploy existing binaries
#   deploy.sh <tenant> <profile> --init-state     # first deploy: create missing state files
#   deploy.sh <tenant> <profile> --dry-run        # show plan, don't ssh
#   deploy.sh --list                              # list tenants/profiles
#
# Each shard runs as systemd unit bucket-next@<shard_id> with its own config
# (/etc/bucket-next/shard-<id>.yaml) and its own state file, so several shards can share a host.
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
TENANTS_DIR="$SCRIPT_DIR/tenants"
INVENTORY_DIR="$SCRIPT_DIR/ssh-inventory"
DEPS_DIR="$SCRIPT_DIR/dependencies"
BIN_DIR="$PROJECT_DIR/bin"
DEFAULT_STATE_PATH="/var/lib/bucket-next/shard-{shard_id}/state.json"

# ---------- arg parsing ----------

TENANT=""
PROFILE=""
ONLY_SHARD=""
SKIP_BUILD=false
DRY_RUN=false
INIT_STATE=0

usage() {
    cat <<EOF
Usage: $(basename "$0") <tenant> <profile> [options]
       $(basename "$0") --list

Options:
  --shard N         deploy only shard N (1-based)
  --skip-build      use existing bin/bucket-next + bin/bucket-next-cli
  --init-state      create the state file on shards that have none (first deploy only;
                    never use it to "fix" a lost file — restore that from backup)
  --dry-run         print the plan and the rendered configs, don't ssh
  --list            list available tenants and their profiles
  -h, --help        this message

EOF
}

list_tenants() {
    echo "Tenants under $TENANTS_DIR:"
    for f in "$TENANTS_DIR"/*.conf; do
        [ -f "$f" ] || continue
        echo "  $(basename "$f" .conf)"
        grep -E '^\[' "$f" | sed -E 's/^\[(.+)\]$/    profile: \1/'
    done
}

if [ $# -eq 0 ]; then usage; exit 2; fi
if [ "$1" = "--list" ] || [ "$1" = "-l" ]; then list_tenants; exit 0; fi
if [ "$1" = "-h" ] || [ "$1" = "--help" ]; then usage; exit 0; fi

TENANT="${1:-}"
PROFILE="${2:-}"
shift 2 || true

while [ $# -gt 0 ]; do
    case "$1" in
        --shard)        ONLY_SHARD="${2:-}"; shift 2 ;;
        --skip-build)   SKIP_BUILD=true; shift ;;
        --init-state)   INIT_STATE=1; shift ;;
        --dry-run)      DRY_RUN=true; shift ;;
        -h|--help)      usage; exit 0 ;;
        *)              echo "unknown option: $1"; usage; exit 2 ;;
    esac
done

if [ -z "$TENANT" ] || [ -z "$PROFILE" ]; then
    usage; exit 2
fi

CONFIG_FILE="$TENANTS_DIR/$TENANT.conf"
if [ ! -f "$CONFIG_FILE" ]; then
    echo "ERROR: tenant config not found: $CONFIG_FILE"
    list_tenants
    exit 1
fi

# ---------- INI parsing ----------

# parse_conf <file> <section> <key>  →  prints value (or empty if missing)
parse_conf() {
    local file="$1" section="$2" key="$3"
    awk -v section="[$section]" -v key="$key" '
        $0 == section { in_section=1; next }
        /^\[/ { in_section=0 }
        in_section {
            line=$0
            gsub(/^[ \t]+/, "", line)
            if (line ~ "^" key "[ \t]*=") {
                idx=index($0, "=")
                if (idx > 0) {
                    value=substr($0, idx+1)
                    gsub(/^[ \t]+|[ \t]+$/, "", value)
                    print value
                }
                exit
            }
        }
    ' "$file"
}

# parse_kv <file> <key>  → reads `key=value` from a flat host file; empty if the key is absent
# (grep exits 1 on no match, which would kill the script under `set -euo pipefail`).
parse_kv() {
    { grep -E "^${2}=" "$1" 2>/dev/null || true; } | head -1 | cut -d= -f2-
}

# list all keys in a section
list_section_keys() {
    awk -v section="[$2]" '
        $0 == section { in_section=1; next }
        /^\[/ { in_section=0 }
        in_section && /=/ && !/^[ \t]*#/ {
            line=$0
            gsub(/^[ \t]+/, "", line)
            sub(/[ \t]*=.*$/, "", line)
            print line
        }
    ' "$1"
}

if ! grep -qE "^\[${PROFILE}\]" "$CONFIG_FILE"; then
    echo "ERROR: profile '$PROFILE' not found in $CONFIG_FILE"
    echo "Available profiles:"
    grep -E '^\[' "$CONFIG_FILE" | tr -d '[]' | sed 's/^/  /'
    exit 1
fi

# ---------- read profile-level config ----------

DESCRIPTION=$(parse_conf "$CONFIG_FILE" "$PROFILE" description)
INVENTORY_TENANT=$(parse_conf "$CONFIG_FILE" "$PROFILE" inventory_tenant)
TOTAL_SHARDS=$(parse_conf "$CONFIG_FILE" "$PROFILE" total_shards)
LISTEN_PORT=$(parse_conf "$CONFIG_FILE" "$PROFILE" listen_port)
STATE_PATH_TMPL=$(parse_conf "$CONFIG_FILE" "$PROFILE" state_path)
SEGMENT_SIZE=$(parse_conf "$CONFIG_FILE" "$PROFILE" segment_size)
SEGMENT_WATERMARK=$(parse_conf "$CONFIG_FILE" "$PROFILE" segment_refill_watermark)
CLOCK_DRIFT_TOL_MS=$(parse_conf "$CONFIG_FILE" "$PROFILE" clock_drift_tolerance_ms)
SNOWFLAKE_EPOCH_MS=$(parse_conf "$CONFIG_FILE" "$PROFILE" snowflake_epoch_ms)
UUID_KEY_FILE=$(parse_conf "$CONFIG_FILE" "$PROFILE" uuid_key_file)
# listen_address: empty = all interfaces; an IP = that IP on every shard;
# "private_ip" = each shard binds the private_ip from its own host file.
LISTEN_ADDRESS=$(parse_conf "$CONFIG_FILE" "$PROFILE" listen_address)

: "${LISTEN_PORT:=7001}"
: "${STATE_PATH_TMPL:=$DEFAULT_STATE_PATH}"
: "${SEGMENT_SIZE:=1000}"
: "${SEGMENT_WATERMARK:=0.5}"
: "${CLOCK_DRIFT_TOL_MS:=0}"
: "${SNOWFLAKE_EPOCH_MS:=1704067200000}"

if [ -z "$INVENTORY_TENANT" ]; then
    echo "ERROR: [$PROFILE].inventory_tenant is required in $CONFIG_FILE"; exit 1
fi
if [ -z "$TOTAL_SHARDS" ]; then
    echo "ERROR: [$PROFILE].total_shards is required in $CONFIG_FILE"; exit 1
fi

# The uuid key must be the same on every shard. It lives in a file outside the tenant config
# (keys/ is gitignored), never in git.
UUID_KEY=""
if [ -n "$UUID_KEY_FILE" ]; then
    case "$UUID_KEY_FILE" in /*) key_path="$UUID_KEY_FILE" ;; *) key_path="$SCRIPT_DIR/$UUID_KEY_FILE" ;; esac
    if [ ! -f "$key_path" ]; then
        echo "ERROR: uuid_key_file not found: $key_path"; exit 1
    fi
    UUID_KEY=$(tr -d ' \t\r\n' < "$key_path")
    if ! [[ "$UUID_KEY" =~ ^[0-9a-fA-F]+$ ]] || { [ ${#UUID_KEY} -ne 32 ] && [ ${#UUID_KEY} -ne 48 ] && [ ${#UUID_KEY} -ne 64 ]; }; then
        echo "ERROR: $key_path must hold 32, 48 or 64 hex characters"; exit 1
    fi
fi

# Collect shard list
SHARDS=()
for k in $(list_section_keys "$CONFIG_FILE" "$PROFILE"); do
    if [[ "$k" =~ ^shard([0-9]+)\.server$ ]]; then
        SHARDS+=("${BASH_REMATCH[1]}")
    fi
done
mapfile -t SHARDS < <(printf '%s\n' "${SHARDS[@]}" | sort -n)

if [ "${#SHARDS[@]}" -eq 0 ] || [ -z "${SHARDS[0]}" ]; then
    echo "ERROR: no shardN.server entries in [$PROFILE] of $CONFIG_FILE"; exit 1
fi
if [ "${#SHARDS[@]}" -gt "$TOTAL_SHARDS" ]; then
    echo "ERROR: total_shards=$TOTAL_SHARDS but found ${#SHARDS[@]} shardN.server entries: ${SHARDS[*]}"; exit 1
fi
for i in "${!SHARDS[@]}"; do
    if [ "${SHARDS[$i]}" != "$((i+1))" ]; then
        echo "ERROR: the shards with a server must be numbered 1..${#SHARDS[@]} without gaps (total_shards=$TOTAL_SHARDS); got ${SHARDS[*]}"; exit 1
    fi
done
# A profile may declare more shards than it deploys: the id space is partitioned for total_shards from day one
# (value i = first + i * total_shards), so a later shard is ADDED as a shardN.server line with its own --init-state —
# never by changing total_shards, which every running shard's state file is bound to.
if [ "${#SHARDS[@]}" -lt "$TOTAL_SHARDS" ]; then
    echo "NOTICE: total_shards=$TOTAL_SHARDS, deployed shard(s) ${SHARDS[*]}: shards $(( ${#SHARDS[@]} + 1 ))..$TOTAL_SHARDS are declared, not deployed (add them later as shardN.server lines; never change total_shards)"
fi

# ---------- resolve every shard (all of them, even with --shard, for the clash check) ----------

declare -A S_SERVER S_HOST S_PORT S_USER S_KEY S_LPORT S_STATE S_ADDR
for sid in "${SHARDS[@]}"; do
    server=$(parse_conf "$CONFIG_FILE" "$PROFILE" "shard${sid}.server")
    host_file="$INVENTORY_DIR/$INVENTORY_TENANT/hosts/$server"
    if [ ! -f "$host_file" ]; then
        echo "ERROR: shard $sid: host file not found: $host_file"; exit 1
    fi
    S_SERVER[$sid]="$server"
    S_HOST[$sid]=$(parse_kv "$host_file" host)
    S_PORT[$sid]=$(parse_kv "$host_file" port)
    S_USER[$sid]=$(parse_kv "$host_file" user)
    S_KEY[$sid]=$(parse_kv "$host_file" key)
    lport_override=$(parse_kv "$host_file" listen_port_override)
    S_LPORT[$sid]="${lport_override:-$LISTEN_PORT}"
    S_STATE[$sid]="${STATE_PATH_TMPL//\{shard_id\}/$sid}"
    case "$LISTEN_ADDRESS" in
        private_ip)
            S_ADDR[$sid]=$(parse_kv "$host_file" private_ip)
            if [ -z "${S_ADDR[$sid]}" ]; then
                echo "ERROR: shard $sid: listen_address = private_ip but $host_file has no private_ip="; exit 1
            fi ;;
        *)  S_ADDR[$sid]="$LISTEN_ADDRESS" ;;
    esac
    if [ -n "${S_ADDR[$sid]}" ] && ! [[ "${S_ADDR[$sid]}" =~ ^[0-9a-fA-F:.]+$ ]]; then
        echo "ERROR: shard $sid: listen_address '${S_ADDR[$sid]}' is not an IP address"; exit 1
    fi
    if [ -z "${S_HOST[$sid]}" ] || [ -z "${S_PORT[$sid]}" ] || [ -z "${S_USER[$sid]}" ] || [ -z "${S_KEY[$sid]}" ]; then
        echo "ERROR: shard $sid: host file missing host/port/user/key: $host_file"; exit 1
    fi
done

# Two shards on one host must not share a port or a state file: they would overwrite each
# other, and one shard's counters would be read by another.
declare -A SEEN_PORT SEEN_STATE
for sid in "${SHARDS[@]}"; do
    pk="${S_HOST[$sid]}:${S_LPORT[$sid]}"
    sk="${S_HOST[$sid]}:${S_STATE[$sid]}"
    if [ -n "${SEEN_PORT[$pk]:-}" ]; then
        echo "ERROR: shards ${SEEN_PORT[$pk]} and $sid both listen on $pk — set listen_port_override in one host file"; exit 1
    fi
    if [ -n "${SEEN_STATE[$sk]:-}" ]; then
        echo "ERROR: shards ${SEEN_STATE[$sk]} and $sid share state file ${S_STATE[$sid]} on ${S_HOST[$sid]} — put {shard_id} in state_path"; exit 1
    fi
    SEEN_PORT[$pk]=$sid
    SEEN_STATE[$sk]=$sid
done

if [ -n "$ONLY_SHARD" ]; then
    if ! [[ "$ONLY_SHARD" =~ ^[0-9]+$ ]] || [ "$ONLY_SHARD" -lt 1 ] || [ "$ONLY_SHARD" -gt "${#SHARDS[@]}" ]; then
        echo "ERROR: --shard must be one of the deployed shards 1..${#SHARDS[@]} (total_shards=$TOTAL_SHARDS)"; exit 1
    fi
    SHARDS=("$ONLY_SHARD")
fi

# ---------- print plan ----------

cat <<EOF
========================================================
  bucket-next deploy
========================================================

Tenant:       $TENANT
Profile:      $PROFILE  ($DESCRIPTION)
Inventory:    $INVENTORY_TENANT
Cluster:      total_shards=$TOTAL_SHARDS  state_path=$STATE_PATH_TMPL
Bind:         $( [ -n "$LISTEN_ADDRESS" ] && echo "listen_address=$LISTEN_ADDRESS" || echo "ALL interfaces (set listen_address — the API has no authentication)" )
Segment:      size=$SEGMENT_SIZE  watermark=$SEGMENT_WATERMARK
uuid key:     $( [ -n "$UUID_KEY" ] && echo "from $UUID_KEY_FILE" || echo "NOT SET — built-in key (unique, but predictable)" )
Build:        $( [ "$SKIP_BUILD" = true ] && echo "SKIPPED" || echo "yes (GOOS=linux GOARCH=amd64)" )
Init state:   $( [ "$INIT_STATE" = 1 ] && echo "yes — create missing state files" || echo "no — a missing state file fails the deploy" )
Dry-run:      $DRY_RUN

Will deploy to ${#SHARDS[@]} shard(s):
EOF
for sid in "${SHARDS[@]}"; do
    printf "  shard %-3s -> %-20s %s@%s:%s  listen=%s:%s  state=%s\n" "$sid" "${S_SERVER[$sid]}" \
        "${S_USER[$sid]}" "${S_HOST[$sid]}" "${S_PORT[$sid]}" "${S_ADDR[$sid]:-*}" "${S_LPORT[$sid]}" "${S_STATE[$sid]}"
done
echo

# ---------- build ----------

if [ "$SKIP_BUILD" = false ]; then
    echo "--- Build phase ---"
    mkdir -p "$BIN_DIR"
    (cd "$PROJECT_DIR" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$BIN_DIR/bucket-next"     ./cmd/bucket-next)
    (cd "$PROJECT_DIR" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$BIN_DIR/bucket-next-cli" ./cmd/bucket-next-cli)
    ls -lh "$BIN_DIR/bucket-next" "$BIN_DIR/bucket-next-cli"
    echo
fi

if [ ! -f "$BIN_DIR/bucket-next" ] || [ ! -f "$BIN_DIR/bucket-next-cli" ]; then
    echo "ERROR: binaries missing under $BIN_DIR (run without --skip-build, or 'make build' first)"
    exit 1
fi

# ---------- per-shard deploy ----------

# render_config <shard_id>   →   writes YAML to stdout
render_config() {
    local sid="$1"
    cat <<EOF
# bucket-next config — generated by deploy.sh
# tenant=$TENANT profile=$PROFILE shard=$sid/$TOTAL_SHARDS
# Do not edit by hand; re-run deploy.sh to regenerate.

shard_id: $sid
total_shards: $TOTAL_SHARDS
listen_address: "${S_ADDR[$sid]}"
listen_port: ${S_LPORT[$sid]}
state_path: ${S_STATE[$sid]}

segment_size: $SEGMENT_SIZE
segment_refill_watermark: $SEGMENT_WATERMARK
clock_drift_tolerance_ms: $CLOCK_DRIFT_TOL_MS
snowflake_epoch_ms: $SNOWFLAKE_EPOCH_MS
EOF
    if [ -n "$UUID_KEY" ]; then
        echo "uuid_key: $UUID_KEY"
    fi
}

deploy_one_shard() {
    local sid="$1"
    local host="${S_HOST[$sid]}" port="${S_PORT[$sid]}" user="${S_USER[$sid]}" lport="${S_LPORT[$sid]}"
    local ssh_key="$INVENTORY_DIR/$INVENTORY_TENANT/keys/${S_KEY[$sid]}"

    if [ "$DRY_RUN" = false ] && [ ! -f "$ssh_key" ]; then
        echo "  [shard $sid] ERROR: SSH key not found: $ssh_key"
        return 1
    fi

    echo "========================================================"
    echo "  Shard $sid  ->  ${S_SERVER[$sid]} ($user@$host:$port  unit=bucket-next@$sid  listen=$lport)"
    echo "========================================================"

    local stage
    stage=$(mktemp -d)
    render_config "$sid"               > "$stage/config.yaml"
    cp "$DEPS_DIR/bucket-next@.service" "$stage/bucket-next@.service"
    cp "$BIN_DIR/bucket-next"           "$stage/bucket-next"
    cp "$BIN_DIR/bucket-next-cli"       "$stage/bucket-next-cli"
    cp "$DEPS_DIR/remote-setup.sh"      "$stage/remote-setup.sh"
    chmod +x "$stage/remote-setup.sh"

    if [ "$DRY_RUN" = true ]; then
        echo "  [DRY-RUN] would copy to $user@$host: $(ls "$stage" | tr '\n' ' ')"
        echo "  [DRY-RUN] config.yaml (as /etc/bucket-next/shard-$sid.yaml):"
        sed -E 's/^(uuid_key: ).*/\1<hidden>/; s/^/    | /' "$stage/config.yaml"
        rm -rf "$stage"
        return 0
    fi

    local ctrl="/tmp/bn-ssh-$$-$sid"
    local server_addr="$user@$host"
    ssh -o ControlPath="$ctrl" -o ControlMaster=yes -o ControlPersist=300 -o StrictHostKeyChecking=accept-new \
        -o ConnectTimeout=10 -p "$port" -i "$ssh_key" -fN "$server_addr"
    # shellcheck disable=SC2064
    trap "ssh -O exit -o ControlPath='$ctrl' '$server_addr' 2>/dev/null || true; rm -rf '$stage'" RETURN

    local remote_tmp="/tmp/bn-deploy-$$-$sid"
    ssh -o ControlPath="$ctrl" "$server_addr" "mkdir -p $remote_tmp && chmod 700 $remote_tmp"
    scp -q -o ControlPath="$ctrl" -P "$port" \
        "$stage/bucket-next" "$stage/bucket-next-cli" "$stage/config.yaml" \
        "$stage/bucket-next@.service" "$stage/remote-setup.sh" \
        "$server_addr:$remote_tmp/"
    if ! ssh -o ControlPath="$ctrl" "$server_addr" \
        "bash $remote_tmp/remote-setup.sh $remote_tmp $sid '${S_STATE[$sid]}' $INIT_STATE; rc=\$?; rm -rf $remote_tmp; exit \$rc"; then
        echo "  [shard $sid] ERROR: remote setup failed"
        return 1
    fi

    # The node must report exactly the identity we deployed. Probe the address it binds.
    local probe="${S_ADDR[$sid]}"
    case "$probe" in ""|0.0.0.0|::) probe=127.0.0.1 ;; esac
    [[ "$probe" == *:* ]] && probe="[$probe]"
    echo "  [shard $sid] verifying http://$probe:$lport/shard-info ..."
    local info
    info=$(ssh -o ControlPath="$ctrl" "$server_addr" "curl -s --max-time 5 http://$probe:$lport/shard-info" || true)
    if echo "$info" | grep -q "\"shardId\":$sid," && echo "$info" | grep -q "\"totalShards\":$TOTAL_SHARDS,"; then
        echo "  [shard $sid] OK — $info"
    else
        echo "  [shard $sid] ERROR — unexpected /shard-info: ${info:-<no answer>}"
        return 1
    fi
}

echo "--- Deploy phase ---"
FAILED=()
for sid in "${SHARDS[@]}"; do
    if ! deploy_one_shard "$sid"; then
        FAILED+=("$sid")
    fi
    echo
done

# ---------- summary ----------

echo "========================================================"
if [ "${#FAILED[@]}" -eq 0 ] && [ "$DRY_RUN" = true ]; then
    echo "  Dry run OK for ${#SHARDS[@]} shard(s) — nothing was changed."
elif [ "${#FAILED[@]}" -eq 0 ]; then
    echo "  All ${#SHARDS[@]} shard(s) deployed successfully."
    if [ "$DRY_RUN" = false ] && [ "${#SHARDS[@]}" -gt 1 ]; then
        echo "  Compare uuidKeyFingerprint and snowflakeEpochMs above — they must match on every shard."
    fi
else
    echo "  ${#FAILED[@]} shard(s) FAILED: ${FAILED[*]}"
fi
echo "========================================================"

[ "${#FAILED[@]}" -eq 0 ]

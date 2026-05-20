#!/bin/bash
#
# Build & deploy bucket-next to every shard in a tenant's profile.
#
# Usage:
#   deploy.sh <tenant> <profile>                  # build + deploy to all shards
#   deploy.sh <tenant> <profile> --shard N        # build + deploy to one shard
#   deploy.sh <tenant> <profile> --skip-build     # deploy existing binaries
#   deploy.sh <tenant> <profile> --dry-run        # show plan, don't ssh
#   deploy.sh --list                              # list tenants/profiles
#
# Example:
#   deploy.sh link3 dev
#   deploy.sh link3 prod --shard 2
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
TENANTS_DIR="$SCRIPT_DIR/tenants"
INVENTORY_DIR="$SCRIPT_DIR/ssh-inventory"
DEPS_DIR="$SCRIPT_DIR/dependencies"
BIN_DIR="$PROJECT_DIR/bin"

# ---------- arg parsing ----------

TENANT=""
PROFILE=""
ONLY_SHARD=""
SKIP_BUILD=false
DRY_RUN=false

usage() {
    cat <<EOF
Usage: $(basename "$0") <tenant> <profile> [options]
       $(basename "$0") --list

Options:
  --shard N         deploy only shard N (1-based)
  --skip-build      use existing bin/bucket-next + bin/bucket-next-cli
  --dry-run         print the plan and the rendered configs, don't ssh
  --list            list available tenants and their profiles
  -h, --help        this message

EOF
}

list_tenants() {
    echo "Tenants under $TENANTS_DIR:"
    for f in "$TENANTS_DIR"/*.conf; do
        [ -f "$f" ] || continue
        local name
        name=$(basename "$f" .conf)
        echo "  $name"
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
        --shard)        ONLY_SHARD="$2"; shift 2 ;;
        --skip-build)   SKIP_BUILD=true; shift ;;
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
    local file="$1"
    local section="$2"
    local key="$3"
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

# parse_kv <file> <key>  → reads `key=value` from a flat host file
parse_kv() {
    local file="$1"; local key="$2"
    grep -E "^${key}=" "$file" 2>/dev/null | head -1 | cut -d= -f2-
}

# list all keys in a section
list_section_keys() {
    local file="$1"; local section="$2"
    awk -v section="[$section]" '
        $0 == section { in_section=1; next }
        /^\[/ { in_section=0 }
        in_section && /=/ && !/^[ \t]*#/ {
            line=$0
            gsub(/^[ \t]+/, "", line)
            sub(/[ \t]*=.*$/, "", line)
            print line
        }
    ' "$file"
}

# Verify profile section exists
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
STATE_PATH=$(parse_conf "$CONFIG_FILE" "$PROFILE" state_path)
SEGMENT_SIZE=$(parse_conf "$CONFIG_FILE" "$PROFILE" segment_size)
SEGMENT_WATERMARK=$(parse_conf "$CONFIG_FILE" "$PROFILE" segment_refill_watermark)
CLOCK_DRIFT_TOL_MS=$(parse_conf "$CONFIG_FILE" "$PROFILE" clock_drift_tolerance_ms)
SNOWFLAKE_EPOCH_MS=$(parse_conf "$CONFIG_FILE" "$PROFILE" snowflake_epoch_ms)

# Defaults
: "${LISTEN_PORT:=7001}"
: "${SEGMENT_SIZE:=1000}"
: "${SEGMENT_WATERMARK:=0.9}"
: "${CLOCK_DRIFT_TOL_MS:=0}"
: "${SNOWFLAKE_EPOCH_MS:=1704067200000}"

if [ -z "$INVENTORY_TENANT" ]; then
    echo "ERROR: [$PROFILE].inventory_tenant is required in $CONFIG_FILE"
    exit 1
fi
if [ -z "$TOTAL_SHARDS" ]; then
    echo "ERROR: [$PROFILE].total_shards is required in $CONFIG_FILE"
    exit 1
fi

# Collect shard list
SHARDS=()
for k in $(list_section_keys "$CONFIG_FILE" "$PROFILE"); do
    if [[ "$k" =~ ^shard([0-9]+)\.server$ ]]; then
        SHARDS+=("${BASH_REMATCH[1]}")
    fi
done

# Sort numerically
IFS=$'\n' SHARDS=($(printf '%s\n' "${SHARDS[@]}" | sort -n)); unset IFS

if [ "${#SHARDS[@]}" -eq 0 ]; then
    echo "ERROR: no shardN.server entries in [$PROFILE] of $CONFIG_FILE"
    exit 1
fi
if [ "${#SHARDS[@]}" -ne "$TOTAL_SHARDS" ]; then
    echo "ERROR: total_shards=$TOTAL_SHARDS but found ${#SHARDS[@]} shardN.server entries: ${SHARDS[*]}"
    exit 1
fi
for i in "${!SHARDS[@]}"; do
    if [ "${SHARDS[$i]}" != "$((i+1))" ]; then
        echo "ERROR: shards must be numbered 1..$TOTAL_SHARDS without gaps; got ${SHARDS[*]}"
        exit 1
    fi
done

# Filter to a single shard if requested
if [ -n "$ONLY_SHARD" ]; then
    if ! [[ "$ONLY_SHARD" =~ ^[0-9]+$ ]] || [ "$ONLY_SHARD" -lt 1 ] || [ "$ONLY_SHARD" -gt "$TOTAL_SHARDS" ]; then
        echo "ERROR: --shard must be in 1..$TOTAL_SHARDS"
        exit 1
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
Cluster:      total_shards=$TOTAL_SHARDS  listen_port=$LISTEN_PORT  state_path=$STATE_PATH
Segment:      size=$SEGMENT_SIZE  watermark=$SEGMENT_WATERMARK
Build:        $( [ "$SKIP_BUILD" = true ] && echo "SKIPPED" || echo "yes (GOOS=linux GOARCH=amd64)" )
Dry-run:      $DRY_RUN

Will deploy to ${#SHARDS[@]} shard(s):
EOF
for sid in "${SHARDS[@]}"; do
    server=$(parse_conf "$CONFIG_FILE" "$PROFILE" "shard${sid}.server")
    host_file="$INVENTORY_DIR/$INVENTORY_TENANT/hosts/$server"
    host=$(parse_kv "$host_file" host)
    port=$(parse_kv "$host_file" port)
    user=$(parse_kv "$host_file" user)
    printf "  shard %d -> %-25s %s@%s:%s\n" "$sid" "$server" "$user" "$host" "$port"
done
echo

# ---------- build ----------

if [ "$SKIP_BUILD" = false ]; then
    echo "--- Build phase ---"
    mkdir -p "$BIN_DIR"
    (cd "$PROJECT_DIR" && \
        GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$BIN_DIR/bucket-next"     ./cmd/bucket-next)
    (cd "$PROJECT_DIR" && \
        GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$BIN_DIR/bucket-next-cli" ./cmd/bucket-next-cli)
    echo "Built:"
    ls -lh "$BIN_DIR/bucket-next" "$BIN_DIR/bucket-next-cli"
    echo
fi

if [ ! -f "$BIN_DIR/bucket-next" ] || [ ! -f "$BIN_DIR/bucket-next-cli" ]; then
    echo "ERROR: binaries missing under $BIN_DIR (run without --skip-build, or 'make build' first)"
    exit 1
fi

# ---------- per-shard deploy ----------

# render_config <shard_id> <listen_port_effective>   →   writes YAML to stdout
render_config() {
    local sid="$1"; local lport="$2"
    cat <<EOF
# bucket-next config — generated by deploy.sh
# tenant=$TENANT profile=$PROFILE shard=$sid/$TOTAL_SHARDS
# Do not edit by hand; re-run deploy.sh to regenerate.

shard_id: $sid
total_shards: $TOTAL_SHARDS
listen_port: $lport
state_path: $STATE_PATH

segment_size: $SEGMENT_SIZE
segment_refill_watermark: $SEGMENT_WATERMARK
clock_drift_tolerance_ms: $CLOCK_DRIFT_TOL_MS
snowflake_epoch_ms: $SNOWFLAKE_EPOCH_MS
EOF
}

# render_systemd <shard_id>   →   writes unit file to stdout
render_systemd() {
    local sid="$1"
    sed \
        -e "s|__TENANT__|$TENANT|g" \
        -e "s|__PROFILE__|$PROFILE|g" \
        -e "s|__SHARD_ID__|$sid|g" \
        -e "s|__TOTAL_SHARDS__|$TOTAL_SHARDS|g" \
        "$DEPS_DIR/bucket-next.service.tmpl"
}

deploy_one_shard() {
    local sid="$1"
    local server host port user key_name privip lport_override lport
    server=$(parse_conf "$CONFIG_FILE" "$PROFILE" "shard${sid}.server")
    local host_file="$INVENTORY_DIR/$INVENTORY_TENANT/hosts/$server"

    if [ ! -f "$host_file" ]; then
        echo "  [shard $sid] ERROR: host file not found: $host_file"
        return 1
    fi
    host=$(parse_kv "$host_file" host)
    port=$(parse_kv "$host_file" port)
    user=$(parse_kv "$host_file" user)
    key_name=$(parse_kv "$host_file" key)
    privip=$(parse_kv "$host_file" private_ip)
    lport_override=$(parse_kv "$host_file" listen_port_override)
    lport="${lport_override:-$LISTEN_PORT}"

    if [ -z "$host" ] || [ -z "$port" ] || [ -z "$user" ] || [ -z "$key_name" ]; then
        echo "  [shard $sid] ERROR: host file missing host/port/user/key: $host_file"
        return 1
    fi

    local ssh_key="$INVENTORY_DIR/$INVENTORY_TENANT/keys/$key_name"
    if [ "$DRY_RUN" = false ] && [ ! -f "$ssh_key" ]; then
        echo "  [shard $sid] ERROR: SSH key not found: $ssh_key"
        return 1
    fi

    echo "========================================================"
    echo "  Shard $sid  ->  $server ($user@$host:$port  listen=$lport)"
    echo "========================================================"

    # render configs to a per-deploy temp dir
    local stage
    stage=$(mktemp -d)
    render_config "$sid" "$lport"   > "$stage/config.yaml"
    render_systemd "$sid"           > "$stage/bucket-next.service"
    cp "$BIN_DIR/bucket-next"       "$stage/bucket-next"
    cp "$BIN_DIR/bucket-next-cli"   "$stage/bucket-next-cli"
    cp "$DEPS_DIR/remote-setup.sh"  "$stage/remote-setup.sh"
    chmod +x "$stage/remote-setup.sh"

    if [ "$DRY_RUN" = true ]; then
        echo "  [DRY-RUN] would copy these files to $user@$host:"
        ls -la "$stage"
        echo "  [DRY-RUN] config.yaml:"
        sed 's/^/    | /' "$stage/config.yaml"
        rm -rf "$stage"
        return 0
    fi

    local ctrl="/tmp/bn-ssh-$$-$sid"
    local server_addr="$user@$host"
    local ssh_base=(-o ControlPath="$ctrl" -o StrictHostKeyChecking=accept-new
                    -o ConnectTimeout=10 -p "$port" -i "$ssh_key")

    # Open multiplexed connection
    ssh "${ssh_base[@]}" -o ControlMaster=yes -o ControlPersist=300 -fN "$server_addr"
    trap "ssh -O exit -o ControlPath='$ctrl' '$server_addr' 2>/dev/null || true; rm -rf '$stage'" RETURN

    # Push tarball over the multiplexed channel
    local remote_tmp="/tmp/bn-deploy-$$-$sid"
    ssh -o ControlPath="$ctrl" "$server_addr" "mkdir -p $remote_tmp"
    scp -o ControlPath="$ctrl" -P "$port" \
        "$stage/bucket-next" "$stage/bucket-next-cli" \
        "$stage/config.yaml" "$stage/bucket-next.service" \
        "$stage/remote-setup.sh" \
        "$server_addr:$remote_tmp/"
    ssh -o ControlPath="$ctrl" "$server_addr" \
        "bash $remote_tmp/remote-setup.sh $remote_tmp && rm -rf $remote_tmp"

    # Health probe
    echo "  [shard $sid] verifying via /health..."
    if ssh -o ControlPath="$ctrl" "$server_addr" \
        "curl -s --max-time 5 http://127.0.0.1:$lport/health" \
        | grep -q '"shard"'; then
        echo "  [shard $sid] OK — service healthy"
    else
        echo "  [shard $sid] WARN — health probe did not return expected payload"
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
if [ "${#FAILED[@]}" -eq 0 ]; then
    echo "  All ${#SHARDS[@]} shard(s) deployed successfully."
else
    echo "  ${#FAILED[@]} shard(s) FAILED: ${FAILED[*]}"
fi
echo "========================================================"

[ "${#FAILED[@]}" -eq 0 ]

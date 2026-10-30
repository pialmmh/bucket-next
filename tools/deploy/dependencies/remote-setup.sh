#!/bin/bash
#
# remote-setup.sh — runs on the target server after remote-deploy-v2.sh copied the files.
# Installs one shard: binaries, its config, the template systemd unit, then starts
# bucket-next@<shard_id>. Several shards can live on one host; each has its own unit,
# config and state directory.
#
# Args: <REMOTE_TMP> <SHARD_ID> <STATE_DIR> <INIT_STATE: 0|1>
# Files staged in REMOTE_TMP: bucket-next, bucket-next-cli, config.yaml, bucket-next@.service

set -euo pipefail

REMOTE_TMP="${1:?REMOTE_TMP required}"
SHARD_ID="${2:?SHARD_ID required}"
STATE_DIR="${3:?STATE_DIR required}"
INIT_STATE="${4:-0}"

SERVICE_USER="bucket-next"
INSTALL_DIR="/opt/bucket-next"
CONF_DIR="/etc/bucket-next"
CONF="$CONF_DIR/shard-$SHARD_ID.yaml"
UNIT="bucket-next@$SHARD_ID"

echo "  [remote] Ensuring service user '$SERVICE_USER' exists..."
id -u "$SERVICE_USER" >/dev/null 2>&1 || sudo useradd -r -s /bin/false -d "$INSTALL_DIR" "$SERVICE_USER"

echo "  [remote] Ensuring state dir $STATE_DIR..."
sudo mkdir -p "$STATE_DIR"
sudo chown "$SERVICE_USER:$SERVICE_USER" "$STATE_DIR"
sudo chmod 0750 "$STATE_DIR"

# Pre-template deployments kept one state file at /var/lib/bucket-next/state.json. Carry it into this shard's directory
# when the shard has no state of its own; the service then carries it over into legacy_tenant, and refuses it if it
# belongs to another shard.
LEGACY_STATE="/var/lib/bucket-next/state.json"
if ! sudo test -f "$STATE_DIR/shard.json" && ! sudo test -f "$STATE_DIR/state.json" && sudo test -f "$LEGACY_STATE"; then
    echo "  [remote] Moving legacy state file $LEGACY_STATE -> $STATE_DIR/state.json"
    sudo mv "$LEGACY_STATE" "$STATE_DIR/state.json"
    sudo chown "$SERVICE_USER:$SERVICE_USER" "$STATE_DIR/state.json"
fi

# What will the NEW binary do with this shard's state? Asked with the staged cli and the staged config, read-only, before
# anything is installed: a refusal leaves the running shard exactly as it runs.
echo "  [remote] Checking the shard's state with the new binary (read-only)..."
set +e
CHECK=$(sudo "$REMOTE_TMP/bucket-next-cli" -config "$REMOTE_TMP/config.yaml" check 2>&1)
RC=$?
set -e
echo "$CHECK" | sed 's/^/  [remote]   /'
NEED_INIT=0
case "$RC" in
    0) ;;
    3)
        if [ "$INIT_STATE" = "1" ]; then
            NEED_INIT=1
        else
            echo "  [remote] ERROR: shard $SHARD_ID has no state in $STATE_DIR."
            echo "  [remote] For a brand-new shard, deploy with --init-state. If the state was lost, restore it from backup —"
            echo "  [remote] starting a fresh one would re-issue IDs. Nothing was installed."
            exit 1
        fi
        ;;
    *)
        echo "  [remote] ERROR: the new binary would refuse this state (above). Nothing was installed; $UNIT runs as it ran."
        exit 1
        ;;
esac

# Pre-template deployments used a single bucket-next.service; retire it.
if systemctl list-unit-files bucket-next.service >/dev/null 2>&1 && [ -f /etc/systemd/system/bucket-next.service ]; then
    echo "  [remote] Retiring legacy bucket-next.service..."
    sudo systemctl disable --now bucket-next.service >/dev/null 2>&1 || true
    sudo rm -f /etc/systemd/system/bucket-next.service
fi

echo "  [remote] Installing binaries to $INSTALL_DIR..."
sudo mkdir -p "$INSTALL_DIR" "$CONF_DIR"
sudo install -m 0755 -o root -g root "$REMOTE_TMP/bucket-next"     "$INSTALL_DIR/bucket-next"
sudo install -m 0755 -o root -g root "$REMOTE_TMP/bucket-next-cli" "$INSTALL_DIR/bucket-next-cli"

echo "  [remote] Installing config $CONF (0640, may hold the uuid key)..."
sudo install -m 0640 -o root -g "$SERVICE_USER" "$REMOTE_TMP/config.yaml" "$CONF"
[ ! -f "$REMOTE_TMP/version.txt" ] || sudo install -m 0644 -o root -g root "$REMOTE_TMP/version.txt" "$INSTALL_DIR/version-shard-$SHARD_ID.txt"

if [ "$NEED_INIT" = "1" ]; then
    echo "  [remote] Creating the state of brand-new shard $SHARD_ID in $STATE_DIR (--init-state)..."
    sudo -u "$SERVICE_USER" "$INSTALL_DIR/bucket-next-cli" -config "$CONF" init-state
fi

echo "  [remote] Installing systemd template unit..."
sudo install -m 0644 "$REMOTE_TMP/bucket-next@.service" /etc/systemd/system/bucket-next@.service
sudo systemctl daemon-reload
sudo systemctl enable "$UNIT" >/dev/null 2>&1

echo "  [remote] Restarting $UNIT..."
sudo systemctl restart "$UNIT"

echo "  [remote] Waiting for $UNIT to become active..."
for _ in 1 2 3 4 5 6 7 8 9 10; do
    if sudo systemctl is-active --quiet "$UNIT"; then
        echo "  [remote] OK — $UNIT active"
        sudo journalctl -u "$UNIT" -n 20 --no-pager 2>/dev/null | grep -E 'state |listening' | sed 's/^/  [remote]   /' || true
        exit 0
    fi
    sleep 1
done

echo "  [remote] ERROR — $UNIT failed to become active. Recent logs:"
sudo journalctl -u "$UNIT" -n 30 --no-pager
exit 1

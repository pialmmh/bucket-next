#!/bin/bash
#
# remote-setup.sh — runs on the target server after files are copied.
# Installs the systemd unit, creates user/dirs, starts the service.
#
# Invoked by deploy.sh with these env vars set:
#   TENANT, PROFILE, SHARD_ID, TOTAL_SHARDS
# And these files staged in REMOTE_TMP:
#   bucket-next, bucket-next-cli, config.yaml, bucket-next.service
#
# Args: <REMOTE_TMP>

set -e

REMOTE_TMP="${1:?REMOTE_TMP required}"
SERVICE_USER="bucket-next"
INSTALL_DIR="/opt/bucket-next"
DATA_DIR="/var/lib/bucket-next"

echo "  [remote] Ensuring service user '$SERVICE_USER' exists..."
id -u "$SERVICE_USER" >/dev/null 2>&1 || sudo useradd -r -s /bin/false -d "$INSTALL_DIR" "$SERVICE_USER"

echo "  [remote] Creating install and data dirs..."
sudo mkdir -p "$INSTALL_DIR" "$DATA_DIR"

echo "  [remote] Installing binaries..."
sudo install -m 0755 -o "$SERVICE_USER" -g "$SERVICE_USER" "$REMOTE_TMP/bucket-next"     "$INSTALL_DIR/bucket-next"
sudo install -m 0755 -o "$SERVICE_USER" -g "$SERVICE_USER" "$REMOTE_TMP/bucket-next-cli" "$INSTALL_DIR/bucket-next-cli"

echo "  [remote] Installing config..."
sudo install -m 0644 -o "$SERVICE_USER" -g "$SERVICE_USER" "$REMOTE_TMP/config.yaml" "$INSTALL_DIR/config.yaml"

echo "  [remote] Setting data dir ownership..."
sudo chown -R "$SERVICE_USER:$SERVICE_USER" "$DATA_DIR"
sudo chmod 0750 "$DATA_DIR"

echo "  [remote] Installing systemd unit..."
sudo install -m 0644 "$REMOTE_TMP/bucket-next.service" /etc/systemd/system/bucket-next.service
sudo systemctl daemon-reload
sudo systemctl enable bucket-next >/dev/null 2>&1

echo "  [remote] Starting service..."
sudo systemctl restart bucket-next

echo "  [remote] Waiting for service to become active..."
for i in 1 2 3 4 5 6 7 8 9 10; do
    if sudo systemctl is-active --quiet bucket-next; then
        echo "  [remote] OK — service active"
        exit 0
    fi
    sleep 1
done

echo "  [remote] ERROR — service failed to become active. Recent logs:"
sudo journalctl -u bucket-next -n 30 --no-pager
exit 1

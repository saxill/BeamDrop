#!/usr/bin/env bash
# Builds the arm64 beamdrop binary and deploys it to the Pi relay, then
# restarts the user systemd service.
#
# Usage: ./deploy.sh user@host
#   user@host  the relay to deploy to, e.g. saxill@<relay-ip>.
#
# The relay runs headless as a user systemd unit (beamdrop.service) with
# Restart=always, so a fresh binary is picked up by restarting that unit.
set -euo pipefail

cd "$(dirname "$0")/.."

if [ $# -lt 1 ]; then
    echo "usage: $0 user@host" >&2
    exit 1
fi
HOST="$1"
GO_BIN="${GO_BIN:-go}"

echo ">> building arm64 binary (CGO_ENABLED=0)"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 "$GO_BIN" build -o /tmp/beamdrop-arm64 ./cmd/beamdrop

echo ">> copying to ${HOST} (temp name, then mv so the running binary is never half-written)"
scp /tmp/beamdrop-arm64 "${HOST}:/home/saxill/beamdrop.new"
ssh "${HOST}" "mv /home/saxill/beamdrop.new /home/saxill/beamdrop && chmod +x /home/saxill/beamdrop"

echo ">> restarting the relay service"
ssh "${HOST}" "systemctl --user restart beamdrop && sleep 2 && systemctl --user is-active beamdrop"

echo ">> done. The relay is running:"
ssh "${HOST}" "ps -o pid,cmd -C beamdrop | grep -v grep"

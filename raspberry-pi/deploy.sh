#!/usr/bin/env bash
# Builds the arm64 beamdrop binary and deploys it to the Pi relay, then
# installs (if needed) and restarts the user systemd service, and verifies
# the relay is actually serving.
#
# Usage: ./deploy.sh [user@host]
#
# Which relay to deploy to, in order:
#   1. the argument (user@host, or just host)
#   2. $PI_HOST (same forms)
#   3. the host in ~/.config/beamdrop/relay.addr — the same address the
#      laptop app dials, so the deploy target can never drift from what
#      the app actually uses. Remote user then defaults to $PI_USER
#      (default "saxill").
#
# Safe to re-run: every step is idempotent, and the binary is copied under
# a temp name and moved into place so the running one is never half-written.
set -euo pipefail

cd "$(dirname "$0")/.."

PI_USER="${PI_USER:-saxill}"
TARGET="${1:-${PI_HOST:-}}"

if [ -z "$TARGET" ]; then
    RELAY_ADDR="${HOME}/.config/beamdrop/relay.addr"
    if [ -f "$RELAY_ADDR" ]; then
        TARGET="${PI_USER}@$(sed 's/:[0-9]*$//' "$RELAY_ADDR" | tr -d '[:space:]')"
        echo ">> deploy target from $RELAY_ADDR: ${TARGET}"
    else
        echo "no relay address known: pass one ('./deploy.sh user@host')," >&2
        echo "set PI_HOST, or create ~/.config/beamdrop/relay.addr" >&2
        exit 1
    fi
fi
case "$TARGET" in
    *@*) ;;
    *) TARGET="${PI_USER}@${TARGET}" ;;
esac

if ! ssh -o BatchMode=yes -o ConnectTimeout=5 "$TARGET" true 2>/dev/null; then
    echo "!! cannot ssh to ${TARGET} (key-based only — no password prompts here)." >&2
    echo "   Is the relay on? Is Tailscale up? Skipping the relay deploy." >&2
    exit 1
fi

echo ">> building arm64 binary (CGO_ENABLED=0)"
# Go does not have to be pre-installed: same user-local bootstrap as
# packaging/install.sh, so this script runs unattended too.
if ! command -v go >/dev/null; then
    if [ ! -x "${HOME}/.local/go/bin/go" ]; then
        GO_ARCH="$(uname -m)"; case "$GO_ARCH" in x86_64) GO_ARCH=amd64;; aarch64) GO_ARCH=arm64;; esac
        echo ">> go not found — installing go1.24.5 to ~/.local/go…"
        curl -fsSL "https://go.dev/dl/go1.24.5.linux-${GO_ARCH}.tar.gz" | tar -xz -C "${HOME}/.local"
    fi
    export PATH="${HOME}/.local/go/bin:$PATH"
fi
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/beamdrop-arm64 ./cmd/beamdrop

echo ">> copying to ${TARGET} (temp name, then mv so the running binary is never half-written)"
scp -q /tmp/beamdrop-arm64 "${TARGET}:beamdrop.new"
ssh "$TARGET" 'mv "$HOME/beamdrop.new" "$HOME/beamdrop" && chmod +x "$HOME/beamdrop"'

echo ">> installing the systemd unit (idempotent)"
scp -q raspberry-pi/beamdrop.service "${TARGET}:beamdrop.service.new"
ssh "$TARGET" 'mkdir -p "$HOME/.config/systemd/user" && mv "$HOME/beamdrop.service.new" "$HOME/.config/systemd/user/beamdrop.service" && systemctl --user daemon-reload && systemctl --user enable --now beamdrop >/dev/null 2>&1 || systemctl --user enable beamdrop'

echo ">> restarting the relay service"
ssh "$TARGET" 'systemctl --user restart beamdrop && sleep 2 && systemctl --user is-active beamdrop'

# Any HTTP answer means the portal is serving; the web UI redirects (308),
# so don't demand 200.
HOST_ONLY="${TARGET#*@}"
if curl -s -m 8 -o /dev/null "http://${HOST_ONLY}:4747/"; then
    echo ">> relay answers on http://${HOST_ONLY}:4747 — deploy verified."
else
    echo "!! service is active but http://${HOST_ONLY}:4747 did not answer." >&2
    exit 1
fi

# First-time convenience: the laptop app dials the relay through this file.
RELAY_ADDR="${HOME}/.config/beamdrop/relay.addr"
if [ ! -f "$RELAY_ADDR" ]; then
    mkdir -p "$(dirname "$RELAY_ADDR")"
    echo "${HOST_ONLY}:4747" > "$RELAY_ADDR"
    echo ">> wrote $RELAY_ADDR (${HOST_ONLY}:4747) so the laptop app dials this relay."
fi

echo ">> done. The relay is running:"
ssh "$TARGET" 'ps -o pid,cmd -C beamdrop | grep -v grep'
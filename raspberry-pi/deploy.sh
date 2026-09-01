#!/usr/bin/env bash
# Builds the beamdrop binary for the relay's own architecture, ships it to
# the relay, installs (if needed) and restarts the user systemd service,
# and verifies the relay is actually serving.
#
# Usage: ./deploy.sh [user@host]
#
# Which relay to deploy to, in order:
#   1. the argument (user@host, or just host)
#   2. $PI_HOST (same forms)
#   3. the host in ~/.config/beamdrop/relay.addr — the same address the
#      laptop app dials, so the deploy target can never drift from what
#      the app actually uses
#   4. otherwise: the Linux machines your tailnet knows about, listed to
#      pick from — a first run should not require knowing an IP by heart
#
# The remote user defaults to $PI_USER, which defaults to *this* machine's
# user name. The first run is allowed to ask two things (which machine is
# the relay, and the relay's ssh password once, to install a key); every
# later run is unattended.
#
# The relay's systemd unit is rendered per deploy: --relay-to is the
# hostname of the machine running this script, because that is the name
# its portal presents when it dials the relay in. Override with RELAY_TO
# when deploying from a machine that is not the destination.
#
# Safe to re-run: every step is idempotent, and the binary is copied under
# a temp name and moved into place so the running one is never half-written.
set -euo pipefail

cd "$(dirname "$0")/.."

PI_USER="${PI_USER:-$(id -un)}"
TARGET="${1:-${PI_HOST:-}}"

if [ -z "$TARGET" ]; then
    RELAY_ADDR="${HOME}/.config/beamdrop/relay.addr"
    if [ -f "$RELAY_ADDR" ]; then
        TARGET="${PI_USER}@$(sed 's/:[0-9]*$//' "$RELAY_ADDR" | tr -d '[:space:]')"
        echo ">> deploy target from $RELAY_ADDR: ${TARGET}"
    fi
fi

if [ -z "$TARGET" ]; then
    # Nothing remembered. Offer the Linux machines this tailnet knows
    # about — a relay has to be a box you can ssh to and run a linux
    # binary on, so phones and tablets are not candidates.
    MACHINES=""
    if command -v tailscale >/dev/null 2>&1 && command -v python3 >/dev/null 2>&1; then
        # NB: the script goes through -c, not a heredoc — a heredoc would
        # claim stdin and the pipe from tailscale would never be read.
        MACHINES="$(tailscale status --json 2>/dev/null | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for p in (d.get("Peer") or {}).values():
    if p.get("Online") and str(p.get("OS", "")).lower() == "linux":
        print(p.get("HostName") or "?", (p.get("TailscaleIPs") or ["?"])[0])
' || true)"
    fi
    if [ -z "$MACHINES" ]; then
        echo "no relay address known and no candidate on your tailnet." >&2
        echo "pass one ('./deploy.sh user@host'), set PI_HOST, or create" >&2
        echo "~/.config/beamdrop/relay.addr" >&2
        exit 1
    fi
    mapfile -t NAMES < <(printf '%s\n' "$MACHINES" | awk '{print $1}')
    mapfile -t ADDRS < <(printf '%s\n' "$MACHINES" | awk '{print $2}')
    if [ "${#ADDRS[@]}" -eq 1 ]; then
        echo ">> one candidate on your tailnet: ${NAMES[0]} (${ADDRS[0]}) — using it as the relay."
        TARGET="${PI_USER}@${ADDRS[0]}"
    elif [ -t 0 ]; then
        echo ">> machines on your tailnet — which one is the relay?"
        echo
        i=0
        while [ "$i" -lt "${#ADDRS[@]}" ]; do
            i=$((i+1))
            echo "  ${i}) ${NAMES[$((i-1))]}  (${ADDRS[$((i-1))]})"
        done
        echo
        while :; do
            read -r -p "relay [1-${#ADDRS[@]}]: " PICK
            case "$PICK" in
                ''|*[!0-9]*) echo "enter a number 1-${#ADDRS[@]}" >&2; continue ;;
            esac
            [ "$PICK" -ge 1 ] && [ "$PICK" -le "${#ADDRS[@]}" ] && break
            echo "enter a number 1-${#ADDRS[@]}" >&2
        done
        TARGET="${PI_USER}@${ADDRS[$((PICK-1))]}"
    else
        echo "no relay address known and no terminal to ask on." >&2
        echo "run with an argument: ./deploy.sh user@host" >&2
        exit 1
    fi
fi

case "$TARGET" in
    *@*) ;;
    *) TARGET="${PI_USER}@${TARGET}" ;;
esac

# Key-based ssh is what makes every run after the first unattended. The
# first run may have to install the key — offer it rather than just fail.
if ! ssh -o BatchMode=yes -o ConnectTimeout=5 "$TARGET" true 2>/dev/null; then
    if [ -t 0 ] && command -v ssh-copy-id >/dev/null 2>&1; then
        echo ">> no key-based ssh to ${TARGET} yet."
        echo "   ssh-copy-id installs this machine's key once — it asks for"
        echo "   ${TARGET}'s password, and every later run is passwordless."
        read -r -p "   run ssh-copy-id ${TARGET} now? [Y/n] " REPLY
        case "$REPLY" in
            n*|N*) ;;
            *) ssh-copy-id "$TARGET" || true ;;
        esac
    fi
fi
if ! ssh -o BatchMode=yes -o ConnectTimeout=5 "$TARGET" true 2>/dev/null; then
    echo "!! cannot ssh to ${TARGET} — key-based only, no password prompts here." >&2
    echo "   Run this once and re-run the deploy:" >&2
    echo "       ssh-copy-id ${TARGET}" >&2
    exit 1
fi

# A user systemd service dies with the last login session unless lingering
# is on — which for a relay means it stops the moment this ssh
# disconnects, after passing every check below. Gate on it up front.
LINGER="$(ssh "$TARGET" 'loginctl show-user "$(id -un)" --property=Linger 2>/dev/null' || true)"
case "$LINGER" in
    *Linger=yes) ;;
    *)
        echo ">> lingering is off on ${TARGET} — without it the relay stops"
        echo "   when this ssh session ends. Trying to enable it…"
        if ssh "$TARGET" 'sudo -n loginctl enable-linger "$(id -un)"' 2>/dev/null; then
            echo ">> linger enabled."
        else
            echo "!! could not enable linger (sudo needs a password there)." >&2
            echo "   Run this once on the relay, then re-run the deploy:" >&2
            echo "       ssh -t ${TARGET} 'sudo loginctl enable-linger \$USER'" >&2
            exit 1
        fi
        ;;
esac

# Build (or fetch) for the architecture the relay actually reports.
REMOTE_ARCH="$(ssh "$TARGET" 'uname -m')"
case "$REMOTE_ARCH" in
    x86_64)          GOARCH_RELAY="amd64" ;;
    aarch64|arm64)   GOARCH_RELAY="arm64" ;;
    *)
        echo "!! the relay is ${REMOTE_ARCH}; deploy supports x86_64 and aarch64." >&2
        exit 1
        ;;
esac

# With a Go toolchain here, build from the tree you cloned — that is the
# code you mean to run. Without one, fetch the matching release binary
# rather than downloading a whole toolchain for a single build.
if command -v go >/dev/null 2>&1 || [ -x "${HOME}/.local/go/bin/go" ]; then
    export PATH="${HOME}/.local/go/bin:$PATH"
    echo ">> building ${GOARCH_RELAY} binary from source (CGO_ENABLED=0)"
    CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH_RELAY" go build -o /tmp/beamdrop-relay ./cmd/beamdrop
else
    echo ">> no go toolchain here — fetching the release binary for ${GOARCH_RELAY}"
    if ! ./packaging/fetch-release.sh "$GOARCH_RELAY" /tmp/beamdrop-relay; then
        echo ">> no release binary either — installing go1.24.5 to ~/.local/go…"
        mkdir -p "${HOME}/.local"
        GO_ARCH="$(uname -m)"; case "$GO_ARCH" in x86_64) GO_ARCH=amd64;; aarch64|arm64) GO_ARCH=arm64;; esac
        GO_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
        case "$GO_OS" in
            linux)  GO_TUPLE="linux-${GO_ARCH}" ;;
            darwin) GO_TUPLE="darwin-${GO_ARCH}" ;;
            *) echo "!! unsupported build host $(uname -s)" >&2; exit 1 ;;
        esac
        curl -fsSL "https://go.dev/dl/go1.24.5.${GO_TUPLE}.tar.gz" | tar -xz -C "${HOME}/.local"
        export PATH="${HOME}/.local/go/bin:$PATH"
        CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH_RELAY" go build -o /tmp/beamdrop-relay ./cmd/beamdrop
    fi
fi

echo ">> copying to ${TARGET} (temp name, then mv so the running binary is never half-written)"
scp -q /tmp/beamdrop-relay "${TARGET}:beamdrop.new"
ssh "$TARGET" 'mv "$HOME/beamdrop.new" "$HOME/beamdrop" && chmod +x "$HOME/beamdrop"'
rm -f /tmp/beamdrop-relay

# Render the unit with THIS machine's hostname: a portal presents
# os.Hostname() as its name on the wire, so "--relay-to <this hostname>"
# is what makes the relay forward to the laptop that deployed it.
RELAY_TO="${RELAY_TO:-$(hostname)}"
if [ -z "$RELAY_TO" ]; then
    echo "!! could not read this machine's hostname — set RELAY_TO and re-run." >&2
    exit 1
fi
UNIT_TMP="$(mktemp)"
sed "s|__RELAY_TO__|${RELAY_TO}|g" raspberry-pi/beamdrop.service > "$UNIT_TMP"
if grep -q '__RELAY_TO__' "$UNIT_TMP"; then
    rm -f "$UNIT_TMP"
    echo "!! the unit template failed to render." >&2
    exit 1
fi
echo ">> installing the systemd unit (relay-to: ${RELAY_TO})"
scp -q "$UNIT_TMP" "${TARGET}:beamdrop.service.new"
rm -f "$UNIT_TMP"
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
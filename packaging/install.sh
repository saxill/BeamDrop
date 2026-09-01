#!/usr/bin/env bash
# Installs beamdrop for the current user: the Go binary, the desktop app and
# its venv, a menu entry and an icon.
#
# Deliberately user-local — no sudo, nothing written outside $HOME, and
# uninstalling is deleting the paths printed at the end.
set -euo pipefail

cd "$(dirname "$0")/.."

BIN_DIR="${HOME}/.local/bin"
APP_DIR="${HOME}/.local/share/applications"
ICON_DIR="${HOME}/.local/share/icons/hicolor/scalable/apps"
LIB_DIR="${HOME}/.local/share/beamdrop"

mkdir -p "$BIN_DIR" "$APP_DIR" "$ICON_DIR" "$LIB_DIR"

echo "installing the engine…"
# A prebuilt binary from this repo's releases when one matches this
# machine; built from source otherwise. The fetch verifies checksums and
# sanity-runs the binary, and any failure falls through to the build —
# so the download can only ever save time, never break an install.
case "$(uname -s):$(uname -m)" in
    Linux:x86_64)               ENGINE_ARCH="amd64" ;;
    Linux:aarch64|Linux:arm64) ENGINE_ARCH="arm64" ;;
    *)                          ENGINE_ARCH="" ;;
esac
ENGINE_OK=0
if [ -n "$ENGINE_ARCH" ] && ./packaging/fetch-release.sh "$ENGINE_ARCH" "$BIN_DIR/beamdrop" 2>/dev/null; then
    echo "engine: prebuilt ${ENGINE_ARCH} binary from the latest GitHub release."
    ENGINE_OK=1
fi
if [ "$ENGINE_OK" -ne 1 ]; then
    echo "engine: building from source…"
    # Go does not have to be pre-installed: first run installs it
    # user-locally (no sudo, nothing outside $HOME); later runs reuse it.
    if ! command -v go >/dev/null; then
        if [ ! -x "${HOME}/.local/go/bin/go" ]; then
            GO_ARCH="$(uname -m)"; case "$GO_ARCH" in x86_64) GO_ARCH=amd64;; aarch64|arm64) GO_ARCH=arm64;; esac
            mkdir -p "${HOME}/.local"
            echo "go not found — installing go1.24.5 to ~/.local/go…"
            curl -fsSL "https://go.dev/dl/go1.24.5.linux-${GO_ARCH}.tar.gz" | tar -xz -C "${HOME}/.local"
        fi
        export PATH="${HOME}/.local/go/bin:$PATH"
    fi
    go build -o "$BIN_DIR/beamdrop" ./cmd/beamdrop
fi

echo "installing the window…"
# Qt lives in its own venv rather than being asked of the system Python:
# no sudo needed, and a distro upgrade cannot change the toolkit under it.
if [ ! -x "$LIB_DIR/venv/bin/python" ]; then
  python3 -m venv "$LIB_DIR/venv"
fi
"$LIB_DIR/venv/bin/pip" install --quiet --upgrade pip
"$LIB_DIR/venv/bin/pip" install --quiet -r laptop/requirements.txt
install -m 0644 laptop/beamdrop_ui.py "$LIB_DIR/beamdrop_ui.py"

cat > "$BIN_DIR/beamdrop-app" <<LAUNCH
#!/usr/bin/env bash
exec "$LIB_DIR/venv/bin/python" "$LIB_DIR/beamdrop_ui.py" "\$@"
LAUNCH
chmod +x "$BIN_DIR/beamdrop-app"

install -m 0644 packaging/beamdrop.svg "$ICON_DIR/beamdrop.svg"

# GNOME here never resolves the theme-name form of Icon= (this user's hicolor
# tree has no index.theme), and the tray app's notifications need a PNG — an
# SVG path or theme name is dropped from the banner. So render the SVG to a
# PNG once, and point both the menu entry and the notifications at it.
echo "rendering the icon…"
QT_QPA_PLATFORM=offscreen "$LIB_DIR/venv/bin/python" - <<RENDER
from PySide6.QtGui import QGuiApplication, QImage, QPainter
from PySide6.QtSvg import QSvgRenderer
import os

app = QGuiApplication([])  # QImage/QPainter need a Gui instance even offscreen
src = os.path.expanduser("~/.local/share/icons/hicolor/scalable/apps/beamdrop.svg")
dst = os.path.expanduser("~/.local/share/beamdrop/beamdrop.png")
img = QImage(128, 128, QImage.Format.Format_ARGB32)
img.fill(0)  # fully transparent
painter = QPainter(img)
QSvgRenderer(src).render(painter)
painter.end()
img.save(dst)
RENDER

# Icon= in the template stays the portable theme-name form; the installed copy
# is rewritten to the absolute PNG because that is the only form this GNOME
# actually renders.
sed "s|^Icon=.*|Icon=$LIB_DIR/beamdrop.png|" \
  packaging/beamdrop.desktop > "$APP_DIR/beamdrop.desktop"

# Without these the launcher may not notice until the next login.
update-desktop-database "$APP_DIR" 2>/dev/null || true
gtk-update-icon-cache -f -t "${HOME}/.local/share/icons/hicolor" 2>/dev/null || true

echo "deploying the relay (raspberry-pi/deploy.sh; --no-pi skips this)…"
if [ "${1:-}" = "--no-pi" ]; then
    echo "skipped (--no-pi)."
else
    if ! ./raspberry-pi/deploy.sh; then
        echo
        echo "The laptop app is installed and working; only the relay deploy"
        echo "was skipped. Re-run packaging/install.sh when the Pi is on."
    fi
fi

cat <<DONE

installed:
  $BIN_DIR/beamdrop          the CLI and engine (portal, send, watch, spool)
  $BIN_DIR/beamdrop-app      the desktop window
  $LIB_DIR/                  its Qt venv
  $APP_DIR/beamdrop.desktop  menu entry
  $ICON_DIR/beamdrop.svg     icon
  $LIB_DIR/beamdrop.png      rendered icon (menu entry + notifications)
  relay                      arm64 binary + systemd unit on the Pi, restarted

Search for "beamdrop" in your applications menu, or run: beamdrop-app
DONE

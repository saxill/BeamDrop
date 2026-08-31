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

echo "building the engine…"
go build -o "$BIN_DIR/beamdrop" ./cmd/beamdrop

echo "installing the window…"
# Qt lives in its own venv rather than being asked of the system Python:
# no sudo needed, and a distro upgrade cannot change the toolkit under it.
if [ ! -x "$LIB_DIR/venv/bin/python" ]; then
  python3 -m venv "$LIB_DIR/venv"
fi
"$LIB_DIR/venv/bin/pip" install --quiet --upgrade pip
"$LIB_DIR/venv/bin/pip" install --quiet -r ui/requirements.txt
install -m 0644 ui/beamdrop_ui.py "$LIB_DIR/beamdrop_ui.py"

cat > "$BIN_DIR/beamdrop-app" <<LAUNCH
#!/usr/bin/env bash
exec "$LIB_DIR/venv/bin/python" "$LIB_DIR/beamdrop_ui.py" "\$@"
LAUNCH
chmod +x "$BIN_DIR/beamdrop-app"

install -m 0644 packaging/beamdrop.svg "$ICON_DIR/beamdrop.svg"
install -m 0644 packaging/beamdrop.desktop "$APP_DIR/beamdrop.desktop"

# Without these the launcher may not notice until the next login.
update-desktop-database "$APP_DIR" 2>/dev/null || true
gtk-update-icon-cache -f -t "${HOME}/.local/share/icons/hicolor" 2>/dev/null || true

cat <<DONE

installed:
  $BIN_DIR/beamdrop          the CLI and engine (portal, send, watch, spool)
  $BIN_DIR/beamdrop-app      the desktop window
  $LIB_DIR/                  its Qt venv
  $APP_DIR/beamdrop.desktop  menu entry
  $ICON_DIR/beamdrop.svg     icon

Search for "beamdrop" in your applications menu, or run: beamdrop-app
DONE

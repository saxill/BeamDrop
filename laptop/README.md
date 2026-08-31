# Laptop side

The desktop app: a PySide6/Qt tray app (`beamdrop_ui.py`) that spawns the
`beamdrop portal` binary as a child process and talks to it over the
loopback API.

## What lives here

- `beamdrop_ui.py` — the tray app. It starts the portal with
  `--connect-to <relay>` when `~/.config/beamdrop/relay.addr` exists, so the
  laptop dials the Pi relay and stays connected to it.
- `requirements.txt` — Python deps (`PySide6-Essentials`, `qrcode`).

## Install

The common `packaging/install.sh` (repo root) installs the Go binary, this
app, its venv, a menu entry and an icon — all user-local, no sudo.

## How the laptop reaches the phone

The laptop has one peer: the Pi relay. When you send with "All devices"
selected, the message goes to the relay, which fans it out to the phone.
The relay's address is read from `~/.config/beamdrop/relay.addr` (e.g.
`<relay-ip>:4747`).

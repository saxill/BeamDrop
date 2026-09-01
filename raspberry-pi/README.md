# Raspberry Pi side

The always-on relay. The Pi runs the same `beamdrop portal` binary as the
laptop, but as a headless store-and-forward hub: it accepts files and
messages addressed to peers that are offline and delivers them when they
come back. Any x86_64 or arm64 Linux box works; a Pi is just the usual one.

## What lives here

- `beamdrop.service` — the *template* for the user systemd unit. Its
  `__RELAY_TO__` placeholder is replaced by deploy.sh with the hostname of
  the deploying laptop — the name a portal presents on the wire — so the
  relay forwards to whoever deployed it.
- `deploy.sh` — builds for the architecture the relay reports, ships it,
  installs the unit, restarts the service and verifies the relay answers.
  Run from the laptop.

## Deploy

```sh
./deploy.sh [user@host]
```

The target is resolved in order: the argument, `$PI_HOST`, the address in
`~/.config/beamdrop/relay.addr` (the same address the laptop app dials, so
the two cannot drift), and — on a first run with nothing saved — a list of
your tailnet's Linux machines to pick from. The remote user defaults to
`$PI_USER`, which defaults to your local user name.

The first run is allowed to be interactive; every later run is unattended:

- **ssh.** Key-based ssh is what keeps later runs unattended. If the key
  is not installed yet, the script offers to run `ssh-copy-id` once —
  you type the relay's password one time, and never again.
- **Lingering.** A user systemd service stops with its last login session,
  so a relay deployed without linger passes every check and then dies the
  moment the deploy disconnects. The script checks `loginctl`, enables
  lingering with passwordless sudo when it can, and otherwise prints the
  one command to run by hand and stops:
  `ssh -t <relay> 'sudo loginctl enable-linger $USER'`.
- **Pairing.** Nothing to do. A relay under systemd has no terminal, so it
  accepts every new peer from your tailnet and logs each acceptance with
  the peer's name and derived code — the tailnet is the security model.
  Your laptop dials it, your phone opens its page, both are remembered by
  key.

The deploy itself: builds for the relay's own architecture from the tree
you cloned (or fetches the matching release binary when no Go toolchain is
on this machine), copies under a temp name and moves it into place so the
running binary is never half-written, renders and (re)installs the unit,
restarts it, writes `~/.config/beamdrop/relay.addr` on the laptop if it
was missing, and ends with an HTTP check — so a successful run means the
relay is actually serving.

`packaging/install.sh` runs this at the end of every install; `--no-pi`
skips it.

## How the relay routes

- **Phone → laptop:** anything arriving from a phone is spooled for the
  relay target (`--relay-to <laptop>`) and delivered by the forwarder when
  the laptop is reachable.
- **Laptop → phone:** anything arriving *from* the relay target is
  broadcast to every other connected peer (the phone), instead of being
  spooled back to the laptop.

The forwarder delivers over a connection the destination already has open
when it can, so a laptop that dials the relay (`--connect-to`) is not
dialled back and evicted from its own registry.
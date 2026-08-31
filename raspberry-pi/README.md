# Raspberry Pi side

The always-on relay. The Pi runs the same `beamdrop portal` binary as the
laptop, but as a headless store-and-forward hub: it accepts files and
messages addressed to peers that are offline and delivers them when they
come back.

## What lives here

- `beamdrop.service` — the user systemd unit. Runs
  `beamdrop portal --relay --relay-to laptop` with `Restart=always`.
- `deploy.sh` — builds the arm64 binary and ships it to the Pi, then
  restarts the service.

## Install the service

```sh
cp beamdrop.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now beamdrop
```

## Deploy a new build

```sh
./deploy.sh saxill@<relay-ip>
```

## How the relay routes

- **Phone → laptop:** anything arriving from a phone is spooled for the
  relay target (`--relay-to laptop`) and delivered by the forwarder when the
  laptop is reachable.
- **Laptop → phone:** anything arriving *from* the relay target is broadcast
  to every other connected peer (the phone), instead of being spooled back
  to the laptop.

The forwarder delivers over a connection the destination already has open
when it can, so a laptop that dials the relay (`--connect-to`) is not
dialled back and evicted from its own registry.

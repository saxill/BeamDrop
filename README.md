# beamdrop

Send files between your laptop and your iPhone over a custom binary
protocol, on your own machines, with nobody else in the path.

One Go binary. Works on the same WiFi, and over [Tailscale](https://tailscale.com)
when the phone is on cellular and the laptop is behind NAT. No third-party
service, no port forwarding, no native app — the phone side is a web page
the laptop serves, and the relay is a machine you own.

## What you need

- a [Tailscale](https://tailscale.com) tailnet — it is the network *and*
  the security boundary (see below)
- a Linux laptop to send from and receive on
- any always-on Linux box for the relay — a Raspberry Pi is the norm;
  x86_64 works identically
- an iPhone (the phone side is a web page, so any modern browser works,
  but the polish is iOS-shaped)

## Install

From the laptop:

```bash
git clone https://github.com/saxill/BeamDrop.git
cd BeamDrop && ./packaging/install.sh
```

One command, no sudo, everything user-local: a prebuilt engine binary from
this repo's releases (built from source only when no release matches your
machine), the desktop app with its own Qt venv, a menu entry, an icon —
and then the relay deployed to your always-on machine.

The first run is the only run that asks anything:

- *which machine is the relay?* — your tailnet's Linux machines are
  listed and you pick. (Later runs remember the address.)
- *the relay's ssh password, once* — to install your key with
  `ssh-copy-id`, after which every later run is passwordless.

Two things it will not guess its way past. Ssh must be key-based after
that first offer — that is what keeps later runs unattended. And the
relay's user account must have *lingering* enabled: a user systemd service
dies with its last login session, so a relay without it would stop the
moment the deploy's ssh disconnects. The deploy enables it with
passwordless sudo when it can, and otherwise prints the one command to
run and stops — rather than deploy a relay that would quietly die.

Pairing is not a step you perform. Your laptop dials the relay and is
accepted; your phone opens the relay's page and is accepted; both are then
remembered by key and never prompt again. What "accepted" means is the
next section.

## Security model

**Your tailnet is the boundary.** Pairing's 6-digit code is compared by a
human only when both ends are interactive — `beamdrop send` between two
machines someone is sitting at. A relay running under systemd has no
terminal and nobody to ask, so it accepts every new peer on its own: the
code is still derived and logged with the peer's name, but nothing
compares it. What carries the weight instead is reachability — the relay
is only reachable from your tailnet, so "who can pair" reduces to "who is
on my tailnet". Treat tailnet membership as full access: a paired device
can upload, and sees the filenames of everything that has passed through.

The weaker door — the Shortcuts upload endpoint — sits behind the same
boundary plus a bearer token. Delete `upload.token` on the relay and that
door stops being registered at all.

## Repository layout

The code is organised by where it runs. The common Go backend lives at the
root; each device's specific code lives in its own folder.

```
.                 common — the Go backend (cmd/, internal/, packaging/)
├── Phone/        the page the phone opens (copy of the embedded web UI)
├── laptop/       the PySide6 desktop app (beamdrop_ui.py)
└── raspberry-pi/ the always-on relay (systemd unit + deploy script)
```

The phone-facing web UI is embedded into the Go binary at build time
(`internal/webui/static/`); `Phone/` holds a working copy so the phone-side
source has one obvious home. See `Phone/README.md` for keeping them in sync.

## Running it by hand

No installer, no relay — just the binary and a phone on the same network
or tailnet:

```bash
go build -o beamdrop ./cmd/beamdrop
./beamdrop portal
```

It prints the URLs to open on the phone, best one first:

```
beamdrop · inbox: /home/you/Portal/inbox

beamdrop portal — type :send /path/to/file or :q to quit
open one of these on your phone:
  https://laptop.tailnet.ts.net:4747   (no warning — use this one)
  https://100.64.0.1:4747   (tailnet — works off-WiFi)
  https://192.168.1.11:4747    (same WiFi)
```

Open the first one in Safari. The phone pairs, shows a 6-digit code, and the
laptop asks you to confirm it — check the two match, press `y`. That is the
only time you will be asked: both sides remember each other afterwards.

### Why the first URL is the one to use

That line only appears when Tailscale can issue a real, publicly trusted
certificate for this machine's MagicDNS name, and it is worth using because
the self-signed alternative costs more than one tap:

- Safari's warning has to be tapped through on every device, and it *hard*
  refuses some certificates with no way forward at all;
- a service worker needs a secure context, which a tapped-through
  certificate does not reliably provide — so the page will not install as an
  app properly;
- iOS does not extend a manually accepted certificate to WebSockets, so
  pairing can fail after the page itself has loaded.

The IP addresses below it still work exactly as before, self-signed warning
and all. Both certificates are held at once and chosen per connection: the
MagicDNS name gets the real one, an IP gets the self-signed one. Nothing
here is required — with no Tailscale, or with HTTPS turned off for your
tailnet, the line is simply absent.

To enable it: turn on **HTTPS Certificates** in the Tailscale admin console
(DNS page). If beamdrop runs as a non-root user, that user also needs
permission to ask tailscaled for a certificate:

```bash
sudo tailscale set --operator=$USER
```

One consequence worth knowing: issuing a certificate publishes that
hostname to public [Certificate Transparency](https://certificate.transparency.dev/)
logs, permanently. The machine stays unreachable from the internet — a
tailnet IP is private and Tailscale still gates access — but the *name*
becomes public. Skip this and use the IP if you would rather it did not.

Then drop files either way. `:send /path/to/file` in the portal pushes to
every connected phone; the drop zone on the page sends the other direction.
Incoming files land in `~/Portal/inbox`.

## The three modes

| Command | What it does |
|---|---|
| `beamdrop portal` | The TUI plus the phone-facing page, sharing one port. This is the thing everything else connects to. |
| `beamdrop send <file>` | Ship one file to a running portal and exit. |
| `beamdrop watch <dir>` | Ship every new or changed file in a directory. Pairs once, then streams. |

With no `--peer`, `send` and `watch` work out where to go themselves:

1. a peer you have paired with before, at the address it was last reached on
   — this is what finds a laptop over Tailscale, where broadcast does not
   reach;
2. a broadcast probe on the local network, for a laptop on the same WiFi you
   have never paired with;
3. a portal on this same machine.

So the common case is just:

```bash
beamdrop send report.pdf
```

Name a host when you want to override that:

```bash
beamdrop send report.pdf --peer 100.64.0.1
beamdrop watch ~/Screenshots --peer laptop.tailnet.ts.net
```

Flags: `--peer`, `--port` on `send`/`watch`; `--port`, `--inbox` on `portal`.

## Adding it to your home screen

The page is a PWA, so it installs like an app rather than living in a
browser tab that iOS suspends and forgets.

In Safari, open the address, then **Share → Add to Home Screen**. You get
the mushroom icon, no browser chrome, and the app shell is cached so it
opens instantly instead of showing white while it finds the laptop.

Nothing transferred is cached — files arrive over the WebSocket into memory
and are saved by you. Caching them would quietly leave copies on the phone
that nothing ever clears.

### It remembers what happened

Opening the app asks the portal what has already passed between the two of
you and shows it under an "earlier" divider. Before this the page built its
feed purely from live events, so it opened blank every single time and
anything sent while it was closed was invisible — which looks identical to
the app being connected and doing nothing.

A past file shows an **Open** button rather than arriving with the history.
The page holds no bytes for anything it did not receive this session, so it
asks the portal to send that one file — which comes back down the ordinary
transfer path and ends up with the same preview and Save link as anything
else. Fetching all of them on every reconnect would be worse than the tap,
particularly on cellular.

The name a peer asks for is reduced to its base component before it is
joined to anything, and the result is then checked to be a regular file
directly inside the inbox — so neither `../` nor a symlink planted in the
inbox turns this into a way to read the rest of the disk.

A paired device is shown the whole conversation with that machine, not only
its own traffic. That is the point — one conversation rather than a separate
one per device — but it does mean pairing hands over the inbox's filenames.

### Notifications

Tap the 🔔 in the header and the phone is told when something arrives, even
with the app closed. On iOS that is the only way to find out: the system
suspends a home-screen app within seconds of backgrounding it, taking the
WebSocket with it.

The button only appears when it can actually work, which needs all of:

- a **publicly trusted** certificate — the Tailscale one above. A
  tapped-through self-signed certificate does not qualify;
- the app added to the home screen (iOS 16.4+);
- permission, which can only be requested from a tap.

Registration travels over the paired WebSocket rather than an HTTP endpoint,
so it inherits the pairing that already happened. An unprotected subscribe
endpoint would let any node on your tailnet register to receive your
filenames.

This is the one part of beamdrop that talks to the public internet: a push
goes to Apple's push service, which then wakes the phone. Nothing else can —
the phone is asleep and only its vendor can reach it. The payload is
encrypted with keys that service does not have, so it learns that a message
went to a device and roughly how big it was, not what it said. The VAPID
keypair lives in `~/.config/beamdrop/push/` and must not be deleted:
it is baked into every subscription your phone has already made, so
replacing it makes notifications stop silently while the phone still
believes it is subscribed.

## Sending from iOS Shortcuts

The drop zone needs Safari open and a picker tapped. For a share-sheet
action instead, the portal accepts an upload:

```
POST http://<your-laptop>:4747/upload
Authorization: Bearer <token>
X-Beamdrop-Filename: IMG_0001.jpeg

<file bytes>
```

Get the token (created the first time the portal starts):

```bash
cat ~/.config/beamdrop/upload.token
```

Then in Shortcuts: **Get Contents of URL** -> Method `POST`, Request Body
`File`, with those two headers. Add it to the share sheet and any photo is
two taps from your laptop.

Two deliberate choices, both worth understanding before you use it:

- **Plain HTTP, not HTTPS.** Tailscale traffic is already WireGuard-encrypted
  end to end, so TLS would be double encryption for no gain -- and Shortcuts
  rejects a self-signed certificate outright, with none of the tap-through
  Safari offers. HTTPS is precisely what would break it.
- **This door is narrower than the front one.** The frame protocol
  authenticates a peer with X25519 and a code you confirm. This
  authenticates a request with a bearer token, which is weaker. So it is
  reachable *only* from the tailnet: a leaked token is not enough on its
  own, the caller also has to be a node on your tailnet. If you would rather
  not have the door at all, delete `upload.token` -- the endpoint stops
  being registered.

## Always-on relay (a Pi, or any machine that stays up)

Both ends have to be awake at the same moment, which a laptop is not. Run
beamdrop on something that never sleeps and it will hold files for you:

```bash
beamdrop portal --relay
```

The phone always sends to the relay — it is always up, so that send always
succeeds. The relay then decides:

```
phone ──POST──▶ relay (always on)
                 │
                 ├─ laptop up?   ──▶ deliver now, notify on the laptop
                 └─ laptop down? ──▶ hold on disk
                                      └─ retry ──▶ deliver when it returns
```

Tell the relay where things go by default:

```bash
beamdrop portal --relay --relay-to my-laptop
```

Then a phone just uploads, with no extra headers to configure — every one
of those is a row to type on a phone keyboard. Override per file with
`X-Beamdrop-To: some-other-peer`, or `X-Beamdrop-To: here` to keep it on
the relay. A name that is not paired with the
relay is refused at upload time rather than held forever for a machine that
can never be resolved.

`--relay-to` also governs the relay's *page*, not just uploads: a file or
message sent from the relay's own web UI is passed on to that same
destination. This is what makes it stop mattering which address you opened.
Install both the relay's page and the laptop's on your phone and either one
reaches the laptop — without it, anything sent to the relay's page landed in
the relay's inbox and simply stayed there, which looks exactly like the app
being connected and doing nothing.

```
phone ──▶ relay's page ──▶ relay's inbox ──▶ passed on to --relay-to
phone ──▶ laptop's page ──▶ laptop's inbox
```

Anything arriving *from* the destination is never passed back to it. On a
machine whose whole job is retrying until delivery succeeds, that would not
be a glitch that settles down — it would be two machines filling each
other's disks.

Note the relay keeps its own copy in its inbox as well as forwarding. On a
Pi that is worth watching: `~/Portal/inbox` grows and nothing prunes it.

Spooled files live in `~/.config/beamdrop/spool` and survive a reboot of the
relay: the payload is committed before its metadata, so a crash mid-write
can never leave something that looks deliverable. Nothing is deleted until
the destination confirms the file's SHA-256 — while a file is spooled, the
relay holds the only copy.

### Setting up a Pi

```bash
./raspberry-pi/deploy.sh           # from the laptop, in the repo
```

It resolves the relay in order: the argument (`user@host`), `$PI_HOST`, the
host in `~/.config/beamdrop/relay.addr` — the same address the laptop app
dials, so the two cannot drift — and, when nothing is known yet, a list of
your tailnet's Linux machines to pick from. Then it: detects the relay's
architecture and builds for it (or fetches the matching release binary when
no Go toolchain is on this machine), copies it across under a temp name so
the running binary is never half-written, renders and installs the systemd
unit, restarts the service, and verifies the relay actually answers on
HTTP. `packaging/install.sh` runs this at the end of every install.

It handles the two classic traps itself:

- **ssh.** If the key is not installed yet it offers to run `ssh-copy-id`
  once (you type the relay's password one time), then proceeds. Every
  later run is passwordless.
- **lingering.** It checks `loginctl`, enables lingering with passwordless
  sudo when it can, and otherwise prints the one command to run and stops
  — a user service without linger dies with the ssh session that deployed
  it, after passing every check.

The only real prerequisites: the box runs Linux, is on your tailnet, and
you can sudo on it once.

The unit it ships is rendered per deploy — the template in
`raspberry-pi/beamdrop.service` has a `__RELAY_TO__` placeholder that
becomes the *deploying laptop's* hostname, because that is the name its
portal presents on the wire:

```ini
[Service]
ExecStart=%h/beamdrop portal --relay --relay-to <deploying-laptop>
Restart=always
```

The upload token the phone's shortcut needs is generated by the portal
itself on first run (`~/.config/beamdrop/upload.token` on the Pi). With
`--relay-to` pointing at your laptop, a phone upload with no destination
header lands on the laptop; `X-Beamdrop-To` overrides per upload.

## How it works

Tailscale is the network. Beamdrop is everything above it.

**One port, two dialects.** The portal listens once and routes each
connection by its first two bytes: a TLS ClientHello goes to the HTTPS
server for the phone, anything else is a raw beamdrop peer. One byte would
not be enough — `0x16` is the TLS handshake record type but also a valid
frame length — so the second byte disambiguates.

**Frames.** `[len:u32 LE][type:u8][payload]`, little-endian throughout, 17
frame types. A transfer is `FILE_OFFER` → `FILE_ACCEPT` → `CHUNK`s (64KB) →
`FILE_DONE` carrying the receiver's computed SHA-256. The sender does not
consider a file delivered until the receiver confirms that hash.

**Pairing.** X25519 key exchange, then
`BLAKE2b-256(sorted(pubA, pubB)) mod 1e6` gives the 6-digit code both sides
display — sorted, so it does not matter who dialled. The shared key is
`BLAKE2b(ECDH ‖ code)`, and both sides prove they derived the same one with
an HMAC-SHA256 challenge/response before anyone is asked to look at a code.

The browser runs the same ceremony as the Go side, using vendored
[@noble](https://github.com/paulmillr/noble-curves) primitives bundled into
one file with no runtime dependencies.

**Discovery.** The portal answers UDP probes rather than beaconing, so a
one-shot `send` gets an answer in milliseconds and an idle laptop is not
putting packets on the wire every two seconds forever. The datagram is its
own small format — magic, version, kind, TCP port, public key, name — and
the reply goes back unicast, so probing does not tell the whole subnet who
is here.

**Trust on first use.** Each machine keeps a long-lived keypair; a paired
peer is remembered by public key and never prompts again. A peer whose key
has changed is refused rather than silently re-paired.

## Where things live

```
~/Portal/inbox/                       received files
~/.config/beamdrop/identity.key       this machine's keypair (0600)
~/.config/beamdrop/known_peers/       remembered peers
~/.config/beamdrop/webui-cert.pem     the self-signed cert, for IP addresses
~/.config/beamdrop/tailscale-cert.pem the real cert, for the MagicDNS name
```

The two certificates are kept apart on purpose: the self-signed one is
regenerated whenever this machine's addresses change, which would be exactly
the wrong thing to do to the other.

Delete a file under `known_peers/` to forget a peer and pair again.

## Layout

```
cmd/beamdrop/          subcommand dispatch
internal/frame/        the wire format
internal/pairing/      X25519, the 6-digit code, TOFU, machine identity
internal/transfer/     sender/receiver state machine, SHA-256 verification
internal/engine/       one engine per peer connection; Registry holds N
internal/netmux/       first-byte port sharing between TLS and raw frames
internal/discovery/    UDP probe/answer, so send needs no address
internal/webui/        TLS server, WebSocket→frame shim, embedded page
internal/push/         VAPID identity, subscriptions, Web Push delivery
internal/webui/static/ the page the phone runs
internal/mode/         portal (TUI + server), send, watch
internal/smoke/        the real browser JS against a real engine over TLS
```

## Tests

```bash
go test ./...
```

`internal/smoke` is the one that matters most: it runs the *actual* page
scripts under Node against a live `webui.Serve` over a real TLS WebSocket,
so the Go↔JS boundary is exercised rather than assumed.

The JS unit suites need Node ≥ 22:

```bash
cd internal/webui/static && node --test ./*.test.mjs
```

## Not here yet

- **Resume.** `FILE_ACCEPT` carries a `resume_from` field that is always 0.
- **Streaming on the phone.** The page holds a whole file in memory, so very
  large videos will struggle from the drop zone. The Shortcuts upload path is
  unaffected.
- **Saving to Photos.** A web page cannot; the page offers a download link
  instead.
- **Renewal without privilege.** A Tailscale certificate is good for 90 days
  and beamdrop re-asks on every start, but if the user it runs as cannot
  reach tailscaled (see `--operator` above) it will keep serving the existing
  certificate until that one expires, then fall back to self-signed.

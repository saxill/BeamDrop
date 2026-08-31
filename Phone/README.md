# Phone side

This is the page the phone opens. It is a copy of the web UI that is
**embedded into the Go binary** at build time (see `internal/webui/static/`
in the repo root — that copy is the one that ships). The copy here is kept
in sync so the phone-facing source lives in one obvious place.

## What the phone does

- Opens the relay's (or laptop's) `https://<host>:4747` address.
- Pairs once (a code is confirmed on the machine running the portal).
- Sends and receives files and text over the same connection the desktop
  app uses.

## Keeping the two copies in sync

The embedded copy is the source of truth for what ships. After editing
here, copy back:

```sh
rsync -a Phone/static/ internal/webui/static/
```

and rebuild the binary. The `//go:embed static` directive in
`internal/webui/assets.go` embeds the whole directory, so any new file here
must be copied back too.

## iOS Shortcuts

The portal also accepts `POST /upload` (token in
`~/.config/beamdrop/upload.token`) for iOS Shortcuts. That endpoint is
served by the same binary; there is no separate phone-side code for it.

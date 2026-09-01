#!/usr/bin/env bash
# Downloads a prebuilt beamdrop engine binary from this project's GitHub
# releases and verifies it against the release checksums.
#
# Usage: fetch-release.sh <amd64|arm64> <output-file>
#
# Exits 0 after moving a verified, executable binary to <output-file>, and
# nonzero on any problem — no release yet, no matching asset, no network,
# a checksum mismatch. Callers fall back to building from source, so this
# can only ever be an optimization.
set -euo pipefail

[ $# -eq 2 ] || exit 1
case "$1" in
    amd64|arm64) ;;
    *) exit 1 ;;
esac

REL_BASE="https://github.com/saxill/BeamDrop/releases/latest/download"
ASSET="beamdrop-linux-$1"
OUT="$2"

# The temp file sits next to the destination so the final move is atomic
# and never lands a half-written binary.
TMP="$(mktemp "${OUT}.XXXXXX")"
trap 'rm -f "$TMP"' EXIT

curl -fsSL -o "$TMP" "${REL_BASE}/${ASSET}" || exit 1

if command -v sha256sum >/dev/null 2>&1; then
    SUMS="$(mktemp)"
    if ! curl -fsSL -o "$SUMS" "${REL_BASE}/beamdrop-checksums.txt"; then
        exit 1
    fi
    WANT="$(awk -v a="$ASSET" '$2==a {print $1}' "$SUMS")"
    rm -f "$SUMS"
    HAVE="$(sha256sum "$TMP" | cut -d' ' -f1)"
    [ -n "$WANT" ] && [ "$WANT" = "$HAVE" ] || exit 1
fi

chmod +x "$TMP"
mv -f "$TMP" "$OUT"
trap - EXIT
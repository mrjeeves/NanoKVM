#!/usr/bin/env bash
# Build the customer-facing USB drive image — the volume the attached machine
# sees when a CEC KVM is plugged in.
#
#   ./scripts/build-usbdisk.sh <output.img.gz>
#
# ONE build step, called by both `just deploy` and .github/workflows/release.yml.
# The two had the mkfs/mcopy sequence copy-pasted between them, which was fine
# while the drive held two static files and stops being fine the moment its
# contents matter: a release that quietly shipped a different drive than a dev
# deploy is the kind of bug nobody finds until a customer is on the phone.
#
# Contents:
#   autorun.inf       drive icon + the name "CEC KVM" in Explorer. NOT for
#                     running anything — Windows disabled AutoRun for removable
#                     drives in 2011 (KB971029) and macOS never had it.
#   cec.ico           the icon autorun.inf points at
#   cecsupport.ps1    fetches and runs the latest CEC Support installer
#   CEC-Support.cmd   what the customer double-clicks (a .ps1 is not
#                     double-clickable; see the file)
#
# The drive carries a launcher rather than the installer itself: the .exe is
# ~37 MB, it would ride inside every over-the-air update for a file used once,
# and the app updates itself on first run anyway.

set -euo pipefail

if [ "$#" -ne 1 ]; then
    echo "usage: $0 <output.img.gz>" >&2
    exit 2
fi

OUT="$1"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$ROOT/support/usbdisk"

# 64 MiB, all but a few KB of it the customer's own scratch space. Mostly zeros,
# so it gzips to well under 100 KB and the release bundle barely notices.
SIZE_MB=64

for tool in mkfs.vfat mcopy mdir; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "error: $tool not found (install dosfstools + mtools)" >&2
        exit 1
    fi
done

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
IMG="$TMP/usbdisk.img"

echo "==> USB drive: ${SIZE_MB} MiB, labelled 'CEC KVM'"

dd if=/dev/zero of="$IMG" bs=1M count=0 seek="$SIZE_MB" 2>/dev/null
mkfs.vfat -n "CEC KVM" "$IMG" >/dev/null

export MTOOLS_SKIP_CHECK=1
# CRLF for everything Windows parses: autorun.inf is read by very old code, and
# a .cmd with LF endings can break on `goto`/label handling. Cheap insurance,
# and a checked-in file with the wrong endings is easy to miss in review.
for f in autorun.inf cecsupport.ps1 CEC-Support.cmd; do
    sed 's/$/\r/' "$SRC/$f" | sed 's/\r\r$/\r/' > "$TMP/$f"
done
mcopy -i "$IMG" "$TMP/autorun.inf"     ::/autorun.inf
mcopy -i "$IMG" "$SRC/cec.ico"         ::/cec.ico
mcopy -i "$IMG" "$TMP/cecsupport.ps1"  ::/cecsupport.ps1
mcopy -i "$IMG" "$TMP/CEC-Support.cmd" ::/CEC-Support.cmd

# Fail loudly if it isn't a filesystem, and if anything the customer needs is
# missing. The device's own checks (S03usbdev's scratch_ready, update.go's
# looksFormatted) exist because a half-formatted drive once had Windows asking
# customers to format their KVM on every boot, forever — same check, applied
# where it is still cheap to fix.
mdir -i "$IMG" :: >/dev/null
for f in ::/autorun.inf ::/cec.ico ::/cecsupport.ps1 ::/CEC-Support.cmd; do
    if ! mdir -i "$IMG" "$f" >/dev/null 2>&1; then
        echo "❌ $f did not land on the drive" >&2
        exit 1
    fi
done

mkdir -p "$(dirname "$OUT")"
gzip -9 -c "$IMG" > "$OUT"

echo "    contents:"
mdir -i "$IMG" :: | sed 's/^/      /'
echo "OK -> $OUT ($(wc -c < "$OUT") bytes gzipped)"

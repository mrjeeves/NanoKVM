#!/usr/bin/env bash
# Bump the NanoKVM release version. Argument is the new version, e.g.
# `./scripts/bump-version.sh 1.1.0`.
#
# Edits:
#   - server/buildinfo/buildinfo.go   const Version = "X.Y.Z"
#       (the single fork-version source: both the mesh presence advert and the
#        firmware updater read it, so we advertise/compare OUR version, never
#        the Sipeed base image's /kvmapp/version)
#   - web/package.json                "version": "X.Y.Z"
#
# After this script: stage + commit + tag — the Justfile's `release` recipe does
# that part (mirrors MyOwnMesh / AllMyStuff).

set -euo pipefail

if [ "$#" -ne 1 ]; then
    echo "usage: $0 <version>" >&2
    exit 2
fi

VERSION="$1"

# Validate looks-like-semver.
if ! echo "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$'; then
    echo "error: '$VERSION' does not look like a semver string" >&2
    exit 2
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILDINFO="$ROOT/server/buildinfo/buildinfo.go"
PKG="$ROOT/web/package.json"

# server/buildinfo/buildinfo.go — the `const Version = "..."` line.
python3 - "$BUILDINFO" "$VERSION" <<'PY'
import re, sys
path, version = sys.argv[1], sys.argv[2]
s = open(path, encoding="utf-8").read()
new, n = re.subn(r'(const Version = ")[^"]*(")', rf'\g<1>{version}\g<2>', s, count=1)
if n != 1:
    print(f"error: could not find `const Version` in {path}", file=sys.stderr)
    sys.exit(1)
open(path, "w", encoding="utf-8").write(new)
print(f"bumped {path} -> {version}")
PY

# web/package.json — the top-level "version" (regex-targeted so the rest of the
# file's formatting is untouched; count=1 takes the package's own version, not a
# dependency's).
if [ -f "$PKG" ]; then
    python3 - "$PKG" "$VERSION" <<'PY'
import re, sys
path, version = sys.argv[1], sys.argv[2]
s = open(path, encoding="utf-8").read()
new, n = re.subn(r'("version"\s*:\s*")[^"]*(")', rf'\g<1>{version}\g<2>', s, count=1)
if n != 1:
    print(f"warning: could not find \"version\" in {path} (skipping)", file=sys.stderr)
else:
    open(path, "w", encoding="utf-8").write(new)
    print(f"bumped {path} -> {version}")
PY
fi

echo "ok"

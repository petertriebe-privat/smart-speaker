#!/bin/sh
# A release's Echo bundle (issue #15): the ARMv7 binary, deploy.sh, files.txt and the files it
# names, in the repository's layout, so the Raspberry Pi installs a release with the bundle's own
# deploy.sh and needs no checkout of its commit. No secrets, no local/: the device's device.json
# and secrets come from the Pi and the PC (services/deployer/check_artifact.py checks it).
#   tools/bundle.sh <binary> <out.tar.gz>
set -eu
[ $# -eq 2 ] || { echo "usage: bundle.sh <binary> <out.tar.gz>" >&2; exit 2; }
ALEXA=$(cd "$(dirname "$0")/.." && pwd)
BIN=$1
OUT=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
put() { # <source> <path in the bundle>
    mkdir -p "$STAGE/$(dirname "$2")"
    cp "$1" "$STAGE/$2"
}
put "$BIN" jarvis
chmod 755 "$STAGE/jarvis"
put "$ALEXA/tools/deploy.sh" devices/echo/tools/deploy.sh
put "$ALEXA/tools/files.txt" devices/echo/tools/files.txt
sed -n 's/^[a-z0-9]* //p' "$ALEXA/tools/files.txt" | while read -r f; do
    put "$ALEXA/$f" "devices/echo/$f"
done
# the same bytes for the same input: sorted, no owner, no times (GNU tar, as on the runners)
tar -C "$STAGE" --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 -cf - jarvis devices |
    gzip -9n >"$OUT"
echo "$OUT: $(du -h "$OUT" | cut -f1)"

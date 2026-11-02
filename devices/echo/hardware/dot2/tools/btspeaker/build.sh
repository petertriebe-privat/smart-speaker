#!/bin/sh
# Builds btspeaker for the Echo Dot 2 in an Alpine armv7 container (Docker with ARM emulation).
# BTstack (BlueKitchen licence: personal, non-commercial use) is fetched at a fixed version into
# devices/echo/local/dot2-tools/btstack and never committed. Output: devices/echo/local/dot2-tools/btspeaker.
set -e
HERE=$(cd "$(dirname "$0")" && pwd)
ALEXA=$(cd "$HERE/../../../.." && pwd)
CACHE="$ALEXA/local/dot2-tools"
BTSTACK_TAG=v1.6.2
BTSTACK_COMMIT=565100c1cae73dc1c5f9b239953dbdd14ae283d3
ALPINE=alpine:3.22

if [ ! -d "$CACHE/btstack" ]; then
    git clone -q --depth 1 --branch "$BTSTACK_TAG" https://github.com/bluekitchen/btstack "$CACHE/btstack"
fi
got=$(git -C "$CACHE/btstack" rev-parse HEAD)
[ "$got" = "$BTSTACK_COMMIT" ] || { echo "BTstack at $got, expected $BTSTACK_COMMIT" >&2; exit 1; }

win() { command -v cygpath >/dev/null 2>&1 && cygpath -m "$1" || echo "$1"; }
MSYS_NO_PATHCONV=1 docker run --rm --platform linux/arm/v7 \
    -v "$(win "$HERE"):/src:ro" -v "$(win "$CACHE/btstack"):/btstack:ro" -v "$(win "$CACHE"):/out" \
    "$ALPINE" sh -c 'apk add -q build-base && cp -r /src /build && cd /build &&
        make -s BTSTACK=/btstack && cp btspeaker /out/btspeaker'
ls -l "$CACHE/btspeaker"

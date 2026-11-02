#!/bin/sh
# Install or update the assistant on the Echo's internal root (run on the Echo as root): main-B on the
# first Echo, main-A on the second (boot-and-root.md), wherever the echo-dev base was installed.
# Expects the files in /mnt/echo-net/assistant-stage (copied by tools/deploy.sh):
#   assistant, settings.json (optional), device.json (optional), assistant-secrets.json (optional),
#   echo-audio.sh, service-loop.sh, udhcpc.script, mount-data.sh, bluetooth.sh
# Only files under /usr/local/echo-dev change; init, bootloader and partitions stay untouched.
set -eu
PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/bin
export PATH
BASE=/usr/local/echo-dev
STAGE=/mnt/echo-net/assistant-stage
case "$(cat /proc/bootmode)" in
main-A | main-B) ;;
*) echo "not booted from an internal main slot" >&2; exit 1 ;;
esac
[ -d "$BASE/private" ] || { echo "echo-dev base installation missing" >&2; exit 1; }
for f in echo-audio.sh service-loop.sh udhcpc.script bluetooth.sh; do /bin/sh -n "$STAGE/$f"; done
"$STAGE/assistant" ctl status >/dev/null 2>&1 || :   # must at least execute on this kernel
finish() { sync; mount -o remount,ro / || :; }
trap finish EXIT
mount -o remount,rw /
# Once, from the installation before #29 (the binary, its folder and data were called jarvis): stop
# it, move its settings, secrets and data to the new names. The service loop of this deploy starts
# the assistant under the new name.
if [ -d "$BASE/jarvis" ] && [ ! -d "$BASE/assistant" ]; then
    for p in $(pidof jarvis); do kill "$p" || :; done
    mv "$BASE/jarvis" "$BASE/assistant"
    [ -f "$BASE/assistant/jarvis" ] && mv "$BASE/assistant/jarvis" "$BASE/assistant/assistant.old"
    [ -f "$BASE/private/jarvis-secrets.json" ] && mv "$BASE/private/jarvis-secrets.json" "$BASE/private/assistant-secrets.json"
    [ -d /mnt/data/jarvis ] && [ ! -d /mnt/data/assistant ] && mv /mnt/data/jarvis /mnt/data/assistant
    sed -i "s#/mnt/data/jarvis#/mnt/data/assistant#; s#\"/mnt/jarvis\"#\"/mnt/assistant\"#" "$BASE/assistant/settings.json" "$BASE/assistant/device.json" 2>/dev/null || :
    echo "migrated from jarvis to assistant"
fi
mkdir -p "$BASE/assistant"
chmod 755 "$BASE/assistant"
# Replace only what changed: a replaced file still open by a running process (the service loop
# reads its script while it runs) keeps the root from going read-only again until a reboot.
replace() {
    cmp -s "$1" "$2" && return 0
    cp "$1" "$2.new"
    chmod 755 "$2.new"
    mv -f "$2.new" "$2"
    echo "updated $2"
}
replace "$STAGE/assistant" "$BASE/assistant/assistant"
[ -f "$STAGE/librespot" ] && replace "$STAGE/librespot" "$BASE/assistant/librespot"
# Tailscale (#46), staged on the data partition (too large for the tmpfs stage)
if [ -f /mnt/data/ts-stage/tailscaled ]; then
    mkdir -p "$BASE/tailscale"
    replace /mnt/data/ts-stage/tailscaled "$BASE/tailscale/tailscaled"
    replace /mnt/data/ts-stage/tailscale "$BASE/tailscale/tailscale"
    rm -rf /mnt/data/ts-stage
fi
[ -f "$STAGE/echo-reboot" ] && replace "$STAGE/echo-reboot" "$BASE/bin/echo-reboot"
for f in echo-audio.sh service-loop.sh udhcpc.script mount-data.sh bluetooth.sh; do
    replace "$STAGE/$f" "$BASE/$f"
done
if [ -f "$STAGE/settings.json" ]; then
    cp "$STAGE/settings.json" "$BASE/assistant/settings.json"
    chmod 644 "$BASE/assistant/settings.json"
fi
if [ -f "$STAGE/device.json" ]; then
    cp "$STAGE/device.json" "$BASE/assistant/device.json"
    chmod 644 "$BASE/assistant/device.json"
fi
if [ -f "$STAGE/assistant-secrets.json" ]; then
    cp "$STAGE/assistant-secrets.json" "$BASE/private/assistant-secrets.json"
    chmod 600 "$BASE/private/assistant-secrets.json"
    rm -f "$STAGE/assistant-secrets.json"
fi
echo ASSISTANT_INSTALLED

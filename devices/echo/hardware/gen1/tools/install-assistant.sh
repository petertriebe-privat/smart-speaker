#!/bin/sh
# Install or update Jarvis on the Echo's internal root (run on the Echo as root): main-B on the
# first Echo, main-A on the second (03-boot.md), wherever the echo-dev base was installed.
# Expects the files in /mnt/echo-net/jarvis-stage (copied by tools/deploy.sh):
#   jarvis, settings.json (optional), device.json (optional), jarvis-secrets.json (optional),
#   echo-audio.sh, service-loop.sh, udhcpc.script, mount-data.sh, bluetooth.sh
# Only files under /usr/local/echo-dev change; init, bootloader and partitions stay untouched.
set -eu
PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/bin
export PATH
BASE=/usr/local/echo-dev
STAGE=/mnt/echo-net/jarvis-stage
case "$(cat /proc/bootmode)" in
main-A | main-B) ;;
*) echo "not booted from an internal main slot" >&2; exit 1 ;;
esac
[ -d "$BASE/private" ] || { echo "echo-dev base installation missing" >&2; exit 1; }
for f in echo-audio.sh service-loop.sh udhcpc.script bluetooth.sh; do /bin/sh -n "$STAGE/$f"; done
"$STAGE/jarvis" ctl status >/dev/null 2>&1 || :   # must at least execute on this kernel
finish() { sync; mount -o remount,ro / || :; }
trap finish EXIT
mount -o remount,rw /
mkdir -p "$BASE/jarvis"
chmod 755 "$BASE/jarvis"
# Replace only what changed: a replaced file still open by a running process (the service loop
# reads its script while it runs) keeps the root from going read-only again until a reboot.
replace() {
    cmp -s "$1" "$2" && return 0
    cp "$1" "$2.new"
    chmod 755 "$2.new"
    mv -f "$2.new" "$2"
    echo "updated $2"
}
replace "$STAGE/jarvis" "$BASE/jarvis/jarvis"
[ -f "$STAGE/librespot" ] && replace "$STAGE/librespot" "$BASE/jarvis/librespot"
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
    cp "$STAGE/settings.json" "$BASE/jarvis/settings.json"
    chmod 644 "$BASE/jarvis/settings.json"
fi
if [ -f "$STAGE/device.json" ]; then
    cp "$STAGE/device.json" "$BASE/jarvis/device.json"
    chmod 644 "$BASE/jarvis/device.json"
fi
if [ -f "$STAGE/jarvis-secrets.json" ]; then
    cp "$STAGE/jarvis-secrets.json" "$BASE/private/jarvis-secrets.json"
    chmod 600 "$BASE/private/jarvis-secrets.json"
    rm -f "$STAGE/jarvis-secrets.json"
fi
echo JARVIS_INSTALLED

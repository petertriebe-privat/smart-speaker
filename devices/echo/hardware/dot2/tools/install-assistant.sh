#!/bin/sh
# Install or update Jarvis on the Echo Dot 2 (run there as root by tools/deploy.sh).
# Expects in /data/echo-dot-dev/jarvis-stage: jarvis, assistant.sh, and optionally device.json,
# jarvis-secrets.json, librespot, btspeaker. Everything lives on userdata under
# /data/echo-dot-dev; the boot image (recovery), LK and the partitions stay untouched.
# assistant.sh is installed as services/jarvis.sh: the paths on the device keep their names
# until the migration in #29 (a second name would start a second service).
# Prints NEW_SERVICE when the service was not installed before: services.sh reads its list at
# boot, so the Dot has to reboot once.
set -eu
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH
BASE=/data/echo-dot-dev
STAGE=$BASE/jarvis-stage
grep -q '^echo-dot-dev' /etc/echo-dot-dev-release || { echo "not the Dot's own Linux" >&2; exit 1; }
[ -d "$BASE/services" ] || { echo "services directory missing" >&2; exit 1; }
chmod 755 "$STAGE/jarvis"
"$STAGE/jarvis" ctl status >/dev/null 2>&1 || : # must at least execute on this kernel
/bin/sh -n "$STAGE/assistant.sh"
mkdir -p "$BASE/jarvis"
chmod 755 "$BASE/jarvis"
replace() {
    cmp -s "$1" "$2" && return 0
    cp "$1" "$2.new"
    chmod 755 "$2.new"
    mv -f "$2.new" "$2"
    echo "updated $2"
}
replace "$STAGE/jarvis" "$BASE/jarvis/jarvis"
[ -f "$STAGE/librespot" ] && replace "$STAGE/librespot" "$BASE/jarvis/librespot"
[ -f "$STAGE/btspeaker" ] && replace "$STAGE/btspeaker" "$BASE/jarvis/btspeaker"
if [ -f "$STAGE/device.json" ]; then
    cp "$STAGE/device.json" "$BASE/jarvis/device.json"
    chmod 644 "$BASE/jarvis/device.json"
fi
# settings.json carries the Echo's paths: on the Dot only device.json and the built-in defaults
rm -f "$BASE/jarvis/settings.json"
rm -rf /mnt/data/jarvis
if [ -f "$STAGE/jarvis-secrets.json" ]; then
    cp "$STAGE/jarvis-secrets.json" "$BASE/private/jarvis-secrets.json"
    chmod 600 "$BASE/private/jarvis-secrets.json"
fi
[ -e "$BASE/services/jarvis.sh" ] || NEW=1
replace "$STAGE/assistant.sh" "$BASE/services/jarvis.sh"
rm -rf "$STAGE"
sync
[ -n "${NEW:-}" ] && echo NEW_SERVICE
echo JARVIS_INSTALLED

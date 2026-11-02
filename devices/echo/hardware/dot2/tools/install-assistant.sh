#!/bin/sh
# Install or update the assistant on the Echo Dot 2 (run there as root by tools/deploy.sh).
# Expects in /data/echo-dot-dev/assistant-stage: assistant, assistant.sh, and optionally device.json,
# assistant-secrets.json, librespot, btspeaker. Everything lives on userdata under
# /data/echo-dot-dev; the boot image (recovery), LK and the partitions stay untouched.
# assistant.sh is installed as services/assistant.sh.
# Prints NEW_SERVICE when the service was not installed before: services.sh reads its list at
# boot, so the Dot has to reboot once.
set -eu
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH
BASE=/data/echo-dot-dev
STAGE=$BASE/assistant-stage
grep -q '^echo-dot-dev' /etc/echo-dot-dev-release || { echo "not the Dot's own Linux" >&2; exit 1; }
[ -d "$BASE/services" ] || { echo "services directory missing" >&2; exit 1; }
chmod 755 "$STAGE/assistant"
"$STAGE/assistant" ctl status >/dev/null 2>&1 || : # must at least execute on this kernel
/bin/sh -n "$STAGE/assistant.sh"
# Once, from the installation before #29 (service, binary and data were called jarvis): stop it,
# drop its service and move settings, secrets and data to the new names.
if [ -e "$BASE/services/jarvis.sh" ] || [ -d "$BASE/jarvis" ]; then
    rm -f "$BASE/services/jarvis.sh"
    for p in $(pidof jarvis); do kill "$p" || :; done
    [ -d "$BASE/jarvis" ] && [ ! -d "$BASE/assistant" ] && mv "$BASE/jarvis" "$BASE/assistant"
    rm -f "$BASE/assistant/jarvis"
    [ -f "$BASE/private/jarvis-secrets.json" ] && mv "$BASE/private/jarvis-secrets.json" "$BASE/private/assistant-secrets.json"
    [ -d /data/jarvis ] && [ ! -d /data/assistant ] && mv /data/jarvis /data/assistant
    rm -f /usr/local/bin/jarvis
    echo "migrated from jarvis to assistant"
fi
mkdir -p "$BASE/assistant"
chmod 755 "$BASE/assistant"
replace() {
    cmp -s "$1" "$2" && return 0
    cp "$1" "$2.new"
    chmod 755 "$2.new"
    mv -f "$2.new" "$2"
    echo "updated $2"
}
replace "$STAGE/assistant" "$BASE/assistant/assistant"
[ -f "$STAGE/librespot" ] && replace "$STAGE/librespot" "$BASE/assistant/librespot"
[ -f "$STAGE/btspeaker" ] && replace "$STAGE/btspeaker" "$BASE/assistant/btspeaker"
if [ -f "$STAGE/device.json" ]; then
    cp "$STAGE/device.json" "$BASE/assistant/device.json"
    chmod 644 "$BASE/assistant/device.json"
fi
# settings.json carries the Echo's paths: on the Dot only device.json and the built-in defaults
rm -f "$BASE/assistant/settings.json"
rm -rf /mnt/data/assistant /mnt/data/jarvis
if [ -f "$STAGE/assistant-secrets.json" ]; then
    cp "$STAGE/assistant-secrets.json" "$BASE/private/assistant-secrets.json"
    chmod 600 "$BASE/private/assistant-secrets.json"
fi
[ -e "$BASE/services/assistant.sh" ] || NEW=1
replace "$STAGE/assistant.sh" "$BASE/services/assistant.sh"
rm -rf "$STAGE"
sync
[ -n "${NEW:-}" ] && echo NEW_SERVICE
echo ASSISTANT_INSTALLED

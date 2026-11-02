#!/bin/sh
# Build the Jarvis app and install it on a Fire TV stick over network ADB.
#
#   devices/android/tools/install.sh <stick host> <room> [name] [--core]
#
# Without --core the stick is the screen of the other Jarvis devices (cards over MQTT). With
# --core it runs a Jarvis of its own (core/, board firetv): the household's secrets go into the
# app's private files, the remote's microphone key talks to him, he answers from the TV.
#
# The stick needs ADB debugging on (Settings > My Fire TV > Developer options); the first
# connection asks on the TV to allow this computer. The secrets (MQTT broker, and with --core all
# of them) come from devices/echo/local/assistant/secrets.json (core/tools/make_secrets.py).
# Environment: ANDROID_HOME (default E:\Android\sdk), JAVA_HOME_17: a JDK 17 (default E:\Android\jdk17),
# SKIP_BUILD=1 to install the last build.
set -e
core=
args=
for a in "$@"; do
    case $a in
    --core) core=1 ;;
    *) args="$args$a
" ;;
    esac
done
host=$(printf %s "$args" | sed -n 1p) room=$(printf %s "$args" | sed -n 2p) name=$(printf %s "$args" | sed -n 3p)
if [ -z "$host" ] || [ -z "$room" ]; then
    echo "usage: $0 <stick host> <room> [name] [--core]" >&2
    exit 2
fi
here=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$here/../.." && pwd)
secrets="$repo/devices/echo/local/assistant/secrets.json"
secrets_w=$(cygpath -w "$secrets" 2>/dev/null || echo "$secrets") # Windows Python in Git Bash
export ANDROID_HOME=${ANDROID_HOME:-'E:\Android\sdk'}
export JAVA_HOME=${JAVA_HOME_17:-'E:\Android\jdk17'}
adb="$(cygpath -u "$ANDROID_HOME" 2>/dev/null || echo "$ANDROID_HOME")/platform-tools/adb"
pkg=dev.jarvis.screen
case "$host" in *:*) target=$host ;; *) target=$host:5555 ;; esac

if [ -z "$SKIP_BUILD" ]; then
    # The core for the stick's 32-bit userspace (armeabi-v7a, as the Echo), packed as a native
    # library: Android executes files only from the app's native library directory.
    lib="$here/firetv/src/main/jniLibs/armeabi-v7a"
    mkdir -p "$lib"
    (cd "$repo/core" && python tools/extract_prompts.py >/dev/null &&
        CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" \
            -o "$(cygpath -w "$lib/libjarvis.so" 2>/dev/null || echo "$lib/libjarvis.so")" ./cmd/assistant)
    (cd "$here" && ./gradlew --quiet :firetv:assembleDebug)
fi
apk="$here/firetv/build/outputs/apk/debug/firetv-debug.apk"
apk=$(cygpath -w "$apk" 2>/dev/null || echo "$apk")

secret() { python -I -c "import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2], ''))" "$secrets_w" "$1"; }
mqtt_url=$(secret MQTT_URL) mqtt_user=$(secret MQTT_USER) mqtt_password=$(secret MQTT_PASSWORD)
if [ -z "$mqtt_url" ]; then
    echo "MQTT_URL missing in $secrets" >&2
    exit 1
fi

"$adb" connect "$target" >/dev/null
# Installed as from Amazon's Appstore: Fire OS' home screen lists no app without an installer
# (installed by plain ADB, Jarvis was found only under Settings > Applications).
"$adb" -s "$target" install -r -i com.amazon.venezia "$apk"
# Fire OS has no settings page for "display over other apps"; ADB may grant it.
"$adb" -s "$target" shell appops set $pkg SYSTEM_ALERT_WINDOW allow
# No microphone symbol at the top right for Jarvis (SystemUI's exemption list; read when SystemUI
# starts, so it takes effect after the stick's next restart).
"$adb" -s "$target" shell settings put global sysui_mic_disclosure_exempt $pkg
if [ -n "$core" ]; then
    "$adb" -s "$target" shell pm grant $pkg android.permission.RECORD_AUDIO
    # The secrets into the app's private files (run-as works for this debug build); the copy in
    # /data/local/tmp lives only for the moment of the transfer.
    MSYS_NO_PATHCONV=1 "$adb" -s "$target" push "$secrets_w" /data/local/tmp/jarvis-secrets.json >/dev/null
    MSYS_NO_PATHCONV=1 "$adb" -s "$target" shell "cat /data/local/tmp/jarvis-secrets.json | run-as $pkg sh -c 'mkdir -p files && cat > files/secrets.json && chmod 600 files/secrets.json'; rm /data/local/tmp/jarvis-secrets.json"
fi
# Values in single quotes for the stick's shell.
q() { printf "'%s'" "$(printf %s "$1" | sed "s/'/'\\\\''/g")"; }
"$adb" -s "$target" shell "am start -n $pkg/.SetupActivity --es mqtt_url $(q "$mqtt_url") \
    --es mqtt_user $(q "$mqtt_user") --es mqtt_password $(q "$mqtt_password") \
    --es room $(q "$room") --es name $(q "$name")" >/dev/null
sleep 4
"$adb" -s "$target" logcat -d -s JarvisScreen:* JarvisCore:* | tail -12

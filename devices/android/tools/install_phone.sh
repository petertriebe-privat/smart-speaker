#!/bin/sh
# Build the Jarvis phone app (#75) and install it on an Android phone over ADB.
#
#   devices/android/tools/install_phone.sh [serial] [--room <name>] [--name <name>] [--device <name>]
#
# --device: the phone's certificate from services/home-gateway/make_certs.sh: the house's services
# then go over the home gateway, at home and away (mobile data) alike.
#
# The phone runs a Jarvis of its own (core/ for arm64, board phone), never the household's
# primary. Its screen is called "Handy" unless --room names it otherwise (the topic of its cards,
# jarvis/screen/<room id>; the phone stands in no room). The household's secrets go into the
# app's private files without the landline's (SIP_*): the phone does not ring for the FritzBox.
#
# The phone needs USB debugging on (Settings > Developer options), connected by USB or paired
# for wireless debugging; with several devices give the serial (adb devices). The secrets come from
# local/secrets.json (core/tools/make_secrets.py).
# Environment: ANDROID_HOME (default E:\Android\sdk), JAVA_HOME_17: a JDK 17 (default E:\Android\jdk17),
# SKIP_BUILD=1 to install the last build.
set -e
serial='' room=Handy name='' device=''
while [ $# -gt 0 ]; do
    case $1 in
    --room) room=$2; shift ;;
    --name) name=$2; shift ;;
    --device) device=$2; shift ;; # its certificate for the way home (services/home-gateway)
    *) serial=$1 ;;
    esac
    shift
done
here=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$here/../.." && pwd)
secrets=${JARVIS_SECRETS:-"$repo/local/secrets.json"} # another checkout's (a worktree has none)
secrets_w=$(cygpath -w "$secrets" 2>/dev/null || echo "$secrets") # Windows Python in Git Bash
export ANDROID_HOME=${ANDROID_HOME:-'E:\Android\sdk'}
export JAVA_HOME=${JAVA_HOME_17:-'E:\Android\jdk17'}
adb="$(cygpath -u "$ANDROID_HOME" 2>/dev/null || echo "$ANDROID_HOME")/platform-tools/adb"
pkg=dev.jarvis.phone
if [ -n "$serial" ]; then adb="$adb -s $serial"; fi

if [ -z "$SKIP_BUILD" ]; then
    # The core for the phone's 64-bit userspace, packed as a native library: Android executes
    # files only from the app's native library directory.
    lib="$here/phone/src/main/jniLibs/arm64-v8a"
    mkdir -p "$lib"
    (cd "$repo/core" && python tools/extract_prompts.py >/dev/null &&
        CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" \
            -o "$(cygpath -w "$lib/libjarvis.so" 2>/dev/null || echo "$lib/libjarvis.so")" ./cmd/assistant)
    (cd "$here" && ./gradlew --quiet :phone:assembleDebug)
fi
apk="$here/phone/build/outputs/apk/debug/phone-debug.apk"
apk=$(cygpath -w "$apk" 2>/dev/null || echo "$apk")

# Installed as from the Play Store: the Desktop Head Unit lists no car app without an installer,
# "unknown sources" in Android Auto's developer settings notwithstanding (the Fire TV's home screen
# did the same, install.sh). A real car refuses a sideloaded car app with any installer: only
# Google Play counts there (06.11.2026).
$adb install -r -i com.android.vending "$apk"
$adb shell pm grant $pkg android.permission.RECORD_AUDIO
$adb shell pm grant $pkg android.permission.POST_NOTIFICATIONS 2>/dev/null || true
# The phone's tools (#75, PhoneTools): calls, contacts, SMS; drawing over other apps is Android's
# exemption for starting Maps, a call or an app from the background; notification access reads
# and answers the messengers' messages (MessageListener).
for p in CALL_PHONE READ_CONTACTS SEND_SMS; do $adb shell pm grant $pkg android.permission.$p 2>/dev/null || true; done
$adb shell appops set $pkg SYSTEM_ALERT_WINDOW allow
$adb shell cmd notification allow_listener $pkg/$pkg.MessageListener
# The secrets without the landline's into the app's private files (run-as works for this debug
# build); the copy in /data/local/tmp lives only for the moment of the transfer.
# The way home (#75, services/home-gateway): with --device the phone's certificate goes along and
# HOME_ENDPOINTS into its secrets: the gateway in the LAN first, then the VPS.
gateway=${HOME_GATEWAY_LOCAL:-"$repo/local/home-gateway"}
endpoints=
if [ -n "$device" ]; then
    . "$gateway/gateway.env"
    endpoints="$GATEWAY_LAN:$PORT,$VPS_HOST:$PORT"
    for f in "ca.crt:ca.crt" "clients/$device.crt:client.crt" "clients/$device.key:client.key"; do
        src=${f%%:*} dst=${f##*:}
        MSYS_NO_PATHCONV=1 $adb push "$(cygpath -w "$gateway/$src" 2>/dev/null || echo "$gateway/$src")" /data/local/tmp/jarvis-$dst >/dev/null
        MSYS_NO_PATHCONV=1 $adb shell "cat /data/local/tmp/jarvis-$dst | run-as $pkg sh -c 'mkdir -p files/state/home && cat > files/state/home/$dst && chmod 600 files/state/home/$dst'; rm /data/local/tmp/jarvis-$dst"
    done
fi
tmp=$(mktemp)
tmp_w=$(cygpath -w "$tmp" 2>/dev/null || echo "$tmp")
python -I -c "import json,sys; s=json.load(open(sys.argv[1])); s={k: v for k, v in s.items() if not k.startswith('SIP_')}; s.update({'HOME_ENDPOINTS': sys.argv[3]} if sys.argv[3] else {}); json.dump(s, open(sys.argv[2], 'w'))" "$secrets_w" "$tmp_w" "$endpoints"
MSYS_NO_PATHCONV=1 $adb push "$tmp_w" /data/local/tmp/jarvis-secrets.json >/dev/null
rm -f "$tmp"
MSYS_NO_PATHCONV=1 $adb shell "cat /data/local/tmp/jarvis-secrets.json | run-as $pkg sh -c 'mkdir -p files && cat > files/secrets.json && chmod 600 files/secrets.json'; rm /data/local/tmp/jarvis-secrets.json"
# The household's wake word model ("Jarvis", tools/wakeword: jarvis.tflite and jarvis.json, e.g.
# copied from an Echo's /mnt/data/jarvis/wakemodel) from local/wakeword or the folder JARVIS_WAKEMODEL names;
# without it the built-in "Hey Jarvis" listens.
JARVIS_WAKEMODEL=${JARVIS_WAKEMODEL:-"$repo/local/wakeword"}
if [ -f "$JARVIS_WAKEMODEL/jarvis.tflite" ]; then
    for f in jarvis.tflite jarvis.json; do
        MSYS_NO_PATHCONV=1 $adb push "$(cygpath -w "$JARVIS_WAKEMODEL/$f" 2>/dev/null || echo "$JARVIS_WAKEMODEL/$f")" /data/local/tmp/$f >/dev/null
        MSYS_NO_PATHCONV=1 $adb shell "cat /data/local/tmp/$f | run-as $pkg sh -c 'mkdir -p files/state/wakemodel && cat > files/state/wakemodel/$f'; rm /data/local/tmp/$f"
    done
fi
# The screen's name and Jarvis' name (values in single quotes for the phone's shell); the app reads
# the broker from the secrets: no password on a command line, adbd logs those. The setup restarts
# the service (in front, it may use the microphone).
q() { printf "'%s'" "$(printf %s "$1" | sed "s/'/'\\\\''/g")"; }
$adb shell "am start -n $pkg/.SetupActivity --es room $(q "$room") --es name $(q "$name")" >/dev/null
$adb shell am start -n $pkg/.PhoneActivity >/dev/null
sleep 6
$adb logcat -d -s JarvisScreen:* JarvisCore:* | tail -15

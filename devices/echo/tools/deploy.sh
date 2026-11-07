#!/bin/sh
# Build Jarvis for the Echo and install it persistently over SSH (run in Git Bash on the PC).
#   tools/deploy.sh            build, copy, install, restart Jarvis
#   tools/deploy.sh --secrets  also regenerate and copy the private secrets
#   tools/deploy.sh --librespot  also copy the Spotify Connect client (local/assistant/librespot)
# Uses the private SSH profile in local/ssh/config (alias echo-dev, ECHO_HOST=echo2 for the second
# Echo) with Windows' OpenSSH. Elsewhere (Linux, the agent on the server over Tailscale, #46):
# ECHO_HOST=jarvis-echo uses the normal ssh/scp and that host from ~/.ssh/config.
# Per-device settings (role, room, Spotify name; devices/echo/docs/18-mehrere-echos.md): when
# local/devices/<host>.json exists it is installed as the device's device.json; otherwise the
# device keeps the one it has.
# The Echo Dot 2 (a host of local/dot-ssh/config, e.g. ECHO_HOST=dot-dev) gets only Jarvis, his
# settings, secrets and librespot, installed by hardware/dot2/tools/install-assistant.sh under /data/echo-dot-dev.
#   tools/deploy.sh --all [...]  every Echo with a local/devices/<host>.json, one after the
#                                       other (vet and tests once)
#   tools/deploy.sh --status     only the running assistant's ctl status line (exit 75: not
#                                reachable)
# The binary carries its version (built here: the commit, "-dirty" with local changes) and its
# commit; after the restart the script waits until ctl status reports the version, and fails when
# it does not.
# The deployer on the Raspberry Pi (services/deployer) runs this script from a release's Echo
# bundle (bundle.sh: this script, files.txt and the files it names, the binary) and passes
# JARVIS_BIN, VERSION (v1.2.0) and COMMIT: installed without building here; the Dot is found by
# its release file when there is no local/dot-ssh/config.
set -eu
ALEXA=$(cd "$(dirname "$0")/.." && pwd)
if [ -z "${VERSION:-}" ]; then
    VERSION=$(git -C "$ALEXA" rev-parse --short=12 HEAD)
    git -C "$ALEXA/../.." diff --quiet HEAD -- core shared/prompts || VERSION=$VERSION-dirty
fi
[ -n "${COMMIT:-}" ] || COMMIT=$(git -C "$ALEXA" rev-parse --short=12 HEAD 2>/dev/null || :)
export VERSION COMMIT
# the files of one model (files.txt), as paths
files() {
    sed -n "s|^$1 |$ALEXA/|p" "$ALEXA/tools/files.txt" | tr '\n' ' '
}
if [ "${1:-}" = --all ]; then
    # An Echo that cannot be reached (switched off, away) is skipped and reported, and gets the
    # version with the next deploy; a reachable one that fails stops the run.
    shift
    done_=0
    for f in "$ALEXA"/local/devices/*.json; do
        h=$(basename "$f" .json)
        echo "== $h"
        rc=0
        ECHO_HOST=$h sh "$0" "$@" || rc=$?
        case $rc in
            0) done_=$((done_ + 1)); export CHECKED=1 ;;
            75) echo "$h: not reachable, skipped" ;;
            *) exit 1 ;;
        esac
    done
    [ "$done_" -gt 0 ] || { echo "no Echo reachable" >&2; exit 1; }
    exit 0
fi
HOST=${ECHO_HOST:-echo-dev}
CFG="$ALEXA/local/ssh/config"
DOTCFG="$ALEXA/local/dot-ssh/config"
DOT=
if [ -f "$DOTCFG" ] && grep -q "^Host ${HOST}[[:space:]]*$" "$DOTCFG"; then
    # The Echo Dot 2 (hardware/dot2/tools, devices/echo/hardware/dot2/docs/21-echo-dot-linux-ssh.md): own Linux, own profile.
    DOT=1
    # MSYS_NO_PATHCONV below (remote paths stay as they are): the profile as a Windows path
    w=$(cygpath -m "$DOTCFG")
    SSH="/c/Windows/System32/OpenSSH/ssh.exe -F $w"
    SCP="/c/Windows/System32/OpenSSH/scp.exe -F $w"
elif [ -f "$CFG" ] && grep -q "^Host ${HOST}[[:space:]]*$" "$CFG"; then
    SSH="/c/Windows/System32/OpenSSH/ssh.exe -F $CFG"
    SCP="/c/Windows/System32/OpenSSH/scp.exe -F $CFG"
else
    SSH=ssh
    SCP=scp
    # the deployer: no private profile here, the Dot is told by its own Linux
    $SSH -o ConnectTimeout=10 "$HOST" 'test -f /etc/echo-dot-dev-release' && DOT=1
fi
# Not reachable: exit 75 (EX_TEMPFAIL), before building and testing, so --all can skip the Echo.
MSYS_NO_PATHCONV=1 $SSH -o ConnectTimeout=10 "$HOST" true 2>/dev/null ||
    { echo "$HOST: not reachable" >&2; exit 75; }
BIN=/usr/local/echo-dev/jarvis/jarvis
[ -z "$DOT" ] || BIN=/data/echo-dot-dev/jarvis/jarvis
if [ "${1:-}" = --status ]; then
    # reachable, but no assistant answers: none runs (the catch-up installs one)
    MSYS_NO_PATHCONV=1 $SSH -o ConnectTimeout=10 "$HOST" "$BIN ctl status" || echo "version=none"
    exit 0
fi
PY="py -3"
command -v py >/dev/null 2>&1 || PY=python3
OUT="$ALEXA/local/assistant" # the Echo's builds, as build-librespot.sh (neutral names, #68)
mkdir -p "$OUT"
REPO=$(cd "$ALEXA/../.." && pwd)
CORE="$REPO/core" # Jarvis' Go program, shared with the Fire TV (core/); not in a release bundle
if [ -n "${JARVIS_BIN:-}" ]; then
    cp "$JARVIS_BIN" "$OUT/jarvis" # built, vetted and tested by the CI
else
    cd "$CORE"
    $PY tools/extract_prompts.py
    # CHECK=cmd vets and tests only what the binary uses (while other packages are being written).
    PKGS=./...
    [ "${CHECK:-}" = cmd ] && PKGS=$(go list -deps ./cmd/assistant | grep "^echo-assistant/")
    if [ -z "${CHECKED:-}" ]; then # --all: once
        GOOS=linux GOARCH=arm go vet $PKGS
        go test $PKGS
    fi
    CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w -X main.version=$VERSION -X main.commit=$COMMIT" -o "$OUT/jarvis" ./cmd/assistant
fi
# After the restart: the new version answers ctl status, and still does 15 s later (no crash loop).
verify() {
    for round in 1 2; do
        i=0
        until s=$(MSYS_NO_PATHCONV=1 $SSH -o ConnectTimeout=5 "$HOST" "$1 ctl status" 2>/dev/null) && case $s in "version=$VERSION "*) true ;; *) false ;; esac; do
            case $s in state=*) echo "$HOST: Jarvis runs, too old to report its version"; return 0 ;; esac
            i=$((i+1)); [ $i -lt 30 ] || { echo "$HOST: version $VERSION is not running" >&2; exit 1; }
            sleep 3
        done
        [ $round = 2 ] || sleep 15
    done
    echo "$HOST: version $VERSION running"
}
# the binary, then the device's own files (device.json, secrets, librespot)
FILES="$OUT/jarvis"
DEVICE="$ALEXA/local/devices/$HOST.json"
if [ -f "$DEVICE" ]; then
    # relative path: Windows' Python does not understand /e/... (MSYS_NO_PATHCONV)
    (cd "$ALEXA" && $PY -c 'import json, sys; json.load(open(sys.argv[1]))' "local/devices/$HOST.json")
    cp "$DEVICE" "$OUT/device.json"
    FILES="$FILES $OUT/device.json"
fi
for arg in "$@"; do
    case $arg in
    --secrets)
        (cd "$CORE" && $PY tools/make_secrets.py >/dev/null)
        FILES="$FILES $REPO/local/secrets.json" ;; # every device's private values (local/README.md)
    --librespot) # 12 MB: only when it changed (tools/build-librespot.sh)
        FILES="$FILES $OUT/librespot" ;;
    --tailscale) # 69 MB, more than the tmpfs stage holds: staged on the data partition (#46)
        TAILSCALE=1 ;;
    esac
done
if [ -n "$DOT" ]; then
    # Everything on userdata under /data/echo-dot-dev; no Echo scripts (the Dot has no DSP chain,
    # no BlueZ, its own network and service scripts), and no settings.json: its Echo paths
    # (state_dir on /mnt) are RAM on the Dot, the built-in defaults are the Dot's (internal/board).
    # Tailscale is not ported yet.
    # btspeaker (#74, hardware/dot2/tools/btspeaker/build.sh): the Bluetooth speaker helper, when built
    BTSPEAKER=$ALEXA/local/dot2-tools/btspeaker
    [ -f "$BTSPEAKER" ] || BTSPEAKER=
    DOTFILES=$(files dot2)
    FILES=$(for f in $FILES $DOTFILES $BTSPEAKER; do
        if command -v cygpath >/dev/null 2>&1; then cygpath -m "$f"; else echo "$f"; fi
    done | tr '\n' ' ')
    STAGE=/data/echo-dot-dev/jarvis-stage
    export MSYS_NO_PATHCONV=1 # remote paths for Windows' ssh/scp
    $SSH "$HOST" "rm -rf $STAGE; mkdir -p $STAGE; chmod 700 $STAGE"
    $SCP $FILES "$HOST:$STAGE/"
    out=$($SSH "$HOST" "cd $STAGE && { [ ! -f secrets.json ] || mv secrets.json jarvis-secrets.json; } && sh install-assistant.sh")
    echo "$out"
    case $out in *JARVIS_INSTALLED*) ;; *) exit 1 ;; esac
    if echo "$out" | grep -q NEW_SERVICE; then
        # services.sh reads its services at boot (about 30 s until SSH is back)
        $SSH "$HOST" 'reboot' || :
        echo "new service: rebooting the Dot"
        sleep 20
        i=0
        until $SSH -o ConnectTimeout=5 "$HOST" true 2>/dev/null; do
            i=$((i+1)); [ $i -lt 30 ] || { echo "the Dot is not back" >&2; exit 1; }
            sleep 3
        done
        echo "the Dot is back"
    else
        # the service loop starts Jarvis again within 3 s
        $SSH "$HOST" 'for p in $(pidof jarvis); do kill $p; done; echo restarted'
    fi
    verify "$BIN"
    exit 0
fi
if [ -n "${TAILSCALE:-}" ]; then
    $SSH "$HOST" 'mkdir -p /mnt/data/ts-stage && chmod 700 /mnt/data/ts-stage'
    $SCP "$ALEXA/local/tailscale/tailscaled" "$ALEXA/local/tailscale/tailscale" "$HOST:/mnt/data/ts-stage/"
fi
$SSH "$HOST" 'rm -rf /mnt/echo-net/jarvis-stage; mkdir -p /mnt/echo-net/jarvis-stage; chmod 700 /mnt/echo-net/jarvis-stage'
FILES="$FILES $(files gen1)"
$SCP $FILES "$HOST:/mnt/echo-net/jarvis-stage/"
$SSH "$HOST" 'cd /mnt/echo-net/jarvis-stage && mv echo-service-loop.sh service-loop.sh && mv udhcpc-echo.script udhcpc.script && mv echo-mount-data.sh mount-data.sh && mv echo-reboot.sh echo-reboot && mv echo-bluetooth.sh bluetooth.sh && { [ ! -f secrets.json ] || mv secrets.json jarvis-secrets.json; } && sh install-assistant.sh'
# Restart Jarvis only (the service loop starts it again within 10 s); the service loop itself
# picks up a new version at the next boot.
$SSH "$HOST" 'for p in $(ps | grep "[e]cho-dev/jarvis/jarvis" | awk "{print \$1}"); do kill $p; done; sleep 2; sync; if mount -o remount,ro / 2>/dev/null; then echo "restarted, root read-only"; else echo "restarted, root still busy: reboot with /usr/local/echo-dev/bin/echo-reboot"; fi'
verify "$BIN"

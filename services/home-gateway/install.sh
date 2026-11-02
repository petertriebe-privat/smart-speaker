#!/bin/sh
# Install the phones' way home (#75) on the home gateway (the Raspberry Pi) and on the VPS.
#
#   services/home-gateway/install.sh [pi|vps]   (both without an argument)
#
# Reads devices/echo/local/home-gateway/gateway.env (git-ignored; the household's values):
#   PORT=47443                    the port on the gateway and on the VPS
#   GATEWAY_WG=10.x.x.x           the gateway's address on the WireGuard link (the VPS forwards there)
#   ECHOS="192.168.x.a 192.168.x.b"  the Echos' addresses, the primary first
#   DEVICES="phone-1"             the client certificates let in (make_certs.sh)
#   PI_SSH=raspberry-pi  VPS_SSH=vps   ssh aliases
# and the certificates next to it (make_certs.sh). Each side: the new file is checked with
# nginx -t and nginx reloaded; a failed check restores what was there.
set -e
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
local=${HOME_GATEWAY_LOCAL:-"$repo/devices/echo/local/home-gateway"} # another checkout's (a worktree has none)
. "$local/gateway.env"
what=${1:-both}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

devices=""
for d in $DEVICES; do devices="$devices    \"CN=$d\" 1;
"; done
servers="" first=1
for e in $ECHOS; do
    if [ -n "$first" ]; then servers="    server $e:47801 max_fails=1 fail_timeout=30s;
"; first=
    else servers="$servers    server $e:47801 backup;
"; fi
done
# @…@ placeholders; multi-line values through awk (sed cannot take newlines portably)
render() {
    awk -v port="$PORT" -v wg="$GATEWAY_WG" -v devmap="$devices" -v rpc="$servers" '
        { gsub(/@PORT@/, port); gsub(/@GATEWAY_WG@/, wg) }
        /@DEVICE_MAP@/ { printf "%s", devmap; next }
        /@RPC_SERVERS@/ { printf "%s", rpc; next }
        { print }' "$1"
}
render "$here/pi-jarvis-mobile.conf.tmpl" > "$tmp/pi.conf"
render "$here/vps-jarvis-mobile.conf.tmpl" > "$tmp/vps.conf"

# put <ssh command> <file> <remote path> <mode>: the file through sudo tee, as root
put() { $1 "sudo tee $3 >/dev/null && sudo chmod $4 $3" < "$2"; }

# nginx -t, reload; on failure the backup (or nothing) back
check() {
    $1 "if sudo nginx -t 2>/tmp/jarvis-nginx-t; then sudo systemctl reload nginx; echo 'nginx reloaded'; else cat /tmp/jarvis-nginx-t; if [ -f $2.bak ]; then sudo mv $2.bak $2; else sudo rm -f $2; fi; exit 1; fi"
}

if [ "$what" = pi ] || [ "$what" = both ]; then
    pi="ssh -o BatchMode=yes $PI_SSH"
    conf=/etc/nginx/streams-enabled/jarvis-mobile.conf
    $pi "sudo mkdir -p /etc/nginx/jarvis-mobile; if [ -f $conf ]; then sudo cp $conf $conf.bak; fi"
    put "$pi" "$local/server.crt" /etc/nginx/jarvis-mobile/server.crt 644
    put "$pi" "$local/server.key" /etc/nginx/jarvis-mobile/server.key 600
    put "$pi" "$local/ca.crt" /etc/nginx/jarvis-mobile/ca.crt 644
    put "$pi" "$tmp/pi.conf" $conf 644
    check "$pi" $conf
    echo "gateway: port $PORT, devices: $DEVICES"
fi

if [ "$what" = vps ] || [ "$what" = both ]; then
    vps="/c/Windows/System32/OpenSSH/ssh.exe -o BatchMode=yes $VPS_SSH"
    conf=/etc/nginx/streams-enabled/jarvis-mobile.conf
    # nginx' stream module and a stream block that includes streams-enabled (once)
    $vps "dpkg -s libnginx-mod-stream >/dev/null 2>&1 || sudo DEBIAN_FRONTEND=noninteractive apt-get install -y libnginx-mod-stream >/dev/null"
    $vps "sudo mkdir -p /etc/nginx/streams-enabled; grep -q 'streams-enabled' /etc/nginx/nginx.conf || { sudo cp /etc/nginx/nginx.conf /etc/nginx/nginx.conf.bak-jarvis-mobile; printf '\nstream {\n\tinclude /etc/nginx/streams-enabled/*.conf;\n}\n' | sudo tee -a /etc/nginx/nginx.conf >/dev/null; }"
    $vps "if [ -f $conf ]; then sudo cp $conf $conf.bak; fi"
    put "$vps" "$tmp/vps.conf" $conf 644
    check "$vps" $conf
    echo "VPS: port $PORT to the gateway"
fi

#!/bin/sh
# The home gateway's certificates (#75): a CA of our own, the gateway's server certificate and one
# client certificate per device (a phone). The CA's key stays in this folder on the PC; nothing of
# it goes into the repository.
#
#   services/home-gateway/make_certs.sh <dir> <device>...
#
# <dir> e.g. devices/echo/local/home-gateway (git-ignored). Existing files are kept: a new device
# gets its certificate from the same CA. Revoking a device takes its name off DEVICES in
# gateway.env and installs the gateway again (install.sh), no new CA needed.
set -e
dir=$1
shift || true
if [ -z "$dir" ] || [ $# -eq 0 ]; then
    echo "usage: $0 <dir> <device>..." >&2
    exit 2
fi
mkdir -p "$dir/clients"
cd "$dir"
export MSYS_NO_PATHCONV=1 # Git Bash: "/CN=…" is no path

if [ ! -f ca.key ]; then
    openssl ecparam -name prime256v1 -genkey -noout -out ca.key
    openssl req -x509 -new -key ca.key -sha256 -days 3650 -subj "/CN=Jarvis home CA" -out ca.crt \
        -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign"
    echo "CA made"
fi

# The gateway answers for the services' names (internal/hometunnel), not for a DNS name.
if [ ! -f server.crt ]; then
    openssl ecparam -name prime256v1 -genkey -noout -out server.key
    openssl req -new -key server.key -subj "/CN=jarvis home gateway" -out server.csr
    printf 'subjectAltName=DNS:rpc.jarvis,DNS:mqtt.jarvis,DNS:ha.jarvis\nextendedKeyUsage=serverAuth\n' > server.ext
    openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 1825 -sha256 \
        -extfile server.ext -out server.crt
    rm -f server.csr server.ext
    echo "gateway certificate made"
fi

for device in "$@"; do
    if [ -f "clients/$device.crt" ]; then
        continue
    fi
    openssl ecparam -name prime256v1 -genkey -noout -out "clients/$device.key"
    openssl req -new -key "clients/$device.key" -subj "/CN=$device" -out "clients/$device.csr"
    printf 'extendedKeyUsage=clientAuth\n' > "clients/$device.ext"
    openssl x509 -req -in "clients/$device.csr" -CA ca.crt -CAkey ca.key -CAcreateserial -days 730 -sha256 \
        -extfile "clients/$device.ext" -out "clients/$device.crt"
    rm -f "clients/$device.csr" "clients/$device.ext"
    echo "certificate for $device made"
done

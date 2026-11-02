#!/bin/bash
# Builds librespot (Spotify Connect client) as one static ARMv7 binary for the Echo, in WSL
# (Ubuntu) without root: rustup, zig as C compiler and cross linker, cargo-zigbuild.
#   wsl -d Ubuntu -- bash /mnt/e/.../devices/echo/tools/build-librespot.sh
# Result: devices/echo/local/assistant/librespot; deploy with tools/deploy.sh --librespot.
#
# musl, hard-float: nothing is linked dynamically, so the Echo's softfp EGLIBC does not matter.
# Features: rustls with compiled-in Mozilla roots (no OpenSSL), zeroconf in pure Rust (libmdns),
# and the pipe backend, which needs no audio library: Jarvis plays the PCM itself.
set -e
VERSION=v0.8.0
ZIG=zig-x86_64-linux-0.14.1
OUT=$(cd "$(dirname "$0")/.." && pwd)/local/assistant
mkdir -p ~/bin ~/opt ~/build "$OUT"
export PATH=~/bin:~/.cargo/bin:$PATH

if ! command -v rustup >/dev/null; then
    curl -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal --no-modify-path
fi
rustup target add armv7-unknown-linux-musleabihf
if [ ! -x ~/opt/$ZIG/zig ]; then
    curl -sSfL -o /tmp/$ZIG.tar.xz https://ziglang.org/download/0.14.1/$ZIG.tar.xz
    tar -xJf /tmp/$ZIG.tar.xz -C ~/opt
fi
ln -sf ~/opt/$ZIG/zig ~/bin/zig
# Build scripts run on the host and need a C compiler: zig cc as well.
printf '#!/bin/sh\nexec zig cc "$@"\n' > ~/bin/cc
printf '#!/bin/sh\nexec zig c++ "$@"\n' > ~/bin/c++
printf '#!/bin/sh\nexec zig ar "$@"\n' > ~/bin/ar
chmod +x ~/bin/cc ~/bin/c++ ~/bin/ar
command -v cargo-zigbuild >/dev/null || cargo install --locked cargo-zigbuild

cd ~/build
[ -d librespot ] || git clone -q https://github.com/librespot-org/librespot.git
cd librespot
git fetch -q --tags
git checkout -q -f $VERSION
git apply "$(dirname "$OUT")/../tools/librespot-ipv4.patch"
# libmdns sets SO_REUSEPORT, which Linux has only since 3.9 (the Echo: 2.6.37, "Protocol not
# available"): a copy that goes on without it, used through [patch.crates-io].
cargo fetch -q
MDNS=$(ls -d ~/.cargo/registry/src/*/libmdns-0.10.* | tail -1)
rm -rf ~/build/libmdns && cp -r "$MDNS" ~/build/libmdns
sed -i 's/socket.set_reuse_port(true)?;/let _ = socket.set_reuse_port(true);/' ~/build/libmdns/src/address_family.rs
grep -q 'let _ = socket.set_reuse_port' ~/build/libmdns/src/address_family.rs
printf '\n[patch.crates-io]\nlibmdns = { path = "../libmdns" }\n' >> Cargo.toml
cargo zigbuild --release --target armv7-unknown-linux-musleabihf --no-default-features \
    --features "rustls-tls-webpki-roots with-libmdns"
cp target/armv7-unknown-linux-musleabihf/release/librespot "$OUT/librespot"
ls -la "$OUT/librespot"

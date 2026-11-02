#!/bin/sh
# Root-only development services for Echo Dot 2 (biscuit).
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export HOME=/root
BASE=/data/echo-dot-dev
RUN=/run/echo-dot-dev
umask 077
mkdir -p "$RUN" /run/wpa /root/.ssh /etc/dropbear
chmod 700 /root /root/.ssh "$BASE" "$BASE/private"
ln -sf "$BASE/private/authorized_keys" /root/.ssh/authorized_keys
if [ ! -s "$BASE/private/dropbear_ed25519_host_key" ]; then
    dropbearkey -t ed25519 -f "$BASE/private/dropbear_ed25519_host_key" > "$RUN/hostkey.log" 2>&1 || exit 1
fi

# The MediaTek kernel opens these exact firmware paths itself.
mkdir -p /system/vendor
ln -sfn "$BASE/firmware" /system/vendor/firmware
[ -e /vendor/firmware/WIFI_RAM_CODE_8163 ] || ln -sfn /system/vendor /vendor
if ! pidof wmtup >/dev/null; then
    /usr/local/bin/wmtup -patches "$BASE/firmware/" -power > "$RUN/wmtup.log" 2>&1 &
fi

# SSH remains available after link loss; a client lease is renewed in the background.
( while true; do
    /usr/sbin/dropbear -F -E -s -p 22 -r "$BASE/private/dropbear_ed25519_host_key" >> "$RUN/ssh.log" 2>&1
    sleep 3
  done ) &

while [ ! -e /sys/class/net/wlan0 ]; do sleep 1; done
ip link set lo up
ip link set wlan0 up
( while true; do
    /sbin/wpa_supplicant -i wlan0 -D nl80211 -c "$BASE/private/wpa_supplicant.conf" -P "$RUN/wpa.pid" >> "$RUN/wpa.log" 2>&1
    sleep 3
  done ) &
( while true; do
    /sbin/udhcpc -f -i wlan0 -s /usr/local/sbin/echo-dot-dhcp -p "$RUN/dhcp.pid" >> "$RUN/dhcp.log" 2>&1
    sleep 3
  done ) &
( while ! ip -4 addr show wlan0 | grep -q 'inet '; do sleep 3; done
  while true; do
      /bin/busybox.static ntpd -n -p time.cloudflare.com -p time.google.com -p 0.pool.ntp.org >> "$RUN/ntp.log" 2>&1
      sleep 15
  done ) &
wait

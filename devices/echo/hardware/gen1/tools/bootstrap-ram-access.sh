#!/bin/sh
# RAM-only trial of WLAN and SSH on a freshly rooted Echo (init=/bin/sh from the SD U-Boot),
# before hardware/gen1/tools/install-persistent-access.sh writes anything to the eMMC. Runs the same
# service-loop.sh the installed init starts, with its base directory in tmpfs.
# Expects /mnt as tmpfs and these files, uploaded with hardware/gen1/tools/echo_console.py:
#   /mnt/echo-net/{sshd,sshd_config,wpa_supplicant.conf,authorized_keys,echo_host_ed25519}
#   /mnt/echo-net/persist-stage/{service-loop.sh,udhcpc.script}
# sshd_config here is hardware/gen1/config/sshd-bootstrap.conf (keys under /mnt/echo-net).
set -eu
PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/bin
export PATH
NET=/mnt/echo-net
RAMBASE=$NET/base
grep -q ' /mnt tmpfs ' /proc/mounts || { echo "/mnt is not tmpfs" >&2; exit 1; }
for f in sshd sshd_config wpa_supplicant.conf authorized_keys echo_host_ed25519 \
         persist-stage/service-loop.sh persist-stage/udhcpc.script; do
    [ -s "$NET/$f" ] || { echo "missing $NET/$f" >&2; exit 1; }
done
# /var and the sshd privilege separation user in RAM; the installer removes both mounts
# before it writes the real files.
if ! grep -q ' /var ' /proc/mounts; then
    mount -t tmpfs -o size=4m,mode=755 tmpfs /var
    mkdir -p /var/run /var/empty /var/log /var/lock /var/tmp
    chmod 755 /var/empty
    chmod 1777 /var/tmp
fi
if ! grep -q '^sshd:' /etc/passwd; then
    if grep -q '^[^:]*:[^:]*:65530:' /etc/passwd; then echo "UID collision" >&2; exit 1; fi
    cp /etc/passwd "$NET/passwd"
    printf 'sshd:x:65530:65530:SSH privilege separation:/var/empty:/bin/false\n' >> "$NET/passwd"
    mount -o bind "$NET/passwd" /etc/passwd
fi
mkdir -p /dev/pts
grep -q ' /dev/pts ' /proc/mounts || mount -t devpts -o mode=0620,gid=5 devpts /dev/pts
ifconfig lo up
mkdir -p "$RAMBASE/bin" "$RAMBASE/private"
chmod 755 "$RAMBASE" "$RAMBASE/bin"
chmod 700 "$RAMBASE/private"
cp "$NET/wpa_supplicant.conf" "$RAMBASE/private/wpa_supplicant.conf"
chmod 600 "$RAMBASE/private/wpa_supplicant.conf"
cp "$NET/persist-stage/udhcpc.script" "$RAMBASE/udhcpc.script"
ln -sf "$NET/sshd" "$RAMBASE/bin/sshd"
ln -sf "$NET/sshd_config" "$RAMBASE/sshd_config"
chmod 755 "$NET/sshd" "$RAMBASE/udhcpc.script"
"$NET/sshd" -t -f "$NET/sshd_config"
ECHO_DEV_BASE=$RAMBASE sh "$NET/persist-stage/service-loop.sh" >>"$NET/services.log" 2>&1 </dev/null &
echo "RAM_ACCESS_STARTED service-loop pid $!"

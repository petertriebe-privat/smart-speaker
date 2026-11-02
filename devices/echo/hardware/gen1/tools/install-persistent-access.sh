#!/bin/sh
# Deliberate installation into the currently mounted, verified main-A/main-B root.
# Does not modify GPT, bootloader, kernel, IDME or another partition.
# The active slot and the eMMC size differ between units (first Echo: main-B, 7634944 sectors;
# second: main-A, 7733248), so both are arguments, checked against the running system.
set -eu
PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/bin
export PATH
[ "$#" = 5 ] || { echo "Usage: installer expected-major:minor expected-emmc-block expected-emmc-sectors expected-bootmode expected-original-init-sha256" >&2; exit 2; }
EXPECTED_DEVICE=$1
EXPECTED_BLOCK=$2
EXPECTED_SECTORS=$3
EXPECTED_BOOTMODE=$4
EXPECTED_INIT=$5
BASE=/usr/local/echo-dev
STAGE=/mnt/echo-net/persist-stage
# Root must be a partition of the named eMMC: its dev number appears under that block device.
python -c 'import os,sys,hashlib;d=os.stat("/").st_dev;dev="%d:%d"%(os.major(d),os.minor(d));assert dev==sys.argv[1];b="/sys/block/"+sys.argv[2];assert open(b+"/device/type").read().strip()=="MMC";assert int(open(b+"/size").read())==int(sys.argv[3]);assert dev in [open(b+"/"+p+"/dev").read().strip() for p in os.listdir(b) if p.startswith(sys.argv[2]+"p")];assert open("/proc/bootmode").read().strip()==sys.argv[4];assert hashlib.sha256(open("/sbin/init","rb").read()).hexdigest()==sys.argv[5];print("INSTALL_TARGET_VERIFIED")' "$EXPECTED_DEVICE" "$EXPECTED_BLOCK" "$EXPECTED_SECTORS" "$EXPECTED_BOOTMODE" "$EXPECTED_INIT"
for f in init.sh service-loop.sh echo-reboot udhcpc.script; do /bin/sh -n "$STAGE/$f"; done
[ -s /mnt/echo-net/wpa_supplicant.conf ]
[ -s /mnt/echo-net/authorized_keys ]
[ -s /mnt/echo-net/echo_host_ed25519 ]
/mnt/echo-net/sshd -t -f /mnt/echo-net/sshd_config
# The original init executable is the final atomic switch. A failure before
# that switch leaves the original boot behavior in place.
finish() { sync; mount -o remount,ro / || :; }
trap finish EXIT
mount -o remount,rw /
# Remove diagnostic RAM bind mounts before changing the actual root files.
if grep -q ' /etc/passwd ' /proc/mounts; then umount /etc/passwd; fi
if grep -q ' /var ' /proc/mounts; then umount /var; fi
mkdir -p "$BASE/bin" "$BASE/private" "$BASE/recovery" /var/empty
chmod 755 /usr/local "$BASE" "$BASE/bin" /var/empty
chmod 700 "$BASE/private" "$BASE/recovery"
cp -p /sbin/init "$BASE/recovery/init.amazon"
cp -p /etc/passwd "$BASE/recovery/passwd.before"
cp -p /etc/group "$BASE/recovery/group.before"
chmod 600 "$BASE/recovery/passwd.before" "$BASE/recovery/group.before"
cp "$STAGE/init.sh" "$BASE/init.sh"
cp "$STAGE/service-loop.sh" "$BASE/service-loop.sh"
cp "$STAGE/echo-reboot" "$BASE/bin/echo-reboot"
cp "$STAGE/udhcpc.script" "$BASE/udhcpc.script"
cp "$STAGE/sshd_config" "$BASE/sshd_config"
cp /mnt/echo-net/sshd "$BASE/bin/sshd"
chmod 755 "$BASE/init.sh" "$BASE/service-loop.sh" "$BASE/bin/echo-reboot" "$BASE/udhcpc.script" "$BASE/bin/sshd"
chmod 644 "$BASE/sshd_config"
cp /mnt/echo-net/wpa_supplicant.conf "$BASE/private/wpa_supplicant.conf"
cp /mnt/echo-net/authorized_keys "$BASE/private/authorized_keys"
cp /mnt/echo-net/echo_host_ed25519 "$BASE/private/host_ed25519"
chmod 600 "$BASE/private/"*
if ! grep -q '^sshd:' /etc/passwd; then
    if grep -q '^[^:]*:[^:]*:65530:' /etc/passwd; then echo "UID collision" >&2; exit 1; fi
    cp -p /etc/passwd /etc/passwd.echo-dev.new
    printf 'sshd:x:65530:65530:SSH privilege separation:/var/empty:/bin/false\n' >> /etc/passwd.echo-dev.new
    mv -f /etc/passwd.echo-dev.new /etc/passwd
fi
if ! grep -q '^sshd:' /etc/group; then
    if grep -q '^[^:]*:[^:]*:65530:' /etc/group; then echo "GID collision" >&2; exit 1; fi
    cp -p /etc/group /etc/group.echo-dev.new
    printf 'sshd:x:65530:\n' >> /etc/group.echo-dev.new
    mv -f /etc/group.echo-dev.new /etc/group
fi
"$BASE/bin/sshd" -t -f "$BASE/sshd_config"
ln -s ../usr/local/echo-dev/init.sh /sbin/init.echo-dev.new
mv -f /sbin/init.echo-dev.new /sbin/init
sync
mount -o remount,ro /
trap - EXIT
printf 'PERSISTENT_ACCESS_INSTALLED_ROOT_READONLY\n'

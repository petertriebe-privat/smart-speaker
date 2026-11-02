#!/bin/sh
# Restore only the verified TWRP recovery backup; do not alter LK, GPT or preloader.
set -eu
BASE=/data/echo-dot-dev
cd "$BASE"
sha256sum -c recovery-twrp.img.sha256
test "$(cat /sys/class/block/mmcblk0p12/size)" = 32768
dd if=recovery-twrp.img of=/dev/mmcblk0p12 bs=65536 conv=fsync
expected=$(cut -d ' ' -f 1 recovery-twrp.img.sha256)
actual=$(sha256sum /dev/mmcblk0p12 | cut -d ' ' -f 1)
test "$expected" = "$actual"
rm -f "$BASE/boot-trial"
printf 'boot-recovery' | dd of=/dev/mmcblk0p8 bs=32 count=1 conv=notrunc,sync
sync
reboot -f

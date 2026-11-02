#!/bin/sh
# Mount the eMMC data partition (Amazon's /var/local, ext3, label "data") read-write at
# /mnt/data for Jarvis' state (/mnt/data/jarvis). The stock system mounts it read-write at every
# boot too; Amazon's files stay untouched. Checks label and ext3 magic before mounting.
PATH=/sbin:/bin:/usr/sbin:/usr/bin
DEV=/dev/mmcblk0p8
MNT=/mnt/data
grep -q " $MNT " /proc/mounts && exit 0
[ -b "$DEV" ] || { echo "mount-data: $DEV missing"; exit 1; }
python -c '
import struct, sys
f = open(sys.argv[1], "rb"); f.seek(1024); sb = f.read(1024)
ok = struct.unpack("<H", sb[56:58])[0] == 0xEF53 and sb[120:136].rstrip(chr(0)) == "data"
sys.exit(0 if ok else 1)' "$DEV" || { echo "mount-data: $DEV is not the ext3 data partition"; exit 1; }
mkdir -p "$MNT"
mount -t ext3 -o rw,noatime,data=ordered "$DEV" "$MNT" || exit 1
mkdir -p "$MNT/jarvis"
chmod 700 "$MNT/jarvis"
echo "mount-data: $MNT ready"

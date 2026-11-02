#!/bin/sh
# Avoid signaling a stock init: our PID 1 is a small maintenance supervisor.
sync || exit 1
# Jarvis' state on the data partition: read-only first, so its journal is clean.
grep -q ' /mnt/data ' /proc/mounts && mount -o remount,ro /mnt/data 2>/dev/null
if ! mount -o remount,ro / 2>/dev/null; then
    # Busy: a running script still holds a replaced file (an update). Force all filesystems
    # read-only through the kernel's emergency remount, after syncing.
    sync
    if [ -w /proc/sysrq-trigger ]; then
        echo u > /proc/sysrq-trigger
        sleep 2
    fi
fi
sync
exec /bin/busybox reboot -f

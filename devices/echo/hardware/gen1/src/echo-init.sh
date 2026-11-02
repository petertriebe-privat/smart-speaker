#!/bin/sh
# Minimal PID 1 for Echo Gen 1. Original kernel/DSP memory layout is retained.
PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/bin
export PATH
BASE=/usr/local/echo-dev
if [ "$$" != 1 ]; then
    echo "This program must only run as PID 1" >&2
    exit 1
fi
trap '' INT QUIT HUP TSTP
# Feed the already armed hardware watchdog before optional services.
watchdog_loop() {
    ( exec 3>/dev/watchdog || exit 1
      while :; do printf . >&3 || exit 1; sleep 10; done ) &
    WATCHDOG_PID=$!
}
watchdog_loop
mount -t proc proc /proc 2>/dev/null || :
mount -t sysfs sysfs /sys 2>/dev/null || :
# Kernel normally supplies devtmpfs; do not hide existing nodes.
if [ ! -c /dev/null ]; then mount -t devtmpfs devtmpfs /dev; fi
mount -o remount,ro / || echo "WARNING: root remount read-only failed"
mount -t tmpfs -o size=32m,mode=755 tmpfs /mnt
mkdir -p /mnt/echo-net /mnt/volatile/tmp /mnt/volatile/run /mnt/volatile/log /mnt/volatile/lock /mnt/volatile/cache /dev/pts /dev/shm
chmod 700 /mnt/echo-net
chmod 1777 /mnt/volatile/tmp /dev/shm
mount -t devpts -o mode=0620,gid=5 devpts /dev/pts
mount -t tmpfs -o size=10m,mode=1777 tmpfs /dev/shm
/sbin/ifconfig lo up
printf 'echo-dev init active; root read-only; starting WLAN and SSH\n'
# The physical UART is the recovery interface; remote access uses SSH keys.
/bin/sh -i </dev/ttyO0 >/dev/ttyO0 2>&1 &
CONSOLE_PID=$!
"$BASE/service-loop.sh" >>/mnt/echo-net/services.log 2>&1 &
SERVICES_PID=$!
while :; do
    if ! kill -0 "$WATCHDOG_PID" 2>/dev/null; then watchdog_loop; fi
    if ! kill -0 "$SERVICES_PID" 2>/dev/null; then
        "$BASE/service-loop.sh" >>/mnt/echo-net/services.log 2>&1 &
        SERVICES_PID=$!
    fi
    if ! kill -0 "$CONSOLE_PID" 2>/dev/null; then
        /bin/sh -i </dev/ttyO0 >/dev/ttyO0 2>&1 &
        CONSOLE_PID=$!
    fi
    sleep 5
done

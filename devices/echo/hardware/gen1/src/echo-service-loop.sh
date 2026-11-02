#!/bin/sh
# Restart individual maintenance services without blocking PID 1 or watchdog.
PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/bin
export PATH
# ECHO_DEV_BASE: only for the RAM trial on a new Echo (hardware/gen1/tools/bootstrap-ram-access.sh).
BASE=${ECHO_DEV_BASE:-/usr/local/echo-dev}
RUNTIME=/mnt/echo-net
WPA_PID=
DHCP_PID=
SSH_PID=
JARVIS_PID=
TS_PID=
JARVIS_STARTS=0
JARVIS_SINCE=
alive() { [ -n "$1" ] && kill -0 "$1" 2>/dev/null; }
load_wifi() {
    grep -q '^compat ' /proc/modules || insmod /lib/modules/2.6.37/compat.ko || return 1
    grep -q '^cfg80211 ' /proc/modules || insmod /lib/modules/2.6.37/cfg80211.ko || return 1
    if ! grep -q '^ath6kl_sdio ' /proc/modules; then
        country=$(sed -n 's/^country=//p' "$BASE/private/wpa_supplicant.conf")
        domain=$(tr -d '\042()' < /etc/default/country-code-to-reg-domain | awk -v c="$country" '$1==c {print $2;exit}')
        case "$domain" in 0x[0-9a-fA-F]*) : ;; *) domain=0x0000 ;; esac
        bdata=/lib/firmware/ath6k/AR6004/hw3.0/bdata.bin
        if /usr/local/bin/boardquery is_Pancake && [ -f /lib/firmware/ath6k/AR6004/hw3.0/bdata-pancake.bin ]; then
            bdata=/lib/firmware/ath6k/AR6004/hw3.0/bdata-pancake.bin
        fi
        mac=$(sed 's/../&:/g;s/:$//' /proc/mac)
        insmod /lib/modules/2.6.37/ath6kl_sdio.ko reg_domain="$domain" ath6kl_p2p=0x613 ath6kl_roam_mode=0x2 bdatapath="$bdata" debug_quirks=0x00a02008 ath6kl_wifi_mac="$mac" || return 1
        unset mac
    fi
    [ -d /sys/class/net/wlan0 ]
}
# A speaker does not move: no firmware roaming. In bssbias mode (the module's 0x2) the Echo 2
# hopped ~40 times an hour between a mesh repeater at -35 dBm and the router at -78 dBm, and every
# hop dropped the live connection and DNS (31.10.2026). Locked, it keeps the AP wpa_supplicant
# picked; if that AP goes away the disconnect makes wpa_supplicant choose again. Checked every
# round: the firmware may reset the mode when it reassociates.
lock_roaming() {
    [ -d /sys/kernel/debug/ieee80211 ] || mount -t debugfs none /sys/kernel/debug 2>/dev/null || return 0
    for d in /sys/kernel/debug/ieee80211/*/ath6kl; do
        [ -w "$d/roam_mode" ] || continue
        grep -q lock "$d/roam_mode" || { echo lock > "$d/roam_mode" && echo "$(cut -d' ' -f1 /proc/uptime) roaming locked"; }
    done
}
# Roaming between mesh APs can drop the default route while the DHCP lease stays valid.
keep_route() {
    [ -s "$RUNTIME/router" ] || return 0
    route -n | grep -q '^0\.0\.0\.0' && return 0
    gw=$(head -n 1 "$RUNTIME/router")
    route add default gw "$gw" dev wlan0 2>/dev/null && echo "$(cut -d' ' -f1 /proc/uptime) default route restored"
}
# Logs live in tmpfs (32 MiB): keep one old generation of at most 2 MiB each.
rotate() {
    for f in "$@"; do
        [ -f "$f" ] || continue
        size=$(wc -c < "$f")
        [ "$size" -gt 2097152 ] || continue
        # copy and truncate: the writers keep their O_APPEND descriptors
        cp -f "$f" "$f.old"
        : > "$f"
    done
}
start_jarvis() {
    [ -x "$BASE/jarvis/jarvis" ] && [ -s "$BASE/private/jarvis-secrets.json" ] || return 0
    [ -e "$BASE/jarvis/disabled" ] && return 0
    # After repeated crashes, back off so a broken build does not spin. A run of more than five
    # minutes was no crash loop (deploys restart it on purpose): count from zero again.
    now=$(cut -d. -f1 /proc/uptime)
    [ -n "$JARVIS_SINCE" ] && [ $((now - JARVIS_SINCE)) -gt 300 ] && JARVIS_STARTS=0
    [ "$JARVIS_STARTS" -gt 5 ] && [ $((now % 300)) -gt 10 ] && return 0
    JARVIS_SINCE=$now
    JARVIS_STARTS=$((JARVIS_STARTS+1))
    [ -x "$BASE/mount-data.sh" ] && "$BASE/mount-data.sh"
    mkdir -p /mnt/jarvis
    chmod 700 /mnt/jarvis
    "$BASE/jarvis/jarvis" -secrets "$BASE/private/jarvis-secrets.json" -settings "$BASE/jarvis/settings.json" \
        -audio "$BASE/echo-audio.sh" -ctl /mnt/jarvis/ctl.sock >>"$RUNTIME/jarvis.log" 2>&1 </dev/null &
    JARVIS_PID=$!
}
# Tailscale (#46): the Echo in the household's tailnet, so a Claude Code session on the server
# reaches it by SSH from anywhere. Userspace networking (no TUN on the old kernel needed);
# inbound connections to its tailnet address go to the local sshd. The login lives on the data
# partition: never start without it, or the node would log in afresh. No log upload to Tailscale.
start_tailscale() {
    [ -x "$BASE/tailscale/tailscaled" ] || return 0
    [ -e "$BASE/tailscale/disabled" ] && return 0
    grep -q ' /mnt/data ' /proc/mounts || { [ -x "$BASE/mount-data.sh" ] && "$BASE/mount-data.sh"; }
    grep -q ' /mnt/data ' /proc/mounts || return 0
    mkdir -p /mnt/data/tailscale
    chmod 700 /mnt/data/tailscale
    HOME=/mnt/data/tailscale "$BASE/tailscale/tailscaled" --tun=userspace-networking --no-logs-no-support \
        --statedir=/mnt/data/tailscale --socket="$RUNTIME/tailscaled.sock" --port=41641 \
        >>"$RUNTIME/tailscale.log" 2>&1 </dev/null &
    TS_PID=$!
}
# Bluetooth (#51): once a minute bluetooth.sh brings up what is missing, in the background (the
# first attach measures the WiFi for ~25 s). It does nothing until /mnt/data/bluetooth.enabled
# exists, and nothing while the WiFi has no route: the WiFi is the only way to the device.
BT_PID=
BT_AT=0
start_bluetooth() {
    [ -f "$BASE/bluetooth.sh" ] || return 0
    [ -e /mnt/data/bluetooth.enabled ] || return 0
    alive "$BT_PID" && return 0
    now=$(cut -d. -f1 /proc/uptime)
    [ $((now - BT_AT)) -ge 60 ] || return 0
    BT_AT=$now
    sh "$BASE/bluetooth.sh" ensure </dev/null >/dev/null 2>&1 &
    BT_PID=$!
}
# Once a minute a short health line on the data partition (survives a hang and the reboot after
# it): load, memory, the busiest processes, new kernel warnings. 16.10.2026 the device answered
# ping but no SSH until it was power-cycled; the RAM logs were lost.
HEALTH_AT=0
health() {
    now=$(cut -d. -f1 /proc/uptime)
    [ $((now - HEALTH_AT)) -ge 60 ] || return 0
    HEALTH_AT=$now
    grep -q ' /mnt/data ' /proc/mounts || return 0
    f=/mnt/data/jarvis/health.log
    {
        echo "== $(date '+%Y-%m-%d %H:%M:%S') up $now load $(cut -d' ' -f1-4 /proc/loadavg) free $(sed -n 's/^MemFree: *//p' /proc/meminfo) tmpfs $(df /mnt | awk 'NR==2 {print $5}')"
        top -b -n 1 2>/dev/null | sed -n '5,9p'
        dmesg -c 2>/dev/null | grep -i -E 'oops|bug|error|warn|hung|blocked|mmc|i2c' | tail -n 5
    } >> "$f"
    [ "$(wc -c < "$f")" -gt 524288 ] && { tail -c 262144 "$f" > "$f.tmp" && mv -f "$f.tmp" "$f"; }
}
# Memory guard. A working SSH connection comes first (20.10.2026: the mirror read the wake word
# recordings into RAM, the kernel dropped everyone's code pages, sshd stopped answering and the
# watchdog reset Echo 1 every two minutes). Jarvis guards himself (internal/memguard: he ends
# above memory_limit_mb, and his children inherit oom_score_adj 500); this is the second line:
# - sshd with its sessions, init, the watchdog feeder and this loop are never chosen by the
#   kernel's OOM killer (-1000; sshd resets it for the sessions it forks);
# - every 2 s: reclaimable memory below 12 MB kills Jarvis and librespot (the loop restarts
#   Jarvis), logged in health.log;
# - the kernel keeps 8 MB free for itself (Wi-Fi, SSH) instead of 1.8 MB.
mem_avail_kb() {
    # no MemAvailable on 2.6.37: free + unmapped file cache + reclaimable slab (as memguard)
    awk '/^MemFree:/{f=$2} /^Active\(file\):/{a=$2} /^Inactive\(file\):/{i=$2} /^Mapped:/{m=$2}
         /^SReclaimable:/{s=$2} END {x=a+i-m; if (x<0) x=0; print f+x+s}' /proc/meminfo
}
protect() {
    for p in 1 $$ "$@" $(pidof sshd); do echo -1000 > /proc/$p/oom_score_adj 2>/dev/null; done
    for d in /proc/[0-9]*; do
        [ "$(readlink $d/fd/3 2>/dev/null)" = /dev/watchdog ] && echo -1000 > $d/oom_score_adj 2>/dev/null
    done
}
memwatch() {
    while :; do
        protect
        avail=$(mem_avail_kb)
        if [ -n "$avail" ] && [ "$avail" -lt 12288 ]; then
            pids=$(pidof jarvis librespot)
            [ -n "$pids" ] && kill -9 $pids 2>/dev/null
            [ -d /mnt/data/jarvis ] && echo "== $(date '+%Y-%m-%d %H:%M:%S') memory guard: $avail kB reclaimable, killed jarvis and librespot ($pids)" >> /mnt/data/jarvis/health.log
            sleep 10
        fi
        sleep 2
    done
}
echo 8192 > /proc/sys/vm/min_free_kbytes 2>/dev/null
memwatch </dev/null >/dev/null 2>&1 &
protect $!
while :; do
    health
    if load_wifi; then
        /usr/sbin/iw dev wlan0 set power_save off 2>/dev/null || :
        if ! alive "$WPA_PID"; then
            /usr/bin/wpa_supplicant -D nl80211 -i wlan0 -c "$BASE/private/wpa_supplicant.conf" -f "$RUNTIME/wpa.log" &
            WPA_PID=$!
        fi
        if ! alive "$DHCP_PID"; then
            /sbin/udhcpc -f -i wlan0 -s "$BASE/udhcpc.script" -p "$RUNTIME/udhcpc.pid" >>"$RUNTIME/dhcp.log" 2>&1 &
            DHCP_PID=$!
        fi
        keep_route
        lock_roaming >>"$RUNTIME/services.log" 2>&1
        start_bluetooth
    fi
    if ! alive "$SSH_PID"; then
        "$BASE/bin/sshd" -D -f "$BASE/sshd_config" -E "$RUNTIME/sshd.log" &
        SSH_PID=$!
    fi
    if ! alive "$JARVIS_PID"; then
        start_jarvis
    fi
    if ! alive "$TS_PID"; then
        start_tailscale
    fi
    rotate "$RUNTIME/jarvis.log" "$RUNTIME/wpa.log" "$RUNTIME/dhcp.log" "$RUNTIME/services.log" "$RUNTIME/tailscale.log"
    sleep 10
done

#!/bin/sh
# Bluetooth at every boot (issue #51, step 4): run by the service loop once a minute, it brings
# up what is missing - the chip, the WiFi coexistence, bluetoothd with its key store, the audio
# profiles and oFono - and nothing that already runs. Jarvis does the rest (pairing, calls).
#
# Off until /mnt/data/bluetooth.enabled exists: a deploy must not change how the device boots
# by itself. The trial (hardware/gen1/tools/bt-try.sh, docs/17-bluetooth.md) found the WiFi unharmed, but the
# WiFi is the only way to the Echo and shares the chip with Bluetooth: so the chip is measured
# before and after it is attached, and if the WiFi suffers, Bluetooth goes down and stays down
# (/mnt/data/bluetooth.blocked) until somebody looks at the log and removes that file.
#
#   sh bluetooth.sh ensure    bring up what is missing (the service loop)
#   sh bluetooth.sh down      everything off (until the next ensure, unless blocked)
#   sh bluetooth.sh status
#
# Never touch GPIO 21 - that is the WiFi reset. Bluetooth is GPIO 22.
PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/bin
export PATH
DATA=/mnt/data/bluetooth                      # the key store (bind-mounted on KEYS)
ENABLED=/mnt/data/bluetooth.enabled
BLOCKED=/mnt/data/bluetooth.blocked
LOG=/mnt/data/bluetooth.log
STATE=/mnt/bluetooth                          # tmpfs: wiped with every boot
RESET=/sys/class/gpio/gpio22/value
KEYS=/var/local/lib/bluetooth                 # compiled into bluetoothd, on the read-only root
AUDIOCONF=/var/local/bluez/audio.conf         # what /etc/bluetooth/audio.conf points to
AUDIOVARIANT=/etc/bluetooth/dopplerAudioHFP.conf
PULSE_SOCKET=/mnt/echo-audio/pulse-native
MAX_ATTACH=3                                  # per boot: a chip that keeps dying stays off
LOSS_MAX=20                                   # percent more loss after attaching: the WiFi suffers

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') $*" >>"$LOG"; }
running() { pidof "$1" >/dev/null 2>&1; }
attached() { [ -d /sys/class/bluetooth/hci0 ]; }
gateway() { route -n | awk '$1 == "0.0.0.0" { print $2; exit }'; }

# loss: percent of 10 pings to the gateway that got no answer (100 without a route).
loss() {
	gw=$(gateway)
	[ -n "$gw" ] || { echo 100; return; }
	ping -c 10 -W 1 -q "$gw" 2>/dev/null | sed -n 's/.* \([0-9]*\)% packet loss.*/\1/p' | head -n 1
}

pulse() {
	env PULSE_SERVER=unix:$PULSE_SOCKET PULSE_COOKIE=/mnt/echo-audio/pulse-cookie pactl "$@"
}

attach() {
	attempts=$(cat "$STATE/attaches" 2>/dev/null || echo 0)
	if [ "$attempts" -ge "$MAX_ATTACH" ]; then
		[ -e "$STATE/gave-up" ] || { log "attach: $attempts attempts this boot, giving up until the next"; touch "$STATE/gave-up"; }
		return 1
	fi
	echo $((attempts + 1)) >"$STATE/attaches"
	before=$(loss)
	[ -w "$RESET" ] && { echo 0 >"$RESET"; usleep 20000; echo 1 >"$RESET"; usleep 100000; }
	setsid /usr/sbin/hciattach -n /dev/ttyO2 ath3k 3000000 flow nosleep $(cat /proc/btmac) >>"$LOG" 2>&1 </dev/null &
	i=0
	while [ $i -lt 20 ] && ! attached; do sleep 1; i=$((i + 1)); done
	if ! attached; then
		log "attach: no hci0 after ${i}s"
		down
		return 1
	fi
	hciconfig hci0 up >>"$LOG" 2>&1
	setsid /usr/local/bin/abtfilt-hw3.0 -n -z -m -a -w wlan0 >>"$LOG" 2>&1 </dev/null &
	sleep 3
	after=$(loss)
	log "attach: hci0 after ${i}s, WiFi loss ${before:-?}% before, ${after:-?}% after"
	if [ "${after:-100}" -gt $((${before:-0} + LOSS_MAX)) ]; then
		log "attach: the WiFi suffers - Bluetooth off and blocked until $BLOCKED is removed"
		touch "$BLOCKED"
		down
		return 1
	fi
}

ensure() {
	[ -e "$ENABLED" ] || return 0
	[ -e "$BLOCKED" ] && return 0
	grep -q ' /mnt/data ' /proc/mounts || return 0
	mkdir -p "$STATE"
	[ -n "$(gateway)" ] || return 0 # the WiFi first: it is the only way to the device
	if ! attached || ! running hciattach; then
		running hciattach && kill $(pidof hciattach) 2>/dev/null
		attach || return 1
	fi
	running abtfilt-hw3.0 || setsid /usr/local/bin/abtfilt-hw3.0 -n -z -m -a -w wlan0 >>"$LOG" 2>&1 </dev/null &
	if ! grep -q " $KEYS " /proc/mounts; then
		mkdir -p "$DATA" && chmod 700 "$DATA"
		mount -o bind "$DATA" "$KEYS" && log "key store $DATA on $KEYS"
	fi
	if [ ! -e "$AUDIOCONF" ]; then
		# The one write to the root file system: a link, as Amazon's bluez job made it.
		if mount -o remount,rw / 2>/dev/null; then
			mkdir -p "$(dirname "$AUDIOCONF")" && ln -sf "$AUDIOVARIANT" "$AUDIOCONF"
			sync
			mount -o remount,ro / || log "root still rw (busy)"
			log "$AUDIOCONF -> $AUDIOVARIANT"
		fi
	fi
	if ! running bluetoothd; then
		setsid /usr/sbin/bluetoothd -n >>"$LOG" 2>&1 </dev/null &
		sleep 3
		running bluetoothd && log "bluetoothd up" || log "bluetoothd did not stay up"
	fi
	# PulseAudio is Jarvis' (echo-audio.sh) and may restart: its modules are checked every time.
	# The policy plays A2DP from the phone on the speaker, but NOT its hands-free profile (hfgw):
	# with that, PulseAudio looped the call between the phone and the speaker and microphone
	# itself, next to Jarvis' bridge and without its gate - the far end heard herself, late
	# (17.10.2026). Call audio is Jarvis' (hw:0,2).
	if [ -S "$PULSE_SOCKET" ]; then
		modules=$(pulse list modules short 2>/dev/null)
		echo "$modules" | grep -q module-bluetooth-discover ||
			{ pulse load-module module-bluetooth-discover >/dev/null 2>&1 && log "PulseAudio module-bluetooth-discover"; }
		if ! echo "$modules" | grep module-bluetooth-policy | grep -q "hfgw=false"; then
			echo "$modules" | grep -q module-bluetooth-policy && pulse unload-module module-bluetooth-policy 2>/dev/null
			pulse load-module module-bluetooth-policy hfgw=false a2dp_source=true >/dev/null 2>&1 &&
				log "PulseAudio module-bluetooth-policy (A2DP only)"
		fi
	fi
	if ! running ofonod; then
		setsid /usr/sbin/ofonod -n >>"$LOG" 2>&1 </dev/null &
		log "ofonod started"
	fi
	# Keep the log small: it is on the data partition.
	[ "$(wc -c <"$LOG" 2>/dev/null || echo 0)" -gt 524288 ] && { tail -c 262144 "$LOG" >"$LOG.tmp" && mv -f "$LOG.tmp" "$LOG"; }
	return 0
}

down() {
	[ -S "$PULSE_SOCKET" ] && for module in module-bluetooth-policy module-bluetooth-discover; do
		pulse unload-module "$module" 2>/dev/null
	done
	for name in ofonod bluetoothd abtfilt-hw3.0 hciattach; do
		running "$name" && kill $(pidof "$name") 2>/dev/null
	done
	attached && hciconfig hci0 down >/dev/null 2>&1
	grep -q " $KEYS " /proc/mounts && umount "$KEYS"
	log "down"
}

status() {
	echo "enabled:    $([ -e "$ENABLED" ] && echo yes || echo no)$([ -e "$BLOCKED" ] && echo ' (BLOCKED, see the log)')"
	for name in hciattach abtfilt-hw3.0 bluetoothd ofonod; do
		printf '%-11s %s\n' "$name:" "$(running "$name" && echo yes || echo no)"
	done
	echo "hci0:       $(attached && echo yes || echo no)"
	tail -n 5 "$LOG" 2>/dev/null
}

case "${1:-}" in
ensure) ensure ;;
down) down ;;
status) status ;;
*)
	echo "usage: $0 ensure|down|status"
	exit 2
	;;
esac

#!/bin/sh
# Bluetooth on the Echo, one step at a time and with a dead man's switch (issue #51).
#
# The Echo hangs on wlan0 and on nothing else, and Bluetooth and WiFi share one Atheros chip:
# a failed attempt can take the device off the network, and the only ways back are the serial
# console and the power plug. So every step here undoes itself unless somebody confirms it from
# a NEW login within a few minutes, the watchdog also undoes it the moment the gateway stops
# answering, and nothing is written into the boot path: after a power cut the device is in its
# known state again.
#
#   sh bt-try.sh attach [minutes]   GPIO reset and hciattach, no daemon at all
#   sh bt-try.sh filter [minutes]   plus abtfilt, the Atheros coexistence daemon
#   sh bt-try.sh daemon [minutes]   plus bluetoothd on the running system bus (with its key
#                                   store on the data partition and Amazon's audio.conf link)
#   sh bt-try.sh audio [minutes]    plus PulseAudio's Bluetooth modules (A2DP: music from the
#                                   phone) and ofonod (HFP: the hands-free role)
#   sh bt-try.sh scan              inquiry: does the chip see anything out there
#   sh bt-try.sh status             what is up, and how the WiFi is doing
#   sh bt-try.sh keep               cancel the rollback (only after the WiFi was checked)
#   sh bt-try.sh down               undo everything, now
#
# The steps build on each other; each arms the switch again with its own window (default
# 2 minutes) BEFORE it does anything, and the watchdog detaches itself with setsid - so a step
# that takes the link with it still gets undone, even though the session that started it is
# gone. The steps themselves run in the session, so their output is seen:
#
#   sh /mnt/bt-try.sh attach 2
#
# The procedure, what to measure and what the device holds: devices/echo/docs/17-bluetooth.md.
# Never touch GPIO 21 - that is the WiFi reset. Bluetooth is GPIO 22.
set -u

TTY=/dev/ttyO2                     # the BT chip's UART, as in Amazon's hciattach job
SPEED=3000000
RESET=/sys/class/gpio/gpio22/value # Bluetooth reset (GPIO 21 is WiFi: hands off)
ABTFILT=/usr/local/bin/abtfilt-hw3.0
STATE=/mnt/bluetooth               # tmpfs on purpose: a power cut wipes the attempt
LOG=/mnt/data/bluetooth-try.log    # data partition: survives a hang and a hard reboot
DEADLINE=$STATE/deadline
KEEP=$STATE/keep
PIDFILE=$STATE/watchdog.pid
RESET_BEFORE=$STATE/reset-before
# BlueZ keeps its link keys in /var/local/lib/bluetooth (compiled in). That is on the read-only
# root; on Amazon's image the data partition was /var. Without it every connection pairs anew.
KEYS=/var/local/lib/bluetooth
KEYSTORE=/mnt/data/bluetooth       # fresh, not Amazon's old store in /mnt/data/local/lib
# bluetoothd reads /etc/bluetooth/audio.conf, a link to this one, which Amazon's bluez job made
# at every start. Without it there is no HFP configuration (SCORouting=PCM).
AUDIOCONF=/var/local/bluez/audio.conf
AUDIOVARIANT=/etc/bluetooth/dopplerAudioHFP.conf
PULSE="env PULSE_SERVER=unix:/mnt/echo-audio/pulse-native PULSE_COOKIE=/mnt/echo-audio/pulse-cookie pactl"
PINGS=50                          # for the before/after measurement
MINUTES=2

mkdir -p "$STATE" 2>/dev/null

log() {
	echo "$(date '+%Y-%m-%d %H:%M:%S') $*" >>"$LOG"
	echo "$*"
}

gateway() { route -n | awk '$1 == "0.0.0.0" { print $2; exit }'; }

alive() {
	gw=$(gateway)
	[ -n "$gw" ] || return 1
	ping -c 1 -W 2 -w 3 -q "$gw" >/dev/null 2>&1
}

# wlan: the link and PINGS pings to the gateway, to compare before and after every step.
wlan() {
	gw=$(gateway)
	signal=$(iw dev wlan0 link 2>/dev/null | awk '/signal/ { print $2, $3 }')
	if [ -z "$gw" ]; then
		log "WLAN $1: no default route"
		return
	fi
	stats=$(ping -c "$PINGS" -q "$gw" 2>&1 | tr '\n' ' ' | sed 's/  */ /g')
	log "WLAN $1: signal ${signal:-?}, $stats"
}

running() { pidof "$1" >/dev/null 2>&1; }
attached() { [ -d /sys/class/bluetooth/hci0 ]; }
watching() { [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }

# arm: (re)start the dead man's switch. Always before a step, never after it: a step that takes
# the network with it must find the switch already armed.
arm() {
	minutes=$1
	expr "$minutes" + 0 >/dev/null 2>&1 || minutes=$MINUTES
	rm -f "$KEEP"
	echo $(($(date +%s) + minutes * 60)) >"$DEADLINE"
	if watching; then
		log "window now $minutes minute(s)"
	else
		setsid sh "$0" watchdog >/dev/null 2>&1 &
		log "watchdog armed: everything goes back in $minutes minute(s) unless 'bt-try.sh keep'"
	fi
}

# watchdog: undoes everything at the deadline, and at once when the gateway stops answering.
watchdog() {
	watching && exit 0
	echo $$ >"$PIDFILE"
	trap 'rm -f "$PIDFILE"' EXIT INT TERM
	misses=0
	while :; do
		sleep 5
		if [ -f "$KEEP" ]; then
			log "watchdog: kept by hand, no rollback"
			return 0
		fi
		if alive; then
			misses=0
		else
			misses=$((misses + 1))
			if [ "$misses" -ge 3 ]; then
				log "watchdog: the gateway stopped answering - rolling back"
				down
				return 0
			fi
		fi
		if [ "$(date +%s)" -ge "$(cat "$DEADLINE" 2>/dev/null || echo 0)" ]; then
			log "watchdog: nobody confirmed - rolling back"
			down
			return 0
		fi
	done
}

attach() {
	if attached; then
		log "attach: hci0 is already there"
		return 0
	fi
	wlan before
	log "attach: resetting the chip (GPIO 22) and attaching $TTY"
	if [ -w "$RESET" ]; then
		[ -f "$RESET_BEFORE" ] || cat "$RESET" >"$RESET_BEFORE" # what down goes back to
		echo 0 >"$RESET"
		usleep 20000
		echo 1 >"$RESET"
		usleep 100000
	else
		log "attach: $RESET not writable - no reset (continuing anyway)"
	fi
	mac=$(cat /proc/btmac 2>/dev/null)
	# -n keeps hciattach in the foreground, so it is backgrounded here (Amazon's job respawned it).
	setsid /usr/sbin/hciattach -n "$TTY" ath3k "$SPEED" flow nosleep $mac >>"$LOG" 2>&1 &
	i=0
	while [ $i -lt 20 ] && ! attached; do
		sleep 1
		i=$((i + 1))
	done
	if ! attached; then
		log "attach: FAILED, no hci0 after ${i}s"
		return 1
	fi
	hciconfig hci0 up >>"$LOG" 2>&1
	log "attach: hci0 is up after ${i}s"
	hciconfig -a hci0 >>"$LOG" 2>&1
	wlan after-attach
}

filter() {
	attached || {
		log "filter: no hci0 - run attach first"
		return 1
	}
	if running abtfilt-hw3.0; then
		log "filter: already running"
		return 0
	fi
	[ -x "$ABTFILT" ] || {
		log "filter: $ABTFILT missing"
		return 1
	}
	log "filter: starting the coexistence daemon"
	setsid "$ABTFILT" -n -z -m -a -w wlan0 >>"$LOG" 2>&1 &
	sleep 2
	running abtfilt-hw3.0 || log "filter: it did not stay up - see $LOG"
	wlan after-filter
}

daemon() {
	attached || {
		log "daemon: no hci0 - run attach first"
		return 1
	}
	if running bluetoothd; then
		log "daemon: already running"
		return 0
	fi
	[ -S /var/run/dbus/system_bus_socket ] || {
		log "daemon: no system bus"
		return 1
	}
	if ! grep -q " $KEYS " /proc/mounts; then
		mkdir -p "$KEYSTORE" && chmod 700 "$KEYSTORE"
		mount -o bind "$KEYSTORE" "$KEYS" && log "daemon: key store $KEYSTORE on $KEYS"
	fi
	if [ ! -e "$AUDIOCONF" ]; then
		# The one write to the root file system: a link, as Amazon's job made it. Back to ro at once.
		mount -o remount,rw / && mkdir -p "$(dirname "$AUDIOCONF")" && ln -sf "$AUDIOVARIANT" "$AUDIOCONF"
		sync
		mount -o remount,ro / || log "daemon: root still rw (busy)"
		log "daemon: $AUDIOCONF -> $AUDIOVARIANT"
	fi
	log "daemon: starting bluetoothd"
	setsid /usr/sbin/bluetoothd -n >>"$LOG" 2>&1 </dev/null &
	sleep 3
	if running bluetoothd; then
		log "daemon: bluetoothd is up"
	else
		log "daemon: bluetoothd did not stay up - see $LOG"
	fi
	wlan after-daemon
}

# audio: what the phone needs to offer music and calls. PulseAudio registers the A2DP and HFP
# endpoints with BlueZ (discover) and plays a phone's A2DP stream on the speaker (policy). The
# hands-free role needs a "Handsfree Agent" at BlueZ, and that is oFono: without it the phone
# connects HFP and BlueZ drops it at once ("Handsfree Agent not registered", 17.10.2026). oFono
# makes a modem per paired phone; powering it connects HFP, and that is up to the assistant.
audio() {
	running bluetoothd || {
		log "audio: no bluetoothd - run daemon first"
		return 1
	}
	$PULSE list modules short 2>/dev/null | grep -q module-bluetooth-discover ||
		{ $PULSE load-module module-bluetooth-discover >/dev/null && log "audio: PulseAudio module-bluetooth-discover"; }
	# A2DP only: with hfgw the policy loops a call between phone and room itself (see
	# hardware/gen1/src/echo-bluetooth.sh); call audio is the assistant's bridge on hw:0,2.
	$PULSE list modules short 2>/dev/null | grep -q module-bluetooth-policy ||
		{ $PULSE load-module module-bluetooth-policy hfgw=false a2dp_source=true >/dev/null && log "audio: PulseAudio module-bluetooth-policy (A2DP only)"; }
	if ! running ofonod; then
		setsid /usr/sbin/ofonod -n >>"$LOG" 2>&1 </dev/null &
		sleep 3
		running ofonod || {
			log "audio: ofonod did not stay up - see $LOG"
			return 1
		}
		log "audio: ofonod is up"
	fi
	# The modem's Powered is the assistant's now (mobile_handsfree, off by default): powered here, the assistant
	# would switch it off again within half a second. Without the assistant, by hand:
	#   dbus-send --system --print-reply --dest=org.ofono /hfp/<adapter>_<phone> \
	#     org.ofono.Modem.SetProperty string:Powered variant:boolean:true
	wlan after-audio
}

# scan: an inquiry proves that the chip sends and receives, and pairs nothing.
scan() {
	attached || {
		log "scan: no hci0 - run attach first"
		return 1
	}
	log "scan: looking for devices (about 10 s)"
	hcitool scan --flush >>"$LOG" 2>&1
	log "scan: done, what it found is in $LOG"
	wlan after-scan
}

down() {
	log "down: undoing everything"
	for module in module-bluetooth-policy module-bluetooth-discover; do
		$PULSE unload-module "$module" 2>/dev/null
	done
	for name in ofonod bluetoothd abtfilt-hw3.0 hciattach; do
		if running "$name"; then
			kill $(pidof "$name") 2>/dev/null
			sleep 1
			running "$name" && kill -9 $(pidof "$name") 2>/dev/null
		fi
	done
	attached && hciconfig hci0 down >/dev/null 2>&1
	grep -q " $KEYS " /proc/mounts && umount "$KEYS" # the keys stay in $KEYSTORE
	# Back to the level GPIO 22 had before attach (1 on 17.10.2026, with the WiFi fine), not 0:
	# holding the chip in reset is a state the WiFi half has never been seen in.
	[ -w "$RESET" ] && [ -f "$RESET_BEFORE" ] && cat "$RESET_BEFORE" >"$RESET"
	rm -f "$DEADLINE" "$KEEP" "$RESET_BEFORE"
	sleep 1
	wlan after-down
	log "down: finished"
}

status() {
	echo "hci0:       $(attached && echo yes || echo no)"
	echo "hciattach:  $(running hciattach && echo yes || echo no)"
	echo "abtfilt:    $(running abtfilt-hw3.0 && echo yes || echo no)"
	echo "bluetoothd: $(running bluetoothd && echo yes || echo no)"
	echo "ofonod:     $(running ofonod && echo yes || echo no)"
	if [ -f "$KEEP" ]; then
		echo "rollback:   cancelled (keep)"
	elif watching; then
		echo "rollback:   in $(($(cat "$DEADLINE" 2>/dev/null || echo 0) - $(date +%s))) s"
	else
		echo "rollback:   not armed"
	fi
	attached && hciconfig -a hci0
	wlan status
}

step() {
	what=$1
	arm "${2:-$MINUTES}"
	if ! "$what"; then
		log "$what: failed - rolling back now"
		down
		exit 1
	fi
}

case "${1:-}" in
attach | filter | daemon | audio) step "$1" "${2:-$MINUTES}" ;;
scan) scan ;;
keep)
	touch "$KEEP"
	log "keep: the rollback is cancelled - 'bt-try.sh down' undoes everything by hand"
	;;
down) down ;;
status) status ;;
watchdog) watchdog ;;
*)
	echo "usage: $0 attach|filter|daemon|audio [minutes] | scan | status | keep | down"
	exit 2
	;;
esac

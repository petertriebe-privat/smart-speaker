#!/bin/sh
# Call audio over Bluetooth, the first look (issue #51, step 2). Run it, then start a call on the
# paired phone while HFP is connected: the phone sends the call over SCO, and with SCORouting=PCM
# it arrives on ALSA device hw:0,2 (8 kHz mono, the clock comes from the BT chip only while SCO
# runs - before that a read fails with an I/O error).
#
# It waits for the SCO link, records what the far end says for 40 s and plays beeps the whole call, which the
# far end should hear. Nothing is routed to the speaker or from the microphone yet.
#
#   sh /mnt/bt-sco-test.sh [seconds to wait for the call, default 180]
#   needs /mnt/bluetooth/beeps.raw (S16_LE, 8 kHz, mono)
OUT=/mnt/bluetooth
WAIT=${1:-180}
LOG=$OUT/sco.log
mkdir -p "$OUT"
: >"$LOG"

i=0
while [ "$i" -lt "$WAIT" ] && ! hcitool con | grep -q SCO; do
	sleep 1
	i=$((i + 1))
done
date >>"$LOG"
hcitool con >>"$LOG" 2>&1
if ! hcitool con | grep -q SCO; then
	echo "no SCO link after $WAIT s" | tee -a "$LOG"
	exit 1
fi
echo "SCO up after $i s, recording" | tee -a "$LOG"
arecord -D hw:0,2 -f S16_LE -r 8000 -c 1 -d 40 -t raw "$OUT/sco-in.raw" >>"$LOG" 2>&1 &
rec=$!
# SCO opens when the phone dials, not when the far end answers (the first trial heard the
# ringback on it, 17.10.2026), and oFono's call state did not tell the moment either. So the
# beeps run as long as the call: whenever the far end picks up, they hear them.
modem=$(dbus-send --system --print-reply --dest=org.ofono / org.ofono.Manager.GetModems | grep -o '/hfp/[^"]*' | head -n 1)
dbus-send --system --print-reply --dest=org.ofono "$modem" org.ofono.VoiceCallManager.GetCalls >>"$LOG" 2>&1
n=0
while hcitool con | grep -q SCO && kill -0 "$rec" 2>/dev/null; do
	aplay -q -D hw:0,2 -f S16_LE -r 8000 -c 1 -t raw "$OUT/beeps.raw" >>"$LOG" 2>&1 || break
	n=$((n + 1))
done
echo "played the beeps $n times" | tee -a "$LOG"
wait "$rec"
ls -l "$OUT/sco-in.raw" | tee -a "$LOG"

#!/bin/sh
# Bring up the original Echo audio chain (DSP, shared memory, audioCtrl,
# audioproxyd) and exactly one PulseAudio server with speaker sink and the
# DSP-processed 16 kHz microphone source. Follows the stock upstart jobs
# modules-ti-dsp, audio-shm-main, audioctrl, audioproxyd-main, pulseaudio-main.
# Usage: echo-audio.sh start|stop|status
PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/bin
export PATH
LD_LIBRARY_PATH=/usr/local/lib:/usr/lib:/lib
export LD_LIBRARY_PATH
RUN=/mnt/echo-audio
BIN=/usr/local/bin
MOD=/lib/modules/$(uname -r)
PULSE_SOCKET=$RUN/pulse-native

log() { echo "$(cat /proc/uptime | cut -d' ' -f1) echo-audio: $*"; }
alive() { [ -n "$1" ] && kill -0 "$1" 2>/dev/null; }
pidof_file() { [ -f "$1" ] && cat "$1"; }

start_modules() {
    grep -q '^cmemk ' /proc/modules || insmod $MOD/cmemk.ko phys_start=0x8D800000 phys_end=0x8DA00000 allowOverlap=1 useHeapIfPoolUnavailable=1 || return 1
    grep -q '^dsplinkk ' /proc/modules || insmod $MOD/dsplinkk.ko || return 1
    grep -q '^lpm_omap3530 ' /proc/modules || insmod $MOD/lpm_omap3530.ko || return 1
}

start_shm() {
    # Only create queues while no user is attached (fresh start).
    $BIN/shmq_tool -m 0 -S /QSpeakerIn.shm -n 95232
    $BIN/shmq_tool -m 0 -S /QEarconIn.shm -n 16000
    $BIN/shmq_tool -m 0 -S /AudiodCmd.shm -n 3000
    $BIN/shmbuf_tool -m 0 -S /BMicsOut.shm -n 1048575
    for n in BPhoneMic BRemoteMic BVoIPMic; do
        $BIN/shmbuf_tool -m 0 -S /$n.shm -n 16000
    done
    $BIN/shmbuf_tool -m 0 -S /BPlaybackAvgPower.shm -s 4 -n 50
    $BIN/shmbuf_tool -m 0 -S /BTraitReport.shm -s 512 -n 20
    $BIN/shmbuf_tool -m 0 -S /BAsrMetadata.shm -s 1 -n 131072
}

write_configs() {
    cat > $RUN/dbus.conf <<'EOF'
<busconfig>
  <type>system</type>
  <listen>unix:path=/var/run/dbus/system_bus_socket</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow user="root"/>
    <allow own="*"/>
    <allow send_destination="*"/>
    <allow receive_sender="*"/>
  </policy>
</busconfig>
EOF
    cat > $RUN/echo.pa <<EOF
load-module module-lab126-sink rate=48000 channels=2 channel_map=front-left,front-right file="/QSpeakerIn.shm" powerfile="/BPlaybackAvgPower.shm" fragment_usecs=16000 minimum_full_fragments=3 static_output_latency_usecs=136000
set-default-sink lab126-sink
load-module module-lab126-audiod fileCmd="/AudiodCmd.shm"
load-module module-lab126-source rate=16000 channels=1 channel_map=front-center file="/BVoIPMic.shm"
set-default-source lab126-source
load-module module-native-protocol-unix socket=$PULSE_SOCKET auth-anonymous=1 auth-cookie=$RUN/pulse-cookie
EOF
}

start() {
    mkdir -p $RUN /var/run/dbus $RUN/pulse-runtime $RUN/pulse-state
    if alive "$(pidof_file $RUN/pulseaudio.pid)"; then log "already running"; return 0; fi
    log "loading DSP modules"
    start_modules || { log "module load failed"; return 1; }
    [ -e /dev/shm/QSpeakerIn.shm ] || start_shm
    write_configs
    if [ ! -S /var/run/dbus/system_bus_socket ]; then
        dbus-daemon --config-file=$RUN/dbus.conf --fork --print-pid > $RUN/dbus.pid
    fi
    if ! alive "$(pidof_file $RUN/audioCtrl.pid)"; then
        # audioCtrl is a PulseAudio client too: tell it where our server listens.
        PULSE_SERVER=unix:$PULSE_SOCKET PULSE_COOKIE=$RUN/pulse-cookie \
            $BIN/audioCtrl -f /AudiodCmd.shm > $RUN/audioCtrl.log 2>&1 &
        echo $! > $RUN/audioCtrl.pid
        sleep 1
    fi
    if ! alive "$(pidof_file $RUN/audioproxyd.pid)"; then
        $BIN/audioproxyd -s2 -c2 -i /QSpeakerIn.shm -i /QEarconIn.shm -o /BMicsOut.shm -o /BPhoneMic.shm -o /BVoIPMic.shm -t /BTraitReport.shm -r /BPlaybackAvgPower.shm > $RUN/audioproxyd.log 2>&1 &
        echo $! > $RUN/audioproxyd.pid
        sleep 3
    fi
    [ -f $RUN/pulse-cookie ] || python -c 'import os; open("'$RUN'/pulse-cookie","wb").write(os.urandom(256))'
    PULSE_RUNTIME_PATH=$RUN/pulse-runtime PULSE_STATE_PATH=$RUN/pulse-state PULSE_COOKIE=$RUN/pulse-cookie \
        pulseaudio -n -F $RUN/echo.pa --daemonize=no --use-pid-file=no --exit-idle-time=-1 --disable-shm \
        --high-priority --realtime=1 --log-target=file:$RUN/pulse.log > $RUN/pulse-stdout.log 2>&1 &
    echo $! > $RUN/pulseaudio.pid
    i=0
    while [ $i -lt 20 ] && [ ! -S $PULSE_SOCKET ]; do sleep 1; i=$((i+1)); done
    if [ -S $PULSE_SOCKET ]; then log "pulseaudio ready"; else log "pulseaudio socket missing"; return 1; fi
    # Fixed DSP level (100: the ring's top is the speaker's maximum, as with Alexa); Jarvis sets
    # the volume as a gain on its own samples. One call only:
    # audioCtrl's LIPC property deadlocked during repeated calls (16.10.2026).
    $BIN/lipc-set-prop -i com.doppler.audiod MainVolume ${DSP_VOLUME:-100} > $RUN/volume.log 2>&1 &
}

stop() {
    for p in pulseaudio audioproxyd audioCtrl; do
        pid=$(pidof_file $RUN/$p.pid)
        alive "$pid" && kill "$pid"
    done
    sleep 2
    for p in pulseaudio audioproxyd audioCtrl; do
        pid=$(pidof_file $RUN/$p.pid)
        alive "$pid" && kill -9 "$pid"
        rm -f $RUN/$p.pid
    done
}

status() {
    for p in audioCtrl audioproxyd pulseaudio; do
        pid=$(pidof_file $RUN/$p.pid)
        if alive "$pid"; then echo "$p running ($pid)"; else echo "$p stopped"; fi
    done
}

case "$1" in
    start) start ;;
    stop) stop ;;
    status) status ;;
    *) echo "usage: $0 start|stop|status" >&2; exit 2 ;;
esac

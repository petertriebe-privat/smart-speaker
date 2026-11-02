#!/bin/sh
# Jarvis on the Echo Dot 2: a service of services.sh, which starts it at boot and again 3 s after
# it ends (its output goes to /run/echo-dot-dev/jarvis.sh.log). Installed by install-assistant.sh.
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH
BASE=/data/echo-dot-dev
J=$BASE/jarvis
RUN=/run/jarvis
LOG=/run/echo-dot-dev/jarvis.sh.log
if [ ! -x "$J/jarvis" ] || [ ! -s "$BASE/private/jarvis-secrets.json" ] || [ -e "$J/disabled" ]; then
    sleep 60
    exit 0
fi
mkdir -p "$RUN" /data/jarvis
chmod 700 "$RUN" /data/jarvis
ln -sf "$J/jarvis" /usr/local/bin/jarvis # jarvis ctl ... over SSH
# After repeated crashes, back off so a broken build does not spin (as the Echo's service loop).
now=$(cut -d. -f1 /proc/uptime)
echo "$now" >> "$RUN/starts"
tail -n 10 "$RUN/starts" > "$RUN/starts.new" && mv -f "$RUN/starts.new" "$RUN/starts"
if [ "$(awk -v n="$now" 'n - $1 < 300' "$RUN/starts" | wc -l)" -gt 5 ]; then
    echo "$now jarvis.sh: crash loop, waiting 5 minutes"
    sleep 300
fi
# A working SSH connection comes first: dropbear (and the sessions it forks) is never chosen by
# the OOM killer, Jarvis and his children (librespot) before anything else.
for p in $(pidof dropbear); do echo -1000 > "/proc/$p/oom_score_adj"; done
echo 500 > /proc/self/oom_score_adj
# The log lives in RAM: keep one old generation of at most 4 MiB (copy and truncate, the writer
# keeps its O_APPEND descriptor). One watcher, also across restarts.
if ! { [ -s "$RUN/rotate.pid" ] && kill -0 "$(cat "$RUN/rotate.pid")" 2>/dev/null; }; then
    ( while sleep 300; do
          if [ "$(wc -c < "$LOG")" -gt 4194304 ]; then cp -f "$LOG" "$LOG.old"; : > "$LOG"; fi
      done ) </dev/null >/dev/null 2>&1 &
    echo $! > "$RUN/rotate.pid"
fi
exec "$J/jarvis" -secrets "$BASE/private/jarvis-secrets.json" -settings "$J/settings.json" -ctl "$RUN/ctl.sock" </dev/null

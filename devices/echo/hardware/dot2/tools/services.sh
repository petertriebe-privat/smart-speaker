#!/bin/sh
# Each executable service script is restarted after exit; logs live in RAM.
BASE=/data/echo-dot-dev
mkdir -p "$BASE/services" /run/echo-dot-dev
for service in "$BASE"/services/*.sh; do
    [ -x "$service" ] || continue
    name=${service##*/}
    ( while true; do
        "$service" >> "/run/echo-dot-dev/$name.log" 2>&1
        sleep 3
      done ) &
done
while true; do sleep 3600; done

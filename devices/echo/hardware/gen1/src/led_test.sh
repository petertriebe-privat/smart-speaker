#!/bin/sh
# LED ring check: chip selftest, then light each of the 36 LP5523 channels in
# turn (0.4 s each), then all red, all green, all blue, then off.
L=/sys/class/leds
B=${1:-60}
for c in 0 1 2 3; do
    d=/sys/bus/i2c/devices/2-003$((c+2))
    echo "chip$c selftest=$(cat $d/selftest 2>&1) temp=$(cat $d/temp 2>&1)"
done
off() { for f in $L/lp5523:*/brightness; do echo 0 > $f; done; }
off
for c in 0 1 2 3; do
    for ch in 0 1 2 3 4 5 6 7 8; do
        echo "on chip$c ch$ch"
        echo $B > $L/lp5523:$c:channel$ch/brightness
        sleep 1
        echo 0 > $L/lp5523:$c:channel$ch/brightness
    done
done
off

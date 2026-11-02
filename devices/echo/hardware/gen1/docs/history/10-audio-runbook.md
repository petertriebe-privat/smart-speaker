# Resuming the successful software/audio test

**Result:** The user last heard the test tone generated on the Echo as "loud and clear". After that no console response; volume reset not confirmed. The test setup is not yet a stable finished audio solution. [Detailed result](09-audio-test.md).

The following single steps were run in the session. The whole cleaned sequence after a fresh cold start has not been tested again as one run. Above all, do not put a second PulseAudio instance on the same queue. After the kernel oops from the direct aplay test a fresh start makes sense for further reliable tests; do not reproduce the oops on purpose.

## 1. Prepare root and RAM

Run the [root start](03-boot.md), then mount proc/sysfs if not present yet. Service the watchdog right after reaching the shell:

```sh
(while :; do printf . >&3; sleep 10; done) 3>/dev/watchdog &
```

In the attempt this helper prevented the earlier short resets; uptime up to 821 seconds actually read. Note the background PID anew for each start. Do not `exit` from PID 1.

First confirm the root mount is `ro`. Do not overmount or delete an existing mount under `/mnt`. In the fresh tested minimal start `/mnt` was unused:

```sh
mount -t tmpfs -o size=16m tmpfs /mnt
mkdir -p /mnt/volatile/tmp /mnt/volatile/run /dev/shm
mount -t tmpfs -o size=8m tmpfs /dev/shm
mkdir -p /var/run/dbus /mnt/pulse-runtime /mnt/pulse-state
cat /proc/mounts
```

`/var/run`, `/tmp` and `/run` must point into the `/mnt` tmpfs as in the result. No internal data mount required. Original firmware files stay read-only; attempted volume persistence must not be "repaired" by remounting.

## 2. Transfer small files over UART

On the host form Base64 of the **byte content actually to be sent**. So far 240 Base64 characters per shell command; a command may have at most 512 characters in the sender.

```sh
: > /mnt/make_tone.py.b64
printf %s 'BASE64_CHUNK_0' >> /mnt/make_tone.py.b64; echo TRANSFER_0
printf %s 'BASE64_CHUNK_1' >> /mnt/make_tone.py.b64; echo TRANSFER_1
```

Replace placeholders. Confirm each marker as its own output line in the new log before the next chunk is sent. A marker in the merely echoed command line is no proof of execution.

```sh
python -c 'import binascii,hashlib; p="/mnt/make_tone.py"; d=binascii.a2b_base64(open(p+".b64").read()); open(p,"wb").write(d); print(hashlib.sha256(d).hexdigest())'
```

Compare the hash with the host. Do not run before a successful comparison. The same procedure suits the small configuration files. The text logger itself is not a binary transfer program.

Transfer `hardware/gen1/src/make_tone.py` to `/mnt/make_tone.py`. Then:

```sh
python /mnt/make_tone.py /mnt/echo-tone.wav
```

The file is deliberately very quiet (1 % digital amplitude), 660 Hz, 0.4 seconds. Python 2.7.2 is present, the modules `wave` and `ctypes` are missing. The script therefore does not use them.

## 3. Original modules and audio IPC

```sh
/sbin/insmod /lib/modules/2.6.37/cmemk.ko phys_start=0x8D800000 phys_end=0x8DA00000 allowOverlap=1 useHeapIfPoolUnavailable=1
/sbin/insmod /lib/modules/2.6.37/dsplinkk.ko
/sbin/insmod /lib/modules/2.6.37/lpm_omap3530.ko
```

Only with the same kernel, matching original modules and the proven reserved memory range. Do not load already loaded modules twice. Then create the original queues/buffers:

```sh
/usr/local/bin/shmq_tool -m 0 -S /QSpeakerIn.shm -n 95232
/usr/local/bin/shmq_tool -m 0 -S /QEarconIn.shm -n 16000
/usr/local/bin/shmq_tool -m 0 -S /AudiodCmd.shm -n 3000
/usr/local/bin/shmbuf_tool -m 0 -S /BMicsOut.shm -n 1048575
for n in BPhoneMic BRemoteMic BVoIPMic; do
    /usr/local/bin/shmbuf_tool -m 0 -S /$n.shm -n 16000
done
/usr/local/bin/shmbuf_tool -m 0 -S /BPlaybackAvgPower.shm -s 4 -n 50
/usr/local/bin/shmbuf_tool -m 0 -S /BTraitReport.shm -s 512 -n 20
/usr/local/bin/shmbuf_tool -m 0 -S /BAsrMetadata.shm -s 1 -n 131072
```

For existing queues first determine which processes use them. Do not re-initialise during operation. Split multi-line blocks into individual checked commands through the single-line sender.

## 4. D-Bus, audioCtrl and DSP proxy

Transfer `hardware/gen1/config/audio-test-dbus.conf` to `/mnt/test-dbus.conf`. It corresponds to the tested local minimal bus without automatic service activation. Start only if no other system bus runs on the socket:

```sh
/usr/bin/dbus-daemon --config-file=/mnt/test-dbus.conf --fork --print-pid > /mnt/test-dbus.pid
```

Previous failed attempts showed: the proxy needs D-Bus and the service `com.doppler.audiod`. So `audioCtrl` first, then the proxy:

```sh
PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin LD_LIBRARY_PATH=/usr/local/lib:/usr/lib:/lib /usr/local/bin/audioCtrl -f /AudiodCmd.shm > /mnt/audioCtrl.log 2>&1 &
PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin LD_LIBRARY_PATH=/usr/local/lib:/usr/lib:/lib /usr/local/bin/audioproxyd -s2 -c2 -i /QSpeakerIn.shm -i /QEarconIn.shm -o /BMicsOut.shm -o /BPhoneMic.shm -o /BVoIPMic.shm -t /BTraitReport.shm -r /BPlaybackAvgPower.shm > /mnt/audioproxyd.log 2>&1 &
```

Note the PIDs immediately, check logs and still-running processes. The proxy initialises the audio hardware including the capture path and needs the microphone buffers. After successful initialisation its microphone DSP was muted:

```sh
/usr/local/bin/lipc-set-prop -i com.doppler.audio DSP_mics_mute 1
```

No microphone recordings were saved or transferred as files and no Alexa application was started. `Mic Average Power` log lines remain visible despite the set DSP mute; do not derive a hardware microphone kill switch from that.

## 5. Start exactly one PulseAudio test server

Create the cookie in RAM only:

```sh
python -c 'import os; open("/mnt/pulse-cookie","wb").write(os.urandom(256))'
```

Transfer `hardware/gen1/config/audio-test.pa` to `/mnt/test.pa`. The template corresponds to the tested module setup including the explicit RAM cookie. `auth-anonymous=1` is only part of this temporary local diagnostic server, not a production configuration. When troubleshooting check the cookie path and module messages; do not put a second server next to a half-started one.

```sh
PULSE_RUNTIME_PATH=/mnt/pulse-runtime PULSE_STATE_PATH=/mnt/pulse-state PULSE_COOKIE=/mnt/pulse-cookie LD_LIBRARY_PATH=/usr/local/lib:/usr/lib:/lib /usr/bin/pulseaudio -n -F /mnt/test.pa --daemonize=no --use-pid-file=no --exit-idle-time=-1 --disable-shm --log-target=file:/mnt/pulse.log > /mnt/pulse-stdout.log 2>&1 &
```

```sh
PULSE_COOKIE=/mnt/pulse-cookie pactl -s unix:/mnt/pulse-native info
```

Expectation: `lab126-sink`, s16le, 2ch, 48000 Hz. No PulseAudio microphone module and no TCP protocol is loaded. The pure monitor source entry belongs to the sink.

## 6. Check volume and play

```sh
/usr/local/bin/lipc-get-prop com.doppler.audiod MainVolume
/usr/local/bin/lipc-set-prop -i com.doppler.audiod MainVolume 30
PULSE_COOKIE=/mnt/pulse-cookie paplay --server=unix:/mnt/pulse-native /mnt/echo-tone.wav
echo PLAYBACK_EXIT=$?
```

Wait for LIPC confirmation; a timeout does not verifiably change the value. Do not guess value ranges from `rawVolume`. Actually observed: MainVolume 30 -> rawVolume 99; 40 -> 115; 100 -> 175. These are internal values, not measured dB SPL.

For the loud test demanded by the user the 1 % signal was multiplied by 100 in RAM (peak about 32700, no exceeding of S16). Once 0.4 seconds were output audibly at confirmed MainVolume 100. **Do not repeat as a standard test:** the console response broke off afterwards. A safe end state or the planned reset to 30 was not confirmed. The next agent should first use a controlled restart and lower volume and investigate the cause (supply, connection, kernel/audio path).

## Error handling

- Direct `aplay` output to `hw:0,1` in the uninitialised minimal start caused a kernel oops; do not use as a shortcut.
- A missing cookie path can leave a half-running server. Check the process list and DSP/Pulse logs.
- With two writers/a disturbed control queue `paplay` can drain or LIPC calls can hang for ten seconds. In the attempt only the checked test processes were ended, the control queue `/AudiodCmd.shm` with stopped users removed with `shmq_tool -m 1` and re-created. Then exactly one `audioCtrl` and one PulseAudio were restarted. That repaired MainVolume. Do not adopt historical PIDs.
- SIGTERM could not always end PulseAudio in the fault state. SIGKILL was necessary in the attempt, but can leave the shared-memory state unusable. When unsure, start fresh rather than stacking new instances.
- Regular root oopses or a missing console response are not a successful stability test. An audible tone alone does not confirm a robust permanent solution.

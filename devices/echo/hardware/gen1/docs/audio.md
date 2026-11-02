# Audio: test, runbook and DSP chain

Dated notes: [history/09-audio-test.md](history/09-audio-test.md), [10-audio-runbook](history/10-audio-runbook.md), [15-hardware-test](history/15-hardware-test.md).

## Summary

- Direct ALSA playback does not work: ALSA card 0 `OMAP3EVM` (capture device 0, speaker playback device 1 = `hw:0,1`, Bluetooth device 2), but output goes through the original DSP and PulseAudio. `aplay -D hw:0,1 -f S16_LE -c 2 -r 48000 -d 1 /dev/zero` in the uninitialised minimal start caused a **kernel oops** (NULL pointer in `__copy_from_user`, process `aplay`, after codec unmute; `Segmentation fault`, shell stays reachable, driver state afterwards not clean). **Do not repeat this direct write path.** With `-M` (mmap) it fails without an oops (`mmap failed: Invalid argument`, `Unable to install hw params`). Missing platform/DMA/DSP initialisation is a working hypothesis, not a proven cause. This old aplay does not support `--dump-hw-params`.
- The chain rebuilt from the original Upstart jobs works and starts automatically (by the assistant) in about 9 s after a restart; memory use of the whole chain including the assistant about 48 MB of 190 MB. Script: [`src/echo-audio.sh`](../src/echo-audio.sh).
- Audible output was first confirmed on 2026-10-15 ("laut und klar", at MainVolume 100). The follow-up fault (console silent after the loud test, cause never attributed: no proven link to volume, supply, vibration/cable or kernel) is discussed in [known-issues.md](known-issues.md).

## Audio chain (production)

Rebuilt from the original Upstart jobs under `/etc/init/` (not `/etc/init.d/`): `modules-ti-dsp`, `audio-shm-main`, `audioctrl`, `audioproxyd-main`, `pulseaudio-main`.

1. DSP modules `cmemk`, `dsplinkk`, `lpm_omap3530` with the original parameters (CMEM `phys_start=0x8D800000 phys_end=0x8DA00000 allowOverlap=1 useHeapIfPoolUnavailable=1`).
2. Shared-memory queues/buffers under `/dev/shm` as in the original.
3. Minimal D-Bus system bus, `audioCtrl`, `audioproxyd`.
4. **Exactly one** PulseAudio (user mode, socket `/mnt/echo-audio/pulse-native`) with `lab126-sink` (48 kHz stereo) and `lab126-source`.

`/etc/default/audioproxyd` defines queue/buffer names. `/etc/pulse/lab126.pa` uses `module-lab126-sink` with `/QSpeakerIn.shm`, not the normal ALSA output. PulseAudio is 3.0-315-gd440 (`paplay`, `pactl`, `pacat` present).

### Microphones

- Usable: **`/dev/shm/BMicsOut.shm`**: 16 kHz, mono, s16le, processed by the DSP (beamforming, noise suppression, **echo cancellation**), 32 kB/s measured. Idle room level RMS about 5-8.
- `BVoIPMic`/`BPhoneMic` stay empty in normal mode (write counter 0), presumably active only in the DSP's call mode. The PulseAudio `lab126-source` hangs on the empty VoIP buffer and delivers nothing.
- Buffer format (determined ourselves): `0x00` u64 counter of written samples, `0x64` u32 sample size 2, `0x68` u32 capacity 2^20, ring from `0x70`. The assistant reads the ring directly via `mmap`; `shmbuf_tool -m 2` into a pipe blocks, into a file it works.
- No microphone recordings were saved or transferred as files during the tests; no private audio samples in Git. Start capture only deliberately and with the user's knowledge.

### Speaker, echo cancellation, volume

- Playback through `pacat` to PulseAudio, 24 kHz mono (PulseAudio resamples). Proof without a listener: recording the sink monitor shows the signal.
- Loopback test (play tone, record `BMicsOut`, [`src/audio_loopback.py`](../src/audio_loopback.py)): a 1 kHz tone is practically invisible in the microphone signal, so echo cancellation is active.
- Conversation test with a 20 s assistant reply at volume 40: residual echo RMS mostly 6-20, briefly up to about 120 at the start; GPT Live recognised no user speech from it. The assistant can therefore be interrupted (barge-in), unlike on the M5. Not checked at higher volumes.
- **Volume:** `lipc-set-prop com.doppler.audiod MainVolume` works after a fresh start but blocked permanently after several calls (timeout 10-20 s; a thread of `audioCtrl` hangs on a mutex; restarting `audioCtrl` alone did not help). `com.doppler.audio rawVolume` can be read, writing is ignored. Solution: the audio script sets `MainVolume` once to 70; the assistant then controls volume as gain on its own samples. Observed mapping: MainVolume 25 -> raw 89, 30 -> 99, 40 -> 115, 70 -> 145, 100 -> 175 (internal values, not dB SPL). `Volume 115 dB` in the proxy log is an internal figure, not a measured level. Do not claim full device volume just because a file has full digital amplitude. Original volume files under `/var/local/system/` are missing in the minimal start and cannot be written on the read-only root; do not "repair" persistence by remounting.
- The proxy logs `Mic Average Power` lines even with the DSP mic mute set (`lipc-set-prop -i com.doppler.audio DSP_mics_mute 1`, the DSP confirms a mute event): this is no hardware microphone kill switch.

## Runbook: reproduce the RAM-only audio test

The single steps were run in the session; the whole cleaned sequence after a fresh cold start has not been tested again as one run. A fresh start is advisable after the kernel oops. **Never put a second PulseAudio instance on the same queue.** Reproduction of the loud test is not recommended (see below). Reader/sender tooling: [boot-and-root.md](boot-and-root.md).

**1. Root and RAM.** Run the rescue boot, mount proc/sysfs if needed, feed the watchdog immediately (never `exit` PID 1):

```sh
(while :; do printf . >&3; sleep 10; done) 3>/dev/watchdog &
```

Confirm the root mount is `ro`; do not overmount or delete an existing `/mnt` mount (it was unused in the tested minimal start):

```sh
mount -t tmpfs -o size=16m tmpfs /mnt
mkdir -p /mnt/volatile/tmp /mnt/volatile/run /dev/shm
mount -t tmpfs -o size=8m tmpfs /dev/shm
mkdir -p /var/run/dbus /mnt/pulse-runtime /mnt/pulse-state
cat /proc/mounts
```

`/var/run`, `/tmp`, `/run` must point into the `/mnt` tmpfs (originally `/tmp -> /mnt/volatile/tmp`, `/run -> /mnt/volatile/run`, `/var/volatile -> ../mnt/volatile`; their targets must be re-created in RAM; `/dev/shm` is missing after the minimal start). No internal data mount.

**2. Transfer small files over UART.** Build Base64 of the exact bytes on the host, 240 Base64 characters per command (sender limit 512 characters):

```sh
: > /mnt/make_tone.py.b64
printf %s 'BASE64_CHUNK_0' >> /mnt/make_tone.py.b64; echo TRANSFER_0
printf %s 'BASE64_CHUNK_1' >> /mnt/make_tone.py.b64; echo TRANSFER_1
```

Confirm each marker as its own output line in the new log before sending the next chunk (a marker inside the echoed command line proves nothing). Then:

```sh
python -c 'import binascii,hashlib; p="/mnt/make_tone.py"; d=binascii.a2b_base64(open(p+".b64").read()); open(p,"wb").write(d); print(hashlib.sha256(d).hexdigest())'
```

Compare with the host hash before running anything. `src/make_tone.py` SHA-256: `b9f8688ca806f02952bc3bef5e34bfb684c7466f68b4a09afa065df6a7caf261` (the first version used `wave`, which is missing in Python 2.7.2, and failed at the import; the corrected version writes the PCM WAV header itself). Run:

```sh
python /mnt/make_tone.py /mnt/echo-tone.wav
```

Result: **76,844 bytes**, 48 kHz, stereo, S16_LE, 660 Hz, 0.4 s, digital peak about 1 % with soft fade in/out, SHA-256 `a42292f6783fd9a35d753f529c6daca5628bba852995697670e772556f664321` (identical on Echo and host). It only creates a file.

**3. Original modules and audio IPC.** `insmod` is not in the minimal PATH: use `/sbin/insmod`. Only with the same kernel, matching original modules and memory range; do not load modules twice (this loads into RAM, it is no installation):

```sh
/sbin/insmod /lib/modules/2.6.37/cmemk.ko phys_start=0x8D800000 phys_end=0x8DA00000 allowOverlap=1 useHeapIfPoolUnavailable=1
/sbin/insmod /lib/modules/2.6.37/dsplinkk.ko
/sbin/insmod /lib/modules/2.6.37/lpm_omap3530.ko
```

Queues and buffers (with existing queues first find which processes use them; do not re-initialise during operation; split multi-line blocks into single checked commands through the single-line sender):

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

The proxy initialisation also needs the original microphone buffers.

**4. D-Bus, audioCtrl, DSP proxy.** Transfer [`config/audio-test-dbus.conf`](../config/audio-test-dbus.conf) to `/mnt/test-dbus.conf` (minimal local system bus at `/var/run/dbus/system_bus_socket` without service autostart). Start only if no other system bus runs on the socket:

```sh
/usr/bin/dbus-daemon --config-file=/mnt/test-dbus.conf --fork --print-pid > /mnt/test-dbus.pid
```

The proxy fails with error 254 without D-Bus and then at the missing LIPC service `com.doppler.audiod`, so start `audioCtrl` **first**. Both need `PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin` and `LD_LIBRARY_PATH=/usr/local/lib:/usr/lib:/lib` (the proxy calls `rmmod`/`insmod` internally; without `/sbin` in PATH they return 127):

```sh
PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin LD_LIBRARY_PATH=/usr/local/lib:/usr/lib:/lib /usr/local/bin/audioCtrl -f /AudiodCmd.shm > /mnt/audioCtrl.log 2>&1 &
PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin LD_LIBRARY_PATH=/usr/local/lib:/usr/lib:/lib /usr/local/bin/audioproxyd -s2 -c2 -i /QSpeakerIn.shm -i /QEarconIn.shm -o /BMicsOut.shm -o /BPhoneMic.shm -o /BVoIPMic.shm -t /BTraitReport.shm -r /BPlaybackAvgPower.shm > /mnt/audioproxyd.log 2>&1 &
```

Note the PIDs, check logs and still-running processes. With the right PATH the proxy reports successful module reload, CMEM/shared-memory setup and the DSP `Ctrl Init Done` (both processes were still active at uptime 404.86 s). Then mute the mics:

```sh
/usr/local/bin/lipc-set-prop -i com.doppler.audio DSP_mics_mute 1
```

No Alexa application is started; no blanket Amazon service starts or flash writes as a shortcut.

**5. Exactly one PulseAudio test server.** Cookie in RAM only:

```sh
python -c 'import os; open("/mnt/pulse-cookie","wb").write(os.urandom(256))'
```

Transfer [`config/audio-test.pa`](../config/audio-test.pa) to `/mnt/test.pa` (module setup with `module-lab126-sink`, `module-lab126-audiod`, a Unix client socket and the explicit RAM cookie `auth-cookie=/mnt/pulse-cookie`; `auth-anonymous=1` belongs only to this temporary local diagnostic server, not to production):

```sh
PULSE_RUNTIME_PATH=/mnt/pulse-runtime PULSE_STATE_PATH=/mnt/pulse-state PULSE_COOKIE=/mnt/pulse-cookie LD_LIBRARY_PATH=/usr/local/lib:/usr/lib:/lib /usr/bin/pulseaudio -n -F /mnt/test.pa --daemonize=no --use-pid-file=no --exit-idle-time=-1 --disable-shm --log-target=file:/mnt/pulse.log > /mnt/pulse-stdout.log 2>&1 &
PULSE_COOKIE=/mnt/pulse-cookie pactl -s unix:/mnt/pulse-native info
```

Expect `lab126-sink`, s16le, 2ch, 48000 Hz; no PulseAudio mic module and no TCP protocol is loaded; the monitor source belongs to the sink. When troubleshooting check cookie path and module messages; do not put a second server next to a half-started one (a startup error does not mean no process is left: the first start failed at the cookie path on the read-only root but left a process with an active sink).

**6. Volume and playback.**

```sh
/usr/local/bin/lipc-get-prop com.doppler.audiod MainVolume
/usr/local/bin/lipc-set-prop -i com.doppler.audiod MainVolume 30
PULSE_COOKIE=/mnt/pulse-cookie paplay --server=unix:/mnt/pulse-native /mnt/echo-tone.wav
echo PLAYBACK_EXIT=$?
```

Wait for LIPC confirmation; a timeout does not verifiably change the value. For the loud test the 1 % signal was multiplied by 100 in RAM (peak about 32700 of 32767), `/mnt/echo-tone-full.wav` SHA-256 `97f832095d250fa6ace52468f3b001f11e372ebafb09652cc907fb299f8f2b18`, played with:

```sh
PULSE_COOKIE=/mnt/pulse-cookie paplay --server=unix:/mnt/pulse-native /mnt/echo-tone-full.wav
```

Sequence of the 2026-10-15 session: at MainVolume 30 three repeats (exit 0) were barely audible; the full-amplitude file ("much clearer, but still quiet"); after repairing the control queue MainVolume 100 was set (`audioCtrl` confirmed 40 -> 100, `audioproxyd` `Type 0 val 100`, rawVolume 175) and the 0.4 s file played once: **"loud and clear"**. The planned reset to 30 and the final uptime/exit code were never confirmed (afterwards only a NUL byte in the log, no answer even to Ctrl-C; COM6 still present). **Do not repeat as a standard test.** A next test should use a controlled restart and a lower volume and investigate the cause (supply, connection, kernel/audio path).

### Troubleshooting

- **Two writers / disturbed control queue:** `paplay` drains (exit 1) or LIPC calls hang for 10 s. Repair used: end only the checked test processes, remove the control queue **with stopped users** (`shmq_tool -m 1`, mode 1 = remove), re-create it with 3000 bytes (as in step 3), keep the DSP proxy running, then restart exactly one `audioCtrl` and one PulseAudio. This repaired MainVolume. Do not adopt historical PIDs.
- SIGTERM could not always end PulseAudio in the fault state; SIGKILL was needed but can leave the shared-memory state unusable. When unsure, start fresh instead of stacking instances.
- A missing cookie path can leave a half-running server: check the process list and DSP/Pulse logs.
- Regular root oopses or a missing console response are no successful stability test; an audible tone alone confirms no robust permanent solution.

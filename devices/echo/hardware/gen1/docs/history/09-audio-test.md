# First software and speaker test, 2026-10-15

**Successful:** Own Python program run in RAM and a tone output through the original speaker; the user confirms at the end "Yes, now loud and clear". No internal firmware installation. **Open follow-up fault:** After the loud test no further console response, reset to 30 not confirmed. Reproduction steps are in the [audio runbook](10-audio-runbook.md).

## New proven results

- New console reader from `hardware/gen1/tools/Read-EchoConsole.ps1` started on COM6; log under `local/runtime/boot-2026-10-15-231813-034.log`. The relocated reader/sender helpers now work live too.
- Renewed superblock check: state 0x1, incompat 0x2 without recovery, main-B unchanged; original kernel loaded and ext2 read-only root reached again.
- `python -V`: **Python 2.7.2** is present on the original system. `/usr/bin/aplay` too. `command -v amixer`, `speaker-test` and `watchdog` gave no path. `/usr/local/bin/mixer` is an Amazon shared-memory mixer (`mixer output input_shm1 ...`), no substitute for ALSA amixer.
- ALSA: card 0 `OMAP3EVM`; capture device 0, speaker playback device 1, Bluetooth device 2. Speaker target is thus `hw:0,1`.
- `/mnt` was mounted successfully as a 16 MiB tmpfs; `/proc/mounts` confirms the RAM target and still read-only internal root.

## Watchdog attempt

Started before the earlier reset limit:

```sh
(while :; do printf . >&3; sleep 10; done) 3>/dev/watchdog &
```

The background process holds the descriptor open and writes every ten seconds. After that uptimes of 84.27 and 95.40 seconds were reached; no reset within this period. This supports the watchdog hypothesis, but is not yet a complete proof of the original reset cause. No sustained function after restart: the helper lives only in RAM. Do not open/close the watchdog casually; driver NOWAYOUT and behaviour on exit remain to be clarified for a regular service.

Reference code: Linux v2.6.37 `drivers/watchdog/omap_wdt.c`, read on the host. It treats non-empty writes as keep-alive. Amazon may include adaptations; the practical runtime test is decisive.

## Playback attempts and errors

`aplay --dump-hw-params` is not supported by this old aplay version. Do not document it further as an available diagnostic parameter.

A **silence test**, not a generated test tone:

```sh
aplay -D hw:0,1 -f S16_LE -c 2 -r 48000 -d 1 /dev/zero
```

caused a kernel oops at start: NULL pointer in `__copy_from_user`, process `aplay`, after codec unmute. The program ends with `Segmentation fault`; the shell stays reachable. **Do not repeat this direct write path unchanged.** The driver state after the oops must not be regarded as clean.

The alternative with `-M` (mmap) and the same parameters fails without another oops: `mmap failed: Invalid argument`, `Unable to install hw params`. So no speaker tone was proven yet. Missing platform/DMA/DSP initialisation is a working hypothesis, not a proven cause.

## Own program transferred and run successfully

`hardware/gen1/src/make_tone.py` was Base64-encoded and transferred over the UART text console in 240-character chunks to `/mnt/make_tone.py.b64`, confirmed with unique output markers per chunk, then decoded with Python/binascii and hashed. Program SHA-256 identical on host and Echo:

`b9f8688ca806f02952bc3bef5e34bfb684c7466f68b4a09afa065df6a7caf261`

Python 2.7.2 contains `math`, `struct`, `binascii` and `hashlib`, but **neither `wave` nor `ctypes`**. The first program version with `wave` failed at the import. The corrected version writes the simple PCM WAV header itself; it was run successfully:

```sh
python /mnt/make_tone.py /mnt/echo-tone.wav
```

Result: **76,844 bytes**, 48 kHz, stereo, S16_LE, 660 Hz, 0.4 seconds, digital peak amplitude about 1 % and soft fade in/out. WAV SHA-256 likewise identical on Echo and host:

`a42292f6783fd9a35d753f529c6daca5628bba852995697670e772556f664321`

This proves successful software transfer and execution, **not yet audible output**. The program only creates a file and does not start playback.

## Further platform initialisation

Original Upstart configurations under `/etc/init/` (not `/etc/init.d/`) describe the DSP/audio chain. `/etc/default/audioproxyd` defines queue/buffer names. `/etc/pulse/lab126.pa` uses `module-lab126-sink` with `/QSpeakerIn.shm`, not the normal ALSA output directly.

`insmod` was not in the minimal shell PATH. Through **`/sbin/insmod`** `cmemk.ko`, `dsplinkk.ko`, `lpm_omap3530.ko` were loaded successfully, with the original CMEM parameters `phys_start=0x8D800000 phys_end=0x8DA00000 allowOverlap=1 useHeapIfPoolUnavailable=1`. This loads modules into RAM; it is no installation. Still no internal write access intended.

Uptime meanwhile **289.25 seconds** with watchdog keep-alive. `/tmp -> /mnt/volatile/tmp`, `/run -> /mnt/volatile/run`, `/var/volatile -> ../mnt/volatile`; their targets must be re-created in RAM. `/dev/shm` was missing after the minimal start and is prepared separately as tmpfs for the shared-memory audio chain.

Still open: initialise the original DSP/playback path in a controlled way and output the generated file audibly. No blanket Amazon service starts or flash writes as a shortcut.

### DSP/IPC path now initialised

Queues `/QSpeakerIn.shm` (95232 bytes), `/QEarconIn.shm` (16000), `/AudiodCmd.shm` (3000) as well as the original microphone/trait/power buffers were created in tmpfs with the existing `shmq_tool`/`shmbuf_tool`. The proxy initialisation also needs the original microphone buffers; no microphone recordings are saved or transferred as a file.

A first proxy start ended without D-Bus with error 254. An own minimal local system D-Bus under `/var/run/dbus/system_bus_socket` (path points into RAM) without service autostart was created. After that the proxy still failed at the missing LIPC service `com.doppler.audiod`.

`audioCtrl -f /AudiodCmd.shm` was therefore started **before** the proxy. Both processes need `PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin` and `LD_LIBRARY_PATH=/usr/local/lib:/usr/lib:/lib`. The proxy internally calls `rmmod`/`insmod`; without `/sbin` in the PATH these return 127. With the correct PATH it reports successful module reload, CMEM/shared-memory setup and the DSP `Ctrl Init Done`. Both processes were still active at uptime **404.86 seconds**.

Original volume files under `/var/local/system/` are missing in the minimal start; write attempts fail on the read-only original root. No writable internal data area was mounted for this. For the test only RAM states are intended. Proxy log message `Volume 115 dB` is an internal figure, **not a measured sound level**. The test signal stays at 1 % digital amplitude.

The regular PulseAudio path loads its own `module-lab126-sink`, which writes into the shared-memory queue. A limited RAM test server with only this sink and a local client socket is examined as the next step. No complete Alexa/Upstart start.

### PulseAudio test and first playback calls

PulseAudio **3.0-315-gd440** is present, as are `paplay`, `pactl` and `pacat`. A RAM test server with `module-lab126-sink`, `module-lab126-audiod` and a Unix client socket was started. The first start failed at the cookie path on the read-only root, but still left a process with an active sink. **An error message at start does not necessarily mean that no process is running any more.**

A freshly created 256-byte cookie in RAM and an explicit `auth-cookie=/mnt/pulse-cookie` in the native protocol module fixed the socket start. `pactl ... info` confirms `lab126-sink`, 48 kHz S16LE stereo. Two simultaneously active test servers on the same shared-memory queue, however, led to the first `paplay` drain timeout (exit 1). The stuck first test server did not respond sufficiently to SIGTERM and was ended by its verified PID. A renewed `paplay` call on the remaining server then ended without error (`Done`). An audible tone was at first **not reliably confirmed** by the user; he asked for a repeat.

Through LIPC `DSP_mics_mute=1` was set; the DSP confirmed a mute event. `MainVolume` was reduced from 40 to 30, confirmed in the proxy. This is a device-internal scale, not a sound pressure value. Further repeat attempts run; LIPC calls can wait and must not be assumed executed as long as confirmation/prompt is missing.

### Speaker tone confirmed by the user

After removing the second PulseAudio writer three repeats ran through with `TONE_1=0`, `TONE_2=0`, `TONE_3=0`. The user heard only something very quiet and demanded full volume.

The already checked WAV was then scaled in RAM by factor 100 (S16 samples stay within the value range, peak about 32700/32767). The generated file `/mnt/echo-tone-full.wav` keeps length/format and has SHA-256 **`97f832095d250fa6ace52468f3b001f11e372ebafb09652cc907fb299f8f2b18`**. Call:

```sh
PULSE_COOKIE=/mnt/pulse-cookie paplay --server=unix:/mnt/pulse-native /mnt/echo-tone-full.wav
```

`FULL_PLAYBACK_EXIT=0`. The user confirms: **"Much clearer! But still quiet"**. This proves software transfer, own tone generation and an audible output through the original hardware. The overall speaker volume is not proven to be at maximum: the last confirmed MainVolume value is 30, `com.doppler.audio rawVolume` gave 99 and `speakerSelect` 1. Get/set of `com.doppler.audiod MainVolume` later timed out with `lipcErrTimedOut` (10000 ms). This control is examined separately; do not claim "full device volume" just because the file has full digital amplitude.

### Volume control repaired and loud test confirmed

The checked test processes PulseAudio and `audioCtrl` were stopped; PulseAudio needed SIGKILL in the fault state. With stopped users, **only the RAM control queue** `/AudiodCmd.shm` was removed with `shmq_tool -m 1` and re-created with 3000 bytes. The DSP proxy stayed active. After that `audioCtrl` and exactly one PulseAudio with a valid cookie were restarted. No second sink writer stayed active in parallel.

Now `MainVolume` answered again. Value 100 was set successfully through LIPC; `audioCtrl` confirmed the change 40 -> 100, `audioproxyd` confirmed `Type 0 val 100`; `rawVolume` gave 175. The user explicitly demanded maximum volume. Then the 0.4-second file with almost full digital amplitude was played once. **The user confirms "Yes, now loud and clear".**

The reset to 30 planned after the `paplay` and the final uptime/exit code were **no longer confirmed**: after the command line there is only a NUL byte in the log, no further answer, not even to Ctrl-C. COM6 is still in the Windows device tree and the reader reports listening. The cause is open (no proven attribution to volume, supply, vibration/cable or kernel). Do not claim a successful reset and derive no permanently stable audio operation from it.

The user then confirmed having disconnected the Echo power supply and the external 3.3 V supply. The PC console reader was closed in a controlled way. The next test begins with a fresh start; do not presume the previous RAM test setup is preserved. All test programs/files on the Echo were in tmpfs; no permanent own program installed. On the PC, source, configuration templates, raw log and result are preserved. Last uptime confirmed before the loud test: **821.03 seconds** with watchdog keep-alive.

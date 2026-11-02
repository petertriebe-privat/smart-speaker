# Developing and transferring your own software

## Current environment and limits

The Echo is a small **ARMv7 Linux computer**, not an ESP microcontroller. Boot ROM/X-Loader load U-Boot, U-Boot loads a Linux kernel, which starts a root filesystem and userspace. In the tested access, `/bin/sh` is started directly as PID 1. Amazon services, network, DSP initialisation and regular init are missing in that case.

256 MiB physical RAM, of which 200 MiB for Linux in the tried start; Cortex-A8 at 1 GHz. Small native programs, a state machine and limited speech functions are sensible development goals. A modern large language model entirely local is not a realistic starting point. Wake word, speech recognition and TTS must each be tested against actual memory needs, CPU power and audio access. No engine has been chosen or measured on the device yet.

**Not yet proven:** toolchain/compiler on the Echo, exact libc/loader/float ABI, Python, a working network connection, SSH access, file transfer, own ARM binary, audio capture/output, persistent own service. These are checkpoints, not finished capabilities.

**Update from the following audio test:** Python 2.7.2, small Base64 file transfer over UART and running your own program in tmpfs are now proven. `hardware/gen1/src/make_tone.py` produced the same WAV checksum on the Echo as on the host. Playback through the original DSP/shared memory/PulseAudio was confirmed audible by the user. `wave` and `ctypes` are missing. The other points listed above, in particular cross compiler, network, capture and persistence, remain open. [Result](09-audio-test.md), [runbook](10-audio-runbook.md).

## Order for first programs

1. Use the reproducible root start and clarify the reset after a short run time. Otherwise transfers/tests are lost during work.
2. Read available programs and libraries selectively; do not dump directories with secrets wholesale.
3. Create a **RAM work area** and test a small shell script there.
4. Determine a suitable ARM toolchain and transfer a small test program, compare the file hash, run it.
5. Only then build individual hardware functions, network and the assistant service.
6. Persistence only after a backup and a worked-out recovery path.

### Capture the environment (proposed diagnosis, not completely executed)

```sh
uname -a
cat /proc/cpuinfo
cat /proc/meminfo
cat /proc/mounts
ls -l /bin/sh /bin/busybox /lib/ld* /lib/libc*
command -v gcc
command -v python
command -v python3
command -v base64
command -v sha256sum
command -v readelf
command -v arecord
command -v aplay
command -v amixer
command -v watchdog
```

`command -v blockdev` returned no path in the original shell that was reached. BusyBox could still offer individual applets; that is to be checked separately. The EchoRoot image and the original system are different userspaces: do not treat tools from one as installed in the other.

### RAM work area (proposed, not yet tested live)

Beforehand check an existing suitable mount point with `ls -ld /mnt /tmp /run` and `/proc/mounts`. `/tmp` and `/run` are symlinks in the original system; their targets and mounts are not set up automatically in the minimal start. Do not write there blindly and conclude persistence from it.

If `/mnt` exists and is unused:

```sh
mount -t tmpfs -o size=16m tmpfs /mnt
mkdir /mnt/echo-dev
printf '%s\n' '#!/bin/sh' 'echo Echo development test' > /mnt/echo-dev/hello.sh
sh /mnt/echo-dev/hello.sh
```

Write only on the checked tmpfs. 16 MiB is a first limit, not a recommendation for the final system. No `mount -o remount,rw /` just to place a test script.

## Native ARM programs

Before a build, check against an **existing original ELF**: ELF32, ARM, endianness, ARM EABI, interpreter (`PT_INTERP`), required libraries (`DT_NEEDED`), float ABI/ARM attributes and symbol versions. Use `file`/`readelf -h -l -A -d` on the host once a verified copy exists. Tool names and toolchain triplet are to be fixed only afterwards.

- ARMv7/Cortex-A8 as CPU target; do not assume ARM64, ESP32/Xtensa or ARMv8.
- A modern built dynamic ARMHF program can fail because of loader, float ABI or glibc version although it is ARM code.
- Linux 2.6.37 can fall below the requirements of new libc/runtime versions. Also check static programs for a compatible kernel ABI and the syscalls actually used.
- A static, suitably built C test can reduce the library dependency; it has not been built/tried yet.
- Develop kernel modules only with exactly matching kernel sources, configuration and symbol versions. The same version string `2.6.37` is not enough.

On the Windows PC, system Python 3.13 was present. A working ARM cross compiler was not found/set up. WSL exists as a program; the installed distribution and toolchain are unchecked. This documentation claims no working build system.

## Transfer paths

| Path | Status | Next proof needed |
| --- | --- | --- |
| UART text | Commands work | Usable for small scripts on tmpfs; verify the output |
| Base64 over UART | Proposal | Check decoder, limited line length, hash check and terminal echo |
| U-Boot `loady` / `loadb` | Commands exist | Test a binary-safe YMODEM/Kermit sender and loading to RAM |
| Load SD files in U-Boot | Tested at 1 bit / 4 MHz | Linux cannot read the SD yet; no normal Linux deploy path |
| WiFi + SCP/SSH | Unchecked | Set up driver/firmware, network and SSH in a controlled userspace |
| External initramfs in RAM | Proposal | Build suitable cpio/BusyBox/loader, test bootm ramdisk support in practice |

Text loggers are **not binary safe**. Do not back up or send `dd` binary data through `Read-EchoConsole.ps1`. For any transfer always compare length and cryptographic hash at the target before running programs.

## Audio, LEDs and buttons

The kernel detects `OMAP3EVM` ALSA and codec mappings for capture/playback, four LP5523 LED drivers and input devices. This is driver detection, not an audio test. As next reading diagnostics, `/proc/asound/cards`, `/proc/asound/pcm`, `/dev/snd/`, `/proc/bus/input/devices` and the related sysfs entries are suitable. Start capture only deliberately and with the user's knowledge; no private audio samples in Git.

The examined **EchoRoot image**, not automatically the original system, contains `S30misto` with WiFi modules, `cmemk`, `dsplinkk`, DSP/audio initialisation, `audioproxyd`, shared-memory tools, `audioCtrl`, `ledd` and `buttond`. DSP pool there `0x8D800000` to `0x8DA00000`. Do not adopt the whole start script block unchecked: it presumes directories/IPC, changes symlinks and starts several services.

Original DSP firmware, codec mixer routing, amplifier control and possible Amazon dependencies remain to be examined. Direct ALSA access may require mixer/DSP configuration. The original far-field processing and echo cancellation are not a guaranteed property of an own minimal program.

## Persistent assistant: possible architecture, not yet implemented

A later small userspace can start hardware initialisation and an own service separately. Configuration, logs and audio buffers need clearly assigned writable storage. Watchdog servicing must deliberately be part of it. First demonstrate wake word/short commands and playback; measure complex speech recognition afterwards. Where APIs or external computers are needed, tell the user openly which processing then does not happen in the Echo.

Alternatives are a tested SD root system, a controlled RAM initramfs with read access to existing driver files, or later an own internal root system. At present none of these installation variants is complete. **Do not claim that a current Debian installation can be flashed directly with full Echo audio support.**

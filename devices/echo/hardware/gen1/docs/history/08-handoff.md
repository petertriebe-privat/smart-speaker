# Chronicle of results and handoff

Status: **2026-10-15**, after completing the first root shell diagnosis. This file documents results and still open technical questions. Keep ongoing tasks/implementation decisions additionally as issues in the repository.

## Current reconnection

After completely disconnecting all connections and additionally switching to the original power supply of the other Echo Gen 1, the Echo lights up again and delivers boot messages. The root console was restored with a re-checked superblock and the original kernel; root read-only, proc/sysfs and watchdog keep-alive active. New log `local/runtime/boot-2026-10-15-234834-881.log`. Automatic SD start still broken; audio not yet set up in fresh RAM. The power supply is the obvious cause, not conclusively proven. [Current result](11-reconnect-led-diagnosis.md). Earlier state descriptions below are history.

## Later handoff after the successful audio test

The earlier chronicle below was extended: Python 2.7.2 and small UART file transfer are confirmed, own program `hardware/gen1/src/make_tone.py` was hashed and run in RAM, original modules/DSP/IPC/PulseAudio initialised. The user confirms the final speaker tone at MainVolume 100 as **"loud and clear"**. Watchdog keep-alive previously allowed at least 821 seconds of uptime. No finished assistant, no eMMC backup and no persistent installation.

**Current open state:** After the loud test no new console reaction; the planned reset to 30 not confirmed. The user then confirmed disconnecting both supplies. The PC console reader was closed in a controlled way; reload the RAM test setup at the next start. Before resuming, check physical power state/port/prompt and start fresh. Do not treat the whole audio session as stable or the current volume as 30.

**Read first:** [Detailed audio result](09-audio-test.md), [cleaned reproduction steps](10-audio-runbook.md). The test showed in particular: exactly one PulseAudio per speaker queue; re-creating a broken control queue with stopped users and restarting audioCtrl/PulseAudio repairs LIPC MainVolume. The direct uninitialised aplay path caused a kernel oops. New live logs are under `local/runtime/`.

## What changed in the project

Originally the main board was to be replaced by a powerful ESP and the remaining hardware kept. Because of effort, cost and space the user explicitly agreed to examine the original board with own software first. The current development path is therefore **Linux on the original board**, not a selected/bought ESP all-in-one design.

The user wants to run as much as possible in the Echo. The extent and quality of local speech recognition/TTS are still to be determined. An external computer is not the agreed default assumption. Do not introduce unnoticed cloud dependencies as a finished solution.

## Chronicle of proven results

1. Main board and debug field identified from photos/pinout. SD adapter, 1.8 V UART and separate 3.3 V supply procured.
2. First Echo: two pads damaged while soldering. Then switched to the second Echo Gen 1; the user reports continuity and no shorts.
3. USB-UART on COM6 functional in both directions. Original internal U-Boot not confirmed as simple interactive root access.
4. 256 GB SD rewritten with approval; EchoRoot v0.8 fully read back/compared and safely ejected.
5. Second Echo loads SD U-Boot. Its default SD access fails at high clock.
6. Source/RAM analysis locates the SD MMC structure. RAM change to 1 bit/4 MHz and re-initialisation allow complete kernel reading with matching CRC.
7. Linux starts, SD initialisation fails with -110. Internal eMMC becomes `mmcblk0`; default root p2 therefore points to internal recovery and fails.
8. Internal main-B can be read from U-Boot. Original kernel (2,599,644 bytes) loaded. First internal root attempt with ext3/noload fails.
9. Superblock check confirms clean ext3 with incompat 0x2 without recovery flag. The ext2 read-only fallback reaches the original shell at about 23:01. `id`, `uname`, proc/sysfs and mount state checked.
10. After uptime 56.07 seconds X-Loader follows again. In the further log standard starts/SD panics. Cause open, watchdog plausible. The PC reader ends at **23:05:30**. Do not claim stable or persistent root access.

## Next technical stages

### First stabilise the session

Repeat the tested root start and promptly investigate the reset/watchdog state. A robust solution must survive several minutes of uptime and repeated commands. This has priority over large transfers or installers.

### Then backup and transfer path

Check available ARM tools/loaders/libraries. Create a network or a binary-safe chunked transfer; then back up the original eMMC completely and verified. No backup exists yet.

### Understand SD under Linux

The bootloader workaround and the successful checksum are strong evidence for basic contact, but no full signal quality check. Diagnose the Linux re-initialisation error separately. Change clock/bus width/wires/supply only with a concrete finding.

### Minimal program and hardware I/O

RAM script, then a compatible ARM test program. Test audio capture and playback separately, then LED/buttons/WiFi. Do not remove original DSP/audio components as optional before their function is understood.

### Only then the permanent assistant

Measure local speech functions, select a suitable small runtime, design startup/watchdog/logs. Prepare a persistent installation plan with backup and recovery. No Debian/EchoRoot installation command approved or tested yet.

## What other agents must by no means presume

- That the Echo is still on, the same COM port applies or an earlier tool session lives on.
- That the last successful shell still exists or the watchdog is already solved.
- That `mmcblk0` is always the SD, I: is always the approved card or the old writer still fits.
- That `ext4load` proves an ext4 filesystem or ext3 `ro` prevents all writes.
- That Debian, SSH, Python, WiFi, audio, a compiler or an own service already work.
- That 256 MiB are completely free or today's ARM binaries run on the old kernel.
- That a complete backup exists or the earlier SD erase approval allows internal flash writes.

## Update after further tests

For every material change record date, hardware identity (private), bootargs, kernel/image version, observed output and actual write targets. In these documents mark experimental guides as "tested" only after successful execution. Keep local profiles/manifests current; do not overwrite or publish raw logs.

## Implementation 2026-10-16

WiFi, DHCP, SSH and SFTP in RAM confirmed; own program run over SSH and 2 MiB file round trip with hash comparison. Persistent files and own /sbin/init link installed on internal main-B; root read-only again and superblock clean. Restart/SD-free start still open. Read the current state and fallback material in the [network result](14-network-ssh.md). Full software replacement approval exists, a complete original backup is not required.

## Acceptance 2026-10-16: internal development access

Started without SD/external 3.3 V, own init active. StrictModes parent permissions corrected, full SSH root access. Three further restarts passed (36.60/36.46/36.44 seconds to SSH), host key identical. WLAN down/up automatically reachable again after 14.26 seconds. Private evidence and keys in local/, generic commands and limits in the [network result](14-network-ssh.md). Normal operation needs only the matching original power supply; no automatic audio/LED/assistant service set up.

## Separate device: Echo Dot RS03QR, 2026-10-20

The additionally connected Dot is **Gen 2 / biscuit / MT8163**, not Gen 1. With the user's explicit approval Amonet v2.0.0 and TWRP 3.7.0_9-0 were installed. Root over USB ADB, both real LK images and recovery by SHA-256 and a restart back into recovery after 34.79 s confirmed. Eleven targeted post-unlock backups checked and stored privately. The Dot stays in TWRP; WiFi/SSH and own normal boot on this Dot not yet set up. The commands/RAM addresses of the tall Echo do not apply to it. Read the current state, Windows drivers and important OTA dummy/block path traps in [20-echo-dot-gen2-usb.md](../../../dot2/docs/20-echo-dot-gen2-usb.md). The big Echo with the assistant was neither contacted nor changed in the process.

**Accepted later on 2026-10-20:** The Dot now runs in our own Alpine 3.24.1/BusyBox Linux, original recovery kernel unchanged. Root SSH with keys, automatic WPA2/DHCP and time sync, SFTP and parallel USB-CDC root console confirmed. Two further restarts reachable again over SSH after 30.74 / 31.62 s, with new boot identity and the same host key. 1 MiB SFTP round trip with hash comparison and a self-built Go 1.26.1 ARMv7 program passed. Linux image in recovery, programs/private data in `/data/echo-dot-dev`; TWRP only a verified fallback copy now. Alias `dot-dev` through `local/dot-ssh/config`, helper `hardware/dot2/tools/Invoke-Dot.ps1`. The assistant has run there since the same evening (own audio, LED and button layer; deploy through `deploy.sh`, which recognises the Dot). Full current operation, limits and reproducible inputs: [21-echo-dot-linux-ssh.md](../../../dot2/docs/21-echo-dot-linux-ssh.md).

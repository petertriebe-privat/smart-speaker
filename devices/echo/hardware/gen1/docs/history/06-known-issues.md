# Known issues and diagnosis

**Current state 2026-10-16:** Internal start without SD, WiFi/SSH/SFTP and three restarts confirmed. The earlier SD boot/reset problems below remain relevant as rescue diagnosis. New limits (old SSH server, missing time sync, DHCP profile, RAM logs, audio not yet integrated) are in the [current result](14-network-ssh.md). OpenSSH StrictModes required /usr/local to be 0755; root still has full access.

Update after the audio test: watchdog keep-alive allowed at least 821 seconds of uptime. Own software and audible sound work through the original DSP chain. After the final loud test no console response; volume last confirmed 100, reset not confirmed. The current new faults and their diagnosis are in [audio test](09-audio-test.md) and [audio runbook](10-audio-runbook.md). The sections below keep the previous result for context.

## 1. Root shell reached, then restart

**Observed:** UID 0, read-only ext2 root and `/proc/uptime` with 56.07 seconds confirmed. After that X-Loader follows again in the same raw log. Further standard starts fail at the SD root. `/proc/sys/kernel/panic` was 0.

**Hypothesis:** A hardware watchdog that is already active is not serviced with a direct `init=/bin/sh`. The kernel reports `OMAP Watchdog Timer Rev 0x31: initial timeout 60 sec`. The EchoRoot image contains `S15watchdog` with `watchdog -t 15 /dev/watchdog`. This is a hint, not proof of the reset cause and not a test on the current original userspace.

**Next diagnosis:** Measure reset intervals with host timestamps, evaluate reset status using matching OMAP/board sources, check the existing watchdog program and driver behaviour. A targeted RAM-based watchdog service might help; clarify `nowayout`, close/magic-close behaviour and program availability first. Do not accidentally arm a so far inactive watchdog by opening/closing it. Use no unsecured register-write recipes. Voltage/contact causes are not yet ruled out.

## 2. SD works in U-Boot only reduced

**Observed:** Default: SD 4-bit, high speed, actual clock about 48 MHz. Reading fails with `mmc_send_cmd: timedout waiting on cmd inhibit to clear`, `No partition table - mmc 0`, followed by `Wrong Image Format for bootm command` and restarts.

**Successful:** Before re-initialisation set 1-bit, no high speed, maximum 4 MHz in the exactly matching U-Boot RAM structure. After that the partition table and the complete kernel are readable, CRC matches the PC image. Recipe in [boot](03-boot.md).

**Further tests:** 4-bit without high speed showed the partition table at 4 MHz and at a 25 MHz limit (actually about 24 MHz). Larger file access with the latter setting failed. A large file test at 4-bit/4 MHz is missing. **Do not present any specific D1/D2 line fault as proven.**

Directly reducing the clock register after an already failed transfer did not solve the problem; re-initialisation and host caps were decisive in the successful test. Long Dupont wires, contact quality, power supply and SD compatibility remain possible causes. Do not demand re-soldering at once: pads were damaged that way on the first device.

## 3. Linux SD initialisation fails

**Observed:** `mmc0: error -110 whilst initialising SD card`. The internal eMMC then appears as Linux `mmcblk0`. The SD controller is re-initialised by Linux; a U-Boot clock setting does not automatically fix Linux.

**Consequence:** The default `root=/dev/mmcblk0p2` now selects the **internal recovery partition**, not the SD root. This ends with `Unable to mount root fs on unknown-block(179,2)`. This does not mean the verified-written SD image is wrong.

**Open:** The exact failing SD command, kernel host configuration, bus width/clock/voltage switching, errors in the new card initialisation flow. Examine debugfs/MMC information after mounting sysfs and possibly debugfs; concrete changeable attributes must be found first. No blanket claim that a sysfs clock switch exists.

## 4. ext3 with `noload` does not start

**Observed:** Original kernel + internal main-B + `ro rootfstype=ext3 rootflags=noload init=/bin/sh` end in a mount error on partition 7.

**Explanation from checked source:** Linux v2.6.37 `fs/ext3/super.c`, around line 1927, does not load the journal with NOLOAD and aborts in the case without a loaded/newly created journal. This differs from the widespread ext4 read guide.

**Tested way out:** Check a clean superblock, then `ro rootfstype=ext2` without `rootflags=noload`. Not a general way out for dirty/incompatible filesystems. `ro` alone with ext3 does not guarantee suppressed journal replay.

## 5. Supply was mis-set

The user first reported 0.6-1.4 V and later OL, corrected the regulator and confirmed a stable 3.3 V. Boot failures remained. Before new soldering work measure again at SD VDD under load; an LED voltmeter on the module alone does not prove a faultless wiring path. Short transients cannot be reliably ruled out with the available multimeter.

## 6. Historical COM/drive numbers and status files

COM6 and disk 3/I: are past values. A finished reader can leave a stale status. The old reader had a time limit and was verifiably ended at 23:05:30. First check actual port occupancy, new log and prompt. Serial session IDs from an old agent run are not reusable.

On `Access denied` at the COM port check whether a reader already holds it. On Git `dubious ownership` do not force a global exception; for necessary local read checks an option limited to that call, `git -c safe.directory=...`, can be used.

## 7. Start scripts and the installer's device mapping

EchoRoot `/etc/inittab` remounts root writable at regular start and starts `rcS`. The direct shell tests bypassed this. `S30misto` presumes complex IPC/DSP state.

The image installer `/home/root/install.sh` copies U-Boot to internal partitions and overwrites main-A. Its expected mapping SD=`mmcblk0`, internal=`mmcblk1` does **not** apply in the currently failed SD Linux. Do not start the installer as a repair or as a convenient next step.

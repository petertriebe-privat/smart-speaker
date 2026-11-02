# Known issues, diagnosis and hardware test results

Dated notes: [history/06-known-issues.md](history/06-known-issues.md), [08-handoff](history/08-handoff.md), [11-reconnect-led-diagnosis](history/11-reconnect-led-diagnosis.md), [15-hardware-test](history/15-hardware-test.md).

## Current state

The internal start without SD, WiFi/SSH/SFTP and software restarts are confirmed ([network.md](network.md)); the SD-boot and reset problems below are **rescue diagnosis**, still relevant when the SD path is used. Open: ten cold starts, a real router outage, a long combined network and audio load test, a replacement for the old bootstrap OpenSSH, a full eMMC backup, and the unexplained console silence after the loud audio test.

## Rescue-path issues (SD U-Boot, PID 1 shell)

### 1. Reset about 60 s after the root shell

Root shell reached (UID 0, read-only ext2 root), `/proc/uptime` 56.07 s, then X-Loader again in the same raw log; later standard starts fail at the SD root. `/proc/sys/kernel/panic` was 0, so a configured panic reboot is no proven explanation. Cause: very probably the hardware watchdog. The kernel reports `OMAP Watchdog Timer Rev 0x31: initial timeout 60 sec`; with `init=/bin/sh` nobody services it. The EchoRoot image contains `S15watchdog` (`watchdog -t 15 /dev/watchdog`). The keep-alive loop ([boot-and-root.md](boot-and-root.md)) removed the resets (uptime up to 821 s, later more than 1997 s). It is strong evidence, not a complete proof of the original cause, and lives only in RAM; driver `nowayout` and close/magic-close behaviour remain to be clarified for a regular service. Do not arm an inactive watchdog by opening/closing it; use no unsecured register-write recipes. Voltage and contact causes were not ruled out at first (see 5).

### 2. SD works in U-Boot only reduced

Default (SD 4-bit, high speed, actual clock about 48 MHz): reading fails with `mmc_send_cmd: timedout waiting on cmd inhibit to clear`, `No partition table - mmc 0`, then `Wrong Image Format for bootm command` and restarts. Success: before re-initialisation set 1 bit, no high speed, maximum 4 MHz in the exactly matching U-Boot RAM structure ([boot-and-root.md](boot-and-root.md)): partition table and complete kernel readable, CRC matches the PC image. Further tests: 4-bit without high speed showed the partition table at 4 MHz and at a 25 MHz limit (actually about 24 MHz); larger file access at the latter failed; a large file test at 4-bit/4 MHz is missing. Directly reducing the clock register after an already failed transfer did not help; re-initialisation and host caps were decisive. **Do not present a specific D1/D2 line fault as proven.** Long Dupont wires, contact quality, supply and SD compatibility remain possible causes. Do not demand re-soldering first: pads were damaged that way on the first device.

### 3. Linux SD initialisation fails

`mmc0: error -110 whilst initialising SD card`. Linux re-initialises the SD controller, so the U-Boot clock setting does not carry over, and the internal eMMC then appears as `mmcblk0`. The default `root=/dev/mmcblk0p2` thus selects the **internal recovery partition** and ends in `Unable to mount root fs on unknown-block(179,2)` (this does not mean the SD image is wrong). Mapping depends on the session: with the SD recognised the SD is `mmcblk0` and the eMMC `mmcblk1` (root `179:23` for partition 7, `179:22` for partition 6); on SD timeout the eMMC is `mmcblk0`. Never adopt block names from another session. Open: the exact failing SD command, kernel host configuration, bus width/clock/voltage switching. Examine debugfs/MMC information after mounting sysfs/debugfs; do not claim a sysfs clock switch exists.

### 4. ext3 with `noload` does not start

Original kernel + internal main-B + `ro rootfstype=ext3 rootflags=noload init=/bin/sh` ends in a mount error on partition 7. Linux v2.6.37 `fs/ext3/super.c` (around line 1927) does not load the journal with NOLOAD and aborts when no journal is loaded or newly created, unlike the common ext4 read guide. Way out (tested): verify a clean superblock, then `ro rootfstype=ext2` without `rootflags=noload`. Not a general way out for dirty/incompatible filesystems; ext3 `ro` alone does not guarantee suppressed journal replay.

### 5. Supply

The external 3.3 V regulator was once mis-set (0.6-1.4 V, then OL); after correction a stable 3.3 V was confirmed but boot failures remained. Before new soldering measure 3.3 V at SD VDD under load; an LED voltmeter at the module does not prove a faultless wiring path; short transients cannot be excluded with the multimeter. See also the power supply case below.

### 6. Historical COM/drive numbers and status files

COM6 and disk 3/I: are past values. A finished reader can leave a stale status (the old reader had a time limit and was verifiably ended at 23:05:30). Check actual port occupancy, new log and prompt first. Serial session IDs from an old agent run are not reusable. On `Access denied` at the COM port check whether a reader already holds it. On Git `dubious ownership` do not force a global exception; use a call-limited `git -c safe.directory=...`.

### 7. Start scripts and the installer's device mapping

EchoRoot `/etc/inittab` remounts root writable at regular start and starts `rcS`; the direct shell tests bypass this; `S30misto` presumes complex IPC/DSP state. The image installer `/home/root/install.sh` copies U-Boot to internal partitions and overwrites main-A, and its mapping SD=`mmcblk0`, eMMC=`mmcblk1` does not hold in the failed-SD-Linux case. Never start it as a repair.

### 8. Other traps

- A text logger is not binary safe ([boot-and-root.md](boot-and-root.md)).
- The kernel has no IPv6, there is no RTC (clock starts in 1970), idle SSH once timed out until WiFi power save was turned off ([network.md](network.md)).
- OpenSSH StrictModes needs `/usr/local` mode 0755.
- Do not remove original DSP/audio components as optional before their function is understood.

## Dark LED ring and silent console after reconnecting (2026-10-15)

After both supplies had been disconnected (following the loud audio test) the owner reconnected and the Echo no longer lit up by itself.

- The minimal start with `init=/bin/sh` starts neither the regular Amazon init nor the LED service `ledd` (also not during the audio test), so a dark ring alone is **no proof of a defect**. After the kernel start the four LP5523 drivers were present; that was not a functional LED test.
- But the console was also silent: after reconnection Windows still detected the adapter as COM6, the new reader opened the port, yet received **0 bytes**, even with repeated CR and after a controlled restart (both supplies off, 3.3 V first, then Echo supply; raw log stayed at 0 bytes). A USB COM port proves only the PC-adapter connection, not the Echo's supply. Isolation step: disconnect both supplies and the USB-UART, wait ten seconds, plug in only the USB-UART, re-open reader/COM port, then power the Echo (COM6 detected again, new reader opened before the supplies were switched on). A back-feed through the UART was never measured and is no established cause.
- **Resolution:** the owner additionally changed the power supply to the original supply of the other Echo Gen 1 of the same series; light and extensive boot messages returned (log of 2026-10-15 23:48). The open SD U-Boot was reachable again; automatic SD start still failed (`No partition table - mmc 0`/`Wrong Image Format`); the main-B superblock re-check was clean (state 0x1, incompat 0x2); the kernel loaded completely (2,599,644 bytes), the known `ro rootfstype=ext2 init=/bin/sh` start worked (`uid=0(root)`, Linux 2.6.37 ARMv7, root read-only, LP5523 entries in sysfs), proc/sysfs and the watchdog keep-alive were restarted.
- **Classification:** the power supply change is a strong hint of a supply problem in the previous setup; a defect of the old supply and the cause of the failure at the loud tone are not proven by measurement/load test. Claim no specific component as defective. The old supply was set aside and not used for further load tests; the audio success was not repeated with the replacement supply in that session.

## Lessons from the bring-up

- Do not presume: that the Echo is still on, the same COM port applies, an earlier tool session lives, the last shell still exists, `mmcblk0` is the SD, `ext4load` proves ext4, ext3 `ro` blocks all writes, a compiler or Debian exists, 256 MiB are free, today's ARM binaries run on the old kernel, or an erase approval for the SD allows internal flash writes.
- Project direction: originally the main board was to be replaced by an ESP and the rest of the hardware kept; because of effort, cost and space the owner agreed to examine the original board with own software first. The path is **Linux on the original board**. Do not introduce unnoticed cloud dependencies as a finished solution where local processing was agreed.
- When recording new findings note date, hardware identity (private), bootargs, kernel/image version, observed output and actual write targets; mark experimental guides "tested" only after successful execution; keep raw logs private and never overwrite them.
- The Echo Dot RS03QR (Gen 2 / biscuit / MT8163) is separate hardware (rooted with Amonet v2.0.0 and TWRP 3.7.0_9-0 on 2026-10-20, later our own Alpine 3.24.1/BusyBox Linux with SSH and the assistant); its documents are [../../dot2/docs/20-echo-dot-gen2-usb.md](../../dot2/docs/20-echo-dot-gen2-usb.md) and [21-echo-dot-linux-ssh.md](../../dot2/docs/21-echo-dot-linux-ssh.md). The commands and RAM addresses of the tall Echo do not apply to it.

## Hardware test results (2026-10-16, over SSH, internal boot)

Detected components: [hardware.md](hardware.md). Audio, microphones and echo cancellation: [audio.md](audio.md).

### LED ring

- Self-test of all four LP5523: `OK`, temperature 31-32 C.
- **The sysfs files of the Amazon driver (`all_leds`, `lp5523:*/brightness`) have no effect:** they change only the driver cache; the PWM registers in the chip stayed 0. After boot the chips' animation engines also run (bootloader animation, rotating blue ring).
- The assistant therefore writes **directly through `/dev/i2c-2`** (`I2C_SLAVE_FORCE`): at start `OP_MODE` (0x01) = 0 (engines off) and `ENABLE` (0x00) = 0x40, then the PWM registers 0x16-0x1E. **Single-register writes only**; multi-byte writes with auto-increment lost individual bytes. Helper for reading/setting: [`src/lp5523_regs.py`](../src/lp5523_regs.py).
- Channel assignment per chip, LED k = 0...2: **green = channel 2k, blue = 2k+1, red = 6+k** (D1/D2 = G/B of the first LED, D7 = its red).
- Positions (top view, mute button on the left at 9 o'clock), confirmed with single channels, photos and a chaser:

| Chip | LED 0 | LED 1 | LED 2 |
| --- | --- | --- | --- |
| 0x32 | 3 o'clock | 2 o'clock | 1 o'clock |
| 0x33 | 9 o'clock | 8 o'clock | 7 o'clock |
| 0x34 | 5 o'clock | 6 o'clock | 4 o'clock (wired out of order) |
| 0x35 | 12 o'clock | 11 o'clock | 10 o'clock |

- The red LED in the mute button is connected neither to the LP5523 GPO outputs nor to `power-led`/`network-led`; the hardware switches it (see mute).
- `led_offset` in the settings rotates the picture; test: `assistant ctl ledtest` (a red light runs clockwise from 12 o'clock).

### Buttons and volume ring

Recorded with [`src/input_capture.py`](../src/input_capture.py):

| Control | Event |
| --- | --- |
| Action button (dot) | `EV_KEY` 138 (`KEY_HELP`), **0 = pressed, 1 = released** |
| Mute | `EV_KEY` 113 (`KEY_MUTE`): **not a button but the state of a hardware latch** (GPIO `VOLUME_MUTE`). Each press toggles it and the red button LED; 0 = muted, 1 = microphones on. The current state comes from `EVIOCGKEY`. |
| Ring | `EV_REL` `REL_X`, +-1 per detent; **clockwise -1** |

Software cannot switch the red button LED. The assistant takes over the latch state (also at start) and never cancels mute itself; the action button gives only a low tone when muted. `gpio-keys` also reports a third code (355), whose button is unknown.

GPIO names per debugfs (read only!): 21 `WIFI_reset`, 22 `Bluetooth_reset`, 129 `PA_Enable`, 70 `VOLUME_MUTE`, 72 `UBER`, 77 `Reset`, 109 `ti_led`. Do not write blindly: GPIO 21 resets the WiFi.

### Network and clock

IPv6 absent, no RTC, mesh roaming route loss: see [network.md](network.md).

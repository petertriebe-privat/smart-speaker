# Console, boot, root access, storage and installation

Dated notes: [history/02-console.md](history/02-console.md), [03-boot](history/03-boot.md), [04-development](history/04-development.md), [07-artifacts-sources](history/07-artifacts-sources.md), [12-storage-and-installation](history/12-storage-and-installation.md).

## Boot paths at a glance

| Path | Status |
| --- | --- |
| **Normal:** internal original bootloader -> internal original kernel -> main-A/main-B -> our own `/sbin/init` -> watchdog/WiFi/DHCP/SSH | Tested; this is how the device runs. Alexa services do not start on this path. Kernel/DSP support is kept for the audio chain |
| **Rescue:** SD card with the open EchoRoot U-Boot, loading the internal kernel and starting `/bin/sh` as PID 1 | Tested on the second and a further unit; manual path, described below |
| Linux from the SD card | Does not work (Linux fails to initialise the SD host, see [known-issues.md](known-issues.md)) |
| Debian / full EchoRoot installation | Not done. Do not claim a current Debian can be flashed directly with full Echo audio support |

| Area | Function | Survives power-off? |
| --- | --- | --- |
| Internal eMMC root slot (main-A or main-B) | Original kernel/hardware base, own init under `/usr/local/echo-dev`, persistent WiFi/SSH files | Yes |
| Internal boot/device configuration | Existing internal bootloader and device identity, backed up in a targeted way ([recovery.md](recovery.md)) | Yes |
| RAM/tmpfs under `/mnt` | Volatile paths, temporary own programs, DHCP/WiFi/SSH logs | No |
| External microSD | Kept rescue medium with open U-Boot, removed for normal operation | Yes, but not needed |

Persistent programs and secrets live under `/usr/local/echo-dev` on the root slot. Root is mounted ext3 read-only in normal operation; the SSH user is unrestricted root and uses `remount,rw`, `sync`, `remount,ro` for targeted changes. About 636 MiB were free before the small installation. No further data partitions were formatted or repurposed. Bootloader, GPT, kernel and IDME were not changed. The historical EchoRoot installer is not a general repair command (fixed mmcblk mapping, see [recovery.md](recovery.md)).

## Serial console on the Windows PC

Windows PowerShell 5.1 with `.NET System.IO.Ports.SerialPort`; no Python serial package required. The FTDI driver worked with version 2.12.36.20. The port in the notes was COM6; determine it again first:

```powershell
Get-ItemProperty HKLM:\HARDWARE\DEVICEMAP\SERIALCOMM
[IO.Ports.SerialPort]::GetPortNames()
```

Bluetooth COM ports are not an Echo connection. If unclear, unplug the USB-UART, compare the port list, plug it in again. Only one program may hold the port. Do not take over a port or an old session ID from another project or agent run.

**Start the reader** (from the folder that contains `tools/`, e.g. `devices/echo/hardware/gen1`):

```powershell
powershell.exe -NoProfile -ExecutionPolicy RemoteSigned -File .\tools\Read-EchoConsole.ps1 -PortName COM6 -Seconds 3600
```

The reader stays active until the time limit or stop; wait for `CONSOLE READY`. For agents a continuing terminal process is suitable. Logs, status and control file go under a runtime directory (`local/runtime/`, override with `-RuntimeDirectory`; use a separate directory per device).

```powershell
Get-Content .\local\runtime\echo-console-status.json
$state = Get-Content .\local\runtime\echo-console-status.json -Raw | ConvertFrom-Json
Get-Content -LiteralPath $state.log -Tail 40
```

`phase=listening` is only a file status and can be stale after an abruptly killed process; check process and log progress. At start the reader ignores the control ID that already exists, so old commands are not repeated. The reader is not a shell-prompt detector.

**Send commands** (second window):

```powershell
.\tools\Send-EchoCommand.ps1 -Command 'version'
```

`version` is the example for `uboot >`. At `sh-3.2#` use for example `-Command 'id; uname -srmo'`. The sender writes a control file; the reader sends the text with a trailing CR. It reports **Queued**, not execution success: always check the new log section. Single-line commands up to 512 characters, no embedded line breaks. In PowerShell use single quotes on the outside so U-Boot's `${filesize}` etc. are not expanded on the PC.

To interrupt the bootloader countdown repeat Enter while the Echo starts (only repeats CR, does not start a boot; some states restart repeatedly, then the interval can hit a following countdown; RAM variables are lost on reset):

```powershell
.\tools\Send-EchoCommand.ps1 -InterruptBoot -Seconds 15
```

Close the reader without switching the Echo off (it does **not** disable any Echo watchdog): `.\tools\Send-EchoCommand.ps1 -StopReader`.

| Output | Meaning |
| --- | --- |
| No data | Check port/cable/GND/supply/boot; first clarify log and adapter detection |
| X-Loader, then U-Boot | UART reception works; no Linux shell yet |
| `uboot >` | Open SD bootloader accepts commands (the original locked U-Boot does not give this interactive access) |
| `Starting kernel ...` | Kernel starts; no proof yet of a usable root filesystem |
| `sh-3.2#`, `id` gives UID 0 | Confirmed root shell |
| `Kernel panic` | Kernel cannot continue; look for the cause in the previous log |

With `init=/bin/sh` never send `exit`/Ctrl-D (PID 1). Do not send binary transfers through the text logger (ASCII decoding and CR handling): YMODEM/Kermit would need an own exclusive binary-safe port user, not implemented. The reader/sender scripts were syntax-checked and used live on 2026-10-15 (bootloader interrupt, root commands, file transfer, audio setup). `tools/echo_console.py` additionally offers gzip+base64 upload (see below and [tools/README.md](../tools/README.md)).

## Rescue boot into a root shell (SD U-Boot)

Tested on the second Echo Gen 1; the start is temporary. The SD only gives access to the open bootloader; kernel and root filesystem come from the internal flash. Neither Debian nor EchoRoot is installed by it. Use it for the first access, for rescue and for installing the persistent access.

**1. Cold start and stop U-Boot.** Wiring per [hardware.md](hardware.md). 3.3 V first, then the Echo supply; repeat Enter during the countdown. Expected:

```text
U-Boot 2010.06-00011-ge1ab9145d5 (Nov 27 2021 - 21:37:39)
uboot >
```

Verify with `version`. The original internal U-Boot carries the date 2022-01-19. With another version do not adopt any documented RAM address. On the further unit X-Loader and U-Boot came from the SD without the SD clock workaround.

**2. Identify the internal partition and check its state.** In U-Boot:

```text
mmc dev 1
mmcinfo
mmc part
ext4ls mmc 1:7 /
ext4ls mmc 1:7 /boot
ext4ls mmc 1:7 /bin
```

Expect eMMC roughly 3.64 GiB and GPT partition 7 named **main-B**. `mmc 1:7` = U-Boot device 1, partition 7. `ext4ls` proves no ext4 (it also reads ext2/ext3).

**The active slot differs per device.** On the second assistant Echo (2026-10-18) **main-A** (partition 6, start LBA `0x58800`, superblock LBA `0x58802`) was the clean system with `/boot/uImage`; main-B had no `/boot` and the RECOVER flag (incompat `0x6`) and was left untouched. So run `ext4ls` on `1:6` and `1:7`, read both superblocks, and take the slot with the kernel and a clean superblock, with its PARTUUID. The GPT UUIDs were identical on the devices (factory), the eMMC was not (SanDisk 7,634,944 vs. Samsung 7,733,248 sectors). The device PARTUUID is in `local/device-profile.json`; compare with `mmc part` and never take the UUID of another Echo. Internal `/boot/uImage`: **2,599,644 bytes**; `/bin/sh` is present.

Read the superblock before the ext2 fallback. **Only with a confirmed main-B start sector `0x258800`** these commands apply (for another start LBA use `start LBA + 2`):

```text
mmc read 83000000 258802 2
md.b 83000000 80
```

This reads two 512-byte sectors from partition offset 1024 into **RAM**; nothing is written to eMMC. **First confirm `2 blocks read: OK`**, otherwise old RAM content could be shown. U-Boot addresses/sector values are hexadecimal.

| Offset in superblock | Read on the device | Meaning |
| --- | --- | --- |
| `0x38`, 16 bit little-endian | `53 ef` | ext magic `0xef53` |
| `0x3a`, 16 bit | `01 00` | Clean/valid state |
| `0x5c`, 32 bit | `3c 00 00 00` | Compat flags `0x3c`, journal among others |
| `0x60`, 32 bit | `02 00 00 00` | Incompat `0x2` = FILETYPE; no RECOVER (`0x4`) |
| `0x64`, 32 bit | `03 00 00 00` | RO-compat `0x3` |

These values allowed the ext2 read start. With RECOVER set, an unclean state or other unknown features **do not continue blindly**: diagnose and back up first, never run `fsck`/journal repair on suspicion. Re-check also after a normal Amazon start.

**3. Load the kernel and set the boot parameters.**

```text
ext4load mmc 1:7 82000000 /boot/uImage
```

Continue only after the load is confirmed complete. Replace `<MAIN_B_PARTUUID>` (use the slot you found, without the angle brackets):

```text
setenv bootargs console=ttyO0,115200n8 root=PARTUUID=<MAIN_B_PARTUUID> ro rootfstype=ext2 rootwait mem=200M@0x80000000 mpurate=1000 earlyprintk init=/bin/sh
printenv bootargs
bootm 82000000
```

Keep `mem=200M@0x80000000` and the other platform parameters. **No `saveenv`**, and do not run `run mmcargs` afterwards (its default overwrites the parameters with `rootfstype=ext3`). Why ext2: `rootfstype=ext3 ro rootflags=noload` fails (in Linux 2.6.37 ext3, suppressing the journal leads into the error path, see [known-issues.md](known-issues.md)); simply dropping `noload` is no equivalent write protection because ext3 can replay the journal even with `ro`. The checked clean state allows ext2 with `ro`.

**4. Verify.** Expected output:

```text
EXT2-fs (mmcblk0p7): warning: mounting ext3 filesystem as ext2
VFS: Mounted root (ext2 filesystem) readonly on device 179:7.
sh-3.2#
```

```sh
id
uname -srmo
mount -t proc proc /proc
mount -t sysfs sysfs /sys
cat /proc/mounts
cat /proc/partitions
cat /proc/uptime
```

Do not mount again if already mounted. Proven: `uid=0(root) gid=0(root)`, `Linux 2.6.37 armv7l GNU/Linux`, `/dev/root / ext2 ro,...`. `rootfs / rootfs rw` is the initial RAM root, no proof of writable flash. `/sys/block/mmcblk0/ro` is **0**: the block device itself is not write-protected, only the mount is read-only; raw write commands stay dangerous.

**5. Feed the watchdog at once.** A shell started as PID 1 does not service the hardware watchdog; the unit resets after about 60 s (56.07 s seen). Right after reaching the shell:

```sh
(while :; do printf . >&3; sleep 10; done) 3>/dev/watchdog &
```

This kept sessions alive for at least 821 s (uptimes of 84.27, 95.40, 289.25, 404.86, 821.03 s were read; later more than 1997 s). Note the background PID per start. The helper lives only in RAM; do not open/close the watchdog casually. Reference: Linux v2.6.37 `drivers/watchdog/omap_wdt.c` treats non-empty writes as keep-alive. Details: [known-issues.md](known-issues.md).

### Optional: SD read mode in U-Boot

Not needed for the start above (it reads internal files); it documents the successful SD access in the open U-Boot. **Only for exactly the same U-Boot build and memory layout.** Check `version` and `md.l 80edb4b0 8` first; abort on differing values and relocate from source/binary. The tested SD structure is `0x80edb478`; eMMC follows at `0x80edb5e8` and must not be confused.

| Field | Address | Original value | Test value |
| --- | --- | --- | --- |
| SD `f_max`, offset `0x3c` | `0x80edb4b4` | `0x03197500` = 52 MHz | `0x003d0900` = 4 MHz |
| SD `host_caps`, offset `0x50` | `0x80edb4c8` | `0x00000911` | `0x00000800` = without HS/4-bit |

The eight words from `0x80edb4b0` are `f_min`, `f_max`, `high_capacity`, `bus_width`, `clock`, `card_caps`, `host_caps`, `ocr` (normally: 400 kHz minimum, 4-bit bus, 50 MHz requested). The structure and field meaning must match.

```text
mw.l 80edb4b4 003d0900
mw.l 80edb4c8 00000800
mmc dev 0
mmc rescan
mmcinfo
mmc part
ext4load mmc 0:2 82000000 /boot/uImage
crc32 82000000 ${filesize}
```

These are reversible changes to U-Boot structures in **RAM**. Afterwards all three SD partitions were detected and the kernel loaded with **2,599,640 bytes**, CRC32 **792a42c1**. The SD kernel is four bytes smaller than the internal one; do not confuse them. The mode is **not inherited by Linux**.

## Persistent install (own init, no SD)

Installed on the root slot under `/usr/local/echo-dev` (see [`src/`](../src), [`config/`](../config), [`tools/`](../tools)):

- `src/echo-init.sh` as `/sbin/init`: starts the watchdog keep-alive, RAM paths, serial rescue shell, then the service loop.
- `src/echo-service-loop.sh`: WiFi modules, `wpa_supplicant`, `udhcpc` (`config/udhcpc-echo.script`) and `sshd` (`config/sshd-persistent.conf`) with simple restart loops; network errors must not block maintenance start or watchdog. Audio, the assistant, Tailscale and Bluetooth are skipped where not installed.
- `src/echo-reboot.sh` as `/usr/local/echo-dev/bin/echo-reboot`: syncs and remounts root read-only before the forced kernel restart. Ordinary Upstart/init signalling does not fit the small PID 1 supervisor.

Procedure, tested on two Echos:

1. Boot to the root shell as above; feed the watchdog.
2. Upload the files over UART with `tools/echo_console.py --upload` (gzip + base64 in chunks, unique ACK markers, SHA-256 checked on the target, no terminal echo, secrets never printed; `ECHO_RUNTIME` selects another reader directory for a second device). The target Python has no zlib, so decompression uses BusyBox gzip. Use it only on the verified shell, not in U-Boot. Windows CRLF in scripts caused a syntax error: normalise to Unix LF and check hashes and `sh -n` on the Echo before the flash write. On the further unit 10 files including sshd took 107 s.
3. Try WiFi, DHCP and SSH in RAM first with `tools/bootstrap-ram-access.sh` (RAM `/var`, RAM passwd with the sshd user, the real service loop with its base directory in tmpfs; nothing written to eMMC). Log in over SSH.
4. Take the targeted backup over SSH ([recovery.md](recovery.md)).
5. Run `tools/install-persistent-access.sh` on the Echo **over UART** (an open SSH session keeps the RAM `/var` busy). Arguments: expected root major:minor (e.g. `179:22`), eMMC block name (e.g. `mmcblk1`), eMMC size in sectors, boot mode (`main-A`/`main-B`), SHA-256 of the original `/sbin/init`. It checks current root dev_t, eMMC type/size, bootmode and the original init hash, briefly remounts only the root filesystem on the active slot ext2-rw, places the init/service scripts, sshd, private WiFi/key files and small fallback copies under `/usr/local/echo-dev`, adds the sshd privilege-separation user to passwd/group and creates `/var/empty`, switches `/sbin/init` atomically to `../usr/local/echo-dev/init.sh`, runs `sync` and goes back to read-only. No raw writes, no bootloader/kernel/GPT/IDME changes. Post-check: link correct, sshd configuration valid, sshd hash matches, root superblock state=1 and incompat=2 (no RECOVER). It is not a repeatable update command after `/sbin/init` has been switched. The original init had the same hash on both units (same firmware state). `/usr/local` is set to 0755 (OpenSSH StrictModes, see [network.md](network.md)).
6. Switch both supplies off, remove the SD and the 3.3 V wire, power on with the Echo supply only. A first restart with the SD still inserted starts the SD default kernel (`root=/dev/mmcblk0p2`) and ends in the known root-mount panic; this is not a fault of the new init, which is not called then.

First internal start without SD (first unit): UART showed the original internal U-Boot 2010.06 of 2022-01-19 and then `echo-dev init active`; no bootloader commands needed. SSH came up (uid 0, PID 1 `/bin/sh /sbin/init --default-console none`, root ext3 read-only, WPA2 COMPLETED, uptime 69.64 s, block list with only the internal eMMC as `mmcblk0` and its eight partitions). Results of the restart tests: [network.md](network.md).

For network problems use the UART as rescue access (the own init provides a physical root shell there). Keep the old SD with the open U-Boot as additional rescue medium; do not leave it inserted in normal use.

## Development environment on the device

The Echo is a small ARMv7 Linux computer (256 MiB RAM, 200 MiB for Linux, Cortex-A8 at 1 GHz), not a microcontroller. Boot ROM/X-Loader load U-Boot, which loads the kernel. Small native programs and limited speech functions are sensible targets; a modern large language model fully local is not realistic. Wake word, speech recognition and TTS must each be tested against actual memory, CPU and audio access. Claude Code and Codex run on the PC and use the SSH path ([network.md](network.md)).

**Proven:** Python 2.7.2 (has `math`, `struct`, `binascii`, `hashlib`; **no** `wave`, `ctypes`, `zlib`), `/usr/bin/aplay`, small Base64 file transfer over UART, running own programs in tmpfs, SSH/SFTP file transfer, a self-built static Go ARMv7 program on the later units (the assistant runs this way). `command -v amixer`, `speaker-test`, `watchdog`, `blockdev` gave no path in the original shell (BusyBox may still offer applets; check separately). `/usr/local/bin/mixer` is an Amazon shared-memory mixer, no substitute for ALSA amixer. EchoRoot and the original system are different userspaces: do not treat tools from one as installed in the other.

**RAM work area:** check an unused mount point first (`ls -ld /mnt /tmp /run`, `/proc/mounts`; `/tmp` and `/run` are symlinks into `/mnt/volatile` in the original system and are not set up in the minimal start):

```sh
mount -t tmpfs -o size=16m tmpfs /mnt
mkdir /mnt/echo-dev
printf '%s\n' '#!/bin/sh' 'echo Echo development test' > /mnt/echo-dev/hello.sh
sh /mnt/echo-dev/hello.sh
```

Write only on the checked tmpfs. 16 MiB is a first limit (the later install uses 32 MiB). No `mount -o remount,rw /` just to place a test script.

**Native ARM programs.** Before a build, check an existing original ELF on the host (`file`, `readelf -h -l -A -d`): ELF32, ARM, endianness, EABI, interpreter (`PT_INTERP`), `DT_NEEDED`, float ABI/ARM attributes, symbol versions. Target ARMv7/Cortex-A8 (not ARM64, ESP32/Xtensa, ARMv8). A modern dynamic ARMHF program can fail on loader, float ABI or glibc version; Linux 2.6.37 can fall below the requirements of new runtimes, so check static programs for kernel ABI and syscalls too. Kernel modules need exactly matching kernel sources, configuration and symbol versions (the version string `2.6.37` is not enough).

Transfer paths: UART text (works for small scripts), Base64 over UART (works, hash-checked), WiFi + SCP/SSH (works, [network.md](network.md)), U-Boot `loady`/`loadb` (commands exist; needs a binary-safe YMODEM/Kermit sender, untested), SD files in U-Boot (works at 1 bit/4 MHz, but no normal Linux deploy path), external initramfs in RAM (untested proposal). Text loggers are **not binary safe**: never back up or send `dd` data through `Read-EchoConsole.ps1`; always compare length and cryptographic hash at the target before running programs.

**EchoRoot image internals** (not automatically true for the original system): `S30misto` with WiFi modules, `cmemk`, `dsplinkk`, DSP/audio initialisation, `audioproxyd`, shared-memory tools, `audioCtrl`, `ledd`, `buttond`; DSP pool `0x8D800000` to `0x8DA00000`; `/etc/inittab` remounts root writable and starts `rcS`; `S15watchdog` runs `watchdog -t 15 /dev/watchdog`. Do not adopt the start script block unchecked (it presumes directories/IPC, changes symlinks, starts several services). Original DSP firmware, codec mixer routing and amplifier control are covered in [audio.md](audio.md). The original far-field processing and echo cancellation are not a guaranteed property of an own minimal program. A Windows PC with system Python 3.13 was used; no working ARM cross compiler was set up on it.

## Artifacts and sources

Release **EchoRoot v0.8** of the historical EchoRoot project (not a current Debian, no installed own assistant). The image contains the Amazon filesystem (state October 2020) with Alexa's wake word engine; how to read it with `debugfs`: [../../../docs/22-alexa-wakeword-original.md](../../../docs/22-alexa-wakeword-original.md).

| Artifact | Size | SHA-256 / check value |
| --- | --- | --- |
| `echoroot-v0.8-sdcard.img.xz` | 313,232,980 bytes | `B41B9C4F911B1C2FC5C0731D7310AAE4FAA675C25DF4680F3D279B3965CFFE0C` |
| `echoroot-v0.8-sdcard.img` | 2,113,929,728 bytes | `F378D8A700B83B8028962D332A01A61250C4965926B32BB49B443A5A5A837657` |
| `echoroot-v0.8-uImage` | 2,599,640 bytes | CRC32 `792a42c1`; extracted from SD partition 2 |
| `echoroot-v0.8-kernel-uncompressed.bin` | 5,282,080 bytes | Analysis artifact after unpacking the zImage; **not a directly flashable uImage** |

The entire 2,113,929,728 written bytes were read back from the SD and compared; the hash checks consistency with the download, it is no independent signature of the publisher. MBR of the image (512-byte sectors):

| SD partition | Start LBA | Sectors | Type |
| --- | --- | --- | --- |
| 1 | 1 | 32,768 | `0x0c`, FAT |
| 2 | 32,769 | 2,048,000 | `0x83`, Linux root |
| 3 | 2,080,769 | 2,048,000 | `0x83`, installation source |

The rest of the (256 GB) card stays unallocated. A drive letter from Windows is not a permanent Linux partition identifier.

SD U-Boot: `2010.06-00011-ge1ab9145d5`, build 2021-11-27 21:37:39, 172,952 bytes, source commit **`e1ab9145d5b7f320644a8235980c22603e7a9c7a`**, link/text base **`0x80e80000`**. Fixed RAM structure addresses apply only to this build. SD kernel uImage: Linux 2.6.37, load/entry **`0x80008000`**, legacy uImage with an ARM zImage payload (uImage compression entry "uncompressed"; the zImage compresses internally); distinct from the **internal** kernel (2,599,644 bytes).

Local archive (private, git-ignored `local/archive/`): boot logs of 2026-10-13/15, U-Boot RAM dumps and source excerpts, Linux ext2/ext3 `super.c` reference, SD write status/log, `archive-manifest.json` (size and SHA-256 of 65 files, verified after copying; 29 hardware photos). Complete logs can contain device identifiers: sanitise before pasting into public issues. The old image parser (`inspect-echo-image.py`) supports old block pointers and simple symlink contents only and is superseded by `tools/read_echo_image.py` (see [network.md](network.md)). The local archive is not published as a freely redistributable firmware package; check licences when adopting source code, Amazon binaries, DSP firmware and Buildroot components.

Sources and references:

- [Echo Hacking wiki: Echo](https://github.com/echohacking/wiki/wiki/Echo) - device target, hardware access, historical root procedures.
- [Vanderpot pinout](https://vanderpot.com/wp-content/uploads/2016/06/pinout.png) - debug pads.
- [EchoRoot project](https://gitlab.com/AndreRH/echoroot) by André Zwing, [setup](https://andrerh.gitlab.io/echoroot/SETUP.html), [v0.8 release](https://gitlab.com/AndreRH/echoroot/-/releases/echoroot-v0.8) - historical software base and image. Check availability and immutable download IDs before downloading again.
- [Matching U-Boot commit](https://gitlab.com/AndreRH/u-boot-echo/-/tree/e1ab9145d5b7f320644a8235980c22603e7a9c7a) - MMC structures, host caps, commands, link address.
- [Buildroot fork](https://github.com/AndreRH/buildroot/tree/linux-2.6.37) - EchoRoot submodule branch (mutable; the exact submodule commit for a reproducible build is still to be determined).
- [Linux v2.6.37 ext3 super.c](https://github.com/torvalds/linux/blob/v2.6.37/fs/ext3/super.c), [ext2 super.c](https://github.com/torvalds/linux/blob/v2.6.37/fs/ext2/super.c) - journal and feature checks (not complete Amazon kernel source).
- [Amazon Echo teardown (iFixit)](https://de.ifixit.com/Teardown/Amazon+Echo+Teardown/33953) - photos of the tall Echo; the Echo **Dot** teardown concerns different hardware.
- [Reversec: Echo rooting](https://labs.reversec.com/advisories/2017/08/amazon-echo-rooting) (MWR Labs, 2017), [CARDIS paper](https://pure-oai.bham.ac.uk/ws/files/56108659/Breaking_all_the_things_CARDIS_.pdf) - historical security/hardware research; partly other production states.

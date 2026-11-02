# Tested start into a root shell

**Since 2026-10-16 this is the manual rescue path.** The normal internal start without SD/external 3.3 V with our own init and automatic WiFi/SSH is tested. Read the [current access state](14-network-ssh.md) first; there is no need to use the SD bootloader for normal development.

This guide was tried on the **second Echo Gen 1**. The start is temporary and was still affected by an unexplained restart. The SD serves here as access to the open bootloader; kernel and root filesystem come from the internal flash. Neither Debian nor EchoRoot is installed by it.

**Later finding:** The watchdog keep-alive from the [audio runbook](10-audio-runbook.md) kept the renewed shell alive for at least 821 seconds. Audio was output successfully; after the loud test, however, the console stayed unresponsive. Take this current state into account for further starts.

## 1. Cold start and stop U-Boot

Wiring per [hardware](01-hardware.md), console per [console guide](02-console.md). Switch 3.3 V on first, then the Echo supply. Repeat Enter during the countdown. Expected identity:

```text
U-Boot 2010.06-00011-ge1ab9145d5 (Nov 27 2021 - 21:37:39)
uboot >
```

Verify with `version`. The original internal U-Boot from an earlier stock log, in contrast, carries the date 2022-01-19. With a different version do not adopt any of the documented RAM addresses.

## 2. Identify the internal partition and check its state

In U-Boot:

```text
mmc dev 1
mmcinfo
mmc part
ext4ls mmc 1:7 /
ext4ls mmc 1:7 /boot
ext4ls mmc 1:7 /bin
```

Expectation: eMMC roughly 3.64 GiB, GPT partition 7 named **main-B**. `mmc 1:7` here means U-Boot device 1, partition 7. The name `ext4ls` proves no ext4 filesystem; the command also reads ext2/ext3.

**The active slot differs per device:** On the second assistant Echo (2026-10-18) **main-A** (partition 6, start LBA `0x58800`, superblock LBA `0x58802`) was the clean system with `/boot/uImage`. main-B there had no `/boot` and the RECOVER flag (incompat `0x6`) and was left untouched. So first run `ext4ls` on `1:6` and `1:7` and read both superblocks, then take the slot with the kernel and a clean superblock, with its PARTUUID. The GPT UUIDs were identical on both devices (factory), the eMMC was not (SanDisk 7,634,944 vs. Samsung 7,733,248 sectors).

The current device PARTUUID is in `local/device-profile.json`; compare it with `mmc part` before use. Do not take the UUID of another Echo. Internal `/boot/uImage`: **2,599,644 bytes**. `/bin/sh` is present.

Before the ext2 fallback, read the superblock. **Only with a confirmed main-B start sector `0x258800`** do these commands apply:

```text
mmc read 83000000 258802 2
md.b 83000000 80
```

This reads two 512-byte sectors from partition offset 1024 into **RAM**. It does not write to eMMC. **First confirm `2 blocks read: OK`** before evaluating the following memory output as a superblock; after a read error old RAM content could be there. U-Boot addresses/sector values are hexadecimal here. If the start LBA is different, recompute the superblock LBA: `start LBA + 2`.

| Offset in superblock | Read on the device | Meaning |
| --- | --- | --- |
| `0x38`, 16 bit little-endian | `53 ef` | ext magic `0xef53` |
| `0x3a`, 16 bit | `01 00` | Clean/valid state |
| `0x5c`, 32 bit | `3c 00 00 00` | Compat flags `0x3c`, journal among others |
| `0x60`, 32 bit | `02 00 00 00` | Incompat `0x2` = FILETYPE; no RECOVER (`0x4`) |
| `0x64`, 32 bit | `03 00 00 00` | RO-compat `0x3` |

These values allowed the ext2 read start here. With RECOVER set, an unclean state or other unknown features **do not continue blindly**. Diagnose and back up first; do not run `fsck`/journal repair on suspicion. Re-check also after a normal Amazon start.

## 3. Load the original kernel and set the boot parameters

```text
ext4load mmc 1:7 82000000 /boot/uImage
```

Continue only after the load is confirmed complete. Then use the following line, replacing `<MAIN_B_PARTUUID>` with the UUID just checked; do not send the angle brackets:

```text
setenv bootargs console=ttyO0,115200n8 root=PARTUUID=<MAIN_B_PARTUUID> ro rootfstype=ext2 rootwait mem=200M@0x80000000 mpurate=1000 earlyprintk init=/bin/sh
printenv bootargs
bootm 82000000
```

Keep `mem=200M@0x80000000` and the other proven platform parameters. **No `saveenv`**. Do not run `run mmcargs` afterwards: the default definition overwrites the parameters again with `rootfstype=ext3`.

Why ext2? `rootfstype=ext3 ro rootflags=noload` was tried and failed. In the checked Linux 2.6.37 ext3 code, suppressing the journal leads into the error path. Simply removing `noload` would not be an equivalent write protection: ext3 can replay the journal even with `ro`. The checked clean state allows ext2 with `ro` here instead.

## 4. Verify success

Expected output:

```text
EXT2-fs (mmcblk0p7): warning: mounting ext3 filesystem as ext2
VFS: Mounted root (ext2 filesystem) readonly on device 179:7.
sh-3.2#
```

In the shell:

```sh
id
uname -srmo
mount -t proc proc /proc
mount -t sysfs sysfs /sys
cat /proc/mounts
cat /proc/partitions
cat /proc/uptime
```

These virtual mounts were tested; do not mount repeatedly when the filesystems are already mounted. Proven: `uid=0(root) gid=0(root)`, `Linux 2.6.37 armv7l GNU/Linux` and `/dev/root / ext2 ro,...`.

`rootfs / rootfs rw` is the initial RAM root, no proof of writable flash. `/sys/block/mmcblk0/ro` did give **0**, however: the block device itself is not write-protected. The mount is read-only; raw write commands would still be dangerous.

**Observe the reset:** In the attempt so far an uptime of 56.07 seconds was read, after which X-Loader started again. Do not claim a lasting root connection. First investigate the watchdog/reset cause and a RAM-based service start. `/proc/sys/kernel/panic` was 0; a configured automatic restart on panic is therefore not a proven explanation.

## 5. Optional: proven SD read mode in the bootloader

For the start from internal files above this change is not needed. It documents the successful access to the SD card in the open U-Boot.

**Use only exactly the same U-Boot version and memory layout.** Check `version` and `md.l 80edb4b0 8` first. With differing values abort and relocate from source/binary. The tested structure for SD is `0x80edb478`; eMMC follows at `0x80edb5e8` and must not be confused.

| Field | Address | Original value | Test value |
| --- | --- | --- | --- |
| SD `f_max`, offset `0x3c` | `0x80edb4b4` | `0x03197500` = 52 MHz | `0x003d0900` = 4 MHz |
| SD `host_caps`, offset `0x50` | `0x80edb4c8` | `0x00000911` | `0x00000800` = without HS/4-bit |

In the check the eight words from `0x80edb4b0` correspond to the fields `f_min`, `f_max`, `high_capacity`, `bus_width`, `clock`, `card_caps`, `host_caps`, `ocr`. In normal operation so far, among others: 400 kHz minimum, 4-bit bus and 50 MHz requested. The state can change through previous tests; the structure and field meaning must match.

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

These are reversible changes to U-Boot structures in **RAM**. Afterwards all three SD partitions were detected, the kernel loaded with **2,599,640 bytes** and CRC32 **792a42c1** confirmed. The SD kernel is four bytes smaller than the internal kernel; do not confuse them.

This mode is **not inherited by Linux**: Linux initialises the SD host itself and so far fails with a timeout. Simply starting the SD kernel with the default root path therefore does not give a working SD Linux installation.

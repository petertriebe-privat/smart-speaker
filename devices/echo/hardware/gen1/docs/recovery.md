# Backup, internal partitions and recovery

Dated notes: [history/05-backup-recovery.md](history/05-backup-recovery.md), [13-development-access-plan](history/13-development-access-plan.md), [14-network-ssh](history/14-network-ssh.md).

## Status

- **No full backup of the internal eMMC exists.** The EchoRoot SD image, the PC check hash and boot logs are no substitute. The owner explicitly approved replacing the internal software completely and does not need the original system, so a full original backup is not a prerequisite.
- A **targeted backup** was taken over SSH before the persistent install, on each of the two installed units: eMMC partitions p1 to p4 (xloader 896 KiB, recovery, boot, idme: 16 MiB each), each decompressed copy compared with its source SHA-256 (hashes checked against the device); GPT beginning and end; the original `init`, the boot/start configuration, and the kernel modules/firmware as an archive. Original-init hash confirmed against the archived file. Stored privately in the git-ignored `local/recovery/` (second unit: `local/echo2/recovery`). Backups hold device identity: keep them private. A firmware backup may contain credentials and personal data.
- The setup used the original internal system read-only before the install. `saveenv`, the EchoRoot installer, internal raw write commands and filesystem repairs were not run. Normal Amazon starts can themselves change boot/journal state; this does not mean the device never wrote internally.
- The install wrote only the root filesystem of the active slot (`/usr/local/echo-dev`, the `/sbin/init` link, passwd/group entries). Bootloader, GPT, kernel and IDME were not changed. Exact write targets and the SD/UART rescue path were checked beforehand.

## Internal GPT (second unit)

U-Boot device **1**; under Linux the eMMC is `/dev/mmcblk0` when the SD is not recognised, `mmcblk1` when it is (see [known-issues.md](known-issues.md)). Re-check identity and mapping for every backup.

| Partition | Name | Start LBA hex | End LBA hex | Size |
| --- | --- | --- | --- | --- |
| 1 | xloader | `0x100` | `0x7ff` | 896 KiB |
| 2 | recovery | `0x800` | `0x87ff` | 16 MiB |
| 3 | boot | `0x8800` | `0x107ff` | 16 MiB |
| 4 | idme | `0x10800` | `0x187ff` | 16 MiB |
| 5 | diags | `0x18800` | `0x587ff` | 128 MiB |
| 6 | main-A | `0x58800` | `0x2587ff` | 1 GiB |
| 7 | main-B | `0x258800` | `0x4587ff` | 1 GiB |
| 8 | data | `0x458800` | `0x747fdd` | about 1.47 GiB |

Linux reported **3,817,472 KiB** = **3,909,091,328 bytes** for the eMMC user area. Device/partition UUIDs are only in the local profile. Do not confuse partition gaps, primary/secondary GPT and separate eMMC boot areas with a collection of individual root partitions. The first unit's active slot was main-B, the further units' main-A; eMMC sizes differ (see [boot-and-root.md](boot-and-root.md)). Internal p3 (boot) is VFAT with `u-boot.bin` (156,532 bytes), only viewed read-only.

## Requirements for a full backup (if ever wanted)

1. Solve the restart (done: watchdog keep-alive) or use a reliable chunked transfer with resume.
2. Back up the whole eMMC user area including the GPT to an independent medium; check whether the eMMC hardware boot partitions/boot data are relevant and accessible.
3. No parallel internal write services. Enforce a block-wise read-only source as far as possible and prove it (so far only the root filesystem is mounted read-only).
4. Use a binary-safe path: since 2026-10-16 the WiFi SSH/SFTP link exists (about 1.3 to 2.2 MiB/s measured, [network.md](network.md)); a real binary transfer over UART was never built.
5. Determine exact byte count and SHA-256, keep a second copy, check samples/partition readability offline, record source/target device and command in writing.
6. Make the recovery plan concrete before the first flash: still bootable open bootloader, right target areas, sufficient power supply, verified images, interruption behaviour.

Over UART at 115200 baud only about 11.5 kB/s are possible before protocol overhead: a 3.9 GB raw dump would take about **94 hours**, a RAM buffer of several GiB does not fit, and a reset during an unsupported transfer destroys the run. There is deliberately no untested `dd ... of=/dev/mmcblk...` recipe: the direction is decisive with `dd`; a swapped target overwrites the original system.

## Recovery paths

- **UART is the recovery path.** Keep the wires or at least the pads accessible. Our own init gives a physical root shell on the serial port.
- **SD with the open U-Boot** (EchoRoot v0.8 image) is kept as an additional rescue medium; rescue procedure in [boot-and-root.md](boot-and-root.md). Do not leave it inserted in normal use: with it in, the unit starts the SD default kernel (`root=/dev/mmcblk0p2`) and ends in a root-mount panic (a repeated CR does not bring a running panic back into U-Boot).
- **Never run the historical EchoRoot installer** (`/home/root/install.sh`) blindly. It copies U-Boot to the internal partitions and overwrites main-A, and assumes SD=`mmcblk0`, eMMC=`mmcblk1`, which is not true in every session. Check the real devices (type SD/MMC, size, root `dev_t`, `/proc/bootmode`) before any write; never adopt block names from earlier sessions.
- **Restart** with `/usr/local/echo-dev/bin/echo-reboot` (syncs, remounts read-only, forces the kernel restart).
- If the clock/network is lost, check address over UART or router and update the private SSH profile; never accept unknown host identities blindly ([network.md](network.md)).

## SD card and historical writer

The 256 GB SD card was completely rewritten with the owner's approval and verified by a full byte comparison. The PC mapping at the time (drive I:, physical disk 3) is **historical**: after writing, labels and partitions change; decline Windows offers to format an unknown Linux partition. `local/archive/echo-diagnostics/write-echo-sd.ps1` is a one-off writer with fixed checks (exFAT volume `Alexa`, I:, disk 3, exact capacity and hash) and `eject-echo-sd.ps1` is bound to the same target: **do not reuse unchanged or remove the checks just to make them run**. No automatic recovery via these scripts is set up, and no raw SD writer is exposed in `tools/`. A new SD write needs a clear target identity and erase approval for the medium actually inserted.

## Open

Full eMMC backup, a tested restore procedure, and a replacement for the old bootstrap OpenSSH ([network.md](network.md)) are not done.

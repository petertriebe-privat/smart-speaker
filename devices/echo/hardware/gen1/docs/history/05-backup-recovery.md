# Backup, internal partitions and recovery

**Update 2026-10-16:** The user explicitly approved a complete internal software replacement and does not need the original system. A full original backup is not a prerequisite for the implementation. Targeted boot/device-configuration backups were verified before the WiFi/SSH installation; for current write targets and fallback material see the [network result](14-network-ssh.md). The earlier, more extensive backup proposals below are historical planning.

## Status

**No full backup of the internal eMMC exists yet.** The EchoRoot SD image, PC check hash and boot logs are no substitute. The interactive root shell alone does not yet provide a reliable backup/restore path.

The setup uses the original internal system read-only. `saveenv`, installers, internal raw write commands and filesystem repairs were not run. Normal Amazon starts can themselves change boot/journal state; do not derive from this a claim that the device never wrote internally in the past.

## Internal GPT of the second device

U-Boot device **1**; in the Linux minimal start with a failed SD **`/dev/mmcblk0`**. Re-check identity and mapping for every backup.

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

Linux reported a total of **3,817,472 KiB** = **3,909,091,328 bytes** for the eMMC user area. Device/partition UUIDs are only in the local profile. Do not confuse partition gaps, primary/secondary GPT and separate eMMC boot areas with a collection of individual root partitions.

## Requirements for a usable backup

1. First solve the restart or create a reliable chunked transfer with resume.
2. Back up the whole eMMC user area including the GPT to an independent medium. For device-specific boot/configuration areas check whether additional eMMC hardware partitions/boot data are relevant and accessible.
3. No parallel internal write services. Enforce a block-wise read-only source as far as possible and then prove it; currently only the root filesystem is mounted read-only.
4. Choose a binary-safe path: network transfer after its own setup, or a real binary transfer over UART. At present both are untested.
5. Determine exact byte count and SHA-256, keep a second copy and check samples/partition readability offline. Record source/target device and command in writing.
6. Make the recovery plan concrete before the first flash: still bootable open bootloader, correct target areas, sufficient power supply, verified images and interruption behaviour.

At 115200 baud only about 11.5 kB/s are possible before protocol overhead. A 3.9 GB raw dump over UART would thus theoretically take about **94 hours**. A RAM buffer of several GiB does not fit; a reset during an unsupported transfer destroys the run. A reliable network path or resumable chunks are much better suited for complete backups.

There is deliberately no untested `dd ... of=/dev/mmcblk...` recipe here. The direction is decisive with `dd`; a swapped target address overwrites the original system. A concrete backup command must first fit the verified transfer path.

## SD card and historical writer

The 256 GB card was completely rewritten with the user's approval. PC mapping at the time: drive I:, physical disk 3. That is **historical**, not fixed. After writing, labels/partitions change. Decline Windows offers to format an unknown Linux partition.

`local/archive/echo-diagnostics/write-echo-sd.ps1` is a one-off historical writer with fixed checks for the exFAT volume `Alexa` at the time, I:, disk 3, exact capacity and hash. **Do not reuse it unchanged or remove the checks** just to make it run. `eject-echo-sd.ps1` is likewise bound to the target at the time. No automatic recovery was set up with these scripts.

Rewriting an SD again needs a clear target identity and erase approval for the medium actually inserted. A firmware backup may contain credentials and personal data; store it only locally/git-ignored and protected.

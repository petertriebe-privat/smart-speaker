# Images, evidence and sources

## Local archive

The existing diagnostic files were moved to `local/archive/echo-diagnostics/`. They remain historical original material: some old scripts have fixed paths/target assumptions and the older `sd-boot-diagnosis.md` is superseded by the new documents. Record new console sessions under `local/runtime/`.

`local/archive-manifest.json` contains the size and SHA-256 of every adopted file, including images. Complete logs can contain device identifiers. Do not paste them into public issues; sanitise relevant excerpts first.

29 uploaded hardware photos were also adopted under `local/archive/hardware-photos/`. A selection index is in `local/README.md`. All 65 archive files were checked after the copy by size and SHA-256 against the original files.

Important files:

| File in the archive | Content / restriction |
| --- | --- |
| `boot-2026-10-13-212422.log` | Early stock start of the first device; no current hardware guarantee |
| `boot-2026-10-15-220927.log` | Early SD start diagnosis |
| `boot-2026-10-15-223506.log` | U-Boot RAM examination |
| `boot-2026-10-15-224530.log` | SD read tests, internal kernel, ext3 error, successful ext2 root start and subsequent restarts |
| `image-startup-inspection.txt` | Start scripts of the EchoRoot image read offline, not executed device scripts |
| `inspect-echo-image.py` | Small read-only ext2/3 parser for exactly this image; not a general filesystem library |
| `parse-uboot-memory.py` | Historical parser of the fixed-name log `223506`; null bytes can mask missing dump areas |
| `uboot-bss-dump.bin` | Extracted 16 KiB RAM area; not a complete RAM/firmware dump |
| `u-boot-*` | Source excerpts of the matching bootloader |
| `linux-2.6.37-ext[23]-super.c` | Official Linux reference code for mount behaviour; not to be treated as complete Amazon kernel source |
| `sd-write-status.json`, `sd-write.log` | Successful SD write and complete byte comparison/hash |
| `sd-write-target.json` | Private disk identity at the time; no current target proof |

The image parser supports old block pointers and simple symlink contents; it does not follow path symlinks automatically, is not checked for ext4 extents/all special cases and can map sparse/indirect holes incompletely. For evidence preservation or arbitrary files use a robust read-only library/filesystem environment and cross-check the results.

## EchoRoot v0.8

Release of the historical EchoRoot project. **Not a current Debian**, not an already installed own assistant.

| Artifact | Size | SHA-256 / check value |
| --- | --- | --- |
| `images/echoroot-v0.8-sdcard.img.xz` | 313,232,980 bytes | `B41B9C4F911B1C2FC5C0731D7310AAE4FAA675C25DF4680F3D279B3965CFFE0C` |
| `images/echoroot-v0.8-sdcard.img` | 2,113,929,728 bytes | `F378D8A700B83B8028962D332A01A61250C4965926B32BB49B443A5A5A837657` |
| `images/echoroot-v0.8-uImage` | 2,599,640 bytes | CRC32 `792a42c1`; extracted from SD partition 2 |
| `images/echoroot-v0.8-kernel-uncompressed.bin` | 5,282,080 bytes | Analysis artifact after unpacking the zImage; **not a directly flashable uImage** |

The image contains the Amazon filesystem (state October 2020) with Alexa's wake word engine; how to read it with `debugfs` and what is in it: [22-alexa-wakeword-original.md](../../../../docs/22-alexa-wakeword-original.md).

The entire 2,113,929,728 written bytes were read back from the SD and compared. The hash checks consistency with the download used, it is not an independent signature of the publisher.

MBR of the image, 512-byte sectors:

| SD partition | Start LBA decimal | Sectors | Type |
| --- | --- | --- | --- |
| 1 | 1 | 32,768 | `0x0c`, FAT |
| 2 | 32,769 | 2,048,000 | `0x83`, Linux root |
| 3 | 2,080,769 | 2,048,000 | `0x83`, installation source |

The remaining capacity of the 256 GB card stays unallocated after the image. `I:` from Windows is not a permanently usable Linux partition identifier.

SD U-Boot: `2010.06-00011-ge1ab9145d5`, build 2021-11-27 21:37:39, 172,952 bytes. Source commit **`e1ab9145d5b7f320644a8235980c22603e7a9c7a`**, link/text base **`0x80e80000`**. Fixed RAM structure addresses apply only to this actually examined build.

SD kernel uImage: Linux 2.6.37, load/entry **`0x80008000`**, legacy uImage with an ARM zImage as payload. The uImage compression entry is "uncompressed"; the ARM zImage internally contains compressed code. The official image is to be distinguished from the **internal** original kernel (2,599,644 bytes).

## Primary project sources and examined references

- [Echo Hacking wiki: Echo](https://github.com/echohacking/wiki/wiki/Echo) - device target, hardware access and historical root procedures.
- [Vanderpot pinout](https://vanderpot.com/wp-content/uploads/2016/06/pinout.png) - orientation and debug pads.
- [EchoRoot project](https://gitlab.com/AndreRH/echoroot), [setup](https://andrerh.gitlab.io/echoroot/SETUP.html), [v0.8 release](https://gitlab.com/AndreRH/echoroot/-/releases/echoroot-v0.8) - historical software base and image. Check availability and immutable download IDs before downloading again.
- [Matching U-Boot commit](https://gitlab.com/AndreRH/u-boot-echo/-/tree/e1ab9145d5b7f320644a8235980c22603e7a9c7a) - MMC structures, host caps, commands and link address.
- [Buildroot fork](https://github.com/AndreRH/buildroot/tree/linux-2.6.37) - EchoRoot submodule branch. The branch is mutable; the exact submodule commit for a reproducible build is still to be determined.
- [Linux v2.6.37 ext3 super.c](https://github.com/torvalds/linux/blob/v2.6.37/fs/ext3/super.c), [ext2 super.c](https://github.com/torvalds/linux/blob/v2.6.37/fs/ext2/super.c) - reference for journal and feature checks, archived locally.
- [Amazon Echo teardown](https://de.ifixit.com/Teardown/Amazon+Echo+Teardown/33953) - comparison photos of the tall Echo. The **Echo Dot** teardown that was temporarily opened in the browser concerns different hardware.
- [Reversec: Echo rooting](https://labs.reversec.com/advisories/2017/08/amazon-echo-rooting), [CARDIS paper](https://pure-oai.bham.ac.uk/ws/files/56108659/Breaking_all_the_things_CARDIS_.pdf) - historical security/hardware research; partly concerns other production states and does not replace the concrete device test.

When adopting source code, Amazon binaries, DSP firmware and Buildroot components, check the respective licences. The local archive is not published here as a freely redistributable firmware package.

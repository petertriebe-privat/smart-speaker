# Echo Dot Gen 2: USB diagnosis and development access

**Current state 2026-10-20:** After the successful Amonet v2 unlock and TWRP test, the Dot now runs its own Alpine-based Linux with automatic WiFi, root SSH and SFTP. Two further reboots passed; the USB CDC root console works. Current operation and the assistant's development base: [21-echo-dot-linux-ssh.md](21-echo-dot-linux-ssh.md). TWRP is kept as a checked fallback copy and is not running at the moment. This file holds the chronicle of the unlock and USB diagnosis; older statements about the device state are historical.

State 2026-10-20. The user confirmed model number **RS03QR** from the underside of the device. The device previously called Gen 1 is a **second-generation Dot**, codename biscuit. It is not a TI DM3725 device: the documented platform is MediaTek MT8163 with 512 MB RAM. The setup of the large Echo cannot be transferred as a flash or pinout guide.

## Actually observed on the Windows PC

- After connecting, there was first no permanent Amazon USB/ADB/fastboot device and no new serial port. The existing Espressif COM port does not belong to the Dot and was not opened.
- The Windows PnP history contains two briefly registered **MT65xx Preloader** entries, hardware ID **VID_0E8D&PID_2000**, deregistered again afterwards. Together with the confirmed model number, this observation explains the MediaTek boot path. Exact connection times and private raw data are in local/dot-usb/2026-10-20-usb-observation.json.
- A preloader entry proves USB data signalling, not yet usable root access or permanent ADB. No installed service in the queried preloader entries; do not conclude from this for all later USB modes.
- ADB/fastboot were first not in PATH and not in the private project area. Official Google Platform Tools **37.0.1-15733141** from https://dl.google.com/android/repository/platform-tools-latest-windows.zip were extracted to local/dot2-tools/, ZIP path boundaries checked, archive SHA-256 and versions recorded in the private manifest. Archive 8044989 bytes. No global installation or PATH change. At first only --version and fastboot devices were run.
- First fastboot devices query: **0 devices** while the Dot was booting normally. This is not proof of a missing fastboot function.

## Next diagnostic mode

The documentation for this model describes: unplug USB/power, hold the action button (the dot), plug USB back in until the ring is green, then release the button. This is the original fastboot mode, not proof of root. The user was asked to do this; feedback and the actually detected interface were still pending.

Afterwards only PnP ID/driver status plus fastboot devices and read-only getvar queries. Keep device serial numbers and raw responses private. A missing Windows driver has to be told apart from a missing USB mode. No drivers bound to other USB devices yet.

## Documented later root paths

For RS03QR there is amonet-biscuit with Windows support. Sources describe factory fastboot, exploit-assisted unlock and TWRP. A USB entry without soldering to the SD card is therefore documented for this model line; it had not yet been run on this particular Dot. Which amonet/Fire OS version and hardware initialisation fit must be decided before any concrete write step. Flash/wipe scripts are not diagnostic commands.

The previous large Echo running the assistant was not contacted or changed in this session. On the Dot so far **no unlock, no flash, no wipe, no driver installation, no root shell and no SSH**. Do not transfer the successful WiFi/SSH tests of the large Echo to the Dot.

## Sources

- [EchoMuse: Rooting Echo Dot Gen 2](https://github.com/wilbowes/EchoMuse/blob/main/docs/rooting.md), model RS03QR/MT8163 and amonet/Fire OS and hardware notes.
- [TECHO5: Getting started, Echo Dot Gen 2](https://github.com/HuskerMinion/techo5/blob/main/docs/getting-started.md#echo-dot-2nd-gen), factory fastboot with the action button and green ring, Windows workflow and a further pointer to amonet.
- [Google: SDK Platform Tools](https://developer.android.com/tools/releases/platform-tools), official diagnostic tools and versions.

Sources read on 2026-10-20. Keep researched vendor/project statements separate from tests actually run.

## Fastboot interface detected despite blue ring

The user reported a permanently blue ring instead of green after the action button/plugging in, and was still holding the button. Asked to release it immediately. Windows now reports **Android**, VID_0BB4&PID_0C01, compatible USB class **FF/42/03**, i.e. fastboot. The concrete USB detection is more telling here than the differing LED colour. No further button combinations needed.

Windows problem code **28**, no driver service bound. fastboot devices therefore stays empty. The normal Windows process has no administrator rights; binding a driver needs a Windows administrator confirmation. This is not a missing root access on the Dot, but the still missing PC-side USB driver assignment.

The existing android_winusb.inf in the DriverStore is a signed Meta/Reality Labs driver, only for its own hardware ID. Do not use or change it. The official Google USB driver from https://dl.google.com/android/repository/usb_driver_r13-windows.zip was extracted privately to local/dot2-tools/usb_driver/. Its androidwinusba64.cat has a valid Google LLC signature. The INF contains Android Bootloader Interface, but no automatic match for VID_0BB4&PID_0C01. Merely searching/installing automatically would therefore not be enough; a targeted manual driver selection must stay limited to exactly the confirmed fastboot device. Do not edit the INF and do not disable signature checking.

State of this entry: driver only checked/prepared, not yet bound. No unlock/flash/wipe and no root shell. Official Platform Tools prepared locally, no ADB server started. Further primary reference for the USB transport check: [AOSP fastboot usb_windows.cpp](https://android.googlesource.com/platform/system/core/+/refs/heads/main/fastboot/usb_windows.cpp).

## Fastboot confirmed after driver assignment

The user assigned the prepared Android Bootloader Interface driver in Device Manager and reported a green ring. The screen recording shows Reality Labs Device as the parent class; this alone does not identify the driver actually bound here. Checked read-only: provider **Google, Inc.**, version **11.0.0.0**, service **WinUSB**, problem code **0**. Do not mistake the existing Meta class label for a wrong driver binding.

Official fastboot 37.0.1 finds exactly one device. Only getvar queries were made:

| Variable | Response |
| --- | --- |
| product | BISCUIT |
| lk_build_desc | 63cb91b-20221007_072309 |
| secure | yes |
| unlock_status | false |
| version-bootloader | Empty response, query exit 0 |
| unlocked | Empty response, query exit 0 |

Serial number and full outputs only in local/dot-usb/fastboot-identity.json and fastboot-devices-after-driver.log. **The USB fastboot transport works, the bootloader is still locked.** A root shell, own firmware or SSH are not yet set up on the Dot. No unlock/flash/wipe performed.

The firebreak project lists this lk_build_desc in its documented Biscuit build table. This supports the choice of a matching software path; it is not yet an unlock run on this Dot. Source: [firebreak device/build table](https://github.com/bboe/firebreak/blob/main/docs/devices.md). Do not use the old amonet v1.0 scripts only because of the same LK ID: the EchoMuse project documents a case where their write step was rejected with restricted on locked hw.

## Unlock step prepared, not yet run

Firebreak **0.6.1** was downloaded from the PyPI package belonging to the bboe/firebreak project, the PyPI SHA-256 checked and the source code read statically. Not installed or run. The source defines Amonet Biscuit v2.0.0 with package SHA-256 `98297293701082bc7272efe077f941c56fc7b6e1f27ef6f2e93b6e4c6fc7b62d`. This 55989416-byte package was also downloaded from the GitHub release specified there and checked against the pin; fully unpacked into the private folder local/dot2-tools/amonet-biscuit-v2-prepared/. No package programs were run.

profile.ps1 explicitly selects `bin/fastbrick-20221007.img` for `63cb91b-20221007_072309`. The file is present and hashed separately; the private prepared-unlock.json holds size/hash. The Windows script asks for YES and then starts a writing fastboot payload. A timeout reported by this script alone counts there as probably successful, but must afterwards be verified by actual recovery/bootloader behaviour.

This is not just another getvar query: Amonet v2 changes early boot components, recovery and the security/storage state; in the Firebreak plan, among others RPMB and preloader are visible as especially critical steps. An incomplete write can prevent booting. Therefore explicit approval for **this Dot** was requested before the first such step, including possible loss of previous settings. The earlier confirmation of the USB driver installation does not count as an unlock confirmation. Feedback was pending at the state of this entry.

**Current device state remains:** an unchanged, locked BISCUIT in working fastboot mode, green ring according to the user. The large Echo/assistant was not contacted. Do not automatically start a full installation with the Firebreak default: its default target is Fire OS 5/Amonet v1.1.0-bboe, while the package prepared here is Amonet v2 for Fire OS 6. The audio/assistant port and the later choice of operating system are not yet implemented on the Dot.

## Approval and corrected fastbrick call (2026-10-20)

The user explicitly approved unlocking this Dot and installing TWRP with "Yes, unlock the Dot". The first call without a sparse size option was rejected locally by Google fastboot before sending: payload 114349580 bytes, reported download maximum 114294784 bytes. No write in this first attempt. Firebreak 0.6.1 explicitly uses `fastboot -S 256M flash brick ...` in stage_fastbrick; the corrected attempt follows this template, with the explicit private device serial number and a re-checked payload hash, product, LK and unlock_status. A host timeout after eight seconds is only a hint that it started, not yet proof of success. Raw log: local/dot-usb/unlock-corrected-attempts.json. Source: https://github.com/bboe/firebreak/blob/main/firebreak/unlock.py

## TWRP and written components verified

The corrected call reported Sending brick OKAY after 3.509 s, then Writing brick and a host timeout after eight seconds. Afterwards USB switched from VID_0BB4/PID_0C01 to VID_18D1/PID_4EE2 (composite, MTP and Android Composite ADB Interface). Both interfaces worked without further manual driver binding. ADB reports recovery, product omni_biscuit, device biscuit. Keep the device serial number private.

Confirmed read-only: TWRP **3.7.0_9-0**, `id` returns UID/GID 0 and context `u:r:su:s0`; kernel **Linux 3.18.19-gecb8cb46060-dirty armv7l**, hardware mt8163, MemTotal 492848 kB. This is a root shell in recovery, not yet an established root/SSH service in the normal operating system.

The first image-sized bytes of `/dev/block/mmcblk0p3` and `mmcblk0p5` match `bin/lk.bin` from the checked package; `mmcblk0p12` matches `bin/twrp.img` (SHA-256). So not just a timeout, but the actually installed internal Amonet/recovery chain is proven. Details are private in `local/dot-usb/twrp-first-probe.json` and, after the backup was completed, `unlock-verification.json`.

**Important block path trap:** In this TWRP variant, the convenient by-name paths `lk_a`, `lk_b`, `preloader`, `tee`, `tee1` and `tee2` point to OTA decoys under `/tmp/ota-decoy`. For real read checks, use the resolved `_real` paths or re-verified physical block devices. Do not diagnose a broken bootloader from a decoy hash. Do not write blindly to a by-name path.

TWRP has already mounted userdata under /data and /sdcard, and cache, **rw** by itself. The agent queries were read-only; the recovery as a whole is not a read-only forensic mode. No wipe/format/installer actions were started.

PC tools: `local/dot2-tools/platform-tools/adb.exe` and fastboot.exe. For starting ADB, `ANDROID_USER_HOME` was set to `local/dot2-tools/android-user`; private host configuration stays there. No global PATH change. Always select the specific device with `-s` from the private profile, not any attached Android device. Never commit the raw profiles and backups.

Backup peculiarities: `adb exec-out dd ...` can also include the device-side dd statistics in the binary stream. Therefore use `2>/dev/null` **on the device** and check every file against its size and target SHA-256. /proc/partitions rounds down to KiB: misc is shown as 512 KiB, but according to sysfs actually has **1025 sectors = 524800 bytes**. Never derive sizes from the rounded display alone.

## Targeted backup after the unlock completed

Eleven areas (kb, dkb, both real LK copies, misc, persist, boot_a, boot_b, recovery, boot0 and boot1) were read completely and each checked against a device-side SHA-256. The full path is private in local/dot-usb/unlock-verification.json; the files are in its backup_directory with manifest.json. These are **backups after the unlock**, not a backup of the earlier locked state. Before the payload, the locked fastboot interface offered no verified raw read access. No complete userdata/system image was made. Two aborted backup preparations are not valid backups; use only the collection of eleven checked files referenced in the final unlock-verification.json.

## Restart of the internal recovery passed

On 2026-10-20 `adb -s <private serial number> reboot recovery` was run. After **34.79 seconds** the same Dot answered again as recovery; root UID 0, TWRP 3.7.0_9-0 and a new kernel uptime of 16.50 s confirmed. No new fastbrick transfer needed. The recovery is thus installed internally and reachable after a software restart. A powerless cold start and the normal Fire OS/own Linux start are not yet tested. Leave the Dot in this working TWRP root session at the end.

Private reboot evidence: `local/dot-usb/twrp-reboot-validation.json`. All helper scripts actually run are kept as audit copies in `local/dot-usb/audit-scripts/`; in particular the unlock helper is **not a start command to repeat automatically**. For normal development use the existing ADB recovery connection. For the USB test use `adb devices -l`, for commands always `adb -s <private serial number> shell <command>`. Read profiles locally and do not carry identifiers into public documentation.

Result of this stage: Amonet v2.0.0 and TWRP installed; root over USB confirmed, targeted backups verified, recovery reboot passed. Still no persistent WiFi/SSH access, no own normal userspace, no Claude/Codex CLI on the Dot, no audio/microphone/assistant test. No connection made to the large Echo and its running assistant was not changed.

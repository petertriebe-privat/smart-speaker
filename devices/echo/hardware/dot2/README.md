# Echo Dot 2nd generation (RS03QR): unlock over USB, own Linux, the assistant

This guide shows how to turn an Amazon Echo Dot 2nd generation (model **RS03QR**, codename
**biscuit**, MediaTek MT8163, 512 MB RAM) into a development device over USB. No soldering is
needed. At the end the Dot runs a small Alpine-based Linux with automatic WiFi, root SSH and
SFTP, and runs the voice assistant.

**The unlock is entirely the work of others.** We only used their tools; see
[Credits](#credits-and-upstream-work). For the exploit itself, follow their instructions, not
this page. This page records what we did and which scripts we added on top.

Status: it works on one device (October 2026). Anything not marked as tested below was not
tested. Open work on the Dot is tracked in issue #72.

## 1. What you get and what you need

You get:

- Root over USB (TWRP recovery) and a backup of the boot-critical partitions.
- Our own Linux in the recovery partition: Alpine 3.24.1 armv7, BusyBox, the original
  Dot kernel (3.18.19), WPA2 WiFi with DHCP, NTP, Dropbear SSH (keys only) on port 22, SFTP.
- A USB CDC root console as physical rescue access.
- The assistant, started at boot as a service.

You need:

- A Windows PC (the tools and scripts here were run on Windows) and a USB cable that carries
  data, not only power.
- Google SDK Platform Tools (`adb`, `fastboot`) and, on Windows, the Google USB driver, bound
  by hand to the Dot's fastboot interface in Device Manager. Do not edit the INF and do not
  disable signature checks. Details: [docs/20](docs/20-echo-dot-gen2-usb.md).
- Python 3, Docker (only for the Bluetooth helper), Git Bash.

Risk. The unlock changes the preloader, LK, recovery and the RPMB/security state. An
interrupted write can leave the device unbootable. You may lose the Amazon software and your
settings. Do it only on a device you can afford to lose. The unlock step is not an
experiment: read the upstream instructions first.

## 2. Identify the device

- The model number **RS03QR** is printed on the underside. A first-generation Dot (model
  S04WQR, TI DM3725) is a different device and none of this applies; see
  [the Gen 1 Dot note](../../docs/19-echo-dot-gen1.md).
- Connected to a PC, the Dot shows up briefly as **MT65xx Preloader** (USB VID 0E8D, PID 2000).
- Factory fastboot: unplug USB/power, hold the action button (the dot), plug USB in, release
  the button when the ring is green. In our case the ring stayed blue, but Windows still saw
  a fastboot interface (VID 0BB4, PID 0C01, class FF/42/03). Trust the USB ID, not the colour.
- Read-only check with `fastboot getvar`: `product` = `BISCUIT`, `secure` = `yes`,
  `unlock_status` = `false`, and `lk_build_desc` (ours: `63cb91b-20221007_072309`). The
  `lk_build_desc` selects the matching payload in the unlock package.

## 3. Unlock and TWRP

What we did, in short (details and the full log: [docs/20](docs/20-echo-dot-gen2-usb.md)):

1. Read the plan of firebreak (a USB unlock tool) and the amonet biscuit package without
   running anything. The package pins Amonet Biscuit **v2.0.0** (Fire OS 6); its profile chose
   `bin/fastbrick-20221007.img` for our `lk_build_desc`. Do not use the old amonet v1.0 scripts
   only because the LK string matches: one reported case was rejected with
   `restricted on locked hw`.
2. Staged the payload with fastboot. The payload (about 114 MB) is larger than the device's
   download limit, so it needs a sparse size option: `fastboot -S 256M flash brick ...`, as in
   firebreak.
3. A host timeout after the payload is only a hint. The proof was that USB switched to
   VID 18D1, PID 4EE2 and `adb` reported `recovery`, product `omni_biscuit`.
4. Result: **TWRP 3.7.0_9-0**, root shell (UID 0) over ADB, kernel 3.18.19 armv7.
5. Verified the install: the first image-sized bytes of `mmcblk0p3` and `mmcblk0p5` match
   the package's `lk.bin`, and `mmcblk0p12` matches `twrp.img` (SHA-256).

Trap in this TWRP: the by-name paths `lk_a`, `lk_b`, `preloader`, `tee`, `tee1` and `tee2`
point to decoy files under `/tmp/ota-decoy`. Use the `_real` paths or checked block devices
when reading, and never write blindly to a by-name path.

## 4. Backups

Right after the unlock we read eleven areas and checked each against a SHA-256 taken on the
device: `kb`, `dkb`, both real LK copies, `misc`, `persist`, `boot_a`, `boot_b`, `recovery`,
`boot0`, `boot1`. They are backups **after** the unlock. A backup of the locked state was not
possible: the locked fastboot offered no verified raw read. No full userdata or system image
was taken.

Notes from the backup run: `adb exec-out dd` can mix dd statistics into the binary stream, so
redirect stderr on the device (`2>/dev/null`) and check size and hash of each file.
`/proc/partitions` rounds sizes down (`misc` is 524800 bytes, not 512 KiB).

All backups, serial numbers and raw logs stay in `devices/echo/local/` (git-ignored). Never
commit them. Amazon firmware files must not go into Git either.

## 5. Own Linux

Design: the root file system is a gzip initramfs inside the **recovery partition**
(`mmcblk0p12`, 16 MiB), unpacked into RAM at boot. The kernel and device trees come
unchanged from the checked TWRP copy. `misc` is re-armed with `boot-recovery` at each start so
a power cycle boots this image again. LK, preloader, GPT and the Amazon system slots are not
touched. Changes to `/etc` or `apk` installs are therefore lost at reboot; durable changes go
into the image build. Programs and data live on the userdata partition under
`/data/echo-dot-dev`.

Tools in `tools/`, in the order of use (all take `--project <path to devices/echo>`):

| Step | Tool | Purpose |
| --- | --- | --- |
| 1 | [`prepare.py`](tools/prepare.py) | Builds the network root file system from pinned, checked inputs. Makes a dedicated SSH key under `local/dot-ssh/` and a WPA2 config from `local/wifi-settings.json`. No flashing. |
| 2 | [`stage-network.py`](tools/stage-network.py) | One-time test of that root file system in a chroot inside TWRP. Not a normal start command. |
| 3 | [`build-image.py`](tools/build-image.py) | Builds the credential-free boot image (recovery kernel plus our initramfs). Never flashes. |
| 4 | [`install-boot.py`](tools/install-boot.py) `--install` | First install into `mmcblk0p12` from TWRP over ADB. Checks hashes, copies the TWRP rollback copy to userdata, sets a trial marker. One-time. |
| 5 | [`update-boot.py`](tools/update-boot.py) `--install` | Later image updates over SSH. Checks that the installed image is the one recorded earlier. |

What runs at boot:

- [`init`](tools/init): PID 1. Mounts virtual file systems and userdata, re-arms `misc`, sets
  up the USB CDC console (USB ID 1D6B:0104), starts BusyBox init.
- [`network.sh`](tools/network.sh): starts the MediaTek helper `wmtup` (firmware from userdata),
  Dropbear, `wpa_supplicant`, the DHCP client ([`dhcp.sh`](tools/dhcp.sh)) and `ntpd`. Each is
  restarted after it exits.
- [`services.sh`](tools/services.sh): runs every executable `/data/echo-dot-dev/services/*.sh`
  and restarts it 3 s after it ends. The list is read at boot only.
- [`shutdown.sh`](tools/shutdown.sh): sync and unmount (or read-only remount) of userdata.
- [`to-twrp.sh`](tools/to-twrp.sh): see "Back to TWRP" below.

Layout on userdata: `bin/`, `services/`, `private/` (WiFi config, `authorized_keys`, SSH host
key), `firmware/` (MediaTek firmware copied from the device itself, never published),
`artifacts/` (installed boot images), `recovery-twrp.img` with its `.sha256`.

### WiFi and SSH

WiFi and the key come from the private files created by `prepare.py`. Dropbear accepts keys
only, as root. SFTP works with plain `scp`. For access from the PC, create a private SSH profile
`devices/echo/local/dot-ssh/config` (git-ignored) with an alias, here `dot-dev`, the device
address, user root, the dedicated key and strict host key checking. Then:

```powershell
ssh -F devices/echo/local/dot-ssh/config dot-dev
```

[`tools/Invoke-Dot.ps1`](tools/Invoke-Dot.ps1) wraps this: `Invoke-Dot.ps1 -Command 'id'`. It
needs that profile. If DHCP changes the address, read it on the USB console with
`ip -4 addr show wlan0` and update the profile.

### USB console and logs

In our Linux, USB offers a CDC serial root console (`/dev/ttyGS0` on the Dot, "Serial USB
device" on Windows) instead of ADB. Find the right COM port by its USB ID; do not open ports
blindly. Logs are in RAM under `/run/echo-dot-dev` (`wmtup.log`, `wpa.log`, `dhcp.log`,
`ssh.log`, `ntp.log`, service logs). `wpa.log` and `dhcp.log` hold network identifiers: do not
publish them.

### Back to TWRP

`echo-dot-to-twrp` (source [`to-twrp.sh`](tools/to-twrp.sh)) checks the SHA-256 and size of the
TWRP backup, writes it to the recovery partition, verifies it, re-arms `misc` and reboots.
Only run it on purpose. Not yet tested by an actual write-back since the Linux replacement.
Safety net: each first boot of a new image creates `boot-trial`. If it is not removed within
240 seconds after SSH is confirmed, the start helper tries this way back by itself.

Tested: two automatic reboots with SSH back in about 31 s; USB console answers. Not tested: a
cold start from power off, a router outage.

## 6. Deploying the assistant

The assistant is the same Go program as on the Echo Gen 1 ([`core/`](../../../../core/)). At start
it detects the board (the Dot has an `amz_privacy` driver in sysfs) and picks the Dot drivers.
Build for ARMv7 without CGO is done by the deploy script.

```sh
# Git Bash on the PC, from devices/echo
ECHO_HOST=dot-dev sh tools/deploy.sh [--secrets] [--librespot]
sh tools/deploy.sh --all      # every device with local/devices/<host>.json, Dot included
```

[`tools/deploy.sh`](../../tools/deploy.sh) recognises the Dot because the host is
in `local/dot-ssh/config`. It runs vet and tests, builds, copies `assistant`, `assistant.sh`,
`install-assistant.sh` and optional `device.json`, secrets, `librespot`, `btspeaker` into
`/data/echo-dot-dev/jarvis-stage`, and runs `install-assistant.sh` there. The first install of the
service reboots the Dot once, because `services.sh` reads its list at boot. After that,
the assistant is restarted by killing it; the service loop starts it again.

Per-device settings (role, room, Spotify name) come from `local/devices/<host>.json`, see
[multiple Echos](../../docs/18-several-echos.md). Keep them private.

Dot specifics:

| Topic | On the Dot |
| --- | --- |
| Settings | **No `settings.json`**; the built-in defaults are the Dot paths. Only `device.json`. |
| Files | Binary, `device.json`, `librespot` in `/data/echo-dot-dev/jarvis`; secrets in `private/jarvis-secrets.json`; state in `/data/jarvis`; socket in `/run/jarvis`. |
| `jarvis ctl` | Under Dropbear `/usr/local/bin` is not in PATH: call `/data/echo-dot-dev/jarvis/jarvis ctl status`. |
| Log | `tail -f /run/echo-dot-dev/jarvis.sh.log` (rotated at 4 MiB). |
| Service | [`assistant.sh`](tools/assistant.sh) (installed as `services/jarvis.sh`) shields Dropbear from the OOM killer, backs off after more than five starts in five minutes, and can be disabled with a `disabled` file in the assistant's folder. |
| Audio | ALSA used directly from Go; no PulseAudio, no DSP. Own mixing loop for playback, microphone channel 6. |
| Echo | No echo cancellation: the echo gate is on, no barge-in while the assistant speaks. |
| Bluetooth | No BlueZ. Only the [`btspeaker`](tools/btspeaker/) helper (issue #74): the Dot as A2DP source, built on BTstack over `/dev/stpbt`. |

Build `btspeaker` with [`tools/btspeaker/build.sh`](tools/btspeaker/build.sh): it needs Docker
with ARM emulation and fetches BTstack (BlueKitchen licence: personal, non-commercial use) at a
fixed commit into `local/dot2-tools/`. The deploy script includes the result when it exists.

## 7. Known issues and open work

- Open work: **issue #72** (echo cancellation with the loopback as reference, beamforming of the
  microphones, wake word model, Tailscale, ring alignment).
- WiFi: after the router's 10-minute group key renewal the Dot received broadcasts and
  multicasts only every other period. Unicast is fine. The assistant works around it (a device option
  so the Dot never takes the lead role, direct peer messages). Discovery by mDNS/SSDP stays
  unreliable. Cause in the MediaTek driver is open.
- Not tested with real input: button presses, mute, volume keys, speech from a few metres.
- The tools carry the "bootstrap-1/-2" naming and read private inputs from `local/`; they are
  a record of our path, not a polished installer.

## Credits and upstream work

The unlock and TWRP are entirely the work of these projects. Please credit and support them.

- **amonet**, MediaTek bootrom exploit, by xyz: <https://github.com/xyzz/amonet>
- First port to the Echo Dot 2 by k4y0z (<https://github.com/chaosmaster>), continued and
  released with TWRP by R0rt1z2: <https://github.com/R0rt1z2/amonet/tree/mt8163-biscuit>.
  XDA thread:
  <https://xdaforums.com/t/unlock-root-twrp-unbrick-amazon-echo-dot-2nd-gen-2016-biscuit.4761416/>
- **firebreak** by bboe, a USB unlock tool; we used it to review the unlock plan (we did not
  install it): <https://github.com/bboe/firebreak>
- **EchoMuse** rooting notes by wilbowes:
  <https://github.com/wilbowes/EchoMuse/blob/main/docs/rooting.md>
- **TECHO5** by HuskerMinion, getting started including factory fastboot with the action button:
  <https://github.com/HuskerMinion/techo5/blob/main/docs/getting-started.md#echo-dot-2nd-gen>
  and <https://github.com/HuskerMinion/techo5-dot>. From the Dot repository our image build uses
  the `mkimage` and `mkrootfs` helpers (pinned commit, MIT licence, kept under
  `local/dot2-tools/techo5-review/source`), the `wmtup` WiFi/BT helper compiled for ARMv7, and
  network packages and a static BusyBox from its release v1.0.24. No assistant software from it
  is installed.

## 8. Detailed documents

| Document | Content |
| --- | --- |
| [docs/20-echo-dot-gen2-usb.md](docs/20-echo-dot-gen2-usb.md) | Chronicle of the USB diagnosis, driver binding, fastboot values, unlock, TWRP checks, backups. |
| [docs/21-echo-dot-linux-ssh.md](docs/21-echo-dot-linux-ssh.md) | The own Linux: storage layout, SSH/WiFi, USB console, the assistant on the Dot, measurements, WiFi broadcast problem, rollback. |
| [../../docs/19-echo-dot-gen1.md](../../docs/19-echo-dot-gen1.md) | Notes on the Dot Gen 1 (different hardware), including why it is not this device. |

# Echo Dot 2: own Linux, WiFi, root SSH and assistant development

State 2026-10-20. Separate device **RS03QR / biscuit / MediaTek MT8163**. Do not contact or change the large Echo running the assistant for this setup. Its UART/SD instructions, block numbers and DSP paths do not apply here.

## What is actually set up

- Own Linux runtime from **Alpine 3.24.1 armv7**, BusyBox and our own start/network scripts. No Android init, no TWRP interface and no Alexa processes in the normal own start.
- Working kernel and device trees taken unchanged from the previously checked TWRP copy: **3.18.19-gecb8cb46060-dirty**. This is still an old, device-specific Android/Linux kernel, not a current distribution kernel.
- WiFi with WPA2/CCMP and DHCP from the WiFi configuration already present privately. MediaTek firmware taken from this device's system_b; the firmware files are not published.
- **Dropbear 2026.91** on port 22, own Ed25519 host key, key login only. Root UID 0, no forced commands. Dedicated PC developer key and host identity checked over the previously trusted USB access. No global Windows SSH configuration changed.
- SFTP with **OpenSSH sftp-server 10.3_p1-r1**; official Alpine package checked with its signing keys. Normal modern scp works without `-O`.
- Automatic time sync: BusyBox ntpd, confirmed at our own start. This is a usable basis for later TLS connections of the assistant.
- Physical root console over USB CDC, in parallel to SSH. Under Windows automatically usbser instead of a driver to be installed manually afterwards.

**The assistant has run on the Dot since 2026-10-20** (section [The assistant on the Dot](#the-assistant-on-the-dot)): the same Go binary as on the large Echos, with speaker, microphones and LED ring through its own driver layer. The original Fire OS userspace is still in its system slots; our normal start does not run it. No Home Assistant or third-party assistant daemon installed.

## What "own Linux" means here

We did not write a new distribution or a kernel from scratch. The runtime combines an Alpine base, the already working Dot kernel, open-source network programs and our own scripts. As a development base it is replaceable. Another base or kernel must fit 32-bit ARMv7, the boot image format, the MediaTek drivers and the device trees. An arbitrary Debian/ARM64 image is not a drop-in replacement.

The Alpine base system and `/init` live in the compressed initramfs of the **internal recovery partition**. At boot they are unpacked into RAM. Changes to `/etc`, `/usr`, package installs with apk etc. are therefore volatile at first. Durable system changes have to go into the reproducible image build. Programs, private configuration and application data are stored under **`/data/echo-dot-dev` on internal userdata** and survive reboots.

## Storage and boot path

| Area | Use in this setup |
| --- | --- |
| recovery, `mmcblk0p12`, 16 MiB | Own kernel/initramfs image, currently bootstrap-2 |
| misc, `mmcblk0p8`, first 32 bytes | `boot-recovery`, set again at every own start |
| userdata, `mmcblk0p16`, `/data` | Own programs, WiFi/SSH configuration, firmware copy, TWRP fallback copy |
| system_a / system_b | Existing Fire OS files; not overwritten during this Linux installation |
| LK, preloader, GPT | Not changed since the documented Amonet unlock in this SSH stage |

This block mapping was checked on the specific Biscuit. Determine it anew for another device. Linux uses `/dev/mmcblk0p12` here; TWRP uses `/dev/block/mmcblk0p12`. Do not take this as a replacement for a fresh mapping check.

The first own image had 13154304 bytes; bootstrap-2 with SFTP and the corrected USB ID has **13209600 bytes** and fits into the 16 MiB partition. Kernel SHA-256 unchanged. Private build/installation evidence under `local/dot2-tools/dot-linux-stage/` and `local/dot-usb/`.

## Connecting, operating and transferring

From the Windows project:

```powershell
devices\echo\hardware\dot2\tools\Invoke-Dot.ps1 -Command 'id'
ssh -F devices/echo/local/dot-ssh/config dot-dev
scp -F <project-dir>/devices/echo/local/dot-ssh/config ./my-program dot-dev:/data/echo-dot-dev/bin/
```

The private profile `local/dot-ssh/config` contains the target address, user root, the dedicated key, strict host checking and the alias **dot-dev**. Do not use the alias echo-dev or the profile of the large Echo. DHCP address, device serial number, key, SSID and PSK stay exclusively in the git-ignored local/. If DHCP changes the address, read `ip -4 addr show wlan0` over the **clearly identified Dot USB console** and update the private profile including the known-hosts address to the same known host key.

Important commands on the Dot:

```sh
id
cat /etc/echo-dot-dev-release
uname -srmo
cat /proc/uptime
cat /proc/sys/kernel/random/boot_id
ip -4 addr show wlan0
wpa_cli -p /run/wpa status
reboot
```

`reboot` uses BusyBox init and our shutdown path with sync and unmount/read-only remount of userdata. Do not replace a reboot by writing to misc at random or by repeating the Amonet exploit.

## Layout and autostart for the assistant

```text
/data/echo-dot-dev/
  bin/                         persistent own executables
  services/                    executable *.sh, started at boot and restarted after exit
  private/                     WiFi configuration, authorized_keys, own SSH host key
  firmware/                    the device's own MediaTek firmware
  artifacts/                   locally installed boot images
  recovery-twrp.img             checked complete TWRP partition from before the Linux replacement
  recovery-twrp.img.sha256      checksum for it
  rootfs/                      historical TWRP chroot test, not the active root file system
```

`/data/echo-dot-dev/services/assistant.sh` (source `hardware/dot2/tools/assistant.sh`) starts the assistant with `exec`. LF line endings and the execute permission are required. New services are read at a reboot; merely copying to services starts nothing yet (the deploy therefore reboots the Dot the first time). Services must wait for network readiness or handle connection errors themselves; the assistant starts before WiFi and time and catches up with both by itself.

An own static **Go 1.26.1 program for linux/arm, GOARM=7, CGO_ENABLED=0** was already copied over SFTP and run on the Dot. The output confirms arm and, in this sample, three CPUs visible online. This proves toolchain, program transfer and simple execution, not yet audio/assistant capability. Source: `hardware/dot2/tools/probe/main.go`; private finding: `local/dot-usb/development-validation.json`. In a runtime sample **MemAvailable 467580 kB** was present.

## The assistant on the Dot

State 2026-10-20. One binary for all Echos: `internal/board` detects the Dot by the privacy driver (`/sys/devices/soc/10010000.keypad/amz_privacy`) and selects the Dot drivers. No PulseAudio, no DSP, no third-party library.

| Area | Large Echo | Dot |
| --- | --- | --- |
| Audio | DSP chain, PulseAudio, `pacat` | `internal/alsa`: ALSA ioctls directly in Go (32-bit ARM structures), `internal/audio/dot.go` |
| Playback | PulseAudio mixes voice and music | own mixing loop, `hw:0,23`, 48 kHz stereo, 4 x 20 ms; runs continuously (silence in between, otherwise the amplifier clicks) |
| Microphone | DSP output `BMicsOut` (beamforming, AEC) | `hw:0,24`, 16 kHz, S24_3LE, 9 channels: channel 6 (centre), DC blocker, software gain `mic_gain_db` (default 6) |
| Echo | DSP echo cancellation | **none**: echo gate on (`echo_gate`), no barge-in while the assistant speaks |
| LED ring | 4 x LP5523 over I²C | IS31FL3236, sysfs `frame` (36 channels as hex) |
| Buttons | gpio-keys, rotary ring | Action button and volume +/- (each ±10 %) through evdev; mute through `privacy_state` (the hardware cuts the microphones itself) |
| Bluetooth | yes (phone, pairing) | **not yet**: The chip is there, the kernel has the MediaTek driver (`CONFIG_MTK_COMBO_BT=y`, `/dev/stpbt` = raw HCI as under Fire OS/Bluedroid), but no BlueZ (`CONFIG_BT` missing, no `/sys/class/bluetooth`). Usable only with an own userspace stack over `/dev/stpbt`. Until then the tools and the prompt paragraph `LIVE_ECHO_BLUETOOTH` are missing |

Mixer values at start (DAPM paths are otherwise unpowered): speaker path and speaker EQ as in EchoLocal (`paths_dot.go`, MIT), `HP Driver Gain Volume` 6 + `speaker_gain_db` (default 12, so level 18 = +12 dB; Fire OS: 6 = 0 dB, too quiet at full volume). Measurement 2026-10-21 with a test tone (-14 dBFS) and the device's own microphones: level 6/9/12/15/18 -> `mic_peak` 5561/7824/10952/15241/21227, so a steady 1 dB per level; music at full volume still sounded clean at 18. `Ext_Amp_Gain` (6-24 dB) cannot be written (EPERM, belongs to the MediaTek codec); microphones DIF1 routing, PGA 40 (+20 dB analog), **Digital Volume 24 = 0 dB** (range 0..64 = -12..+20 dB; the EchoMuse value 88 is outside this driver's range). Capture rejects 16 periods (EINVAL), 8 x 320 frames work.

Measured on 2026-10-20:

- Test tone -20 dBFS: microphones +35 dB above room level (about -68 dBFS), loopback 7/8 bit-exact the playback signal. Without an ear at the device, this allows checking whether the DAC plays: `ctl status` shows `loopback_peak` on the Dot (maximum since the last status); ready tone at volume 40 exactly 786.
- Self-test: if the ready tone does not arrive in the loopback, an entry in the error log and a yellow ring.
- Idle level `mic_peak` 40-60 at 6 dB, in conversation 90-130 (room). Raise `voice_peak` (120) in `device.json` for false triggers.
- The assistant needs about 3 % CPU when idle; 460 MB RAM remain free.
- Conversation through `ctl start` and `ctl inject` complete: acknowledgement, delegation to the backend, answer, farewell, hang-up, 0 underruns. Wake word (built-in model) detected at 0.57; the peers decided "answering here".
- With the wake word and the question directly following in one breath (TTS without a pause), GPT Live did not hear the question and answered only with a filler sentence: quick path, not Dot-specific (#73).
- Not yet tested with a real button press or voice in the room: action/mute/volume buttons, ring alignment (`led_offset`), speech recognition from a few metres.

Operation:

```sh
# from the PC (Git Bash): build, vet/test, install, restart the assistant
ECHO_HOST=dot-dev sh tools/deploy.sh [--secrets] [--librespot]
sh tools/deploy.sh --all           # all Echos including the Dot (local/devices/dot-dev.json)
# on the Dot (dropbear's PATH does not include /usr/local/bin)
/data/echo-dot-dev/assistant/assistant ctl status
tail -f /run/echo-dot-dev/assistant.sh.log
```

Layout: binary, librespot and `device.json` in `/data/echo-dot-dev/assistant`, secrets in `private/assistant-secrets.json`, state (`state_dir`) in **`/data/assistant`**, socket and runtime in `/run/assistant`. The Dot gets **no `settings.json`**: its Echo paths (`/mnt/data/assistant`) would be in RAM here; the built-in defaults are the Dot paths on the Dot. `assistant.sh` protects dropbear from the OOM killer, sets the assistant to `oom_score_adj` 500, slows crash loops (more than five starts in five minutes: five minutes' pause) and rotates the log (4 MiB). To disable: `touch /data/echo-dot-dev/assistant/disabled` and stop the assistant.

### WiFi loses broadcasts (measured 2026-10-20)

The router renews the WPA2 group key every 10 minutes (`Group rekeying completed` in `wpa.log`). Afterwards the Dot alternately receives all broadcasts and multicasts for ten minutes and **none** for ten minutes: a measurement run (age of the peer heartbeats every 2 s) showed the switch exactly at the renewal times. Presumably the MediaTek driver only takes over every second key index. Unicast (SSH, internet, mirrors) is not affected.

Consequences on the first evening: the Dot considered another device in the household failed and took over the lead role at 18:49, reported all others as missing, answered the wake word together with the M5, and the Spotify app did not find it (pairing via mDNS).

Countermeasures: `no_lead` in its `device.json` (never takes the lead role); the peer link additionally sends every datagram directly to all devices seen in the last 10 minutes (Echos and M5); librespot got the Spotify credentials (`/data/assistant/spotify/credentials.json`) from an Echo and registers with them through Spotify's servers, which needs no mDNS. The actual driver cause is open (#72); mDNS/SSDP services (YouTube DIAL search, pairing new Spotify accounts) therefore remain unreliable on the Dot.

Peer name, Spotify name and room have been set since 2026-10-20 (household values, not published here).

Open (#72): echo cancellation with the loopback as reference and beamforming of the seven microphones (then barge-in and wake word over music), trained wake word model of the second Echo, Tailscale, fix room and ring alignment.

## Services and diagnosis

`hardware/dot2/tools/init` mounts the virtual file systems and userdata, sets the next recovery boot and the USB console and starts BusyBox init. Its inittab starts the network and own services. `network.sh` starts the MediaTek helper wmtup, Dropbear, wpa_supplicant, the permanently renewing DHCP client and ntpd. SSH/supplicant/DHCP processes are restarted after an exit. A router restart or a longer WiFi outage is not yet a tested continuous-load case.

Runtime logs under **`/run/echo-dot-dev`**, in RAM and fresh after a reboot:

- `wmtup.log`: chip initialisation and firmware patches.
- `wpa.log`: WiFi login; contains private network identifiers, do not publish unchecked.
- `dhcp.log`: lease acquisition/renewal, also private network information.
- `ssh.log`, `ntp.log` and logs of own services.

The logs have no rotation yet. Continuous operation and reconnection under audio/CPU load still have to be checked. USB is the physical rescue access for WiFi problems.

## USB: two different modes

In TWRP, **ADB** works. Our own Linux start offers a **USB CDC root console** instead, VID_1D6B/PID_0104, Linux side `/dev/ttyGS0`. The current Windows COM number is private in `local/dot-usb/usb-console-validation.json`; never open COM7 or any other historical port blindly. Identify exactly this Dot by PnP ID and private serial number, do not occupy an M5/Echo UART port. One reader at a time, do not replay old command queues. A root answer over this USB console was actually tested.

The Google ID 18d1:4ee7 used at first led Windows to wrongly load Android Composite ADB Interface / WinUSB although Linux offered CDC. Switching to 1d6b:0104 fixed that; Windows automatically used Serial USB Device / usbser. The final image contains the corrected ID. The old large Echo UART at 115200 baud is no throughput yardstick for this USB bulk connection; the CDC baud value does not limit the USB bulk transport accordingly. USB is not generally safer than SSH. Both accesses have root rights here; SSH authenticates with keys, USB requires physical access.

## TWRP way back and boot image changes

TWRP does not run in parallel to our Linux. The complete backed-up recovery is on the PC and userdata. On the Dot, run **`echo-dot-to-twrp` only for a deliberate return**: the script checks SHA-256 and partition size, writes the own TWRP backup exclusively to recovery, checks the result, sets boot-recovery and reboots. Afterwards USB ADB/TWRP is active again instead of our Linux/SSH runtime. This way back is prepared in the script, but since the Linux replacement it has not yet been tested by an actual write-back.

Every first boot of a new development image has a file `boot-trial`. If it is not removed within 240 seconds after SSH is confirmed in the expected system, the start helper automatically tries the TWRP way back described. The first and the updated Linux start were successfully confirmed over SSH and the markers removed. In normal, accepted operation, a mere WiFi outage does not automatically write recovery back.

`hardware/dot2/tools/prepare.py` creates from checked inputs the own network rootfs test and the private WPA configuration. `build-image.py` builds the credential-free image from this base and the checked recovery kernel. For this, the locally archived, pinned upstream mkimage/mkrootfs helpers are used. `stage-network.py` was a **one-time TWRP test**, `install-boot.py` the first specifically approved recovery replacement; do not repeat these as normal start commands. `update-boot.py` checks the identity of the image installed so far and updates only the own recovery over SSH. The file names bootstrap-1/-2 mark this bootstrap stage; rework them before a later generalisation to upgrades. Keep the manifest/hash checks and the fallback path.

## Checks and evidence so far

- WPA2 association and DHCP first confirmed without a boot change in the TWRP chroot; root SSH with strictly checked host identity successful.
- Both own boot images fully verified by SHA-256 in the area used after writing. No changes to LK, preloader or GPT in this stage.
- Own Linux start and the subsequent updated Linux start with root SSH and new kernel confirmed. USB root answer in the updated Linux confirmed.
- 1 MiB of random data there and back over modern scp/SFTP, SHA-256 on PC and Dot matching: **1.422 s upload / 0.913 s download**, each including SSH/SFTP setup. Single measurement, not a maximum throughput and not yet a USB speed comparison.
- Own ARMv7 Go program transferred and run. No paid API call, no access to the large Echo.
- **Two further automatic reboots passed:** root SSH back after **30.74 s / 31.62 s**, each with a new kernel boot identity, an unchanged strictly checked SSH host key, no remaining trial marker. Private evidence: `local/dot-usb/ssh-reboot-validation.json`. Powerless cold start and router outage still separately untested.

## Sources and reproducible inputs

Linux boot/MediaTek reference: [TECHO5 Dot](https://github.com/HuskerMinion/techo5-dot), commit **33b402480ed946dda98bd615a2dfcdeeb4e7a167** archived locally for review, MIT. From it the mkimage/mkrootfs helpers and the wmtup compiled by ourselves for ARMv7; no third-party assistant adopted. Network APKs and static BusyBox from the signature-checked release **v1.0.24**; release manifest and file hashes in `local/dot2-tools/dot-linux-stage/inputs-manifest.json`. Alpine minirootfs **3.24.1 armv7**, SHA-256 **50942d567e6ee422c16cb46d5c282ed9d8adc9007c2a483faf4148a18c64ce32**. Additional SFTP server from the official Alpine repository, signature check with its official keys. Do not take any Amazon firmware files into Git.

Further agents should read this file first, then the unlock history [20-echo-dot-gen2-usb.md](20-echo-dot-gen2-usb.md) and the private current test reports. For Dot Linux development, do not assume TWRP or the normal Fire OS is currently running.

# Network, SSH and development access

Dated notes: [history/13-development-access-plan.md](history/13-development-access-plan.md), [14-network-ssh](history/14-network-ssh.md), [12-storage-and-installation](history/12-storage-and-installation.md).

Goal and state: an Echo with our own software that connects to WiFi after power-on, is reachable over SSH and starts without the SD card and the external 3.3 V supply. A development agent changes files and runs tests through SSH. **Implemented and tested** on two units (2026-10-16 and 2026-10-18). Installation procedure: [boot-and-root.md](boot-and-root.md); fallback material: [recovery.md](recovery.md).

## Environment found (read-only inventory, 2026-10-15)

| Property | Observed |
| --- | --- |
| CPU | `armv7l`, 32-bit Cortex-A8, no ARM64 |
| RAM | Physical 256 MiB; Linux `MemTotal: 194556 kB`, about 185,876 kB free in the simple minimal start |
| Kernel | Linux 2.6.37, original build of 2022-01-19, GCC 4.8.2 |
| C runtime | EGLIBC 2.19 |
| WiFi tools | `/usr/bin/wpa_supplicant`, `wpa_cli` v2.0-devel-590 (no proof of WPA3) |
| DHCP | `/sbin/udhcpc` (BusyBox 1.19.4) |
| TLS | `libssl.so.1.0.0` found; do not derive modern TLS/CLI compatibility from it |
| SSH server | none found in the standard paths (no full search done); OpenSSH 7.5p1 taken from the EchoRoot image |

The slow console link is UART at 115200 baud behind a USB adapter (about 11.5 kB/s at 8N1); USB is not the bottleneck.

## WiFi (RAM-proven, then persistent)

- Modules `compat`, `cfg80211`, `ath6kl_sdio` loaded with the original parameters, the German regulatory-domain value `0x8114`, `bdata.bin` and the MAC taken dynamically from `/proc/mac`. Board query `is_Pancake` returned 1 (false). Interfaces `wlan0`/`p2p0`, firmware AR6004 hw3.0 / 3.5.0.9999. The driver also reports WMI/scatter warnings; their practical meaning is open.
- WPA2-PSK/CCMP: `wpa_state=COMPLETED`. DHCP, default route and ping from the Echo to the PC work. `/etc/resolv.conf` points to `/var/run/resolv.conf` in tmpfs. Do not assume the interface name; the supplicant configuration lives in tmpfs (RAM phase) or in `private/` (persistent).
- The old supplicant is no proof of WPA3: check radio band, router mode, WPA2/WPA3 and driver/firmware combination; if the router mode does not fit use a test SSID rather than changing the router configuration. Do not overwrite router configuration or existing credentials unasked; no internet port forwarding is part of the setup.
- The WiFi file was prepared privately (SSID hex-encoded, PSK derived) and transferred over UART without terminal echo, target hash checked. The derived PSK is password-equivalent and stays private. Do not read out or publish old Amazon credentials.
- With WiFi power save an idle TCP timeout once appeared between logins; after `iw dev wlan0 set power_save off` and two pings SSH worked. Power saving as the cause is not isolated; auto-reconnect was tested later (below).
- The kernel supports **no IPv6**; DNS returns AAAA records first for OpenAI, so `ntpdate` fails and Go programs must force IPv4.
- No RTC: the device starts in 1970. The base install has no time sync (`ntpd`/`ntpdate` exist); the assistant sets the clock itself via SNTP once DNS works (about 30 s after boot). Check the time before any TLS use.
- When roaming between two mesh access points the default route was lost while the DHCP lease still applied. The service loop now restores the route from the gateway address stored by the DHCP script.
- Bluetooth and WiFi share one Atheros chip, and `wlan0` is the only way into the device. Bluetooth bring-up is documented in [../../../docs/17-bluetooth.md](../../../docs/17-bluetooth.md) and `tools/bt-try.sh` (nothing enabled by default).

## SSH

- OpenSSH 7.5p1 from the EchoRoot image, running with the existing Amazon libraries; **an old bootstrap server, not a maintained one**; a replacement/update remains sensible. SFTP server works. sshd SHA-256 `4a26332ba9814fd8959f81c6bf8b1529a14f4ed6c52c6252adaa7406bdd9e405` (657,756 bytes). Separately extracted libraries are not installed system-wide and not needed.
- Ed25519 host and developer keys, strict known host identity (pinned once, never disable host-key checking, never accept unknown host identities blindly), no password/ChallengeResponse login. The missing privilege-separation user `sshd` is added with UID 65530 (RAM passwd in the RAM phase, `passwd`/`group` in the persistent install); `/var/empty` exists. This build supports neither `UsePAM` nor `PrintLastLog`: those options are removed.
- **StrictModes:** the original `/usr/local` had mode 0775, so the first SSH login failed. `/usr/local` must be 0755 (the installer sets it). This takes no write rights from root; key checking stays enabled.
- Full administrative root access over SSH, no forced commands or chroot. The host identity stays the same across restarts.
- Windows keys must be created under the actual Windows user: a key created by a sandbox account was unreadable for that user's normal OpenSSH. Keys, host key, `authorized_keys` and `local/ssh/config` (alias `echo-dev`; `echo2` for the second Echo) are git-ignored; no global SSH configuration is changed.
- Windows helper: `tools/Invoke-Echo.ps1 -Command 'id'` (`-EchoHost echo2` for the second unit); SCP/SFTP with the same `-F` configuration, first experiment only in `/mnt/echo-net`. For a second device use `ECHO_RUNTIME` (e.g. `local/runtime-echo2`) with `echo_console.py`.
- Verified: `uid=0`, Linux 2.6.37 armv7l; `src/ssh_probe.py` copied via SFTP, hash matched, run with Python over SSH (`ECHO_REMOTE_DEVELOPMENT_OK`, exit 0). 2 MiB random data PC to Echo 0.894 s including connection setup (about 2.24 MiB/s), back 1.586 s (about 1.26 MiB/s), SHA-256 matched: a successful round trip, not a maximum-throughput proof.
- A reader bug was fixed on the way: the old archive reader skipped null pointers in indirect block tables, which shifted and shortened extracted ELF files (sshd was 655,360 instead of 657,756 bytes). `tools/read_echo_image.py` (`ECHO_IMAGE` environment variable, partition offset 32769*512) preserves sparse blocks and checks the length. The first SSH crash was no proven library conflict. Public binaries were transferred briefly through a LAN HTTP server restricted to one archive file, hash-checked on the target, server ended afterwards; credentials were never offered over HTTP.

## Daily use

1. SD stays out, the external 3.3 V supply is disconnected; power on with the matching original Echo supply only. UART is an optional rescue access and does not power the Echo.
2. Wait about 40 seconds. Connect with `ssh -F <local>/ssh/config echo-dev` or `tools/Invoke-Echo.ps1 -Command 'id'`.
3. Runtime logs in RAM (new after each restart; no rotation yet): `/mnt/echo-net/services.log`, `wpa.log`, `dhcp.log`, `sshd.log`. Persistent files under `/usr/local/echo-dev`; private WiFi/SSH files in `private/` (directory 0700, files 0600). Secrets and household data stay in private git-ignored files, never in the repository.
4. Changes to the read-only root: `mount -o remount,rw /`, edit, `sync`, `mount -o remount,ro /`.
5. Controlled restart: `/usr/local/echo-dev/bin/echo-reboot`.
6. If the router hands out another DHCP address, check it over UART or the router, update the private SSH profile and the known-host entry. The address is not fixed by mDNS or a router reservation yet.

The normal start uses the original internal bootloader and kernel with our own init instead of the Alexa start services. In the pure network install the LED ring and audio are no start indicator; audio, LEDs and the assistant are started by the assistant's own service layer ([audio.md](audio.md)). A normal software restart with the SSH client left open stays open until the client timeout; the serial log proves the restart.

## Acceptance results

| Test | Result |
| --- | --- |
| WiFi in RAM | Connected; local data transport possible; root session answered after more than 1997 s |
| Internal start without SD / 3.3 V (first unit) | One physical start confirmed; SSH after StrictModes fix, uptime 69.64 s |
| Software restarts via `echo-reboot` (first unit) | 3 passed, SSH back after 36.60 / 36.46 / 36.44 s, different kernel boot identity each time, WPA COMPLETED, same Ed25519 host key |
| WiFi link down/up (`wlan0` down 3 s) | SSH back automatically after 14.26 s without `wpa_cli reconnect` (local link interruption only) |
| Final sample | UID 0, MemTotal 194556 kB, MemFree 173748 kB, root ext3 read-only; with the serial reader closed `ROOT_SSH_WITHOUT_SERIAL_READER_OK`, uptime 221.35 s |
| Second unit (2026-10-18) | Whole procedure in one session without rescue need; SSH about 50 s after switch-on; software restart: SSH back after 43 s (kernel uptime 36.8 s), root ext3 read-only |
| SD-free start | One physical start per unit; ten cold starts **not** done |
| Real router outage, long mixed network + audio load (30 min), AP failure | **Not** done |

Further units: set up as the second Echo above (own `authorized_keys`, own Ed25519 host key, same scripts; original init hash identical, same firmware state). Audio, the assistant, Tailscale and Bluetooth are installed per unit; the service loop skips what is missing.

## Remaining limits

- Bootstrap OpenSSH 7.5p1 on Linux 2.6.37; a current ARMv7-compatible maintenance base is separate work (no Debian was installed).
- No time sync in the base install (see above); the assistant does SNTP.
- RAM logs have no rotation; longer runs and supervisor behaviour on repeated service crashes are pending.
- Reconnection was tested by link down/up, not with a real AP failure or router restart.

## Development path and tool compatibility

The PC (or a development computer) runs Codex/Claude Code, project and Git; the agent uses SSH for commands, transfer and feedback. On the Echo only SSH and the assistant service start automatically, no interactive coding CLI. Hardware/working rules are in the repository's `CLAUDE.md`.

- **Claude Code:** the official setup page (read 2026-10-16) names 4 GB+ RAM and x64/ARM64; a 256 MiB ARMv7 Echo does not meet it. A new OS does not change architecture or RAM.
- **Codex CLI:** the CLI documentation gives no minimum RAM figure or ARMv7 commitment (none was invented). The published package `@openai/codex` 0.163.0 was inspected read-only: Linux dependencies only `linux-x64` and `linux-arm64`; the launcher selects `x86_64-unknown-linux-musl` or `aarch64-unknown-linux-musl` and ends with `Unsupported platform` otherwise (launcher lines 27-71 and the manifest at `https://registry.npmjs.org/@openai/codex/latest` are the evidence). Nothing from the package was installed or run. Compiling/porting it yourself would be a separate experiment with unknown success and no prerequisite for development access.
- A CLI installed on a device is not a locally running AI model: voice-assistant requirements and development-tool requirements are separate questions; whether speech processing can be entirely local is a separate measurement question.
- A newer distribution/kernel is a separate compatibility decision: audio/WiFi modules and DSP files do not automatically work with another kernel or libc. That risk was not combined with network, SSH and assistant logic.
- Dropbear ([project](https://matt.ucc.asn.au/dropbear/dropbear.html)) was considered as an alternative SSH server (ARMv7/kernel 2.6.37/EGLIBC 2.19 compatibility never tested; do not copy arbitrary ARM64 binaries). [wpa_supplicant](https://w1.fi/wpa_supplicant/): do not transfer current project features unchecked to the installed old version.
- Original plan targets, kept as test goals rather than results: at least 30 minutes of uptime under network/audio load; throughput at least 200 KiB/s (measured: above); ten cold starts with SSH reachable within 90 s of switch-on, measured from power-on; router unavailable at start and connected later without physical intervention; no deliberate abrupt power interruptions during flash writes; only after that remove the superfluous SD lines and adapter (insulate contacts, repeat a cold start), keeping the SD image and UART rescue path.

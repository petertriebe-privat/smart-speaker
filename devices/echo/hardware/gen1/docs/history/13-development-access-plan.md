# Plan: permanent access, WiFi, SSH and internal start

State **2026-10-16**. Goal: An Echo with own software that connects to WiFi by itself after power-on, is reachable over SSH and later starts without SD card/external SD supply. A development agent should be able to change files and run tests through it.

## Authorisation and scope

The user explicitly approved replacing the internal software completely with their own and wants permanent development access. This new approval replaces the earlier mere exploration of a possible internal installation. In later implementation within this goal, do not ask again generally whether internal software may be replaced.

The current task is **implementation of persistent WiFi and SSH**. The user approved a complete internal software replacement and does not need the original system. A full original backup is not a prerequisite. Before concrete changes only the necessary device-specific boot/configuration/calibration areas and the working hardware support are backed up. Check exact write targets and the SD/UART rescue path; do not start the historical installer blindly.

Keep WiFi/SSH keys and household data only in private, git-ignored files. Do not overwrite router configuration or existing credentials unasked. Network connection and local SSH use belong to the desired development path; internet port forwarding is not part of the plan.

## Basis already checked

Reading inventory in the restored root system, raw log `local/runtime/boot-2026-10-15-234834-881.log`, markers `PLAN_AUDIT` / `PLAN_AUDIT_MORE`:

| Property | Actually observed |
| --- | --- |
| CPU | `armv7l`, 32-bit ARM Cortex-A8, no ARM64 |
| RAM | Physical 256 MiB; Linux `MemTotal: 194556 kB`, in the simple minimal start about 185876 kB free |
| Kernel | Linux 2.6.37; original build of 2022-01-19, GCC 4.8.2 |
| C runtime | `ldd (EGLIBC) 2.19` |
| Python | Proven earlier: 2.7.2; `wave` and `ctypes` missing |
| WiFi tools | `/usr/bin/wpa_supplicant`, `/usr/bin/wpa_cli` present; version `v2.0-devel-590` |
| DHCP | `/sbin/udhcpc`, BusyBox 1.19.4 present |
| WiFi interface | Currently only `lo` in sysfs; WiFi modules not yet loaded in the minimal start |
| SSH | No server found in the checked standard paths; no general full search done, so do not claim "nowhere present" |
| TLS library | `libssl.so.1.0.0` found; do not derive modern TLS/CLI compatibility from it |
| Stability | Original replacement power supply of the same series, watchdog keep-alive; last 914.14 seconds uptime at this inventory |

The slow connection is **UART at 115200 baud behind a USB adapter**. Theoretical payload upper limit at 8N1 about 11.5 kB/s before further overhead. USB itself is not the bottleneck here. WiFi throughput must be measured with a file transfer.

## Answers to the four ideas

### 1. Complete software replacement and permanent access

Fundamentally plausible and now authorised. Permanent access needs an **internal start path**, a controlled init system and persistently stored configuration/keys. An open SD bootloader plus RAM SSH alone would be gone again after power-off.

The first goal is a small own system or controlled start with the already proven kernel/DSP components. A newer distribution/newer kernel is a compatibility decision of its own: audio/WiFi modules and DSP files do not automatically work with another kernel or an arbitrary current libc. This risk should not be introduced at the same time as network, SSH and assistant logic.

### 2. Automatic WiFi and SSH

A good basis for faster transfers, backups and development. The WiFi/DHCP programs needed already exist. First connect in RAM, start SSH and measure. Only after a successful backup make the start and the credentials internally persistent.

The installed old supplicant version is **no proof of WPA3 support**. Check radio band, router mode, WPA2/WPA3 and driver-firmware combination concretely. If the router mode does not fit, check a suitable test SSID rather than silently changing the entire router configuration.

### 3. Codex CLI or Claude Code directly on the Echo

**Claude Code:** The official setup page read on 2026-10-16 names 4 GB+ RAM and x64/ARM64. Our ARMv7 Echo with 256 MiB does not meet this. An automatic start is therefore not a realistic building block for this hardware. A new operating system does not change CPU architecture and RAM capacity.

**Codex CLI:** The official CLI documentation was read; in the version read it gives no concrete minimum RAM figure or ARMv7 commitment. Therefore the published official package **`@openai/codex` 0.163.0** was additionally checked by reading: Linux dependencies only `linux-x64` and `linux-arm64`. The package launcher selects `x86_64-unknown-linux-musl` or `aarch64-unknown-linux-musl` accordingly; for other architectures it ends with `Unsupported platform`. An ARMv7 Echo does not fit. The npm wrapper does not turn the native program into a portable Python/JavaScript agent. Nothing from the downloaded package was installed or run.

This is a concrete architecture blocker of the checked official distribution, not just a RAM estimate. An official Codex minimum RAM figure was not found and is not invented. Compiling/porting it yourself would be a separate experiment with unknown success; it is no prerequisite for development access.

**Proposed development path:** Codex/Claude on the PC or a suitable development computer; project, build tools and Git there. The agent uses SSH for commands, file transfer and feedback on the Echo. On the Echo, SSH and the own assistant service start automatically, no interactive coding CLI. The development station then does not have to be part of later assistant operation. Whether its speech processing can be entirely local remains a separate measurement/function question.

A CLI installed on a device is also not the same as an AI model running locally there. Consider requirements of the voice assistant and requirements of development tools separately.

### 4. Remove SD and 3.3 V

Yes, as a goal after a successful **internal** boot/autostart test. SSH reachability alone does not yet replace the SD bootloader. Only when internal bootloader, kernel, root system, WiFi, SSH and configuration start without SD can its hardware go. The separate 3.3 V source powers our SD setup; it is not an intended part of the final internally starting Echo.

## Test and implementation order

### Phase A: stable base and WiFi in RAM

1. Keep using the replacement power supply, keep serial rescue access and clean root start; service the watchdog early. At least 30 minutes of uptime under network/later audio load as a goal; do not equate the short proofs so far.
2. Read the original WiFi start definition and firmware paths. Load `compat`, `cfg80211`, `ath6kl_sdio` in a targeted way with parameters matching the original kernel. Do not start the whole Alexa/Upstart environment just to get WiFi.
3. Check interface, radio/encryption capabilities and driver errors. Have SSID and WiFi secret provided privately only for this step; do not wholesale read out or publish old Amazon credentials.
4. Supplicant configuration in the confirmed tmpfs, DHCP through the existing `udhcpc`; do not assume the interface name. Test address, route, local reachability and DNS. Check system time/NTP before later TLS, since the start so far reported no usable RTC.

**Passed:** Router connection established, local data transport possible; disconnect/connect three times without manual driver restart. On router/AP failure independent reconnection after its return. Claim no internal persistence yet.

### Phase B: temporary SSH access and real development

1. Check existing servers more closely. If none is usable, provide a small compatible SSH server, such as a suitably built Dropbear. The project is meant for embedded devices; compatibility with ARMv7, kernel 2.6.37 and EGLIBC 2.19 must be tested in practice. Do not copy an arbitrary current ARM64 binary.
2. First host key and configuration in RAM. Set up key access from the development computer and note the host key fingerprint. Check SSH/SFTP/SCP compatibility; the availability of an SSH server does not guarantee an SFTP server.
3. Copy a small own test program over SSH, compare the target hash with the host, start it, read back output and exit code. This proves actual agent development over this path.
4. Measure throughput in both directions with a sufficiently large, uncritical file and compare SHA-256; log time, CPU and RAM. As a first target value e.g. at least 200 KiB/s; this is a test goal, not a forecast. With slower values measure the cause and do not blindly estimate backup time.

**Passed:** Several SSH logins, file transfer with matching hashes, executed test and re-login after WiFi interruption. No permanently active PC agent on the Echo required.

### Phase C: back up the necessary device-specific base in a targeted way

The user wants no full original backup. Actually backed up: small boot/recovery/IDME partitions p1 to p4 with source/copy SHA-256, GPT beginning/end, original init and start/hardware files. Private storage under local/recovery/. No full main-A/main-B/data copy presumed. Exact internal write targets and the SD/UART rescue path were checked before implementation.

### Phase D: own internal start and services

1. Define a small internal software base: start/service control and assistant logic ourselves, initially adopt existing working audio/WiFi support. Determine internal storage allocation/free space.
2. Build an own init/supervisor sequence: RAM directories -> watchdog -> necessary drivers -> WiFi -> DHCP/time -> SSH; add audio initialisation and the assistant process in a targeted way. Network errors must not block maintenance start or watchdog.
3. Store WiFi configuration, SSH host keys and allowed developer keys internally and persistently. The SSH server identity must stay the same after restart. No private keys or WiFi passwords in the public repository.
4. Install the chosen internal start path deliberately. Do not adopt the historical EchoRoot installer unchanged: its assumed SD/eMMC mapping does not apply in our fault states. Every write area must match the backup/device profile beforehand.

**Passed:** Three provisional restarts without UART intervention; WiFi and SSH automatically, independent restart after router interruption, controlled watchdog/audio state. Prove internal start and other services through logs.

### Phase E: accept without SD and external 3.3 V

1. With power off remove SD and disconnect the separate 3.3 V supply. Keep the existing maintenance UART for observation at first.
2. Ten cold starts only with a suitable original power supply. Goal: reachable over SSH after 90 seconds at the latest; measured from switch-on, no claimed already achieved timing.
3. Keep the router unavailable once at start, connect it later: connection/SSH must return without physical intervention. Then at least 30 minutes of network and reasonable audio load; no deliberate abrupt power interruptions during flash writes.
4. Only then remove superfluous SD lines/adapters with power off, insulate contacts and repeat a cold start. Keep SD image and UART rescue path.

**Passed:** Start without SD/external 3.3 V and without manual UART commands, stable SSH identity and repeatable audio function. Only then is the cable removal technically proven.

## Compact acceptance matrix

| Test | Success criterion | State |
| --- | --- | --- |
| CPU/kernel/libc and tool audit | Architecture/versions and existing programs known | Done by reading |
| CLI compatibility | Official matching platform proven | Claude: requirements not met; checked Codex distribution: ARMv7 not supported |
| WiFi | Driver, authentication, DHCP, reconnection | Confirmed; link down/up recovered automatically, real AP failure open |
| SSH + agent work | Login, hash transfer, test program/exit code from the PC | Confirmed, full root access |
| Targeted fallback backup | Small boot/configuration areas, init/hardware base | Verified; full original backup not required |
| Internal persistence | WiFi/SSH after restart without manual steps | Three further restarts passed |
| SD-free start | Physical start without SD and external 3.3 V | One physical start confirmed; ten cold starts not done |

## Keep recording findings

Each phase gets date, image/tool versions used, commands, expected and observed result, measurements and next open question. Raw data/identities under `local/`, sanitised results in `docs/`. The new approval and this plan apply across agent changes; do not mark still open tests as done. Also keep ongoing implementation tasks in the issue system per project convention.

## Sources and check material

- [Claude Code: system requirements](https://code.claude.com/docs/en/setup), read on 2026-10-16: 4 GB+ and x64/ARM64, supported OS base; no ARMv7 proof.
- [Official Codex CLI documentation](https://learn.chatgpt.com/docs/codex/cli), read on 2026-10-16. The general installation page read there gives no concrete minimum RAM figure for this device.
- Official published npm package `@openai/codex` 0.163.0: manifest from `https://registry.npmjs.org/@openai/codex/latest`, archived package and launcher read purely as text under `local/research/2026-10-16/`. Linux architecture selection in the launcher, lines 27-71, and platform dependencies in the manifest are the evidence for the concrete ARMv7 incompatibility of this distribution. No package code was run.
- [Dropbear project](https://matt.ucc.asn.au/dropbear/dropbear.html): small SSH server for embedded systems; matching build compatibility remains to be tested.
- [wpa_supplicant project](https://w1.fi/wpa_supplicant/): WiFi authentication and background operation. Do not transfer current project features unchecked to the installed old version.

## Implemented state

WiFi/DHCP/SSH/SFTP and own code confirmed. Internal main-B installation done. One physical start and three additional software restarts without SD/external 3.3 V passed; SSH after about 36 seconds. Automatic reconnection after local link interruption passed. Full SSH root access. Details and remaining long-term tests in the [implementation result](14-network-ssh.md). The original larger test goals above are not measurements already carried out.

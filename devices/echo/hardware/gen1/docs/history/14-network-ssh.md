# WiFi and SSH: implementation result

State 2026-10-16. Task: automatic WiFi and SSH across restarts, own software on internal storage. Full software replacement is approved. At the user's request a complete original backup is not a prerequisite; targeted backups of the necessary device-specific base remain part of the implementation.

## Proven in RAM

- Replacement power supply and watchdog: root session answers after more than 1997 seconds. Changing the PC reader does not end the Echo shell.
- /mnt set up as a 32 MiB tmpfs, volatile paths and devpts. Root still read-only.
- WiFi modules compat, cfg80211 and ath6kl_sdio loaded successfully. Original parameters with the German regulatory domain value 0x8114, bdata.bin and MAC dynamically from /proc/mac. Board query is_Pancake returned 1 (false).
- wlan0/p2p0 present, firmware AR6004 hw3.0 / 3.5.0.9999. The driver also reports WMI/scatter warnings; their practical meaning is still open.
- WPA2 login confirmed: wpa_state=COMPLETED, WPA2-PSK, CCMP. DHCP, default route and ping from Echo to PC work. /etc/resolv.conf points to /var/run/resolv.conf in tmpfs.
- User WiFi file read privately, SSID hex-encoded and PSK derived. Transferred over UART without terminal echo, target hash matches. The PSK is equivalent to a password and stays private.
- Python has no zlib module. hardware/gen1/tools/echo_console.py uses host gzip, target gunzip and Python binascii/hashlib, own ACK markers and SHA-256. No secrets are output.

## SSH and own software over WiFi confirmed

OpenSSH 7.5p1 from the existing EchoRoot image runs with the existing Amazon libraries. This is an old bootstrap server, with no claim of a current maintained server. Further replacement/update remains sensible. Internal SFTP server works.

Ed25519 host and developer keys, strict known host identity, no password/ChallengeResponse login. The missing privilege separation user sshd was added with free UID 65530 in a RAM passwd. /var/empty also in RAM. This build supports neither UsePAM nor PrintLastLog; options removed.

Windows keys must be created under the actual Windows user. A private key created by the Codex sandbox account was not readable by the normal OpenSSH of that user. Corrected project-specific developer key, host key and configuration under git-ignored local/ssh. No global SSH configuration changed.

SSH returned uid=0 and Linux 2.6.37 armv7l. hardware/gen1/src/ssh_probe.py copied via SFTP, target hash matches, run with Python over SSH: ECHO_REMOTE_DEVELOPMENT_OK, exit 0.

2 MiB random data PC to Echo: 0.894 s including connection setup (about 2.24 MiB/s). Back: 1.586 s (about 1.26 MiB/s). SHA-256 of both PC files matches; this is a successful round trip, not a proof of maximum WiFi throughput.

A TCP timeout occurred between logins. After iw dev wlan0 set power_save off and two ping packets from Echo to PC, SSH worked. Power saving as the cause is not yet isolated. Auto-reconnect remains to be tested.

## Error in the previous image reader corrected

The old archive reader skipped null pointers in indirect block tables. Unallocated null ranges were therefore missing from the content, ELF files were shifted and shortened. sshd: wrong 655360, correct 657756 bytes; its ELF section headers were previously behind the end of the extracted file. The corrected hardware/gen1/tools/read_echo_image.py preserves all holes and checks the length. The first SSH crash was not a proven library conflict.

Correct sshd SHA-256: 4a26332ba9814fd8959f81c6bf8b1529a14f4ed6c52c6252adaa7406bdd9e405. Separately extracted libraries are not installed system-wide and not needed for this running SSH test. Public binary files were transferred briefly through a LAN HTTP server restricted to one archive file, target hash checked, server ended afterwards. Credentials were never offered over HTTP.

## Current storage mapping re-checked

In this session **SD is mmcblk0**, internal eMMC **mmcblk1**. Types SD/MMC and sizes checked; root has dev_t 179:23 and is eMMC partition 7. Internal /proc/bootmode reports main-B. Earlier sessions had a different mapping on SD timeout; never adopt their block names blindly.

Internal p3 is VFAT with u-boot.bin (156532 bytes), only viewed read-only. Root has roughly 636 MiB available space. Before persistent changes, small boot/device configuration areas are backed up in a targeted way; no complete backup presumed.

## Not yet confirmed

Internal autostart and operation without SD, reconnection after WiFi failure and restart tests. Raw data, addresses, device identifiers and secrets exclusively under git-ignored local/. No internal flash changes yet at the state of this entry.

## Internal installation done, restart still to be checked

Targeted backup of eMMC p1/p2/p3/p4 (896 KiB + three times 16 MiB), each decompressed copy compared with the source SHA-256. GPT beginning/end read separately; init, boot/start configuration as well as kernel modules/firmware as a private archive. Original-init hash confirmed with the archived file. No complete root/data backup created and none required.

Tools/scripts normalised to Unix LF, hashes and sh -n checked on the Echo. Windows CRLF had previously caused a syntax error; corrected before the flash write.

Installer hardware/gen1/tools/install-persistent-access.sh checks current root dev_t, eMMC type/size, bootmode main-B and original-init hash. Then only the root filesystem on main-B was briefly remounted ext2-rw. Under /usr/local/echo-dev own init/service scripts, SSH server, private WiFi/key files and small fallback copies were placed. sshd privilege separation user added in passwd/group and /var/empty created. /sbin/init switched atomically to ../usr/local/echo-dev/init.sh. Immediately sync and root read-only again.

No write accesses to bootloader, kernel, GPT or IDME. Postcheck: sbin/init link correct, persistent sshd configuration valid, sshd binary hash matches. Root superblock again state=1 and incompat=2 (no RECOVER). The own init starts watchdog, RAM paths, serial rescue shell access, WiFi/supplicant/DHCP and SSH with simple restart loops. The normal Alexa services are not started by it; audio is a service to be integrated separately later.

**Do not yet count as restart success:** Files are installed internally, the previous manual RAM session is still running. Only a fresh kernel start can confirm the new PID 1 start and the network services. Afterwards test the internal boot without SD separately.

## First restart attempt and necessary physical test

Controlled restart triggered through /usr/local/echo-dev/bin/echo-reboot. The SSH connection stayed open until the client timeout; the serial log proves the restart. Because the SD was still inserted, its default kernel started again with root=/dev/mmcblk0p2 and ended in the known SD I/O/root mount panic. No evidence of a fault in the newly installed init: it was not called in the process. Repeated CR did not bring this already running panic back into U-Boot; another automatic reset was not yet observed in this attempt.

The user was asked to switch off both supplies, remove the microSD and disconnect the external 3.3 V supply from the setup. UART stays connected with VCC free. After that switch on only the matching original Echo power supply. This real internal start is the next outstanding proof.

## Development access from the PC

Private connection: local/ssh/config, alias echo-dev. Windows OpenSSH uses a predefined Ed25519 host identity and the project-specific private developer key. hardware/gen1/tools/Invoke-Echo.ps1 -Command 'id' runs a command. SCP/SFTP with the same -F configuration, at first experiment only in /mnt/echo-net. On this computer the private files are in the requested project folder; no global SSH configuration or global host list was changed.

Claude Code and Codex run on the PC and use this SSH path. CLAUDE.md describes the hardware/working rules. Neither modern agent CLIs nor a large language assistant are installed on the Echo. The next agent must check the actual boot state and not just assume from the presence of the files.

## First internal start without SD confirmed

The user disconnected both supplies, removed the SD, disconnected the external 3.3 V and switched on only the original Echo power supply. UART shows the original internal U-Boot 2010.06 of 2022-01-19 and then echo-dev init active. No bootloader commands needed.

WiFi/supplicant/DHCP and SSH were started automatically. The first SSH authentication failed because of StrictModes: the original /usr/local had mode 0775. Via UART briefly root-rw in a targeted way, /usr/local to 0755, sync, root-ro. Key checking stays enabled. Installer corrected accordingly.

SSH then successful: uid=0; PID 1 is /bin/sh /sbin/init --default-console none; root ext3 read-only; WPA2 COMPLETED; uptime 69.64 seconds. The block list contains only the internal eMMC as mmcblk0 and its eight partitions, no SD. The internal bootloader, original kernel and our persistent init path thus work together for the first time without SD/external supply. Further restart/reconnection tests are running.

## Acceptance: automatic restarts and WiFi reconnection

Three further software restarts through the own echo-reboot helper, exclusively with internal boot and without UART intervention: SSH back after 36.60 / 36.46 / 36.44 seconds. Each new kernel had a different boot identity; WPA COMPLETED each time, the same known Ed25519 host key. Private measurements under local/network/reboot-validation.json.

Then wlan0 down for three seconds and up again. Without explicit wpa_cli reconnect/reassociate SSH came back automatically after 14.26 seconds; same kernel boot identity. This tests a local link interruption, not a real router restart or longer AP failure. These remain separate long-term tests. Claim no proof of ten physical cold starts or 30 minutes of combined network/audio load.

Final sample after this test: UID 0, MemTotal 194556 kB, MemFree 173748 kB. Root stays ext3 read-only. **Full administrative root access over SSH**, no forced commands or user chroot. Mode 0755 on /usr/local does not take any write rights from root; it prevents third-party changes of the key path and satisfies OpenSSH StrictModes. For development root can run mount -o remount,rw /, change files, sync and use mount -o remount,ro /. SSH key checking was not disabled.

## Normal start and daily use

1. SD stays out; external 3.3 V supply is disconnected. Switch on the matching original Echo power supply.
2. Wait about 40 seconds. Use the SSH configuration under local/ssh/config, alias echo-dev, e.g. hardware/gen1/tools/Invoke-Echo.ps1 -Command 'id'.
3. Runtime logs: /mnt/echo-net/services.log, wpa.log, dhcp.log and sshd.log. These are new after restart because of tmpfs. Own persistent files are under /usr/local/echo-dev; private WiFi/SSH files there in private/ with mode 0600 and directory 0700.
4. Controlled restart: /usr/local/echo-dev/bin/echo-reboot. The helper syncs and remounts root read-only before the forced kernel restart. Ordinary Upstart/init signalling does not fit our small PID 1 supervisor.
5. For network problems use UART as rescue access. The own init provides a physical root shell there. Keep the old SD with open U-Boot as additional rescue, do not insert it as a normal boot medium.

The normal start uses the original internal bootloader and kernel, but our own init instead of the Alexa start services. LED ring and audio are no start indicator in this minimal start and not yet automatically integrated services. The earlier successful speaker test remains reproducible in the audio runbook.

## Remaining technical limits

- Bootstrap SSH is OpenSSH 7.5p1 on Linux 2.6.37; develop a current ARMv7-compatible maintenance base separately later. No Debian was installed here.
- No automatic time sync set up. ntpd/ntpdate exist; the target system starts without a usable RTC and initially shows an old time. Host-side time measurements and kernel uptime were used for this acceptance.
- The DHCP address is stored in the private SSH profile. If the router assigns a different one, check the address over UART or router and update the private configuration plus the previously known host key entry; never accept unknown host identities blindly. mDNS/router reservation not yet set up.
- RAM logs have no rotation yet. Longer continuous runs and supervisor behaviour on repeated service crashes are pending.
- Reconnection was tested by interface down/up; a real AP failure/router restart not yet tested.
- Audio/DSP initialisation, microphones and own voice assistant are subsequent development tasks. Claude Code/Codex themselves run on the PC.

## End of this session

PC UART reader closed in a controlled way; this ends neither init nor watchdog nor SSH. Afterwards hardware/gen1/tools/Invoke-Echo.ps1 again successful: ROOT_SSH_WITHOUT_SERIAL_READER_OK and 221.35 seconds of uptime. The Echo stays switched on and reachable over SSH. Normal operation needs no running serial reader. Private connection data and device profile updated; public files checked for WiFi credentials, local keys/logs excluded by git-ignore. No commits or pushes made.

## Second Echo, 2026-10-18

Another Echo Gen 1 device (different chip, different eMMC) got the same access, in one session and without rescue need:

1. Connect only USB-UART, open the reader with its own runtime folder (`-RuntimeDirectory local/runtime-echo2`), start Enter repetition, then switch on 3.3 V and Echo power supply. The SD U-Boot stopped; X-Loader and U-Boot came from the SD here without the SD clock workaround.
2. Determine the active slot (see [root start](03-boot.md)): here **main-A**. Kernel loaded from `mmc 1:6`, `root=PARTUUID=<main-A> ro rootfstype=ext2 ... init=/bin/sh`, watchdog keep-alive immediately. With the SD present the eMMC is `mmcblk1` under Linux, root `179:22`.
3. Repository state of the scripts (`hardware/gen1/src/echo-init.sh`, `hardware/gen1/src/echo-service-loop.sh`, `hardware/gen1/src/echo-reboot.sh`, `hardware/gen1/config/udhcpc-echo.script`, `hardware/gen1/config/sshd-persistent.conf`), sshd from the EchoRoot image, WiFi file, `authorized_keys` of the first Echo and an **own** Ed25519 host key transferred with `hardware/gen1/tools/echo_console.py --upload` (with `ECHO_RUNTIME`); all target hashes checked. 10 files including sshd in 107 seconds.
4. `hardware/gen1/tools/bootstrap-ram-access.sh`: WiFi, DHCP and SSH at first only in RAM, with the same service loop. SSH login as root worked.
5. Over SSH eMMC p1-p4 and GPT beginning/end backed up (hashes checked against the device), privately under `local/echo2/recovery`.
6. `hardware/gen1/tools/install-persistent-access.sh 179:22 mmcblk1 <sectors> main-A <init-sha256>` **over UART** (an open SSH session keeps the RAM /var busy). The original init had the same hash as on the first Echo (same firmware state). Superblock clean afterwards, `/usr/local` 0755.
7. Supply off, SD and 3.3 V removed, only Echo power supply: internal U-Boot (2022-01-19), own init, SSH about 50 seconds after switch-on. Software restart through echo-reboot: SSH back after 43 seconds (kernel uptime 36.8 s), root ext3 read-only.

PC access: alias `echo2` in `local/ssh/config`, `hardware/gen1/tools/Invoke-Echo.ps1 -EchoHost echo2 -Command 'id'`. Audio, the assistant, Tailscale and Bluetooth are not installed on this device; the service loop skips them.

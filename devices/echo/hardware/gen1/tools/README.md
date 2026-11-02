# Console helpers

Run these scripts from Windows PowerShell 5.1. The serial helpers do not install firmware or write raw disks. The separately named installer deliberately writes the verified internal main-A/main-B filesystem; do not run it as a console diagnostic. They send arbitrary console input with the privileges of the current Echo prompt, so inspect that prompt before sending commands.

| Script | Purpose |
| --- | --- |
| `Read-EchoConsole.ps1` | Exclusive serial-port reader, ASCII log, bounded lifetime, JSON control queue |
| `Send-EchoCommand.ps1` | Queue one command, repeated CR for a boot countdown, or a reader-stop request |

Defaults: 115200 8N1, no flow control, DTR/RTS off, COM6, runtime directory `devices/echo/local/runtime`. Override COM6 when device discovery gives a different port. Both helpers accept the same optional `-RuntimeDirectory` override. Use a separate runtime directory for another device, and never run two readers on the same port.

Examples are in [the boot and root guide](../docs/boot-and-root.md). These are minimally adapted copies of the previously used console implementation. The relocated versions were syntax-checked and successfully used live for the later Python/audio test on 2026-10-15.

The JSON queue is a single-slot control channel, not a durable job queue or acknowledgement protocol. Wait for visible command output before queuing another dependent command. Do not use it for binary transfers. No raw SD writer is exposed here; historical device-specific writers are archived locally and require a fresh reviewed target check before any reuse.

## Network and installation tools (2026-10-16)

- Invoke-Echo.ps1: run a root command through the ignored local/ssh/config, alias echo-dev. Uses Windows OpenSSH and strict pinned host identity. The private key belongs to the normal Windows user.
- echo_console.py: serial-reader JSON queue client with unique output acknowledgements; gzip/base64 upload without terminal echo and with target SHA-256. Use only on the verified shell, not U-Boot. Target Python lacks zlib, so decompression uses BusyBox gzip.
- read_echo_image.py: read-only ext2/3 image extractor, ECHO_IMAGE environment variable, known partition offset 32769*512. Preserves sparse blocks correctly; older archived extractor did not.
- Invoke-Echo.ps1 -EchoHost echo2: the same for the second Echo (alias in local/ssh/config).
- echo_console.py: set ECHO_RUNTIME to another reader directory (e.g. local/runtime-echo2) for a second device.
- bootstrap-ram-access.sh: RAM-only trial of WLAN and SSH on a freshly rooted Echo (init=/bin/sh): RAM /var, RAM passwd with the sshd user, and the real service-loop.sh with its base directory in tmpfs. Nothing written to the eMMC.
- install-persistent-access.sh: guarded one-time installer into the active main-A/main-B root. Arguments: expected root major:minor, eMMC block, eMMC size in sectors, bootmode and original-init SHA-256; the active slot and eMMC size differ between units. Installed on both Echos. No raw writes, GPT/kernel/bootloader changes or historical installer invocation. See [../docs/boot-and-root.md](../docs/boot-and-root.md) and [../docs/network.md](../docs/network.md); not a repeatable update command after /sbin/init has been switched. Run it from the UART, not over SSH: an open SSH session keeps the RAM /var busy.

The running system starts from internal eMMC without SD/external 3.3 V. Restart through /usr/local/echo-dev/bin/echo-reboot. Own application edits require deliberate root remount,rw followed by sync and remount,ro. A read-only root does not reduce SSH root privileges.

## Bluetooth bring-up (2026-10-17, issue #51)

- bt-try.sh: brings Bluetooth up on the Echo in steps (hciattach, abtfilt, bluetoothd) with a dead man's switch. Every step arms a detached watchdog first and is undone unless confirmed from a new login, or the moment the default gateway stops answering. Writes only to /mnt and /mnt/data, nothing into the boot path. Measures the WiFi before and after each step, because Bluetooth and WiFi share one Atheros chip and wlan0 is the only way into the device.
- bt-agent.py: pairing agent for the trial (BlueZ 4, python 2.7 with dbus). Makes the adapter visible for a while and confirms with Secure Simple Pairing's numeric comparison, asked on the console; in the finished feature the assistant says the number at the speaker instead.

Procedure, findings and abort criteria: [docs/17-bluetooth.md](../../../docs/17-bluetooth.md). Nothing of this is enabled on the device.

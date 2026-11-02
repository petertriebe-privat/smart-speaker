# Serial console on the Windows PC

## Port and tools

Windows PowerShell 5.1 with `.NET System.IO.Ports.SerialPort` was used; no Python serial package is required. The FTDI driver worked with version 2.12.36.20. The port at the time was COM6. Determine it again first:

```powershell
Get-ItemProperty HKLM:\HARDWARE\DEVICEMAP\SERIALCOMM
[IO.Ports.SerialPort]::GetPortNames()
```

Bluetooth COM ports are not an Echo connection. If unclear, unplug the USB-UART, compare the port list, plug it in again. Only one program may have the port open. Do not take over another device's port from the parent project, nor an old session ID.

## Starting the reader

Run in the `Alexa` folder:

```powershell
powershell.exe -NoProfile -ExecutionPolicy RemoteSigned -File .\tools\Read-EchoConsole.ps1 -PortName COM6 -Seconds 3600
```

The reader stays active until the time limit or stop. For agents a continuing terminal process is suitable; its session ID is valid only for that one session. Wait for the output `CONSOLE READY`. Logs, status and control file are under `local/runtime/`, not in the historical archive files.

```powershell
Get-Content .\local\runtime\echo-console-status.json
$state = Get-Content .\local\runtime\echo-console-status.json -Raw | ConvertFrom-Json
Get-Content -LiteralPath $state.log -Tail 40
```

`phase=listening` is only a file status and may be stale after an abruptly killed process. Check the process/log progress. At start the reader ignores the last control ID that already exists: old commands are not repeated on purpose. The reader itself is not a shell-prompt detector.

## Sending commands

Second PowerShell window or separate CLI call, also in `Alexa`:

```powershell
.\tools\Send-EchoCommand.ps1 -Command 'version'
```

**Only at `uboot >`** is `version` the right example command. At `sh-3.2#` use for example:

```powershell
.\tools\Send-EchoCommand.ps1 -Command 'id; uname -srmo'
```

The sender writes a control file; the reader sends the text with a trailing CR. It reports **Queued**, not execution success. Always check the new log section afterwards. Single-line commands up to 512 characters; no embedded line breaks. In PowerShell use single quotes on the outside so that U-Boot's `${filesize}` etc. are not already expanded on the PC.

To interrupt the bootloader countdown, repeat Enter while the Echo starts:

```powershell
.\tools\Send-EchoCommand.ps1 -InterruptBoot -Seconds 15
```

This only repeats CR, it does not start a boot. The bootloader should stop at `uboot >`. Some states restart repeatedly; the interval can then hit a following countdown. After a reset RAM variables are lost. The original locked U-Boot does not have the same interactive access; the goal is the open SD U-Boot.

Close the reader in a controlled way without switching the Echo off:

```powershell
.\tools\Send-EchoCommand.ps1 -StopReader
```

This ends only the PC reader. It does **not** disable any Echo watchdog and does not switch the Echo off.

## Prompts and expected output

| Output | Meaning |
| --- | --- |
| No data | Check port/cable/GND/supply/boot; first clarify log and adapter detection |
| X-Loader, then U-Boot | UART reception works; no Linux shell yet |
| `uboot >` | Open SD bootloader accepts commands |
| `Starting kernel ...` | Kernel starts; no proof yet of a usable root filesystem |
| `sh-3.2#`, `id` gives UID 0 | Actually confirmed root shell |
| `Kernel panic` | Kernel cannot continue; no shell; look for the cause in the previous log |

With `init=/bin/sh` do not send `exit`/Ctrl-D. It is PID 1. Do not send binary transfers through this text-oriented logger: ASCII decoding and CR handling are unsuitable for it. YMODEM/Kermit require their own exclusive, binary-safe port user; such a helper has not been implemented.

Historical reader/sender scripts are in the local archive unchanged. New tools were derived from them and given a configurable runtime directory. Syntax check and later live testing on 2026-10-15 were successful: bootloader interrupt, root commands, file transfer and audio setup ran through them. This does not confirm stable device operation after the final loud tone; for that read the current [audio result](09-audio-test.md).

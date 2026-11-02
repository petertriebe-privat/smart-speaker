# Reconnection and dark LED ring, 2026-10-15

After both supplies had been disconnected as confirmed earlier, the user reconnected and reports that the Echo no longer lights up by itself.

## Proven so far

- The previous tone was audible, last loud and clear; after that the console response broke off. Cause still open.
- The minimal start with `init=/bin/sh` starts neither the regular Amazon init nor the LED service `ledd` automatically. During the audio test `ledd` was not started. Missing light with this software path can therefore be due solely to the uninitialised LED service; it is **no proof of a defect**.
- This explanation is not yet a diagnosis of the current state: after reconnection Windows still detects the FTDI adapter as COM6, the new reader opens the port successfully, but at first receives **no bytes**. Repeated CR also gives no answer at first. A USB COM port proves only the PC-adapter connection, not the supply or functionality of the Echo.
- New raw log: `local/runtime/boot-2026-10-15-234057-651.log`; reader limited to 1800 seconds. The current prompt/boot state is **not known**. Send no further boot/audio/register commands as long as no prompt or start log is confirmed.

## To be clarified

The user confirms that original power supply and external 3.3 V supply are both switched on. Despite the logger still running, no bytes so far. Next, observe a controlled cold start with the logger already running: both supplies off, wait a few seconds, external 3.3 V first, then original power supply. If there are still no messages at all: check supply and UART lines/GND, make changes to the setup with power off. Claim no further soldering work or hardware defects without a concrete finding.

A targeted LED test through sysfs or `ledd` only makes sense after the console is restored. After the kernel start so far the four LP5523 drivers were present; that was not yet a functional LED test.

## Controlled restart with both supplies, still no data

The user confirmed the requested restart (both supplies off, external 3.3 V first, then Echo supply) with "switched on". The raw log that was already open stayed at **0 bytes**. Windows reader process and COM6 are verifiably present; no bootloader or Linux prompt confirmed. Therefore regard neither an LED defect nor a software-only reason as proven.

Next check: disconnect both supplies **and additionally the USB-UART from the PC**, wait ten seconds; first plug in **only the USB-UART** again and re-open reader/COM port before the Echo is powered again. The PC reader was closed in a controlled way for this. This step isolates a not yet tested complete connection/adapter restart; a concrete back-feed through the UART was not measured and is not an established cause.

## USB-UART reconnected

The user confirms "usb plugged in again" after the requested complete disconnection. Windows detects the adapter as COM6 again. A new reader was opened successfully before the two Echo supplies are to be switched on again. Now expected: external 3.3 V first, then the original power supply; evaluate only from the new start messages.

## After power supply change: boot and root console restored

When switching on again the user additionally changed the power supply and reports visible light again. On request confirmed: the replacement is the original power supply of the other Echo Gen 1 of the same series.

In the new log `local/runtime/boot-2026-10-15-234834-881.log` extensive start messages arrived again. The open SD U-Boot `2010.06-00011-ge1ab9145d5` is reachable interactively. The automatic SD start still fails with `No partition table - mmc 0`/`Wrong Image Format`; this known fault is not fixed by the power supply change.

The bootloader was stopped, internal main-B and clean superblock checked again (state 0x1, incompat 0x2 without RECOVER). The original kernel was loaded completely (2,599,644 bytes); the known `ro rootfstype=ext2 init=/bin/sh` start works again. Confirmed: `uid=0(root) gid=0(root)`, Linux 2.6.37 ARMv7, root mount read-only, LP5523 entries in sysfs. Proc/sysfs and RAM watchdog keep-alive were restarted.

**Classification:** The power supply change is a strong hint of a supply problem in the previous setup. A defect of the old power supply or the cause of the failure at the loud tone are not yet proven by measurement/load test; USB-UART and all supplies had also been completely disconnected beforehand. Do not claim a specific component is definitely defective. Do not use the old adapter for further load tests for now.

The current setup is a fresh minimal start, without the previous audio test files/processes in RAM. The audible audio success of the previous session remains proven, but was not repeated with the replacement power supply in this session. The current root access is reachable again; long-term stability with the replacement power supply is to be checked separately.

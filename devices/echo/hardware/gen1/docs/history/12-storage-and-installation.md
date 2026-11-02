# Persistent storage and operation without SD

State 2026-10-16: **Internal start with automatic WiFi and SSH is implemented and tested.** Complete internal software replacement is approved by the user; the original system does not have to be backed up completely.

| Area | Function | Survives power-off? |
| --- | --- | --- |
| Internal eMMC main-B | Original kernel/hardware base, own init under /usr/local/echo-dev, persistent WiFi/SSH files | Yes |
| Internal boot/device configuration | Existing internal bootloader and device identity, targeted private backup | Yes |
| RAM/tmpfs under /mnt | Volatile paths, temporary own programs, DHCP/WiFi/SSH logs | No |
| External microSD | Kept rescue medium with open U-Boot, removed for normal operation | Yes, but not needed |

The tested path is now **internal original bootloader -> internal original kernel -> main-B -> own /sbin/init -> watchdog/WiFi/DHCP/SSH**. Alexa start services do not run on this path. Kernel/DSP support is kept for own later audio functions. No reinstallation of Debian or a complete EchoRoot was done.

One physical start and three further software restarts without SD/external 3.3 V were confirmed; SSH was reachable again each time. Separate 3.3 V and SD are no longer required for this running internal development access. Ten cold starts and a combined audio long-term test have not been done yet. The UART is an optional maintenance/rescue access and does not power the Echo.

Programs and secrets live persistently under /usr/local/echo-dev on main-B. In normal operation root is mounted ext3 read-only; the SSH user is unrestricted root and can use remount,rw for targeted changes, then sync and remount,ro. About 636 MiB were free there before the small installation. No further data partitions were formatted or repurposed.

Bootloader, GPT, kernel and IDME were not changed. Small boot/device configuration areas as well as init/hardware base were privately backed up in a targeted way before changes. The historical EchoRoot installer is still not a suitable general repair command: its fixed mmcblk mapping does not apply in every session.

Full commands, checks, write targets, fallback material and known limits: [WiFi/SSH result](14-network-ssh.md). The earlier planning of a merely possible SD-free start is thereby superseded.

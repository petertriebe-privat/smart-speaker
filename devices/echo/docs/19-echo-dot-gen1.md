# Echo Dot Gen 1: feasibility check

As of 2026-10-20. Research only, no hardware or root test on the Dot yet. The running large Echo and its assistant were not changed.

## Documented hardware

iFixit lists the model S04WQR for the first Dot, with a TI DM3725CUS100, 256 MB RAM, 4 GB eMMC and a QCA6234 WLAN/Bluetooth chip. The platform is very similar to our large Echo Gen 1. The speaker assembly, the 2 W amplifier and the extra 3.5 mm output need a separate check of the audio initialization.

## Root access remains unconfirmed

The device-specific echohacking page assumes the same debug pinout and the same SD root path, but states explicitly that no Dot was available for testing. A similar conversion is therefore plausible, not guaranteed. The successful root access on our large Echo is no proof for the Dot.

UART tools, the init/watchdog and WLAN/SSH concept, and the application source are expected to be reusable. Do not blindly take over images, partition names, U-Boot RAM addresses or device identities. The Dot gets its own device profile and its own SSH host identity.

## Next practical step

A photo of the underside with the model number; for a device that is already open, additionally an overview and a detail of the debug contacts. First determine the contact layout with the power off, check ground/supply/UART levels, and start by reading the boot output. Do not yet present any Dot solder pinout as verified. After getting root: check RAM-WLAN/SSH, the necessary targeted device backups, internal autostart and restarts. Test audio/microphones/LEDs/assistant device-specifically afterwards.

## Sources

- [iFixit: Echo Dot Teardown](https://www.ifixit.com/Teardown/Amazon+Echo+Dot+Teardown/61304), hardware/model.
- [echohacking: Echo Dot](https://github.com/echohacking/wiki/wiki/Echo-Dot), assumed pinout and explicitly untested SD root path.
- [EchoRoot](https://andrerh.gitlab.io/echoroot/), background on the large Echo; no guarantee of Dot support.

Sources read on 2026-10-20. No Dot flash operation, no connection to the running Echo and no commit/push was performed.

## Micro-USB as a possible way in

Addendum 2026-10-20: before any further soldering, first examine the existing micro-USB socket on the PC with a cable known to carry data. The socket alone proves neither an active USB data function nor a serial console, ADB, fastboot or root access. The research found no confirmed USB root procedure specifically for the TI-based Dot Gen 1. Do not transfer instructions for the MediaTek-based Dot Gen 2.

The more detailed Spanish iFixit teardown names a MAX14585 USB/analog signal switch. The manufacturer datasheet confirms the USB signal capability of this part. That is only a hardware hint: the actual trace connection, the switching of the Dot's socket and the USB function offered by the bootloader/system are not established by it.

First attempt: unplug the normal Dot power adapter from the micro-USB socket and connect the socket to the PC with a known data cable; watch the USB enumeration in the Device Manager or by comparing the PnP inventory when plugging in and during startup. Record vendor/product ID, device class and timing privately. Do not replace drivers on suspicion, press button reset combinations or run write/unlock commands at first. A newly detected USB device is not yet root access.

A normal PC port may not supply enough current for stable Dot operation. Distinguish restarts/dropouts from a missing data function; check known data cables and the supply first. Do not create an uncontrolled Y connection between the PC's 5 V and an additional power supply. Without USB enumeration, cable, supply, firmware state, USB routing and any boot modes remain separate open questions.

Further primary sources:

- [iFixit: Dot teardown with additional chip identification](https://es.ifixit.com/Desmontaje/Desmontaje+del+Amazon+Echo+Dot/61304).
- [Analog Devices: MAX14585](https://www.analog.com/en/products/max14585.html), USB/analog signal switch; no proof of a Dot root function.

No USB cable test or USB device comparison has been done on the Dot yet. UART/SD soldering could only be skipped if the USB attempt shows a genuinely usable alternative entry; this is still open.

## Assignment of the actually connected device corrected

On 2026-10-20 the user confirmed the model number **RS03QR**. The connected device is therefore an **Echo Dot Gen 2 (biscuit)** and not a Dot Gen 1. The MT65xx preloader observed during USB startup matches the MediaTek device. The Gen 1 research above is kept as a model comparison but does not count as a working guide for this device. Current USB/device findings: [20-echo-dot-gen2-usb.md](../hardware/dot2/docs/20-echo-dot-gen2-usb.md).

# Hardware, power supply and pinout

## Device

Tall Amazon Echo Gen 1. Board as read from the photos: PCB `30-000791 REV 06`, PCBA `31-000832`. Original board roughly **85 x 56 mm**, tallest component roughly **19 mm**. The 19 mm is a measurement on the previous board, not a full survey of the free installation space.

| Component | State of identification |
| --- | --- |
| TI DM3725CUS100 | Readable on photos; ARM Cortex-A8, the previous kernel starts it at 1 GHz |
| RAM | Bootloader reports 256 MiB; Linux gets 200 MiB in the tested start |
| Internal flash | SanDisk eMMC; Linux reports 3.64 GiB, eight GPT partitions |
| WiFi/Bluetooth | Qualcomm/Atheros chip on the main board; firmware and driver required |
| Microphones/audio | Multi-channel original hardware, codec drivers are detected; audio function in the minimal start still open |
| LEDs | Four LP5523 drivers are detected at kernel start |
| Controls | Kernel registers gpio-keys and rotary-encoder; function not yet tested |

The first Echo that was worked on lost two pads. **All successful software tests refer to the second unit.** Old photos and the early stock boot log must not be treated as the current device state without checking.

## Debug pad field

View onto the pads so that **B is top left**, **C bottom left** and **TP90 below**. Count rows from the top, columns from the left. If the photo is rotated, orient yourself by the labels first.

| Position | c1 | c2 | c3 | c4 | c5 |
| --- | --- | --- | --- | --- | --- |
| r1 | GND | SD D0 | SD CMD | +15 V | +15 V |
| r2 | GND | LED/Test | no pad | GND | UART RX |
| r3 | SD CLK | LED/Test | no pad | GND | Reset |
| r4 | SD PWR | SD D2 | SD D1 | SD D3 | UART TX |

Pinout reference: [Vanderpot pad image](https://vanderpot.com/wp-content/uploads/2016/06/pinout.png). **Connect nothing from the UART/SD wiring to the 15 V pads.** LED/test pads and Reset are not needed in this setup.

## Current wiring plan

| Colour at the Echo | Pad | Other end |
| --- | --- | --- |
| Black | r1c1 GND | Common ground: SD, UART GND and 3.3 V minus |
| Green | r1c2 D0 | Full-size SD pin 7 |
| Orange | r1c3 CMD | SD pin 2 |
| White | r2c5 UART RX | USB-UART TXD |
| Brown/olive | r3c1 CLK | SD pin 5 |
| Red | r4c1 SD PWR | External +3.3 V; together with SD VDD pin 4 |
| Violet | r4c2 D2 | SD pin 9 |
| Blue | r4c3 D1 | SD pin 8 |
| Grey | r4c4 D3 | SD pin 1 |
| Yellow | r4c5 UART TX | USB-UART RXD |

Colours are a project convention, not an electrical guarantee. The user checked continuity; photos alone prove neither sound solder joints nor absence of shorts. Relieve the wires mechanically. After any re-soldering, check continuity and unwanted connections with the power off.

### Full-size SD adapter

A passive microSD-to-SD adapter is soldered, not the memory chip directly. Look at the contact side, contacts up, bevelled corner top left: **left to right 9, 1, 2, 3, 4, 5, 6, 7, 8**.

| SD pin | Signal | Colour |
| --- | --- | --- |
| 9 | D2 | Violet |
| 1 | D3 | Grey |
| 2 | CMD | Orange |
| 3 | GND | Black |
| 4 | VDD 3.3 V | Red |
| 5 | CLK | Brown/olive |
| 6 | GND | Black |
| 7 | D0 | Green |
| 8 | D1 | Blue |

Both black SD wires go to ground. SD pin 4 and the Echo's SD PWR share the external supply. The Echo is **not** powered from the UART adapter. Never connect the SD adapter to the Echo and to a powered PC card reader at the same time.

### USB-UART

Present: FTDI/FT232RNL-compatible BLAHAJ USB-C adapter. USB-C on top, six-pin header at the bottom, board labelling readable: **DTR, RXD, TXD, VCC, CTS, GND** from left to right. The labelling on the actual adapter is authoritative.

- Level jumper on **1V8**; not 3V3/5V. RX/TX are crossed as in the table.
- VCC, DTR and CTS stay free; no hardware handshake.
- Historical Windows port COM6, 115200 baud, 8 data bits, no parity, 1 stop bit.
- Remove packing foam under the pins; it can be conductive.

### Power and measuring

Original Echo power supply plus a separate USB-powered adjustable 3.3 V module. Model/order references are in the local device profile. The current reserve under load has not been measured; the regulator is not a proven current-limited bench supply.

1. Change wiring only with the power off; Echo supply and 3.3 V module off.
2. Check 3.3 V at the module output, then directly at the SD adapter under load. Protect the regulator against being turned.
3. To start, **3.3 V first**, then switch on the original Echo supply. The UART can be plugged into the PC beforehand.
4. For a full cold start switch both supplies off, wait a few seconds, then switch them on again in this order.

AstroAI AM33D: black in **COM**, red on the right in **VΩmA**, rotary switch **20 V DC (V=)** top left. Black to GND, red to SD VDD. Do not use the current, resistance or continuity ranges while the circuit is powered. Do not bridge neighbouring pads with the probe tip.

Earlier 0.6-1.4 V/OL was reported; the mis-set regulator was then corrected and 3.3 V confirmed. That does not rule out short dips or signal problems. A multimeter is no substitute for an oscilloscope here.

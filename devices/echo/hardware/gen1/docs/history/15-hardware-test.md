# Hardware test over SSH: speaker, microphones, LED ring, buttons

State **2026-10-16**, internal boot without SD, everything over the WiFi SSH access. Raw data only locally; sanitised results here.

## Detected components

| Component | Linux view | Remark |
| --- | --- | --- |
| CPU | ARMv7 Cortex-A8, 1 GHz, NEON, VFPv3 | Userland softfp EABI (`/lib/ld-linux.so.3`), EGLIBC 2.19 |
| Speaker codec | `tlv320aic32xx` on I2C 3-0018 | ALSA card `OMAP3EVM`, but playback only through DSP/PulseAudio (direct ALSA -> kernel oops, see [audio test](09-audio-test.md)) |
| Microphone ADCs | 4 x `tlv320adc31xx` (A-D) on I2C 2-0018...001b | Together 8 channels; the DSP does the processing |
| LED drivers | 4 x `lp5523` on I2C 2-0032...0035 | 36 channels = 12 RGB LEDs, plus `power-led` and `network-led` |
| Buttons | `gpio-keys` -> `/dev/input/event0` | Action button and mute button |
| Volume ring | `rotary-encoder` -> `/dev/input/event1` | Relative events |
| PMIC | `tps65910` on I2C 1-002d | not changed |

## Audio chain

Rebuilt from the original Upstart jobs (`modules-ti-dsp`, `audio-shm-main`, `audioctrl`, `audioproxyd-main`, `pulseaudio-main`), script [hardware/gen1/src/echo-audio.sh](../../src/echo-audio.sh):

1. DSP modules `cmemk`, `dsplinkk`, `lpm_omap3530` with the original parameters.
2. Shared-memory queues/buffers under `/dev/shm` as in the original.
3. Minimal D-Bus system bus, `audioCtrl`, `audioproxyd`.
4. **Exactly one** PulseAudio (user mode, socket `/mnt/echo-audio/pulse-native`) with `lab126-sink` (48 kHz stereo) and `lab126-source`.

Tested: after a restart the chain starts automatically (by the assistant) in about 9 seconds. Memory need of the whole chain including the assistant: about 48 MB of 190 MB used.

### Microphones

- Usable is **`/dev/shm/BMicsOut.shm`**: 16 kHz, mono, s16le, processed by the DSP (beamforming, noise suppression, **echo cancellation**). 32 kB/s measured.
- `BVoIPMic`/`BPhoneMic` stay empty in normal mode (write counter 0); presumably active only in the DSP's call mode. The PulseAudio `lab126-source` hangs on the empty VoIP buffer and therefore delivers nothing.
- Buffer format (determined ourselves): `0x00` u64 counter of written samples, `0x64` u32 sample size 2, `0x68` u32 capacity 2^20, ring from `0x70`. The assistant reads the ring directly via `mmap`; `shmbuf_tool -m 2` into a pipe blocks, into a file it works.
- Idle level in the room: RMS about 5-8 (very quiet, DSP noise suppression).

### Speaker and echo cancellation

- Playback through `pacat` to PulseAudio, 24 kHz mono (PulseAudio resamples). Proof without a listener: a recording of the sink monitor shows the signal; the user had already confirmed the speaker audibly earlier.
- Loopback test (play tone, record `BMicsOut`, [hardware/gen1/src/audio_loopback.py](../../src/audio_loopback.py)): a 1 kHz tone is practically not visible in the microphone signal; this suggests active echo cancellation.
- Conversation test with a 20 s assistant reply at volume 40: residual echo in the microphone RMS mostly 6-20, only briefly up to ~120 at the start. GPT Live recognised **no** user speech from it. The assistant can thus fundamentally be interrupted (barge-in), unlike on the M5. Not yet checked at higher volumes.
- **Volume:** `lipc-set-prop com.doppler.audiod MainVolume` worked after a fresh start, but blocked permanently after several calls (timeout 10-20 s, a thread of `audioCtrl` hangs on a mutex; restarting `audioCtrl` alone did not help). `com.doppler.audio rawVolume` can be read, writing is ignored. Solution: the audio script sets `MainVolume` once to 70; the assistant then controls the volume as gain on its own samples. Observed mapping: MainVolume 25 -> raw 89, 30 -> 99, 40 -> 115, 70 -> 145, 100 -> 175.

## LED ring

- Self-test of all four LP5523: `OK`, temperature 31-32 C.
- **The sysfs files of the Amazon driver (`all_leds`, `lp5523:*/brightness`) have no effect:** They change only the driver's cache, the PWM registers in the chip stayed 0. After boot the chips' animation engines also run (bootloader animation, rotating blue ring).
- The assistant therefore writes **directly through `/dev/i2c-2`** (`I2C_SLAVE_FORCE`): at start `OP_MODE` (0x01) = 0 (engines off) and `ENABLE` (0x00) = 0x40, then the PWM registers 0x16-0x1E. **Single-register writes only**; multi-byte writes with auto-increment lost individual bytes. Helper script for reading and setting: [hardware/gen1/src/lp5523_regs.py](../../src/lp5523_regs.py).
- Channel assignment per chip, LED k = 0...2: **Green = channel 2k, blue = 2k+1, red = 6+k** (D1/D2 = G/B of the first LED, D7 = its red).
- Positions (top view, mute button left at 9 o'clock), confirmed on 2026-10-16 with single channels, photos and a chaser:

| Chip | LED 0 | LED 1 | LED 2 |
| --- | --- | --- | --- |
| 0x32 | 3 o'clock | 2 o'clock | 1 o'clock |
| 0x33 | 9 o'clock | 8 o'clock | 7 o'clock |
| 0x34 | 5 o'clock | 6 o'clock | 4 o'clock (wired out of order) |
| 0x35 | 12 o'clock | 11 o'clock | 10 o'clock |

- The red LED in the mute button is connected neither to the GPO outputs of the LP5523 nor to `power-led`/`network-led`: it is switched by the hardware (see mute).
- `led_offset` in the settings rotates the picture if needed; test: `jarvis ctl ledtest` (a red light runs clockwise from 12 o'clock).

## Buttons and volume ring

Pressed or turned by the user on 2026-10-16 and recorded ([hardware/gen1/src/input_capture.py](../../src/input_capture.py)):

| Control | Event |
| --- | --- |
| Action button (dot) | `EV_KEY` 138 (`KEY_HELP`), **0 = pressed, 1 = released** |
| Mute | `EV_KEY` 113 (`KEY_MUTE`): **not a button, but the state of a hardware latch** (GPIO `VOLUME_MUTE`). Each press toggles it and the red button LED; 0 = muted, 1 = microphones on. The current state is given by `EVIOCGKEY`. |
| Ring | `EV_REL` `REL_X`, +-1 per detent; **clockwise -1** |

Software cannot switch the red button LED. The assistant therefore takes over the latch state (also at start) and never cancels mute itself; the action button gives only a low tone when muted. `gpio-keys` also reports a third code (355), whose button is unknown.

GPIO names per debugfs (read only!): 21 `WIFI_reset`, 22 `Bluetooth_reset`, 129 `PA_Enable`, 70 `VOLUME_MUTE`, 72 `UBER`, 77 `Reset`, 109 `ti_led`. Do not write blindly: GPIO 21 resets the WiFi.

## Network and clock

- The kernel supports **no IPv6**. DNS returns AAAA records first for OpenAI; `ntpdate` fails because of it, Go programs must force IPv4.
- The device starts without an RTC in the year 1970. The assistant sets the clock itself via SNTP as soon as DNS is available (about 30 s after boot).
- When roaming between two mesh access points the default route was lost while the DHCP lease still applied. The service loop now restores it from the gateway address stored by the DHCP script.

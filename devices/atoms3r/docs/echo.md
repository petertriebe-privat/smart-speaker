# The assistant on an Amazon Echo (first generation)

Goal: every feature of the AtomS3R + Echo Pyramid version also runs on an Amazon Echo Gen 1,
without Alexa. Tracking issue: #42.

Status (2026-10-16): **done and in daily use.** The Echo is the household's main assistant; since
2026-10-19 the M5 is a member of the Echos' network (`PEER_KEY` in `secrets.h`, wake word on and
arbitrated, memory on the leader: [architecture.md](architecture.md#a-network-of-devices-66)). The Echo
project lives in [devices/echo/](../../echo/): unlock and setup per model in
[hardware/gen1](../../echo/hardware/gen1/README.md) and [hardware/dot2](../../echo/hardware/dot2/README.md),
the assistant in [devices/echo/docs/16-assistant.md](../../echo/docs/16-assistant.md), the code in
`core`.

## How it turned out (versus the plan of 2026-10-10)

| Plan | What we did, and why |
|---|---|
| EchoRoot: mainline kernel and buildroot from an SD card | Amazon's own Linux 2.6.37 on the eMMC stays; our own PID 1 on the main-B root (read-only in normal use), WiFi, SSH and the assistant start from it. Bootloader, kernel, partitions unchanged; no SD card needed. |
| DSP unusable, AEC in software (WebRTC/speexdsp) | The DSP chain runs (cmemk/dsplink, audioCtrl, PulseAudio). It delivers the 7 mics **beamformed and echo-cancelled** at 16 kHz in a shared-memory buffer, which the assistant reads directly: barge-in without our own AEC. |
| One C++ code base for ESP32 and Linux behind a platform layer | A **port to Go**, statically built for ARMv7 (no dependency on the old EGLIBC), same prompts (extracted from `include/*.h` into `prompts.json`), same GPT Live protocol and tool set. The M5 code is unchanged apart from the second-speaker switches. |
| microWakeWord on TFLite for armv7 | The same model in pure Go: TFLite Micro's frontend bit for bit and a small int8 interpreter, checked against the reference runtime; ~2 % CPU. |

## What the Echo has that the M5 has not

- **Parallel conversations:** a phone call and a conversation at the speaker at the same time,
  messages passed between them (`tell_other_conversation`, `speak` during a call).
- **Spotify Connect speaker:** librespot under the assistant, one volume for voice and music, music
  ducked while it talks, quick music commands ("Pause", "Lauter", "Spiel was von X") run on the
  device without the GPT Live round trip.
- **Barge-in** at the speaker (the DSP cancels its own voice).
- Error log and self-test on the data partition, health log every minute.

Not available: `room_sensor` (no presence sensor), display and touch (LED ring, buttons and the
volume ring instead).

## Hardware as used

| Part | How the assistant uses it |
|---|---|
| SoC | DM3725 (OMAP3), 1 × Cortex-A8; idle ~11 % CPU with wake word and LED loop |
| Microphones | DSP output `/dev/shm/BMicsOut.shm`, read by mmap (the pipe blocked) |
| Speaker | PulseAudio via `pacat` (24 kHz voice, 44.1 kHz music), DSP `MainVolume` fixed at 100, volume as software gain (repeated LIPC volume calls deadlocked audioCtrl) |
| LED ring | 4 × LP5523 over I2C directly (`I2C_SLAVE_FORCE`, single-register writes; the sysfs files do not reach the chip) |
| Buttons, ring | input events: action `KEY_HELP`, mute `KEY_MUTE` (hardware latch, its red LED switched by hardware), ring `REL_X` |
| Storage | data partition (ext3, ~1.5 GB) for memory, tasks, clips, Spotify login |
| Network | WiFi with WPA2; the kernel has no IPv6 (librespot needed two small patches) |

## Lessons worth keeping

- **Microphone levels differ:** the M5's `VOICE_PEAK` (1000) is far above a voice through the
  Echo's DSP (a few hundred); with it, speech while connecting was dropped as silence.
- **Do not send the pre-roll in one burst:** what was said while connecting went to GPT Live at
  once and it heard only the first word; it is now sent at most 1.5× real time.
- **GPT Live sometimes announces an action and does not delegate it** ("Ich starte Eminem").
  The Echo reminds it with a system note after 3 s; music commands bypass it altogether.
- **The strict tool schema** makes the model send every field: `"volume": 0` and `"level": 0`
  next to a `step` must not be taken literally.

See [devices/echo/docs/16-assistant.md](../../echo/docs/16-assistant.md) for details, measurements and the
control commands.

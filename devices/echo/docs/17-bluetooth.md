# Bluetooth on the Echo

As of **2026-10-17**, preparation for #51:
connect the phone to the Echo via Bluetooth, use the Echo as a speaker and
hands-free device, and optionally place outgoing calls through the phone. This document covers
what the device ships with, how commissioning works and how it protects itself. **None of it is
switched on yet**: the findings were read, not tried.

## What is on the device

The complete stack is present, shipped by Amazon and never removed:

| Part | Location |
| --- | --- |
| BlueZ 4.101 | `/usr/sbin/bluetoothd`, `hciconfig`, `hcitool`, `sdptool`, `rfcomm`, `hciattach` |
| oFono 1.5 | `/usr/sbin/ofonod` (hands-free profile over D-Bus) |
| PulseAudio 3.0 | `module-bluetooth-discover`, `-device`, `-policy`, `libbluetooth-util.so` |
| Firmware | `/lib/firmware/ar3k` (Atheros AR3k, the BT half of the combo chip) |
| Coexistence | `/usr/local/bin/abtfilt-hw3.0` — shares the chip between WLAN and BT |
| Configuration | `/etc/bluetooth/`: `main.conf`, `audio.conf`, `dopplerAudioHFP.conf`, `pancakeAudio*.conf`, `input.conf`, `network.conf`, `rfcomm.conf` |
| Python | 2.7.2 with `dbus`, `gobject`, `glib` — enough for a pairing agent |

Important details:

- **`SCORouting=PCM`** in `dopplerAudioHFP.conf`: call audio does not run over HCI but
  over the PCM path. Matching this, ALSA device `hw:0,2` has playback **and** capture
  ([audio.md](../hardware/gen1/docs/audio.md)), so the audio of a phone call appears as an
  ordinary ALSA device, in the same form that the bridge from #49 already handles.
- `Enable=Source,Sink,Media,Voice,Control,Gateway` — HFP is configured, not only A2DP.
- `main.conf`: `Class = 0x2C2414` (audio/video, speaker), `Name = dp-dev-%d`.
- **The system bus is already running**, started by the audio chain (`/mnt/echo-audio/dbus.conf`,
  socket `/var/run/dbus/system_bus_socket`), and its policy is open (`allow own="*"`):
  `bluetoothd` can take over `org.bluez` there without changes.

## What is not running

`/sys/class/bluetooth/` is empty, there is no `bluetoothd` process and no BT driver module loaded, only
`ath6kl_sdio` for WLAN. The reason is not the hardware but the startup: Amazon's jobs
hang off Upstart, and `/sbin/init` has pointed to `init.sh` since rooting. The job files are
still there and are thus a build recipe, not a mechanism:

```
/etc/init/hciattach.conf   reset via GPIO 22 (20 ms low, 100 ms high), then
                           hciattach -n /dev/ttyO2 ath3k 3000000 flow nosleep $(cat /proc/btmac)
/etc/init/abtfilt.conf     abtfilt-hw3.0 -n -z -m -a -w wlan0
/etc/init/btmd-ng.conf     btmd-ng, Amazon's own BT manager (probably dispensable)
```

`hci_uart` is in `/sys/module/`, so the driver is built into the kernel — only attaching the chip
to the UART is missing.

## The approach

The Echo hangs on `wlan0` and nothing else: Tailscale and SSH both run over it, and BT
and WLAN share one chip. A failed attempt can take the device off the network; the only way back
is then the serial console ([boot-and-root.md](../hardware/gen1/docs/boot-and-root.md)) and the power socket. Hence three
rules that survive every step:

1. **Nothing becomes permanent** before it has proven itself. The scripts live under `/mnt`
   (tmpfs), `init.sh` and `service-loop.sh` do not call them. Pulling the plug always leads back to the
   known state. The root file system helps: it is mounted **read-only**
   (`/dev/root … ro`), and writes go only to `/mnt` (tmpfs) and `/mnt/data` (data partition),
   so an accidental permanent change is practically ruled out.
2. **Dead man's switch.** [`hardware/gen1/tools/bt-try.sh`](../hardware/gen1/tools/bt-try.sh) sets up a watchdog before every step,
   detached from the session with `setsid`, which undoes everything if nobody says `bt-try.sh keep`
   from a **new** login within a few minutes — and immediately
   if the default gateway no longer answers three times in a row.
3. **GPIO 21 is not touched.** It is the WLAN reset ([known-issues.md](../hardware/gen1/docs/known-issues.md)).
   Bluetooth is GPIO 22, with Amazon's timings.

Log: `/mnt/data/bluetooth-try.log` on the data partition, not in RAM. The service loop
records in a comment why: in an earlier hang there was ping but no SSH anymore,
and the RAM logs were lost after pulling the power.

### Procedure

Copy the files over (from the server; the container jumps via the host):

```sh
scp devices/echo/hardware/gen1/tools/bt-try.sh devices/echo/hardware/gen1/tools/bt-agent.py assistant-echo:/mnt/
```

From the PC (Git Bash) with the private SSH profile:

```sh
/c/Windows/System32/OpenSSH/scp.exe -F devices/echo/local/ssh/config devices/echo/hardware/gen1/tools/bt-try.sh devices/echo/hardware/gen1/tools/bt-agent.py echo-dev:/mnt/
```

Read beforehand (2026-10-17): `/sys/class/gpio/gpio22` is exported and set to **1**,
`/proc/btmac` and `/dev/ttyO2` are there, as are `usleep`, `iw`, `setsid`, `hciconfig`, `hcitool`.
`down` resets GPIO 22 to the value from before `attach`, not to 0. About 20 MB of RAM are free
beside the assistant; what `bluetoothd` and later oFono cost is measured by the trial.

Then stage by stage, each with its own window, comparing the measurement between stages:

```sh
sh /mnt/bt-try.sh attach 2     # only GPIO reset and hciattach
sh /mnt/bt-try.sh status       # from a second login: is it running, how is the WLAN
sh /mnt/bt-try.sh keep         # only when the measurement is fine
sh /mnt/bt-try.sh filter 5     # add abtfilt — the piece that protects the WLAN
sh /mnt/bt-try.sh scan         # inquiry: does the chip see anything at all
sh /mnt/bt-try.sh daemon 10    # bluetoothd on the running system bus
python /mnt/bt-agent.py        # pairing window, number comparison on the console
sh /mnt/bt-try.sh down         # at the end of the evening: everything back
```

Each stage writes a WLAN line to the log before and after: signal strength and 50 pings
to the gateway with loss rate and round-trip time.

### When to abort

- Packet loss above 2 % or more than a doubling of the mean round-trip time compared with the
  previous measurement.
- The SSH session stalls for longer than a few seconds.
- `hciattach` delivers no `hci0` after 20 seconds.

In all three cases `down`, read the log, think — do not build on.

### Only once it has proven itself

Commissioning becomes permanent at the earliest when it has held for several days, and then in the
service loop beside WLAN and sshd, with the same checks: if `hciattach` dies, it is
restarted; if the WLAN measurably suffers, Bluetooth stays off. Until then `/mnt` remains the right
place.

## First attempt (2026-10-17)

With the owner at the device, stage by stage. **The WLAN did not suffer in any stage:** 0 %
loss in every measurement, mean round-trip time 12–18 ms as before, signal unchanged at −36 to
−39 dBm. The assistant kept running undisturbed throughout.

| Stage | Result |
| --- | --- |
| `attach` | `hci0` after 1 s, Bluetooth 3.0, no errors |
| `filter` | `abtfilt` running |
| `scan` | nothing found (phones are visible only while the BT settings are open) |
| `daemon` | `bluetoothd` running, about 1 MB RAM; connectable, not visible (`PSCAN`) |
| Pairing | Number comparison works; the agent had to move from `argparse` to `optparse` (Amazon's Python 2.7.2 has no `argparse`, nor `runpy`, so no `python -m`) |
| A2DP | Music from the phone comes out of the Echo |
| HFP | Service connection is up, oFono sees the phone's mobile network |

Three things were missing, all three had been done by Amazon's Upstart; `bt-try.sh` now does them itself:

- **The key store.** BlueZ has the link keys compiled in under
  `/var/local/lib/bluetooth`, and that lies on the read-only root file system —
  at Amazon the data partition was mounted there (`/mnt/data/local` is the old
  `/var/local`). Without a store every connection pairs anew, with a new code, and `Trusted` cannot
  be set. Now: a fresh store `/mnt/data/bluetooth`, bind-mounted at the
  compiled-in location. Amazon's old store stays unused.
- **`audio.conf`** is a link to `/var/local/bluez/audio.conf`, which Amazon's `bluez.conf` job
  set to `dopplerAudio[HFP].conf` at every start. Created once (pointing to
  `dopplerAudioHFP.conf`), with a brief `remount,rw` and straight back to `ro`.
- **PulseAudio and oFono.** Without `module-bluetooth-discover` the Echo offers only "calls", without
  `module-bluetooth-policy` A2DP does not reach the speaker. In BlueZ 4, HFP needs a
  "Handsfree Agent", and that is oFono: without `ofonod` the phone sets up HFP and BlueZ closes it
  again immediately (`Handsfree Agent not registered`; on the phone the switch for calls
  flips back). oFono creates a modem per paired phone (`/hfp/<Adapter>_<Phone>`), which
  has to be switched on (`Powered`); then the service connection is up, with
  `VoiceCallManager`, `Handsfree`, `CallVolume` and `NetworkRegistration`.

### Call audio: both directions work

Once HFP is up, the phone sends a call over eSCO, and with `SCORouting=PCM` it lands as an
ordinary ALSA device on `hw:0,2`: **S16_LE, 8 kHz, mono** (the device refuses 16 kHz,
so no wideband/mSBC). The BT chip provides the clock; reading without a running SCO ends with
`Input/output error`. [`hardware/gen1/tools/bt-sco-test.sh`](../hardware/gen1/tools/bt-sco-test.sh) waits for the
SCO connection, records with `arecord` and plays beeps into it with `aplay`.

- **Phone → Echo:** ringback tone (425 Hz) and the other party's voice arrive cleanly, peaks up to
  18,000. Exactly zero between sentences — the phone sends digital silence.
- **Echo → phone:** proven only on the third attempt, with a loop that needs no second
  human: the assistant calls the phone from the landline (`ctl tool dial_for_me`), the phone answers over
  HFP at the Echo, the beeps go out over SCO and come back over the landline on the
  speaker. Clearly audible. The first two attempts came to nothing because the
  tones played only once and too early:
- **SCO opens before the conversation.** On an outgoing call already while dialing (the ringback tone comes
  through it), on an incoming call already while it rings (state `incoming`): the phone sends
  its ringtone **in-band**. #51 provides that this never reaches the speaker — the
  bridge must therefore give `hw:0,2` to the room only from the moment of answering, not from SCO.
- The call state from oFono (`VoiceCallManager.GetCalls`) said "active" immediately in the second attempt;
  check again more precisely as the signal for answering (`PropertyChanged` instead of
  polling).

For the bridge from #49 this means: the Bluetooth line is the same kind of audio as the landline
(8 kHz, 20 ms frames), only over ALSA instead of RTP and without G.711. Mixing, gate, backlog limit,
announcement and ring can be taken over.

## In the assistant: a phone call at the Echo (step 1 of #51)

`internal/phone/mobile.go`, `ofono.go`, `sco.go`. The assistant asks oFono every `MOBILE_POLL_MS`
(500 ms) over the system bus (`github.com/godbus/dbus/v5`, pure Go) for modems and calls.
If no `ofonod` runs, it does nothing and reports that once in the log. Bluetooth itself is still
brought up by hand with `bt-try.sh`.

- **The switch on the phone decides** (`mobile_handsfree`, private, default `auto`): whether
  calls land at the Echo is the "Calls" switch in the phone's Bluetooth settings. The
  phone remembers it and sets up HFP by itself on coming home, and the assistant leaves that alone. The
  first version kept the modem off by default. On the first homecoming (2026-10-17) the
  phone set up HFP four times, the assistant cut it four times, the phone switched "Calls" off and no longer
  connected by itself even for music. Overridden by voice: `on` ("use my phone for
  calls") connects from here, `off` ("calls back to the phone") keeps the modem off, then
  BlueZ rejects the profile and the phone keeps its calls. `on` and `off` are stored in
  `mobile.json` on the data partition and also apply after a restart. Music (A2DP) works
  either way.
- **It rings:** two soft, rising tones every `MOBILE_RING_EVERY_MS` (6 s), at most
  five cycles. In the first cycle and in every second one after it, an announcement follows via TTS: "Alex is
  calling on the mobile", "Number not in the address book" or "Number withheld". The name comes
  first from the household list, then from the address book. Not during a conversation at the speaker and not
  while the landline has the speaker; the LED ring still pulses slowly green. The phone's
  ringtone, which it sends in-band over SCO, is never heard in the room.
- **Answer, reject, hang up:** `mobile_call` by voice ("pick up", "reject", "hang
  up"), the action button (if it rings: answer, if a call is running: hang up), "end connection"
  (`speaker_call off`) or the phone itself.
- **In the call:** From `dialing`, `alerting`, `active` or `held`, `hw:0,2` goes to the bridge from
  #49, in mode `talk`. `arecord` delivers the other party (8 → 24 kHz into the room), the microphone
  goes through gate and gain, via 16 → 24 → 8 kHz, to `aplay`. Both with an 80 ms buffer, because the
  default of `aplay` (half a second) would be audible as delay and late echo.
  Announcement tones, backlog limit and ring come from the bridge. A second call waiting
  does not displace the running call.

**First real call (2026-10-17), caller on the phone:** Ringing and name arrived,
answering was done via the action button. Three findings:

- **PulseAudio looped the call itself.** `module-bluetooth-device` creates for the phone,
  beside `a2dp_source` (music), the profile `hfgw`, with *higher* priority. Without arguments
  `module-bluetooth-policy` connects both by loopback to speaker and microphone: a
  second path beside the assistant's bridge, without gate and with a large buffer. The caller heard almost only
  themselves, late, and hardly the user. Now the script loads the policy with
  **`hfgw=false a2dp_source=true`**. Music continues over PulseAudio, call audio only over
  `hw:0,2`.
- **"Pick up" did not arrive:** During the ringtones the wake word was blocked, as always when
  the speaker is playing. Now it stays on while ringing (the DSP calculates the tones out), and
  "Jarvis, pick up", "answer", "reject", "hang up" are quick commands (`quick.go`), without a
  detour via GPT Live.
- For tone quality the assistant writes a line every `MOBILE_STATS_MS` (10 s) during a phone call:
  frames in both directions with peak level, of which attenuated by the gate, and the
  queue to the room.

**Second call, same evening:** The caller heard the user better, but themselves
about 3 s later. The direction to the room held 80–120 ms according to the measurement line. The backlog was in the
direction to the phone: `aplay` reads from a pipe of 64 KB, which at 8 kHz is **four seconds**.
If `aplay` stalls once (start, underrun), it fills unnoticed and the delay stays.
Now a pacer (`pacer` in `sco.go`) lets through at most `MOBILE_OUT_LEAD_MS` (160 ms) beyond
real time, like the RTP sender with its own clock on the landline. The wake word during
ringing reached only 0.35 (threshold 0.8): while ringing and during a conversation at the
speaker the music threshold (0.5) now applies.

**Echo test** (`assistant ctl echotest <mobile number> [beeps]`, `echotest.go`): the assistant calls
the paired phone from the landline, without GPT Live, without its own bridge, without ringback tone in the room. The
Echo itself answers the phone call, which goes to speaker and microphone like any other.
On the landline side beeps (1 kHz, 200 ms) go out every 3 s, and the assistant looks for them in
the return path: landline → phone → speaker → microphone → phone → landline, the path on which a
caller hears themselves. Result per beep: delay and level of the echo. No second
human needed.

First measurement with pacer (2026-10-17): **0 of 8 beeps came back**, all below −50 dB
(line noise −84 dB). Lead to the phone 48–53 ms, nothing discarded, queue to the room
100 ms, speaker buffer 30–40 ms.

**Third call, places swapped:** On the phone one heard oneself delayed, the answer from
the room came "first quietly in the background, then loud again". According to the measurement line the
**speaker held 1.47 s** until the end of the call. A wake word in the middle of the call had opened
a conversation whose answer went into the same pipe. After that the call kept writing in
real time and never caught up. The gate (0.7 s from queuing) was long open again when the
voice sounded 1.5 s later, and the gain control raised the residual echo up to eightfold.
Now:

- The bridge discards frames as long as the speaker holds more than `PHONE_SPEAKER_MAX_MS` (200 ms)
  (`Speaker.Queued`).
- The gate counts from the moment the speaker plays the frame, not from queuing.
  It closes already from a peak of 100 instead of 300: quiet word endings previously went out unattenuated
  and amplified.
- The echo test speaks a TTS sentence instead of beeps (mobile networks and phones filter steady tones as
  noise) and compares the loudness envelopes by cross-correlation. It looks for a second path
  after the first has been calculated out. `echotest <number> 3 nogate` is the control run without
  gate. Measurement afterwards: 0 of 4 sentences came back. Without gate one of three came
  back at −37 dB. The DSP calculates the speaker out very well: the microphone delivered peaks around 300
  while the sentence played at 12,000.
- **Wake word while ringing:** It reached 0.40–0.50, and the "pick up" via a GPT Live session
  took 9 s. Now a wake word while ringing **answers immediately**, without a conversation
  (`MOBILE_WAKE_CUTOFF` 0.4). Reject by button or "Jarvis, hang up" afterwards.

In addition (`mobile_ring.go`):

- **Night quiet** from `MOBILE_QUIET_FROM` to `MOBILE_QUIET_TO` (22:00–07:00): no tones, only the announcement,
  at half volume. The call wakes nobody; whoever is awake hears it.
- **The announcement is cached**, following the pattern of the "Yes, Sir?" clips: generated once
  via TTS, stored under `mobile-voice/` on the data partition, afterwards without wait and without
  cost. For a new name the first cycle has only the tones.
- **If a conversation is running at the speaker**, nothing rings. The assistant gets a
  system hint once (`LIVE_MOBILE_RINGING`) and asks whether it should answer.
- **The music stays down** while it rings and the call runs. This is one rule
  together with the lowering during a conversation with the assistant (`duckMusic`), so that its end
  does not turn the music back up in the middle of the call.

## Which line (step 2, `mobile_dial.go`)

"Whose voice, whose number": `dial_for_me` (the user speaks themselves) goes via the phone if
its hands-free connection is up, otherwise via the landline. If the landline is not registered, the
call goes via the phone. `via` overrides ("via my phone", "via the landline"). Via the phone without a
standing HFP means: connect just for this call (`oneCall`), then disconnect again. If the
connection is not established within `MOBILE_DIAL_WAIT_MS` (15 s) or dialing fails, the
landline takes over, and the room hears why. Via the phone only its owner dials (the admin).
`phone_call` (the assistant speaks) always uses the landline and does not call the admin as long as their
phone is attached to the Echo: they are then at home, and the call would come out of this speaker again.
On an outgoing phone call the ringback tone runs over SCO in the room, two tones on answering,
the ring circles.

## Pairing by voice (step 3, `internal/bluetooth`)

`bluetooth_pair` (private, only at the speaker and on the console). `start` makes the Echo visible as
`BT_NAME` (the assistant's name) for `PAIR_WINDOW_S` (120 s), the ring pulses blue. When the phone asks,
the assistant receives the six digits as a system hint, says them in two groups of three and asks for confirmation.
If no conversation is open, it starts one. `confirm` or `reject` must
come within 20 s, otherwise it is rejected (BlueZ waits 25 s). After the yes the phone becomes `Trusted`, and the
window closes. Old phones without number comparison get a code, which the assistant names.
`list` shows the paired phones, `forget` removes one.

The agent is **permanently** registered with BlueZ (`DisplayYesNo`) and is re-registered after every restart of
`bluetoothd`. Outside the window it rejects every pairing without asking and
writes it to the error log. Without an agent BlueZ would fall back to "Just Works". Services (calls, music)
are given only to a paired device.

## At startup (step 4, `hardware/gen1/src/echo-bluetooth.sh`)

The service loop calls `bluetooth.sh ensure` once a minute in the background. The script
starts only what is missing: chip, `abtfilt`, key store, `audio.conf`, `bluetoothd`, the
PulseAudio modules (PulseAudio may restart with the assistant) and `ofonod`. It takes effect only if
**`/mnt/data/bluetooth.enabled`** exists, because a deploy should not change by itself how the
device starts. It measures the chip before and after switching on (10 pings to the gateway). If
the loss rises by more than 20 percentage points, Bluetooth goes off and stays off until someone removes
`/mnt/data/bluetooth.blocked`. At most three attempts per start. Log:
`/mnt/data/bluetooth.log`. The Echo reads the new service loop only at the next restart.

As of 2026-10-22: `bluetooth.enabled` exists on no Echo, `bluetoothd` and `ofonod` run
nowhere. "Pair my smartphone" in the study (21st) therefore failed. The tools now name
the reason (`bluetooth.NotRunning`: never switched on, blocked because of WLAN, still starting)
and say that the assistant cannot switch it on itself.

```sh
touch /mnt/data/bluetooth.enabled     # switch on (from the next pass)
sh /usr/local/echo-dev/bluetooth.sh status
rm /mnt/data/bluetooth.enabled; sh /usr/local/echo-dev/bluetooth.sh down   # switch off
```

## Open questions

- **Which `audio.conf` applies.** For HFP the matching variant (`dopplerAudioHFP.conf`)
  must go to `audio.conf`. The root file system is read-only: this needs a deliberate
  `mount -o remount,rw /`, a backup copy and the way back to `ro` — only once the
  chip is up. Which of the two variants (`doppler`, `pancake`) belongs to the hardware is shown by the
  trial.
- **Does oFono 1.5 handle the HF role** the way we need it (dialing via D-Bus,
  call status)? Unverified.
- **How long does the HFP setup take?** For "just for this call" this is the pause before the
  ringback tone.
- **Does the phone show** a notification on every connect?
- **Is `btmd-ng` needed?** Amazon's manager probably does things we do not want; try
  without it first.

## The tools

| File | Purpose |
| --- | --- |
| [`hardware/gen1/tools/bt-try.sh`](../hardware/gen1/tools/bt-try.sh) | Commissioning in stages, dead man's switch, WLAN measurement, rollback |
| [`hardware/gen1/tools/bt-agent.py`](../hardware/gen1/tools/bt-agent.py) | Pairing agent for the trial: visible for a limited time, number comparison on the console |

In the finished feature the assistant says the number over the speaker and hears the answer — that is
the only difference from the agent here. Everything else (when HFP is up, which line a
call takes) is in #51.

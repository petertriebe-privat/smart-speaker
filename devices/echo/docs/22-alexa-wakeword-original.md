# Alexa's original wake word on the Echo Gen 1

As of 2026-10-22. How the original Amazon system of the Echo Gen 1 ("Doppler") detects its wake word, and what the assistant can take over from it (#78). Read from the archived EchoRoot v0.8 image (`local/archive/echo-diagnostics/images/`, see [boot-and-root.md](../hardware/gen1/docs/boot-and-root.md)), which contains the Amazon file system as of October 2020. Only read, nothing executed. Amazon's files (models, configurations, library) do not go into the repository; only the findings are written down here.

## How to read the image

Partition 2 of the SD image is an ext3 root. 7-Zip extracts the partitions but does not open ext3; `debugfs` from WSL reads it without mounting:

```sh
"/c/Program Files/7-Zip/7z.exe" e -o<scratch> echoroot-v0.8-sdcard.img 1.img   # the root partition
wsl -e debugfs -R "ls -l /usr/local/data/models/keyword/doppler/de-DE/ALEXA" /mnt/c/<scratch>/1.img
wsl -e debugfs -R "cat /usr/local/data/models/keyword/doppler/de-DE/ALEXA/pryon.config" /mnt/c/<scratch>/1.img
```

`grep -a` over the whole 2 GB image also finds the terms, but a regex with context takes longer than 5 minutes.

## Structure

- **Service:** `/usr/local/bin/asrd` ("Doppler Automatic Speech Recognition Signal Processing Daemon", Upstart `/etc/init/asrd.conf`) with `/usr/local/lib/libpryon.so`. Pryon is Amazon's own Kaldi-based engine (FSTs, lattices, HMM). Later devices and third parties get the slimmed-down variant "Pryon Lite".
- **Input:** `asrd` reads **`/BMicsOut.shm`**, the same signal as the assistant: the DSP output with beamforming and echo cancellation. This confirms the decision from #61 to train our own model on `BMicsOut`. `asrd` writes the results (start and end of the word in the buffer) to `/BAsrMetadata.shm`, so that the audio from shortly before the word is sent to the cloud.
- **DSP** (`/usr/local/etc/audioproxyd/DSP.cfg` in the image; whether the `audioproxyd` started by the assistant reads the same file on the device has not been checked): 6 beams, echo cancellation on (tail 96, delay 640), double-talk detection on, **noise reduction off** (`NR Bypass: true`). The Bluetooth profile, in contrast, uses 1 beam and noise reduction on.
- **Models:** `/usr/local/data/models/keyword/doppler/<locale>/<WORD>/`, for de-DE, en-GB, en-US and es-US each ALEXA, COMPUTER, ECHO, AMAZON. At startup `updateAsrdDynConfig.sh` picks, by `device.locale` and the chosen word, the `pryon.manifest` that points to `pryon.config`.
- **Also:** `/usr/local/data/models/AED/`, a separate small network for sounds (presence, smoke alarm, glass break) with its own thresholds and near-miss thresholds.

## Two-stage detection

A model folder (de-DE/ALEXA) is about 2.8 MB:

| File | Content |
|---|---|
| `pryon.config` | Frontend, scorer, search (key = value) |
| `dnn_quantized.mlp` (~1 MB) | quantized acoustic network |
| `final.mdl`, `pdf.counts`, `train_glob.cmvn` | Kaldi transition model, priors, global normalization |
| `ALEXA.fg.hclg.pfst` / `ALEXA.bg.hclg.pfst` | search FST for the word (foreground) and for "everything else" (background) |
| `STOP.fg/bg.hclg.pfst` | the same for the second keyword STOP |
| `ALEXA.psvm` (~1.8 MB), `ALEXA.scales` | second stage: SVM and the normalization of its features |
| `kw.cfg.json` | thresholds and flow per keyword |
| `op.cfg.json` | state machine (when which word counts) |
| `phones.txt` | 6 phones for ALEXA ("a l E k s a"), 4 for STOP ("S t O p"), speech/silence |

1. **Frontend:** 20 log-mel bands (LFBE) from 100 to 6000 Hz, 25 ms window, 10 ms step, running mean normalization (decay 0.003), 20 frames stacked on the left and 10 on the right (310 ms context). Similar to the microWakeWord frontend (40 bands, PCAN).
2. **First stage:** The network scores the frames, and a Kaldi keyphrase search (beam 25) continuously compares the foreground FST ("that was ALEXA") with the background FST. The network computes only every second frame and interpolates in between; with a backlog over 300 ms only every fifth frame (saving compute on the single core).
3. **Second stage:** Every candidate goes to an SVM. Its ~420 features describe the whole segment: duration, values per phone, confidence statistics, log energy in 20 bands, a speech/silence estimate. It decides whether it was really the word.

## Three thresholds instead of one

Both stages have three thresholds each (`kw.cfg.json`):

| Threshold | Meaning | ALEXA, SVM stage |
|---|---|---|
| accept | the device decides alone: awake | 0.1 |
| escalate | borderline: the cloud verifies the word | −0.4 |
| notify | near miss, only logged (`emit_nearmiss = 1`) | −2.0 |

Amazon describes the cloud verification publicly ([AVS: Wake Word Verification](https://developer.amazon.com/docs/alexa-voice-service/implement-ww-verification.html)): the device sends 500 ms of audio before the word, the word and the speech; if the cloud rejects the word, it closes the stream with `StopCapture`, without the user hearing anything.

**Depending on the device state** different thresholds apply: while music plays (`AudioPlayerState = 1`) or an alarm rings (`AlarmState = 1`), accept drops from 0.1 to −0.2. Echo cancellation then also removes part of the voice, as we measured.

Also: after a hit, 40 frames (400 ms) are blocked; per window of 100 frames at most 6 hits count. A "fingerprinter" is on: with it Amazon suppresses "Alexa" from advertisements and broadcasts through a database in the cloud.

## "Alexa, stop" without the cloud

STOP is a second keyword in the same model, with only the first stage (the SVM is "trivial") and without escalation (escalate and notify at 10³⁷). The state machine in `op.cfg.json`:

- `sleep`: only ALEXA counts, it leads to `awake`.
- `awake`: ALEXA stays in `awake`, STOP leads to `sleep`; after 175 units (presumably 1.75 s with 10 ms frames) back to `sleep`.

So "stop" only counts shortly after "Alexa": the narrow window keeps false triggers low, and "Alexa, stop" takes effect immediately, even without internet.

## Comparison with the assistant

| Alexa | Assistant (as of 2026-10-22) |
|---|---|
| Detection on `BMicsOut` | ✅ same signal |
| Lower threshold during music/alarm | ✅ `wake_cutoff_music`, `MOBILE_WAKE_CUTOFF` while the phone rings |
| Music gets quieter already at half the word | ✅ `HushFor` from 0.25 (`cmd/assistant/main.go`) |
| Send audio from before the word | ✅ preroll (`internal/live/live.go`) |
| Log near misses | ✅ `near` clips (`cmd/assistant/wakeclips.go`) |
| Re-check in the borderline range | partly: `_silent` only marks false starts after the fact (#78) |
| Second check stage on the device | ❌ one model, one threshold |
| Local "stop" | ❌ every quick command (#75) goes through the GPT Live transcript (#78) |
| Lockout after a hit | Reset and warm-up after every conversation, no separate lockout |

## What we take over (#78)

1. **Local "Jarvis, stop":** a second small microWakeWord model for "stop"/"pause", trained on `BMicsOut` with `tools/wakeword`, active only for about 2 s after the wake word. A hit pauses, interrupts or ends without a GPT Live session. The biggest gain: no path through the cloud, no GPT Live seconds for the most frequent command.
2. **Borderline range with re-check:** ~~two thresholds~~ – measured on 2026-10-22 and discarded: with v3 the false starts in a quiet room reach 0.85–0.97, exactly like real hits (0.85–0.96). The level in the clip does not separate them either (peak of false start 100–413, median 162; real median 253, about 21 dB above the noise for both): triggers come from real voices in the room, in the evening mostly TV (the fast detection heard, for example, sentences from a cooking show). A second threshold therefore does not help. What remains is Amazon's second half: ending a false start **silently and briefly** (implemented on 2026-10-22 on the Echo and M5, see [16-assistant.md](16-assistant.md#wake-word)). Today each one costs 13–26 s of GPT Live and audibly ends with the greeting ("Good evening! How can I …") when nobody speaks. Against the false starts themselves only training helps (v4 with these clips and TV recordings, `tools/wakeword`). Caution with the label `_silent`: twice, at 00:41 and 00:43, both Echos heard the same wake word (peer value 0.91), the command was lost and came again 20 s later. Those were real words, not false starts.
3. **Lockout** of about 400 ms after a hit: small, probably little effect.

Do not take over: Amazon's models and `libpryon.so` (license-protected, only for Amazon's words, not for "Jarvis") and the fingerprinter (needs Amazon's database).

## Sources

- The archived image (above), files under `/usr/local/bin/asrd`, `/usr/local/data/models/`, `/usr/local/etc/audioproxyd/`, `/etc/init/asrd.conf`, `/etc/default/audioproxyd`.
- [AVS: Wake Word Verification Requirements](https://developer.amazon.com/docs/alexa-voice-service/implement-ww-verification.html), [Streaming requirements of the cloud verification](https://developer.amazon.com/docs/alexa-voice-service/streaming-requirements-for-cloud-based-wake-word-verification.html)
- [VentureBeat 2017: Amazon verifies the wake word in the cloud](https://venturebeat.com/ai/following-snl-parody-amazon-adds-cloud-verification-of-alexa-wake-word)
- [XMOS: Pryon Lite](https://www.xmos.com/documentation/XM-014601-PC/html/doc/wakeword/WW_setup.html), [RDK wiki: Pryonlite](https://wiki.rdkcentral.com/x/NTbRCw)
- [Amazon Science: Metadata-aware end-to-end keyword spotting](https://www.amazon.science/publications/metadata-aware-end-to-end-keyword-spotting) (device state as model input)
- [Google: A Cascade Architecture for Keyword Spotting on Mobile Devices](https://arxiv.org/pdf/1712.03603) (the same two-stage principle)
- [Home Assistant forum: Alexa Local Voice Control](https://community.home-assistant.io/t/local-voice-control/296830) (later Echos: lights and sockets offline, only with a built-in Zigbee hub)

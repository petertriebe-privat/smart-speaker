# Training the wake word (#61)

The assistant's name is configuration (`ASSISTANT_NAME`, default "Jarvis"); the built-in wake word model is
"Hey Jarvis" (microWakeWord by Kevin Ahrendt / esphome). This directory trains an own model for the default
word "Jarvis"; for another word the spellings, sound-alikes and takes change, the pipeline stays.

An own microWakeWord model for "Jarvis" alone, trained on the household's voices as the Echo hears
them (BMicsOut, the DSP's processed signal) plus synthetic voices. Detection runs on the devices;
the PC only trains. Sample generation and features run natively on Windows; **the training runs in
Docker** (TensorFlow on the CPU: 10 000 steps take ~45 minutes on an i7-4790K, half of it the
validations, since 27.10.2026 every 1000 steps).

**Memory and CPU.** microWakeWord's validation loads the ambient set cut into overlapping windows
as one float array. With all 9.7 h of dinner_party_eval that was a 3.8 GB block and ~12 GB peak:
natively on Windows it froze the PC (20.10.2026), and in Docker with 20 GB and 6 cores the 32 GB PC
hung during a validation twice (22.10., 27.10.2026, no bluescreen, only Kernel-Power 41). Since
27.10.2026 `train.py` validates on half of it (`negative_datasets/dinner_party_eval_half`, made
once from the full set, the test set unchanged) every 1000 steps, the container gets 13 GB and 4 of
the 8 threads (`pipeline.sh`; over the limit Docker kills the container, not the PC), and
`C:\Users\<user>\.wslconfig` has `memory=16GB`, `processors=4`, `swap=0` (the swap file would lie
on C:; `wsl --shutdown` applies it). A run that broke off keeps its best weights:
`pipeline.sh <name> <steps> export` exports them without training (WSL peak 5.9 GB), and
`pipeline.sh <name> <steps> resume <done>` goes on from its last checkpoint (`restore/`, written at
each new best, so `<done>` is the step of `best_weights`, see `train/*_weights_<step>`): microWakeWord
restores weights and optimizer but counts from step 1 again, so `train.py` gives it only the steps
still missing of both phases. The PC hung once more with these limits (28.10.2026, step 21 000, in
plain training, not a validation); it had hung the same way on 16. and 17.10. before any training, so
the limits are not the whole cause (hardware under load is suspect). Resumed, the run finished in
an hour at 8.6 steps/s.

```sh
docker build -t jarvis-wakeword tools/wakeword
MSYS_NO_PATHCONV=1 docker run -d --name wakeword-v1 --memory 13g --memory-swap 13g --cpus 4 \
    -v E:/wakeword-work:/work -v <repo>/tools/wakeword:/tools jarvis-wakeword \
    python /tools/train.py /work v1 10000
docker logs -f wakeword-v1      # one line every 15 s: step, loss, recall, steps/s, time left
docker stats wakeword-v1        # memory and CPU
```

Household voices never go into the repository: the recordings live on the Echo
(`/mnt/data/jarvis/voices/`), in `local/wakeword/recordings/` (git-ignored) and in the work
directory `E:/wakeword-work` outside the repository. The trained model is kept out of the
repository too (it is trained on them). The augmentation datasets have mixed licences: models
trained with them are for personal, non-commercial use.

How the original Alexa engine on the Echo Gen 1 does it (two stages, three thresholds, a local
"stop" keyword) and what we take from it (#78): `devices/echo/docs/22-alexa-wakeword-original.md`.

## Once: the environment (about 35 GB on E:)

```sh
# Python 3.11 venv with uv, every cache on E: (C: is nearly full)
export UV_CACHE_DIR=E:/wakeword-work/uv-cache UV_PYTHON_INSTALL_DIR=E:/wakeword-work/python
cd /e/wakeword-work
git clone https://github.com/kahrendt/microWakeWord          # tested at 4665173
git clone https://github.com/rhasspy/piper-sample-generator  # tested at 2971426
uv venv --python 3.11 venv
uv pip install --python venv/Scripts/python.exe "tensorflow>=2.18" audiomentations audio_metadata \
    "datasets<4" mmap_ninja "numpy>=2" pymicro-features pyyaml webrtcvad-wheels ai-edge-litert \
    tensorboard "setuptools<81"
uv pip install --python venv/Scripts/python.exe --no-deps -e ./microWakeWord
uv pip install --python venv/Scripts/python.exe -e ./piper-sample-generator
uv pip install --python venv/Scripts/python.exe -U audiomentations   # the generator pins 0.33
venv/Scripts/python.exe <repo>/tools/wakeword/fetch.py E:/wakeword-work
```

Pitfalls found on 20.10.2026: `datasets` 4 decodes audio with torchcodec (FFmpeg DLLs on
Windows): keep 3.6. The generator's old `webrtcvad` needs `pkg_resources` (setuptools < 81).
`piper_train` lies next to the generator's package: `PYTHONPATH=<work>/piper-sample-generator`
(the scripts set it).

## Per person: the recordings

On the Echo, the person in the room: `jarvis ctl enroll <person> guided` (eight parts, ~15 min,
the assistant announces each; `guided <part>` goes on after an interruption). Then on the PC:

```sh
cd local/wakeword/recordings
ssh -F ../../ssh/config echo-dev 'cd /mnt/data/jarvis/voices && tar cf - <person>' | tar xf -
venv/Scripts/python.exe tools/wakeword/cut_takes.py <recordings> E:/wakeword-work/real
```

`cut_takes.py` finds every beep in the raw microphones and cuts the take after it from
`micsout.wav`; the stretches between takes and the free-talk parts become negatives.

## The model

```sh
venv/Scripts/python.exe tools/wakeword/spellings.py E:/wakeword-work core   # optional
venv/Scripts/python.exe tools/wakeword/generate.py E:/wakeword-work                 # ~2 h
sh tools/wakeword/pipeline.sh v1 10000          # features, training (Docker), export, evaluation
```

| Script | Does |
|---|---|
| `fetch.py` | Piper voices, the LibriTTS-R and MLS generators, microWakeWord's negative features, FMA music, MIT room impulses |
| `spellings.py` | which spelling makes each voice say "Jarvis": "Hey <spelling>" scored by the stock English model |
| `generate.py` | ~8 500 synthetic positives, ~3 500 sound-alike negatives ("Paris", "Davis", "darf ich", ...) |
| `cut_takes.py` | the real takes and negatives out of the guided recordings |
| `harvest.py` | the devices' wake clips: false detections as hard negatives, real "Jarvis" (a conversation followed) as positives |
| `cut_tv.py` | a long TV recording in the room as negatives (3 s pieces) and test blocks |
| `prepare.py` | microWakeWord's features: synthetic with full augmentation, real takes lightly (they carry the room and the DSP) and repeated 10x |
| `train.py` | mixednet as in microWakeWord's notebook, int8 streaming export, manifest `jarvis.json` |
| `evaluate.py` | the Echo's Go detector (`cmd/wakeeval`) over whole sessions: takes heard and false detections per cutoff |
| `reference.py` | LiteRT reference data for `go test ./internal/wakeword -run Reference` with a new model |

Spellings (20.10.2026): German Piper voices say "Jarvis" as "Jarwis"; spelled "Dschaarvis" or
"Dschahrwis" they score 0.8-1.0. The English generator says "Jarvis" itself (1.00). The MLS voices
babble (3.7-4.8 s for two words) and are left out.

## From real use: false detections and real hits (v3)

The Echos keep 3 s before every detection and near miss (`state_dir/wake/`). After a few days of use
they are the best data there is: what fired the model in the room, and real "Jarvis" in the room's
sound. v3 (21.10.2026) was v2 plus these; on held-out clips at cutoff 0.85 the false detections fell
from 18/82 to 2/82 and the real words heard rose from 8/15 to 12/15.

v4 (23.10.2026) added the clips of 21./22.10.: false starts during video meetings in the study and
a phone call in the living room, and the real "Jarvis" that followed requests. Verdicts by rule
(a request followed -> positive, meeting clips heard by two Echos at once -> negative). Held-out
field clips at cutoff 0.85: false detections 15/139 (v3) -> 6/139 (of 22.10.: 12/33 -> 4/33), real
"Jarvis" 24/28 -> 22/28; guided takes at 0.80: 136/183 -> 120/183, with music 11/32 -> 7/32. In use
since 23.10.2026 on every device at cutoff **0.80** (as few false starts on the field clips as at
0.85), v3 kept as `wakemodel/jarvis-v3.tflite`. Music is the weak case for v5.

v5 (26.10.2026): v4 ran at 0.80 from 23.10. to 25.10. with ~120 starts nobody asked for against ~16
real ones, most of them heard by one device only (peer 0) and many in a silent room: on the Dot
six between 01:49 and 05:38 at an RMS of 12-19 (int16, the microphone's noise floor; real calls on
the Dot 100-230). The score of these was 0.80-0.97, so a higher cutoff would not have helped. v5 is
v4 plus the clips up to 25.10. (911 hard negatives instead of ~330, 156 field positives), 30 000
steps. Held-out field clips (wakeeval, the Go detector) at 0.80: false detections 29/226 -> 23/226
(of 23.-25.10.: 23/84 -> 12/84, the silent ones 10/15 -> 4/15), real "Jarvis" 31/37 -> 36/37;
guided takes 120/183 -> 128/183, with music 7/32 -> 13/32. In use since 26.10.2026 on the three
Echos and the M5, cutoff still 0.80, v4 kept as `wakemodel/jarvis-v4.tflite`.

v5 in the living room was a step back: 61 starts nobody asked for on 26.10. (v4: 14-28 a day) and
232 near misses (v4: 33-71), many at a level of RMS 6 / peak 32. Over that day's clips v4 fired on
4, v5 on 30; Echo 1 went back to v4 the same evening. The scores of real calls and false starts
overlap fully (0.80-0.98): a cutoff of 0.88 on Echo 1 would have dropped 120 false starts to 44
and 12 real calls to 6, so no higher cutoff. A gate on the word's rise over the room's noise
floor does not separate them either (false starts 5-37 dB, real calls from 14 dB).
v6 (27.10.2026): v5's data plus the clips up to 26.10. (1251 hard negatives, 235 of them from Echo
1 on 26.10.), and the ten faintest field positives (peak < 150, a call from another room that the
device only just heard) left out: they teach a device to wake on near silence. The run hung the PC
at step 33 500 of 45 000; v6 is its best weights (`export`). Compare models **per device**: the
held-out clips (wakeeval, 0.80; false of all / of 26.10. / real):

| | v4 | v5 | v6 |
|---|---|---|---|
| Echo 1 (163 / 59 / 10) | 13 / 5 / 8 | 26 / 13 / 9 | **5 / 1 / 9** |
| Echo 2 (51 / 11 / 18) | 5 / 0 / 15 | 5 / 2 / 18 | **1 / 0 / 18** |
| Dot (97 / 14 / 10) | 17 / 0 / 9 | **7 / 0 / 10** | 6 / 0 / 7 |

Guided takes at 0.80: v6 110/183 (v5 128), but 9 false in all (v5 17). In use since 27.10.2026:
v6 on Echo 1 and Echo 2, v5 on the Dot and the M5 (each Echo keeps the others as
`wakemodel/jarvis-v<n>.tflite`).
v7 (28.10.2026): v6's data, the full run (broke off at step 21 000, resumed from 16 000). Guided
takes at 0.80: 140/183, 9 false in all; but held out it wakes more often: Echo 1 15 / 2 / 10, Echo 2
3 / 0 / 18, Dot 7 / 0 / 8. Not deployed: false starts are the problem, so v6 stays. The extra phase-two
steps did not help against room talk.

The Go detector is not bit-exact with LiteRT on quiet input: on the silent clips 1-24 of 99
inferences differ, by up to 63/255 with v4 and mostly up to 7 (at most 22) with v5, in both
directions (detections at 0.85: v4 Go 7/15, LiteRT 6/15; v5 4/15 each). The features match exactly;
the cause is the requantization in float against double (`wakeword_test.go`). Run the reference test
with `go test -count=1`: a cached result for the same `WAKEWORD_REF` directory hides a new model.

A training interrupted by a crash started again at step 0 although `train.py` asks to restore the
checkpoint. Nothing heavy next to it: with WSL at 22 GB, a firmware build and the Go tests
alongside took the 32 GB PC down (22.10.2026). The validation also counts against Windows' commit
limit (RAM + the page file on C:): with browsers and other apps open, 37 of 42 GB were committed
and the WSL VM died at the first validation (step 500, Docker "unexpected EOF", 25.10.2026). Check
the commit charge before starting (`Get-CimInstance Win32_OperatingSystem`:
TotalVirtualMemorySize - FreeVirtualMemory): leave the WSL limit (16 GB) free.

```sh
# clips and conversation records of every device (Echo: /mnt/data/jarvis, Dot: /data/jarvis)
ssh -F devices/echo/local/ssh/config echo-dev 'cd /mnt/data/jarvis/wake && tar cf - .' | tar xf - -C E:/wakeword-work/clips/echo-dev
ssh -F devices/echo/local/ssh/config echo-dev 'cat /mnt/data/jarvis/memory/sessions.jsonl' > E:/wakeword-work/sessions/echo-dev.jsonl
python harvest.py E:/wakeword-work "local/secrets.h" 5 <sessions.jsonl,...> <clips_dir> ...
# decide every clip in hard/review.csv (clip,verdict: positive|negative|skip -> hard/verdicts.csv), run again
# TV in the room, nobody says "Jarvis": jarvis rawcapture 600 /mnt/data/jarvis/raw/tv-<date>, then
python cut_tv.py E:/wakeword-work <micsout.wav>
sh tools/wakeword/pipeline.sh v<n> 20000
```

- Since 22.10.2026 every hit has a record next to it (`<clip>.json`: peer score, who answered,
  whether GPT Live heard the user, the quick command done, tools, seconds; the tar above copies it
  along). `harvest.py` labels those hits itself and transcribes only what it cannot decide: a
  request done (here or on the Echo that answered the same word) -> positive; nobody spoke, no other
  Echo heard it and no request on any device within 60 s -> negative; everything else, and a near
  miss up to 10 s before a real hit (the user's first, missed "Jarvis"), -> review, with the record
  in `review.csv`. `verdicts.csv` still overrides. Older clips without a record go the way below.
- `harvest.py` transcribes each clip once (gpt-4o-mini-transcribe, ~0.10 $ for 550 clips) and matches
  it with the conversation it started. A clip is a positive only when a real conversation with a
  request followed: after a false start people say "Jarvis" too ("was will der jetzt?"). Clips with a
  "Jarvis" or a conversation wait for a verdict; all others are negatives. Every fifth clip of a device
  is held out (`hard/test`, `hard/positive_test`), spread over all days: a time holdout kept the
  latest TV evening, the worst case, out of the training.
- The transcriber writes "Jarvis" as "Chavez", "Charles", "Travis", "Čavuš" ...: the verdict needs
  the conversation, not the transcript alone. The assistant's Bluetooth announcement "mit Jarvis
  Schlafzimmer verbunden" is left out.
- Compare models with `wakeeval` on `hard/test` (false detections) and on `hard/positive_test` with
  0.5 s of silence appended (the clips end at the old model's detection).

## On the devices

- The Echo: copy `models/<name>/jarvis.tflite` and `jarvis.json` to `/mnt/data/jarvis/wakemodel/` (not `wake/`:
  the clips there are pruned to the newest 400) and
  set `"wake_model": "wakemodel/jarvis.tflite"` in `device.json` (a model that does not load falls back
  to the built-in "Hey Jarvis"). Current microWakeWord models use SPLIT_V, which the Go interpreter
  has since 7d40935.
- Check a model against LiteRT before using it: `reference.py` and the Go reference test (with
  `-count=1`, on a loud and a quiet clip).
- The M5 registers SPLIT_V already; the model goes in as a header (`devices/atoms3r/models/make_model_header.py`).

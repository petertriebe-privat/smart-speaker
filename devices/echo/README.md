# The assistant on Amazon Echo devices

Old Amazon Echos get a second life: we replace Alexa's userspace with our own small Linux setup
and run the voice assistant as one Go program. It talks directly to OpenAI GPT Live and offers
the same tools and persona as the M5 version ([devices/atoms3r](../atoms3r/)). Several Echos in one
home work together as one assistant. The assistant's name is configuration (`ASSISTANT_NAME`,
default "Jarvis"; see [Your own assistant](../../README.md#your-own-assistant)).

| Model | How it is unlocked | Setup guide |
|---|---|---|
| Echo Gen 1 (tall cylinder, 2015/2016) | Soldered SD card adapter and UART on the test pads (EchoRoot) | [hardware/gen1](hardware/gen1/README.md) |
| Echo Dot 2nd gen (RS03QR, "biscuit") | Over USB, no soldering (amonet bootrom exploit, TWRP) | [hardware/dot2](hardware/dot2/README.md) |

The Echo Dot Gen 1 has been looked at but not unlocked:
[docs/19-echo-dot-gen1.md](docs/19-echo-dot-gen1.md).

## Layout

```
assistant/       the Go program for every Echo model; internal/board picks the model's drivers
tools/           deploy.sh (all models), librespot build, server skill (openhands/)
docs/            the assistant on the Echo, several Echos, Bluetooth, the original wake word
hardware/gen1/   Echo Gen 1: system scripts (src/), config/, bring-up tools and docs
hardware/dot2/   Echo Dot 2: own Linux (init, network, services), unlock and boot tools, docs
local/           git-ignored: SSH profiles, WiFi, keys, device settings, persona, images, backups
```

On the Echo the program lives in `/usr/local/echo-dev/assistant/assistant` (the Dot:
`/data/echo-dot-dev/assistant/assistant`), its state in `/mnt/data/assistant`, its secrets in
`assistant-secrets.json` (made by `tools/make_secrets.py` from `devices/atoms3r/include/secrets.h`)
and it is started by `services/assistant.sh`. The command line tool is `assistant ctl …`
(`assistant rawcapture …` records raw microphones). An own wake word model goes to
`/mnt/data/assistant/wakemodel/` (see [tools/wakeword](../../tools/wakeword/README.md)).

## Deploying

```
tools/deploy.sh --all             # every Echo in local/devices/*.json
ECHO_HOST=dot-dev tools/deploy.sh # one device
```

The script runs `go vet` and `go test`, extracts the shared prompts
(`shared/prompts` → `core/internal/prompts/prompts.json`), builds a static ARMv7 binary and
installs it with the model's files. Per-device settings (role, room, Spotify name) live in the
git-ignored `local/devices/<host>.json`.

A merge into `main` can deploy on every Echo through a CI job on a Raspberry Pi
(`services/deployer`); the CI pipeline itself is set up per repository and is not part of this
repository yet.

**First deploy from this repository.** An Echo installed under the earlier names (everything
called `jarvis`) is migrated once by the installer: it moves the folders and the data to the
`assistant` names. The Gen 1 reboots once; the Dot reboots once for the new service. Later
deploys do not.

For a household's own persona, run `py -3 core/tools/extract_prompts.py --persona`: it writes
`local/assistant/persona.json`, which the deploy puts into the Echo's state directory as
`persona.json`.

## Documents

| Document | What it covers |
|---|---|
| [docs/16-assistant.md](docs/16-assistant.md) | The assistant on the Echo: architecture, control commands (`assistant ctl`), measurements, lessons |
| [docs/18-several-echos.md](docs/18-several-echos.md) | Several Echos as one assistant: leader, rooms, shared state, failover |
| [docs/17-bluetooth.md](docs/17-bluetooth.md) | Bluetooth on the Echo Gen 1 |
| [docs/22-alexa-wakeword-original.md](docs/22-alexa-wakeword-original.md) | How Amazon's own engine on the Echo Gen 1 detects its wake word |
| [hardware/gen1/docs](hardware/gen1/docs/hardware.md) | Echo Gen 1 by topic: hardware, boot and root, recovery, network, audio, known issues (dated originals in `docs/history/`) |
| [core/PORTING.md](../../core/PORTING.md) | How the M5's C++ maps to the Go packages |

The Echos only run our own software thanks to the people who opened them first: see
[Thank you](../../README.md#thank-you).

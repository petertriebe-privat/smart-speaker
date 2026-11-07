# Working in this repository

## Where things are

```
core/                the assistant's Go program (module echo-assistant): runs on the Echos, the Fire TV
                     stick and the phone app; boards in internal/board
devices/atoms3r/     the M5 AtomS3R with the Echo Pyramid (PlatformIO, C++)
devices/echo/        Amazon Echo models with their own Linux: deploy in tools/, settings.json, per model
                     hardware/gen1 and hardware/dot2
devices/android/     the Android apps (Gradle, Java): shared/ (HUD, hologram, cards, overlay, full
                     screen), firetv/ (a screen for every speaker, and an assistant of its own running
                     core/), phone/ (the phone app with Android Auto)
shared/prompts/      persona.h, prompts.h, assistant_name.h: one source for every device (core embeds
                     prompts.json, made by core/tools/extract_prompts.py)
services/            servers next to the devices: deployer, home-gateway, mcp-servers, agent-mcp,
                     openhands (the server agent's skill), whatsapp-relay
tools/               cross-device tools (wake word training, Google and Spotify login, OpenAI costs,
                     archive migration, github-settings.sh for the repository)
local/               git-ignored: the household's own values for every device (secrets.h, persona,
                     wake word, home gateway); only README.md and the *.example templates are tracked
```

- Open work, ideas and bugs: GitHub issues of this repository (`gh issue list`), not a file in
  the repo and not the old private `m5-stack`. Issues are public: English, no household data.
- Design, measurements and lessons learned: `devices/atoms3r/docs/` (`architecture.md`, `ram.md`,
  `google.md`), `devices/echo/docs/` (`16-assistant.md`, `17-bluetooth.md`, `18-several-echos.md`)
  and the hardware docs per Echo model. Update them after a change that matters; add new findings
  there, not in commit messages only.
- The repository is public: no household data (names, addresses, phone numbers, e-mail addresses,
  IPs, specific calendars or devices) in code, prompts, tests, docs or commit messages. Household
  values belong in the git-ignored `local/` (every device: `secrets.h`, `persona_local.h`, the
  wake word, the home gateway; `local/README.md`) and `devices/echo/local/` (the Echos only: SSH
  profiles, device files, builds); behaviour must come from generic data.

## The assistant's name

Jarvis is the default, and the name is configuration (`ASSISTANT_NAME` in `secrets.h`). Where a
user sees or hears the name, use `ASSISTANT_NAME` (C++, `shared/prompts/assistant_name.h`) or
`prompts.Name()` (Go); Home Assistant events and entities take `prompts.Slug()` / `name::slug()` as
prefix (`jarvis_…` by default). What devices, the Pi and Home Assistant already use keeps its name
whatever the assistant is called: paths (`/mnt/data/jarvis`), the binary (`jarvis ctl`), the apps
(`dev.jarvis.*`), MQTT topics (`jarvis/…`), secrets switches (`JARVIS_*`), the Pi's deployer and
`jarvis-diag`. New code, folders and files use neutral names (`assistant`), and prose says "the
assistant" (it/its). The persona in `shared/prompts/persona.h` is the default (Jarvis); another
character goes into `persona_local.h` (M5) and `persona.json` in the Echo's state directory
(`core/tools/extract_prompts.py --persona`).

## Several devices, one assistant

The assistant runs on the Amazon Echos (`devices/echo/`, Go in `core/`), the AtomS3R
(`devices/atoms3r/`, C++), the Fire TV stick and the phone (`devices/android/`, also `core/`).

- **The repository is the truth.** Every change (code, prompts, scripts, device configuration
  that is not secret) is made here and committed; nothing is changed only on a device. Start from
  the current `main` (`git pull`). Never leave a device running code the repository does not have.
  Commits and pushes only with the owner's ok. Secrets stay out of the repository.
- **All devices in mind.** A new feature or fix on one device is checked for the others: port it
  in the same change if it fits, with the other device's limits (the M5: 512 KB internal RAM,
  8 MB PSRAM, one TLS connection at a time, prompt and tool-list size; the Echo Gen 1: 190 MB RAM,
  one ARMv7 core). If it does not fit now, or is a separate piece of work, open an issue for the
  port that says what fits and what not. Shared prompts (`shared/prompts/prompts.h`) must not
  promise a tool one device lacks: device-only abilities go in a device-only prompt (e.g.
  `LIVE_ECHO_ABILITIES`).
- **Tests before a deploy or flash.** `core/`: `go vet` and `go test` (the deploy script runs
  them), then tools directly with `jarvis ctl tool …` and spoken input with `jarvis ctl
  inject`. The M5: `pio run -e atoms3r`, the serial suite `devices/atoms3r/tests/device/run_tests.py`
  and the tool golden file. The services: their `test_*.py`. A new tool gets a test. Real GPT Live
  conversations and paid APIs only sparingly, after the free checks.
- **CI.** `.github/workflows/ci.yml` runs every check that needs no device, for what a change
  touches, on pull requests, pushes to `main` and by hand; `ci-ok` is the required check. Keep it
  green: run the same tools before a push (gofmt, `staticcheck` with `core/staticcheck.conf`,
  `ruff check .`, `shellcheck -S warning`, `gradlew lint`, `pio check -e atoms3r`). Lint
  exceptions are made in the config with a reason, not by a baseline. No workflow ever runs on a
  runner in the household for a pull request.
- **Issues and docs.** Open work goes into GitHub issues; findings into the device's docs and,
  when the work is done, a comment on the issue. Every new, changed or removed feature also goes
  into the feature list of the root `README.md` (by area, *(Echo)* / *(M5)* where only one device
  has it), in the same commit, without being asked.
- **Deploying.** The Echos: `devices/echo/tools/deploy.sh` (`--all` for every Echo, `--secrets`
  for new secrets). The M5: `pio run -e atoms3r -t upload` in `devices/atoms3r/`, one serial user
  at a time. The Fire TV: `devices/android/tools/install.sh <host> <room> [--core]` (builds core/
  for ARMv7 into the APK; the stick's log: `adb shell run-as dev.jarvis.screen cat
  files/run/jarvis.log`). A change in core/ goes to the Echos, the sticks and the phone.

## Working on the Echo hardware

Read `devices/echo/README.md` and the model's docs (`hardware/gen1/docs/` or
`hardware/dot2/docs/`) before operating a device.

- The Echo Gen 1 is a 32-bit ARMv7 Linux 2.6.37 device with 256 MiB RAM; the Dot 2 runs its own
  Alpine-based Linux. Development tools run on the PC and reach the devices over SSH (the private
  profiles in `devices/echo/local/`); never install coding agents on an Echo.
- Device addresses, Wi-Fi credentials, host keys, developer keys and eMMC backups are private: do
  not print, commit or upload them, and never disable host-key checking.
- Prototype under tmpfs (`/mnt/echo-net` on the Gen 1), verify hashes after copying. The Gen 1's
  root is read-only; our software lives in `/usr/local/echo-dev`, supervised by our own init and
  watchdog. Restart with `/usr/local/echo-dev/bin/echo-reboot`, never with stock init signals.
- Never probe uninitialized direct ALSA playback on the Gen 1 (it caused a kernel Oops): audio goes
  through the DSP/shared-memory/PulseAudio chain (`hardware/gen1/docs/audio.md`), with exactly one
  PulseAudio instance writing the speaker queue.
- UART is the physical recovery path (1.8 V logic, adapter VCC disconnected); one serial-port owner
  at a time; commands run immediately as root. Identify storage by capacity and partition table,
  not by `mmcblk` number. Never run the historical EchoRoot installer, `saveenv`, partitioning or
  internal `dd` as a diagnostic step; before any write to internal storage, name the exact target
  and keep the boot and calibration data and the SD/UART recovery route.
- Keep facts, hypotheses and experiments apart, and document each material finding in the model's
  docs as you go.

## Delegating to cheaper models

Simple, self-contained tasks may go to subagents on cheaper models (Agent tool, `model`
parameter).

| Model | Use for |
|---|---|
| `haiku` | Searching and locating code, reading long logs or serial output and extracting the relevant lines, summarizing a file or a diff, listing call sites, mechanical checks (does every tool have a golden entry, are all strings English) |
| `sonnet` | A clearly specified change in one or two files with a known pattern (add a config constant, a tool following an existing one, adjust tests or the golden file), updating the docs or an issue from facts you give it, writing a helper script |
| stay in the main model | Design decisions and trade-offs, anything that needs the conversation's context, prompts and persona texts, changes across many modules, debugging with unclear cause, anything the user reads directly, reviewing a subagent's result |

- Delegate only when the task fits in a self-contained prompt: files, goal, constraints, what to
  report back. If explaining it costs as much as doing it, do it yourself.
- Always check the result (read the diff, build) before building on it.
- Never delegate: flashing, the serial port (one user at a time, so no parallel test runs either),
  commits and pushes, questions to the user.
- Subagents follow the same rules: code, logs, tool results and prompts in English; no code in
  shell heredocs (write script files or use an editor).

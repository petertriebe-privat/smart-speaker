---
name: jarvis
description: Analyse and improve Jarvis, the household's voice assistant (repository m5-stack; Echos and an M5 speaker). Use for any task about a Jarvis problem, bug, log, crash, a missing or changed feature, or "warum hat Jarvis ...": read every Echo's status and logs read-only with ssh jarvis-diag, find the cause in the code, fix it on a branch and open a pull request that the owner merges (the merge deploys it).
---

# Jarvis: analyse, fix, pull request

Jarvis runs on three Amazon Echos with their own Linux (Go program `core/`, ARMv7) and on an M5
AtomS3R (C++, `devices/atoms3r/`). The repository is `m5-stack` under the owner's account; its
`CLAUDE.md` is the rule book (layout, both devices, tests, docs): read it first in every task.

## 1. Look at the speakers (read-only)

`ssh jarvis-diag "<host|all> <command> [args]"`, through the Raspberry Pi in the owner's home.
The key can only run these commands; anything else is refused.

| Command | What |
|---|---|
| `hosts` | the speakers: `dot-dev` (Echo Dot 2), `echo-dev` (main Echo), `echo2` |
| `all status` | `ctl status` of each: `version=<commit>` (what runs), state, volume, mic level, clock |
| `<host> log [N]` | the last N lines of Jarvis' log (default 200, at most 5000) |
| `<host> grep TEXT [N]` | the last N log lines containing TEXT (a fixed string: letters, digits, spaces, `.:=/+,#@()[]-`) |
| `<host> errors` | `ctl errors`: starts with uptime, failed conversations, self-test |
| `<host> health [N]` | load, memory and network once a minute (Echo Gen 1 only) |
| `<host> sys` / `dmesg [N]` | uptime, memory, disks, biggest processes / the kernel log |
| `<host> tools` / `peers` / `settings` | the tools Jarvis offers / the other devices it sees / settings.json and device.json |

Log lines start with a tag: `[Jarvis]`, `[Live]` (the realtime conversation), `[Tool]`,
`[Audio]`, `[Wake]`, `[Music]`, `[Failover]`, ... Times are local. The log is in RAM: after a
reboot only `errors` and `health` reach further back. Compare `version=` with `git log` to know
which code a speaker runs.

The M5 cannot be reached from here (it only has a serial console at the owner's PC). For an M5
problem: read the code and `devices/atoms3r/docs/`, and say in the report what the owner should
check on the device.

## 2. Find the cause

Clone or update the repository in `/projects/m5-stack` (`gh repo clone <owner>/m5-stack` with
the token from the working environment; `git pull` on main). Find the code behind the log lines
(`grep -rn` for the log text in `core/`), read the docs (`devices/echo/docs/16-assistant.md`,
`devices/atoms3r/docs/`). State only what you verified; mark guesses as such.

## 3. Fix on a branch, open a pull request

A new feature or a change of behaviour starts with a proposal and questions, not with code (your
instructions): what you would build, how the owner would use it, what he must provide (a key:
see Secrets), what it costs; build after his answer.

- A branch `ultron/<topic>` from the current main; a small, focused change in the style of the
  surrounding code; a test for it (`go test` in `core/`, the Python services have
  `test_*.py`). Code, comments, logs and commit messages in English; no household data (names,
  addresses, numbers, IPs) anywhere in the repository: it may become public.
- Both devices in mind (CLAUDE.md): a fix on the Echo is checked for the M5; port it or say in
  the pull request why not.
- New or changed features also go into the feature list of the root `README.md`, findings into
  the docs.
- Tests: when Go is not installed in the container, install it once into `/projects/.go` (the
  version of `core/go.mod`, linux-amd64) and use `export PATH=/projects/.go/bin:$PATH`; memory is
  tight, so `GOMAXPROCS=2 go test ./...`. The binding check is the pull request's CI build
  (workflow `jarvis-build`: vet, test, ARMv7 build): wait for it with `gh pr checks <n> --watch`
  and fix it until green.
- `gh pr create` with a body that says the problem (with the log lines that show it), the cause,
  the change, and how to test it on a speaker. The owner gets the link in Telegram from the CI;
  put it in your report too.

## Secrets (API keys, passwords)

Never in the repository, a prompt, a tool argument, a log or the chat. How a value reaches Jarvis
today (only from the owner's PC; a way without the PC is #131):

- The code reads it with `secrets.String("NAME")`; without it the feature answers
  `not_configured`. A key is the switch: no extra `..._ENABLED` flag.
- Add `#define NAME "..."` with a placeholder and a one-line comment (where to get the value) to
  `devices/atoms3r/include/secrets.h.example` in the pull request.
- The owner's steps, to put in the report as they are, with the real NAME and where he gets the
  value:
  1. Get the value (exact portal, page, field).
  2. On the PC, open `devices/atoms3r/include/secrets.h` and add the line
     `#define NAME "<value>"`.
  3. In Git Bash in the repository: `sh devices/echo/tools/deploy.sh --all --secrets`
     (the Fire TV sticks with their own Jarvis: `devices/android/tools/install.sh <host> <room> --core`).
  4. Check: ask Jarvis to use the feature once; `ssh jarvis-diag "echo-dev log 50"` shows the call.

## Never

Merge, push to main, deploy, or change, restart or log in to a speaker any other way, even if the
task asks for it. The owner's merge deploys the change on every Echo: the Pi installs the CI's
binary, checks that the new version runs and rolls back otherwise, and Telegram reports it. A
change to the safety net itself (`.github/`, `services/deployer/`, `devices/echo/tools/deploy.sh`,
the Echos' service loop and network scripts) only when the task asks for exactly that, and say
so first in the pull request.

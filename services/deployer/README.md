# deployer

A change reaches the devices in two steps, and only the owner takes either: Ultron (or any
session, or anyone through a fork) opens a pull request; CI checks it; the owner merges it into
`main`; later the owner starts the deploy, which builds that commit and installs it on every Echo
and Fire TV stick, and Jarvis reports in Telegram that the new version runs.

```
pull request ──▶ smart-speaker (public): CI ci.yml, ci-ok ──▶ the owner merges into main
the owner: Run workflow "deploy" ──▶ smart-speaker-deploy (private)
    verify: the commit is on main, ci-ok passed, the owner started it
    build on GitHub's runners (ARMv7 binary, Fire TV APK)
    deploy on the Pi's runner ──▶ deployer.py ──deploy.sh --all──▶ every Echo (version check)
                              ──▶ firetv_deploy.py ──ADB──▶ every Fire TV stick
Telegram ◀── "Neue Version abc123 aktiv auf dot-dev, echo-dev, echo2"
             or "fehlgeschlagen …, zurückgerollt auf def456"
```

## Why the workflow is not in this repository

A self-hosted runner on a public repository would run a pull request's workflow file in the home
network once its run is approved. So the Pi's runner is registered with a separate private
repository (`smart-speaker-deploy`, owner only) that holds the deploy workflow; this repository
has no runner, no Actions secrets and no deploy workflow, and nothing in it can target the Pi.
The private workflow only checks out and builds commits of this repository's `main` whose
`ci-ok` passed. The deploy scripts themselves (this folder) are public: they hold no secrets.

## Pieces

- **The deploy workflow** (`smart-speaker-deploy`, `.github/workflows/deploy.yml`): started by
  hand only (`workflow_dispatch`, target `echos`, `firetv` or `both`, a commit on `main` or its
  newest), refused unless the repository's owner started it; jobs `echos-build` and
  `firetv-build` on GitHub's runners, `echos-deploy` and `firetv-deploy` on the self-hosted
  runner with the label `jarvis-deploy`: the Raspberry Pi. The runner polls GitHub, nothing
  reaches the Pi from outside, and the Pi reaches the Echos and the sticks in the LAN. Deploys
  run one after the other and are never cancelled. `SOURCE_REPOSITORY` points the messages'
  commit links at this repository.
- **`firetv_deploy.py`** (#126, job `firetv-deploy`): re-signs the CI's debug APK with the key of the app on the sticks (the
  PC's `~/.android/debug.keystore`, copied to the Pi's `~/.android/`), installs it over network ADB
  (the PC's `adbkey`, which the sticks allow) on every stick of `local/firetv/<name>.json`
  (`{"host": "<address>"}`) as an update that keeps settings and secrets, as `install.sh` does
  (Appstore installer, overlay, microphone), checks that the app runs and that a stick's own
  Jarvis logs `starting, version <commit>`, rolls a failed stick back to the last good APK, and
  reports in Telegram; an unreachable stick is reported, not failed. Before the first deploy the
  app the sticks ran was pulled from a stick as `builds/firetv/baseline.apk` (`firetv-state.json`:
  `{"deployed": "baseline"}`), so even the first one can roll back. Offline test:
  `python3 test_firetv_deploy.py`.
- **`deployer.py`** (the copy `install.sh` put in `/opt/jarvis-deployer`, not the commit's):
  `devices/echo/tools/deploy.sh --all` from the runner's checkout with `JARVIS_BIN` (the CI's
  binary) and `VERSION`: the Dot first (alphabetical), then the Echos. After each restart
  deploy.sh waits until `ctl status` reports `version=<commit>`, twice 15 s apart (no crash
  loop); a failure stops the rollout, and deployer.py installs the last good commit and binary on
  every Echo again (it keeps the three newest builds). An Echo that does not answer SSH (switched
  off, away) is skipped and named in the report; it gets the version with the next deploy. A last
  good commit the repository does not have (one from before the move to this repository) cannot
  be checked out: the report says the rollback failed. Then the message in Telegram through
  Jarvis' bot (sending only). Secrets, librespot and Tailscale are never part of it: those stay
  with `deploy.sh --secrets` etc. from the PC.
- **agent-mcp** gives Ultron the rule for change tasks (`CODE_RULES`): branch `ultron/…`, tests,
  pull request, its link in the report (the report reaches Telegram), never merge.

## Ultron's read-only diagnostics (#124)

`jarvis_diag.py` lets Ultron read every Echo for an analysis without any other access to them:
from the OpenHands container `ssh jarvis-diag "<host|all> <command>"` (alias in the container's
SSH config: the Pi's WireGuard address, jump host `host`). On the Pi the user `ultron` has one
key with `restrict,command="/usr/local/bin/jarvis-diag"`; that wrapper runs `jarvis_diag.py` as
`jarvis-deploy` (sudoers allows exactly that), which maps each command to a fixed read-only
command line on the speaker: status, log, grep (fixed string, checked characters), errors, health,
sys, dmesg, tools, peers, settings (`help` lists them). Other commands, a shell, a PTY and port
forwarding are refused. `install.sh` installs it; `ULTRON_KEY=<public key>` sets up the user once.
Offline test: `python3 test_jarvis_diag.py`.

## What holds it

- Only the owner merges into `main` (the ruleset; others need a code owner's approval) and only
  the owner can start a deploy (the private repository has no other collaborator; the workflow
  also checks the actor).
- Jarvis, Ultron and the PC sessions work with GitHub tokens of the owner's account. Their tokens
  must be fine-grained and limited to this repository (contents, pull requests, issues), without
  access to `smart-speaker-deploy`: then they can open pull requests but never deploy.
- CI before every merge, `ci-ok` checked again before every deploy, the version check and
  rollback on the devices, the Telegram message after every deploy.
- The SSH keys to the Echos, the ADB keys and the Telegram token are on the Pi, not in GitHub.

While a session tests a branch on an Echo, a deploy installs over it.

## Setup on the Pi

1. `sudo REPO=<owner>/smart-speaker-deploy RUNNER_TOKEN=$(token) sh services/deployer/install.sh`
   from a checkout of this repository (the token: `gh api -X POST
   repos/<owner>/smart-speaker-deploy/actions/runners/registration-token --jq .token`, valid one
   hour; `REPO` is the private deploy repository, never this one): user `jarvis-deploy`, `/opt/jarvis-deployer`,
   `/etc/jarvis-deployer.env`, the runner as a service.
2. The PC's speaker keys (`local/ssh/developer_windows_ed25519`, `local/dot-ssh/id_ed25519`;
   the speakers and the Pi share the private network, decided 31.10.2026) to
   `/opt/jarvis-deployer/.ssh/` (mode 600), so nothing changes on the speakers.
3. `/opt/jarvis-deployer/.ssh/config`: one `Host` per speaker (the names of `local/devices/`)
   with its `HostName` and `IdentityFile`, `User root`; accept the host keys once
   (`sudo -u jarvis-deploy ssh <host> true`).
4. Copy `devices/echo/local/devices/*.json` to `/opt/jarvis-deployer/local/devices/`.
5. `/etc/jarvis-deployer.env`: Jarvis' Telegram bot token and the owner's chat id.

Updating deployer.py: `sudo sh services/deployer/install.sh` again. The first deploy has no
earlier version to roll back to.

Offline test: `python3 test_deployer.py`.

# deployer

The assistant changes itself, with a safety net (#122): the server agent (or any session) builds a change on a
branch and opens a pull request, the assistant sends the owner its link; the owner merges it on GitHub;
the CI deploys it on every Echo and the assistant reports in Telegram that the new version runs.

```
Agent ──push branch, open PR──▶ GitHub ──CI assistant-build: vet, test, ARMv7 binary (the PR's test build)
                                 └─ job notify on the Pi ──▶ Telegram: PR link + test build
the owner merges ─────────────▶ main ──CI assistant-build: build ──▶ job deploy on the Pi's runner
                                         deployer.py ──deploy.sh --all (CI binary)──▶ every Echo
                                                       checks ctl status: version=<commit>
Telegram ◀── "Neue Version abc123 aktiv auf dot-dev, echo-dev, echo2"
             or "fehlgeschlagen …, zurückgerollt auf def456"
```

## Pieces

- **`.github/workflows/assistant-build.yml`**: job `changes` decides what a pull request or a
  commit on main touches; `build` (the Echos: `core/`, `shared/prompts/`, `devices/echo/`) and
  `firetv-build` (the app: `core/`, `shared/prompts/`, `devices/android/`) run on GitHub's
  runners only where needed; `deploy` and `firetv-deploy` only for main, on the self-hosted runner
  with the label `assistant-deploy`: the Raspberry Pi. The runner polls GitHub, nothing reaches the
  Pi from outside, and the Pi reaches the Echos and the sticks in the LAN. Deploys run one after
  the other and are never cancelled. The pipeline (workflow file, runner label) is set up per
  repository: `assistant-build` and `assistant-deploy` are the defaults of `install.sh`; use your
  own names and keep workflow and runner in step.
- **`firetv_deploy.py`** (#126, job `firetv-deploy`, switched on by the repository variable
  `FIRETV_DEPLOY=on`): re-signs the CI's debug APK with the key of the app on the sticks (the
  PC's `~/.android/debug.keystore`, copied to the Pi's `~/.android/`), installs it over network ADB
  (the PC's `adbkey`, which the sticks allow) on every stick of `local/firetv/<name>.json`
  (`{"host": "<address>"}`) as an update that keeps settings and secrets, as `install.sh` does
  (Appstore installer, overlay, microphone), checks that the app runs and that a stick's own
  The assistant logs `starting, version <commit>`, rolls a failed stick back to the last good APK, and
  reports in Telegram; an unreachable stick is reported, not failed. Before the first deploy the
  app the sticks ran was pulled from a stick as `builds/firetv/baseline.apk` (`firetv-state.json`:
  `{"deployed": "baseline"}`), so even the first one can roll back. Offline test:
  `python3 test_firetv_deploy.py`.
- **Job `notify`** (every pull request, on the Pi's runner): `deployer.py notify-pr` sends the
  owner the pull request's link and its test build through the assistant's bot, when it is opened or
  reopened, and after a push when the build failed. So every pull request reaches him, whoever
  opened it (the server agent, a session).
- **`deployer.py`** (the copy `install.sh` put in `/opt/assistant-deployer`, not the commit's):
  `devices/echo/tools/deploy.sh --all` from the runner's checkout with `ASSISTANT_BIN` (the CI's
  binary) and `VERSION`: the Dot first (alphabetical), then the Echos. After each restart
  deploy.sh waits until `ctl status` reports `version=<commit>`, twice 15 s apart (no crash
  loop); a failure stops the rollout, and deployer.py installs the last good commit and binary on
  every Echo again (it keeps the three newest builds). Then the message in Telegram through
  the assistant's bot (sending only). Secrets, librespot and Tailscale are never part of it: those stay
  with `deploy.sh --secrets` etc. from the PC.
- **agent-mcp** gives the server agent the rule for change tasks (`CODE_RULES`): branch `agent/…`, tests,
  pull request, its link in the report (the report reaches Telegram), never merge.

## The server agent's read-only diagnostics (#124)

`assistant_diag.py` lets the server agent read every Echo for an analysis without any other access to them:
from the OpenHands container `ssh assistant-diag "<host|all> <command>"` (alias in the container's
SSH config: the Pi's WireGuard address, jump host `host`). On the Pi the user `agent` has one
key with `restrict,command="/usr/local/bin/assistant-diag"`; that wrapper runs `assistant_diag.py` as
`assistant-deploy` (sudoers allows exactly that), which maps each command to a fixed read-only
command line on the speaker: status, log, grep (fixed string, checked characters), errors, health,
sys, dmesg, tools, peers, settings (`help` lists them). Other commands, a shell, a PTY and port
forwarding are refused. `install.sh` installs it; `AGENT_KEY=<public key>` sets up the user once.
Offline test: `python3 test_assistant_diag.py`.

## The trade-off

The assistant, the server agent and the PC sessions work with the owner's GitHub account (decided 31.10.2026).
GitHub cannot tell them apart, a pull request of one's own cannot be approved, and without GitHub
Pro a private repository has no branch protection: **the merge is the release, and the server agent could
technically merge too.** What holds it: the agent's rule, the CI's tests before every deploy, the
version check and the rollback, and the Telegram message after every deploy. The SSH keys to the
Echos and the Telegram token are not in GitHub, so a workflow run elsewhere cannot use them;
a workflow file on a branch could still target the Pi's runner, which the same account allows
anyway.

While a session tests a branch on an Echo, the next merge deploys over it.

## Setup on the Pi

1. `sudo REPO=<owner>/<name> RUNNER_TOKEN=$(token) sh services/deployer/install.sh` from a
   checkout (the token: `gh api -X POST repos/<owner>/<name>/actions/runners/registration-token
   --jq .token`, valid one hour): user `assistant-deploy`, `/opt/assistant-deployer`,
   `/etc/assistant-deployer.env`, the runner as a service.
2. The PC's speaker keys (`local/ssh/developer_windows_ed25519`, `local/dot-ssh/id_ed25519`;
   the speakers and the Pi share the private network, decided 31.10.2026) to
   `/opt/assistant-deployer/.ssh/` (mode 600), so nothing changes on the speakers.
3. `/opt/assistant-deployer/.ssh/config`: one `Host` per speaker (the names of `local/devices/`)
   with its `HostName` and `IdentityFile`, `User root`; accept the host keys once
   (`sudo -u assistant-deploy ssh <host> true`).
4. Copy `devices/echo/local/devices/*.json` to `/opt/assistant-deployer/local/devices/`.
5. `/etc/assistant-deployer.env`: the assistant's Telegram bot token and the owner's chat id.

Updating deployer.py: `sudo sh services/deployer/install.sh` again. The first deploy has no
earlier version to roll back to.

Offline test: `python3 test_deployer.py`.

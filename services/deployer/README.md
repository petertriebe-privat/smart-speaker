# deployer

A change reaches the devices in two steps, and only the owner takes either: Ultron (or any
session, or anyone through a fork) opens a pull request; CI checks it; the owner merges it into
`main`; later the owner makes a **release** of a commit on `main`: the private workflow builds
it with its version, publishes the signed GitHub release, and the Raspberry Pi installs it on
every Echo and Fire TV stick of its channel. A device that was switched off catches up as soon
as it is back. Jarvis reports in Telegram (issue #15).

```
pull request ──▶ smart-speaker (public): CI ci.yml, ci-ok ──▶ the owner merges into main
the owner: Run workflow "deploy" (channel dev|rel, bump) ──▶ smart-speaker-deploy (private)
    verify: the commit is on main, ci-ok passed, the owner started it; the next version
    build on GitHub's runners: Echo bundle, Fire TV APK, both checked for secrets
    release: manifest.json signed (ECDSA P-256), GitHub release v1.2.0 in smart-speaker
             (the release app), releases.json (the index) signed
    on the Pi's runner ──▶ release_store.py add: signatures and sha256 checked, into the store
                      ──▶ deployer.py v1.2.0 ──bundle's deploy.sh──▶ the Echos of the channel
                      ──▶ firetv_deploy.py v1.2.0 ──ADB──▶ the sticks of the channel
    on the Pi, every 10 min (jarvis-catch-up) ──▶ what a device missed, once it answers again
Telegram ◀── "Neue Version v1.2.0 aktiv auf dot-dev, echo-dev, echo2"
             "echo2: v1.2.0 nachgeholt (lief v1.1.0)", "fehlgeschlagen …, zurückgerollt auf v1.1.0"
```

## Versions and channels

- Tags `vMAJOR.MINOR.PATCH` (the patch number is the service pack). Two channels: **rel**, a
  normal GitHub release (`v1.2.0`), and **dev**, a pre-release (`v1.3.0-dev.4`, before
  `v1.3.0`). The workflow counts on from the newest release: `bump` patch, minor or major,
  for dev with the next `-dev.N`; or an explicit `version`.
- Each device has its channel: `"channel": "dev"` in its `local/devices/<host>.json` (Echos) or
  `local/firetv/<name>.json` (sticks), else `rel`. A dev release goes to the dev devices only; a
  release to every device. A dev device runs the newest of both.
- The version is in every build: core/ (`-X main.version=v1.2.0 -X main.commit=<sha>`; `ctl
  status` reports `version=v1.2.0 commit=<sha>`, Home Assistant the sensors Version and Commit
  and the device's software version), the APKs (`-PassistantVersion=v1.2.0`: versionName, and
  versionCode `MMmmppdd` rising with it, `release.py version-code`), the M5 firmware
  (`ASSISTANT_VERSION=v1.2.0` in the environment of the private build; sensor Version). A build
  from the PC is `dev` (M5, apps) or its commit (deploy.sh).
- Release notes: GitHub's generated notes from the merged pull requests since the channel's
  previous tag. They are public: no household data (the rules for pull requests already ensure it).

## Releases, signed

| Device | Artifact in the release | |
|---|---|---|
| Echo Gen 1 / Dot 2 | `echo-bundle.tar.gz` (`devices/echo/tools/bundle.sh`): the ARMv7 binary, `deploy.sh`, `files.txt` and the files it names | public |
| Fire TV | `jarvis-firetv.apk` (debug APK, core as `libjarvis.so`) | public |
| Phone | not yet (needs a release signing key; the Android updater, issue #15 piece 3) | |
| M5 AtomS3R | never: `secrets.h` is compiled in; built privately, the release only names the version | |

`check_artifact.py` refuses an artifact with a household file (`local/`, `secrets.json`,
`device.json`, keys) or a key-shaped string, archives inside it included; CI runs it on its
builds, the workflow before it publishes.

`manifest.json` (version, channel, commit, date, sequence, per artifact file, size and sha256,
optional `min_version`) and the index `releases.json` (sequence, `current` per channel, the
newest five releases per channel with their manifest's sha256) are signed with **ECDSA P-256**
over SHA-256, the signature next to them as `<file>.sig` (DER): one scheme that Go, Java,
mbedTLS (the M5 has no Ed25519) and openssl all check. `release.py` makes and checks them;
`shared/release/` describes the format and holds the test vectors every device's verifier
shares. The private key is a secret of the private repository; the public key is a variable
there and `/opt/jarvis-deployer/release-key.pub` on the Pi.

## The Pi's release store

`/opt/jarvis-deployer/releases/` holds `releases.json(.sig)` and per release
`<version>/manifest.json(.sig)` and its artifacts. `release_store.py add` takes the workflow's
hand-over only with the owner's signatures and the artifacts' sha256, refuses an index older
than the stored one, fetches a listed release it lacks from the public GitHub release (checked
the same way), and keeps what the index lists and every version a device still runs or may roll
back to (`state.json`, `firetv-state.json`). The service `jarvis-releases` serves the store over
HTTP (`release_store.py serve`, port 8380, `RELEASE_BIND` / `RELEASE_PORT`) to the home network
only (never forwarded; the phone over Tailscale): plain HTTP is fine, because the signature,
not the transport, is the trust. The devices fetch from here once they update themselves
(issue #15, pieces 2-4).

## Pieces

- **The deploy workflow** (`smart-speaker-deploy`, `.github/workflows/deploy.yml`): started by
  hand only (`workflow_dispatch`: `channel`, `bump` or `version`, `target` `both`, `echos`,
  `firetv` or `check`, a commit on `main` or its newest), refused unless the repository's owner
  started it; builds on GitHub's runners, the release through the **release app** (a GitHub App
  of the owner with `contents: write` on smart-speaker only, the only actor that may create `v*`
  tags: `RELEASE_APP_ID=<id> tools/github-settings.sh`), the store and the deploys on the
  self-hosted runner with the label `jarvis-deploy`: the Raspberry Pi. The runner polls GitHub,
  nothing reaches the Pi from outside, and the Pi reaches the Echos and the sticks in the LAN.
  Releases run one after the other and are never cancelled.
- **`deployer.py <version>`** (the copy `install.sh` put in `/opt/jarvis-deployer`, not the
  commit's): unpacks the release's Echo bundle from the store and runs the bundle's own
  `deploy.sh` (no checkout of the commit) with `JARVIS_BIN`, `VERSION` and `COMMIT` on one Echo
  of the channel after the other, the Dot first (alphabetical). deploy.sh waits until `ctl
  status` reports `version=<version>`, twice 15 s apart (no crash loop). A failure stops the
  rollout and puts the Echos it reached back on the release each ran before. An Echo that does
  not answer SSH (switched off, away) is skipped and named in the report.
- **`deployer.py --catch-up`** (timer `jarvis-catch-up`, every 10 minutes, and 2 minutes after
  the Pi's start): every Echo whose recorded version is not its channel's newest good release is
  asked (`deploy.sh --status`); one that answers gets it, with the same check and rollback. Quiet
  when nothing happened; skips itself while a release runs (`deploy.lock`).
- **`firetv_deploy.py <version>` / `--catch-up`** (#126): re-signs the release's APK with the key
  of the app on the sticks (the PC's `~/.android/debug.keystore`, copied to the Pi's
  `~/.android/`), installs it over network ADB (the PC's `adbkey`, which the sticks allow) on
  every stick of `local/firetv/<name>.json` (`{"host": "<address>", "channel": "rel"}`) of the
  channel as an update that keeps settings and secrets, as `install.sh` does (Appstore
  installer, overlay, microphone), checks that the app runs and that a stick's own Jarvis logs
  `starting, version <version>`, rolls a failed stick back to the release it ran (`adb install
  -d`: an older versionCode; the sticks run debug builds), and reports in Telegram. The
  catch-up reads the installed `versionName`. Before the releases the app the sticks ran was
  pulled from a stick as `builds/firetv/baseline.apk` (`firetv-state.json`: `{"deployed":
  "baseline"}`): the first release can roll back to it.
- Secrets, librespot and Tailscale are never part of a release: those stay with
  `deploy.sh --secrets` etc. from the PC. The first release on an Echo has no earlier release to
  roll back to (the builds by commit from before are not in the store).
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
  the owner can start a release (the private repository has no other collaborator; the workflow
  also checks the actor).
- Jarvis, Ultron and the PC sessions work with GitHub tokens of the owner's account. Their tokens
  must be fine-grained and limited to this repository (contents, pull requests, issues), without
  access to `smart-speaker-deploy`: then they can open pull requests but never deploy. They
  cannot create a release either: `v*` tags are the release app's only (the tag ruleset), and
  without the owner's signing key no device and not the Pi take a release.
- CI before every merge, `ci-ok` checked again before every release, the secrets check of every
  artifact, the signatures checked on the Pi, the version check and rollback on the devices, the
  Telegram message after every release.
- The SSH keys to the Echos, the ADB keys and the Telegram token are on the Pi, not in GitHub;
  the signing key only in the private repository's secrets.

While a session tests a branch on an Echo, a release installs over it. The catch-up goes by
what the Pi recorded, not by what runs: a branch installed from the PC stays until the next
release of the Echo's channel.

## Setup

On GitHub (once):

1. A P-256 key pair on the PC: `openssl ecparam -name prime256v1 -genkey -noout -out
   release-key.pem` and `openssl ec -in release-key.pem -pubout -out release-key.pub`. In
   `smart-speaker-deploy`: the secret `RELEASE_SIGNING_KEY` (the `.pem`), the variable
   `RELEASE_PUBLIC_KEY` (the `.pub`). Keep the `.pem` offline, never in a repository.
2. The release app: a GitHub App of the owner's account (no webhook), repository permission
   Contents: read and write, installed on `smart-speaker` only. In `smart-speaker-deploy`: the
   variable `RELEASE_APP_CLIENT_ID` and the secret `RELEASE_APP_PRIVATE_KEY`. Then
   `RELEASE_APP_ID=<the app's id> tools/github-settings.sh <owner>/smart-speaker`: only the app
   creates `v*` tags.

On the Pi:

1. `sudo REPO=<owner>/smart-speaker-deploy RUNNER_TOKEN=$(token) sh services/deployer/install.sh`
   from a checkout of this repository (the token: `gh api -X POST
   repos/<owner>/smart-speaker-deploy/actions/runners/registration-token --jq .token`, valid one
   hour; `REPO` is the private deploy repository, never this one): user `jarvis-deploy`,
   `/opt/jarvis-deployer`, `/etc/jarvis-deployer.env`, the runner, `jarvis-releases` and the
   timer `jarvis-catch-up` as services.
2. The PC's speaker keys (`local/ssh/developer_windows_ed25519`, `local/dot-ssh/id_ed25519`;
   the speakers and the Pi share the private network, decided 31.10.2026) to
   `/opt/jarvis-deployer/.ssh/` (mode 600), so nothing changes on the speakers.
3. `/opt/jarvis-deployer/.ssh/config`: one `Host` per speaker (the names of `local/devices/`)
   with its `HostName` and `IdentityFile`, `User root`; accept the host keys once
   (`sudo -u jarvis-deploy ssh <host> true`).
4. Copy `devices/echo/local/devices/*.json` to `/opt/jarvis-deployer/local/devices/` (with
   `"channel": "dev"` for the development Echos).
5. `/etc/jarvis-deployer.env`: Jarvis' Telegram bot token, the owner's chat id, and
   `SOURCE_REPOSITORY=<owner>/smart-speaker`.
6. The public key to `/opt/jarvis-deployer/release-key.pub`.

Updating the deployer: `sudo sh services/deployer/install.sh` again, before the first release
that needs the new version of it.

Offline tests: `python3 -m unittest test_release test_release_store test_deployer
test_firetv_deploy test_check_artifact` (openssl; the bundle test needs Linux).

#!/usr/bin/env python3
"""The assistant's deploy on the Raspberry Pi: a release goes onto every Echo (issue #15).

    python3 /opt/jarvis-deployer/deployer.py <version>     the release onto the Echos of its channel
    python3 /opt/jarvis-deployer/deployer.py --catch-up    the Echos that missed one (timer, every 10 min)

Run by the owner's deploy workflow (a private repository, README.md) on the Pi's self-hosted
runner, after release_store.py took the release into the store, and by the timer
jarvis-catch-up (install.sh). The release comes from the store: signature and sha256 checked,
the Echo bundle (devices/echo/tools/bundle.sh) unpacked, and its own deploy.sh installs it on one
Echo after the other (alphabetical: the Dot first). A dev release goes to the Echos of the
channel dev only, a release to all (local/devices/<host>.json: "channel", default rel).
deploy.sh checks that the new version runs; a failure stops the rollout and puts the Echos it
reached back on the release each ran before. An Echo that is switched off is skipped; the
catch-up installs the current release of its channel as soon as it answers again. Telegram
reports through the assistant's bot (sending only). The SSH keys to the Echos and the Telegram
token stay on the Pi (/etc/jarvis-deployer.env), not in GitHub. Standard library only (and
openssl for the signatures). Offline test: python3 test_deployer.py
"""
import contextlib
import json
import logging
import os
import re
import shutil
import subprocess
import sys
import tarfile
import urllib.request
from pathlib import Path

import release
import release_store

log = logging.getLogger("deployer")
BUNDLE = "echo-bundle.tar.gz"
OFFLINE = 75  # deploy.sh: not reachable (EX_TEMPFAIL)


def read_env(path):
    """KEY=VALUE lines (the runner does not load the env file for its jobs)."""
    env = {}
    try:
        for line in Path(path).read_text().splitlines():
            key, sep, value = line.strip().partition("=")
            if sep and not key.startswith("#"):
                env[key] = value
    except FileNotFoundError:
        pass
    return env


class Config:
    def __init__(self, env):
        self.env = env
        # the code's repository (links, release downloads); the workflow runs in another one
        self.repo = env.get("SOURCE_REPOSITORY") or env.get("GITHUB_REPOSITORY", "")
        self.run_repo = env.get("GITHUB_REPOSITORY", "")
        self.server = env.get("GITHUB_SERVER_URL", "https://github.com")
        self.run_url = (f"{self.server}/{self.run_repo}/actions/runs/{env['GITHUB_RUN_ID']}"
                        if env.get("GITHUB_RUN_ID") else "")
        self.bot = env.get("TELEGRAM_BOT_TOKEN", "")        # the assistant's bot: sendMessage only
        self.chat = env.get("TELEGRAM_CHAT_ID", "")         # the owner's chat
        self.home = Path(env.get("DEPLOYER_HOME", "/opt/jarvis-deployer"))
        self.releases = self.home / "releases"              # the release store (release_store.py)
        self.public_key = Path(env.get("RELEASE_PUBLIC_KEY") or self.home / "release-key.pub")
        self.work = self.home / "work"                      # unpacked Echo bundles
        self.local = self.home / "local"                    # devices/<host>.json for deploy.sh
        self.state_file = self.home / "state.json"

    def release_url(self, version):
        return f"{self.server}/{self.repo}/releases/tag/{version}" if self.repo else ""


def send_telegram(cfg, text):
    if not (cfg.bot and cfg.chat):
        log.info("telegram (not configured): %s", text)
        return
    body = json.dumps({"chat_id": cfg.chat, "text": text, "disable_web_page_preview": True}).encode()
    req = urllib.request.Request(f"https://api.telegram.org/bot{cfg.bot}/sendMessage", data=body,
                                 headers={"Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=20).read()
    except Exception as e:  # a lost message must not fail the deploy
        log.warning("telegram: %s", e)


@contextlib.contextmanager
def lock(cfg, wait=True):
    """One deploy or catch-up at a time, Echos and sticks; yields False when busy and not waiting."""
    import fcntl  # the Pi; the offline tests on Windows do not take the lock
    cfg.home.mkdir(parents=True, exist_ok=True)
    with open(cfg.home / "deploy.lock", "w") as f:
        try:
            fcntl.flock(f, fcntl.LOCK_EX | (0 if wait else fcntl.LOCK_NB))
        except BlockingIOError:
            yield False
            return
        yield True


def load_state(path):
    """{"devices": {name: version it runs}, "good": {channel: newest release that went out}}"""
    try:
        state = json.loads(Path(path).read_text())
    except FileNotFoundError:
        state = {}
    # before the releases: {"deployed": "<commit>"}; a commit is no release to go back to
    state.setdefault("devices", {})
    state.setdefault("good", {})
    return state


def save_state(path, state):
    release.write(path, json.dumps(state, indent=1).encode())


def target(state, device_channel):
    """The release a device of this channel should run: the newest that went out well."""
    good = state["good"]
    return good.get("rel") if device_channel == "rel" else release.newest([good.get("rel"), good.get("dev")])


def channels(folder):
    """{name: channel} of the <name>.json in folder, alphabetical; "rel" unless "channel": "dev"."""
    out = {}
    for p in sorted(Path(folder).glob("*.json")):
        try:
            ch = json.loads(p.read_text()).get("channel", "rel")
        except ValueError:
            ch = "rel"
        out[p.stem] = ch if ch in release.CHANNELS else "rel"
    return out


class Echos:
    """The release store and deploy.sh from an unpacked bundle."""

    def __init__(self, cfg):
        self.cfg = cfg
        self.store = release_store.Store(cfg.releases, cfg.public_key)

    def hosts(self):
        return channels(self.cfg.local / "devices")

    def manifest(self, version):
        """The checked manifest of a release in the store, or ReleaseError."""
        return self.store.manifest(version)

    def bundle(self, version):
        """The unpacked, checked Echo bundle of a release, and its manifest."""
        m = self.manifest(version)
        if BUNDLE not in {a["file"] for a in m["artifacts"]}:
            raise release.ReleaseError(f"{version} has no Echo bundle")
        folder = self.cfg.work / version
        if not (folder / "jarvis").exists():
            tmp = self.cfg.work / f".{version}.tmp"
            shutil.rmtree(tmp, ignore_errors=True)
            tmp.mkdir(parents=True)
            with tarfile.open(self.store.artifact(version, BUNDLE)) as t:
                t.extractall(tmp, filter="data")
            shutil.rmtree(folder, ignore_errors=True)
            tmp.rename(folder)
            for old in sorted(self.cfg.work.iterdir(), key=lambda p: p.stat().st_mtime)[:-3]:
                if old != folder:
                    shutil.rmtree(old, ignore_errors=True)
        link = folder / "devices/echo/local"
        if not link.is_symlink():
            link.symlink_to(self.cfg.local)
        return folder, m

    def run(self, folder, host, version, commit, *args):
        env = {**os.environ, "ECHO_HOST": host, "JARVIS_BIN": str(folder / "jarvis"), "VERSION": version,
               "COMMIT": commit[:12], "CHECKED": "1"}
        out = subprocess.run(["sh", "devices/echo/tools/deploy.sh", *args], cwd=folder, env=env,
                             capture_output=True, text=True, timeout=900)
        text = (out.stdout + out.stderr).strip()
        print(f"== {host}\n{text}", flush=True)  # into the job's log
        return out.returncode, text

    def install(self, host, version):
        """'ok', 'offline' or 'failed', and deploy.sh's output."""
        try:
            folder, m = self.bundle(version)
        except (release.ReleaseError, OSError, tarfile.TarError) as e:
            return "failed", f"{version}: {e}"
        rc, text = self.run(folder, host, version, m["commit"])
        return ("ok" if rc == 0 else "offline" if rc == OFFLINE else "failed"), text

    def status(self, host, version):
        """The version the Echo runs, None when it does not answer (deploy.sh --status of any
        bundle: the check is the same for every release)."""
        try:
            folder, m = self.bundle(version)
        except (release.ReleaseError, OSError, tarfile.TarError):
            return None
        rc, text = self.run(folder, host, version, m["commit"], "--status")
        if rc != 0:
            return None
        found = re.search(r"\bversion=(\S+)", text)
        return found.group(1) if found else "unknown"


class Deployer:
    def __init__(self, cfg, echos=None, telegram=None):
        self.cfg = cfg
        self.echos = echos or Echos(cfg)
        self.telegram = telegram or (lambda text: send_telegram(cfg, text))
        self.state = load_state(cfg.state_file)

    def save(self):
        save_state(self.cfg.state_file, self.state)

    def rollback(self, hosts, before):
        """Puts each host back on the release it ran; the lines for the report."""
        lines = []
        for h in hosts:
            prev = before.get(h)
            if not prev or not release.is_version(prev):
                lines.append(f"{h}: keine frühere Release zum Zurückrollen, bitte prüfen.")
                continue
            result, _ = self.echos.install(h, prev)
            if result == "ok":
                self.state["devices"][h] = prev
                lines.append(f"{h}: zurückgerollt auf {prev}.")
            else:
                lines.append(f"{h}: Zurückrollen auf {prev} ging AUCH schief, bitte prüfen.")
        return lines

    def run(self, version):
        """True when the release runs on every reachable Echo of its channel."""
        try:
            m = self.echos.manifest(version)
        except release.ReleaseError as e:
            self.telegram(f"Deploy von {version} abgebrochen: {e}\n{self.cfg.run_url}".strip())
            return False
        hosts = [h for h, ch in self.echos.hosts().items() if release.accepts(ch, m["channel"])]
        before = dict(self.state["devices"])
        done, skipped = [], []
        for h in hosts:
            result, text = self.echos.install(h, version)
            if result == "ok":
                done.append(h)
                self.state["devices"][h] = version
                self.save()
            elif result == "offline":
                skipped.append(h)
            else:
                lines = [f"Deploy von {version} fehlgeschlagen auf {h}:", "\n".join(text.splitlines()[-6:])]
                lines += self.rollback(done + [h], before)
                lines.append(self.cfg.run_url)
                self.save()
                self.telegram("\n".join(line for line in lines if line))
                return False
        if hosts and not done:
            self.telegram(f"Deploy von {version}: kein Echo erreichbar ({', '.join(skipped)}); "
                          "sie holen sie nach, sobald sie wieder da sind.")
            return True
        self.state["good"][m["channel"]] = version
        self.save()
        lines = [f"Neue Version {version} aktiv auf {', '.join(done)}." if done
                 else f"Neue Version {version}: kein Echo im Kanal {m['channel']}."]
        if skipped:
            lines.append(f"Nicht erreichbar, holt sie nach, sobald es wieder da ist: {', '.join(skipped)}.")
        lines.append(self.cfg.release_url(version))
        self.telegram("\n".join(line for line in lines if line))
        return True

    def catch_up(self):
        """The Echos that do not run their channel's release yet (switched off during a rollout,
        or new). Quiet when nothing happened."""
        lines, ok = [], True
        for h, ch in self.echos.hosts().items():
            want = target(self.state, ch)
            if not want or self.state["devices"].get(h) == want:
                continue
            running = self.echos.status(h, want)
            if running is None:
                continue  # still away
            if running != want:
                result, text = self.echos.install(h, want)
                if result == "offline":
                    continue
                if result == "failed":
                    ok = False
                    lines.append(f"{h}: {want} nachholen fehlgeschlagen: {(text.splitlines() or [''])[-1]}")
                    lines += self.rollback([h], {h: running})
                    self.save()
                    continue
                lines.append(f"{h}: {want} nachgeholt (lief {running}).")
            self.state["devices"][h] = want
            self.save()
        if lines:
            self.telegram("\n".join(lines))
        return ok


def main():
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
    cfg = Config({**read_env("/etc/jarvis-deployer.env"), **os.environ})
    if len(sys.argv) != 2:
        sys.exit("usage: deployer.py <version> | --catch-up")
    if sys.argv[1] == "--catch-up":
        with lock(cfg, wait=False) as free:
            sys.exit(0 if not free or Deployer(cfg).catch_up() else 1)
    release.parse(sys.argv[1])
    with lock(cfg):
        sys.exit(0 if Deployer(cfg).run(sys.argv[1]) else 1)


if __name__ == "__main__":
    main()

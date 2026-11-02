#!/usr/bin/env python3
"""Jarvis' deploy on the Raspberry Pi (#122): a merge into main goes onto every Echo.

Run by the CI (job "deploy" of .github/workflows/jarvis-build.yml) on the Pi's self-hosted runner,
after the build of a commit on main:

    python3 /opt/jarvis-deployer/deployer.py <commit> <binary>

Installs the CI's binary with devices/echo/tools/deploy.sh --all from the runner's checkout of
that commit, checks that the new version runs (deploy.sh), rolls back to the last good commit and
binary when not, and reports the outcome in Telegram through Jarvis' bot (sending only). The SSH
keys to the Echos and the Telegram token stay on the Pi (/etc/jarvis-deployer.env), not in
GitHub. Standard library only. Offline test: python3 test_deployer.py
"""
import json
import logging
import os
import shutil
import subprocess
import sys
import urllib.request
from pathlib import Path

log = logging.getLogger("deployer")


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
        self.repo = env.get("GITHUB_REPOSITORY", "")
        self.server = env.get("GITHUB_SERVER_URL", "https://github.com")
        self.run_url = (f"{self.server}/{self.repo}/actions/runs/{env['GITHUB_RUN_ID']}"
                        if env.get("GITHUB_RUN_ID") else "")
        self.bot = env.get("TELEGRAM_BOT_TOKEN", "")        # Jarvis' bot: sendMessage only
        self.chat = env.get("TELEGRAM_CHAT_ID", "")         # the owner's chat
        self.home = Path(env.get("DEPLOYER_HOME", "/opt/jarvis-deployer"))
        self.workspace = Path(env.get("GITHUB_WORKSPACE", "."))
        self.builds = self.home / "builds"                  # <commit>/jarvis, the last three
        self.local = self.home / "local"                    # devices/<host>.json for deploy.sh
        self.state_file = self.home / "state.json"


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


class Workspace:
    """The runner's checkout: deploy.sh runs from the commit it installs."""

    def __init__(self, cfg):
        self.cfg = cfg

    def git(self, *args):
        return subprocess.run(["git", "-C", str(self.cfg.workspace), *args], capture_output=True,
                              text=True, check=True).stdout.strip()

    def checkout(self, sha):
        if self.git("rev-parse", "HEAD") != sha:
            self.git("fetch", "--quiet", "--depth", "1", "origin", sha)  # the checkout's credentials
            self.git("checkout", "--quiet", "--force", "--detach", sha)
        link = self.cfg.workspace / "devices/echo/local"
        if not link.exists():
            link.symlink_to(self.cfg.local)

    def subject(self, sha):
        try:
            return self.git("log", "-1", "--format=%s", sha)
        except subprocess.CalledProcessError:
            return ""

    def deploy(self, sha, binary):
        self.checkout(sha)
        env = {**os.environ, "JARVIS_BIN": str(binary), "VERSION": sha[:12], "CHECKED": "1"}
        out = subprocess.run(["sh", "devices/echo/tools/deploy.sh", "--all"], cwd=self.cfg.workspace,
                             env=env, capture_output=True, text=True, timeout=1800)
        text = out.stdout + out.stderr
        print(text, flush=True)  # into the job's log
        return out.returncode == 0, text


class Deployer:
    def __init__(self, cfg, ws=None, telegram=None):
        self.cfg = cfg
        self.ws = ws or Workspace(cfg)
        self.telegram = telegram or (lambda text: send_telegram(cfg, text))
        try:
            self.state = json.loads(cfg.state_file.read_text())
        except FileNotFoundError:
            self.state = {}

    def save(self):
        tmp = self.cfg.state_file.with_suffix(".tmp")
        tmp.write_text(json.dumps(self.state, indent=1))
        tmp.replace(self.cfg.state_file)

    def keep_build(self, sha, binary):
        """Copies the binary to builds/<sha>; keeps the three newest and the running one."""
        target = self.cfg.builds / sha
        target.mkdir(parents=True, exist_ok=True)
        shutil.copy(binary, target / "jarvis")
        (target / "jarvis").chmod(0o755)
        for p in sorted(self.cfg.builds.iterdir(), key=lambda p: p.stat().st_mtime)[:-3]:
            if p.name not in (sha, self.state.get("deployed")):
                shutil.rmtree(p, ignore_errors=True)
        return target / "jarvis"

    def run(self, sha, binary):
        """True when the new version runs on every Echo."""
        subject = self.ws.subject(sha)
        build = self.keep_build(sha, binary)
        ok, out = self.ws.deploy(sha, build)
        running = [line.split(":")[0] for line in out.splitlines() if line.endswith(" running")]
        if ok:
            self.state["deployed"] = sha
            self.save()
            self.telegram(f"Neue Version {sha[:12]} aktiv auf {', '.join(running)}.\n{subject}\n"
                          f"{self.cfg.server}/{self.cfg.repo}/commit/{sha}")
            return True
        tail = "\n".join(out.strip().splitlines()[-6:])
        good = self.state.get("deployed")
        if not good or not (self.cfg.builds / good / "jarvis").exists():
            self.telegram(f"Deploy von {sha[:12]} fehlgeschlagen, keine frühere Version zum "
                          f"Zurückrollen.\n{subject}\n{tail}\n{self.cfg.run_url}")
            return False
        back, _ = self.ws.deploy(good, self.cfg.builds / good / "jarvis")
        self.telegram(f"Deploy von {sha[:12]} fehlgeschlagen: {subject}\n{tail}\n"
                      + (f"Zurückgerollt auf {good[:12]}, die Echos laufen wieder."
                         if back else f"Zurückrollen auf {good[:12]} ging AUCH schief: bitte prüfen.")
                      + f"\n{self.cfg.run_url}")
        return False


BUILDS = (("ECHO_RESULT", "Echos"), ("FIRETV_RESULT", "Fire TV"))


def pr_message(env):
    """The owner's notice of a pull request after its builds (job "notify", env from the workflow):
    a build that did not run (skipped) means the pull request does not touch that device."""
    text = f"PR #{env['PR_NUMBER']}: {env['PR_TITLE']}\n{env['PR_BRANCH']}\n"
    results = {name: env.get(key, "") for key, name in BUILDS}
    built = [name for name, r in results.items() if r == "success"]
    failed = [f"{name} ({r})" for name, r in results.items() if r in ("failure", "cancelled")]
    if failed:
        text += f"Build fehlgeschlagen: {', '.join(failed)}: {env['RUN_URL']}"
    elif built:
        text += (f"Test-Build ok ({', '.join(built)}), Version {env['PR_SHA'][:12]}: {env['RUN_URL']}\n"
                 f"Merge = Deploy auf {' und '.join(built)}.")
    else:
        text += "Ändert weder die Echos noch die Fire TVs: ein Merge deployt nichts."
    return text + "\n" + env["PR_URL"]


def main():
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
    env = {**read_env("/etc/jarvis-deployer.env"), **os.environ}
    cfg = Config(env)
    if sys.argv[1:] == ["notify-pr"]:
        send_telegram(cfg, pr_message(env))
        return
    if len(sys.argv) != 3:
        sys.exit("usage: deployer.py <commit> <binary> | deployer.py notify-pr")
    sys.exit(0 if Deployer(cfg).run(sys.argv[1], Path(sys.argv[2])) else 1)


if __name__ == "__main__":
    main()

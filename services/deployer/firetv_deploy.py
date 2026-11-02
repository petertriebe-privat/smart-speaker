#!/usr/bin/env python3
"""The assistant's deploy on the Fire TV sticks (#126), on the Raspberry Pi: a merge into main goes onto
every stick, as the Echos get it (deployer.py).

Run by the CI (job "firetv-deploy" of .github/workflows/assistant-build.yml) on the Pi's runner:

    python3 /opt/assistant-deployer/firetv_deploy.py <commit> <apk>

The CI's debug APK is re-signed with the key of the app already on the sticks (the PC's debug
keystore, ~/.android/debug.keystore; Android installs an update only with the same key), then
installed over network ADB as devices/android/tools/install.sh does it, as an update: the app
keeps its settings and secrets, so nothing secret is needed here. The Pi's ADB key is the PC's
(~/.android/adbkey), which the sticks already allow. A stick whose app runs a assistant of its own
(files/secrets.json) must log "starting, version <commit>"; one that fails gets the last good
APK back. Telegram reports the outcome. Sticks: /opt/assistant-deployer/local/firetv/<name>.json
with {"host": "<address>"}. Offline test: python3 test_firetv_deploy.py
"""
import json
import os
import subprocess
import sys
import time
from pathlib import Path

import deployer

PKG = "dev.smartspeaker.screen"


class Adb:
    def __call__(self, *args, timeout=120):
        return subprocess.run(["adb", *args], capture_output=True, text=True, timeout=timeout)


class FireTV:
    def __init__(self, cfg, adb=None, telegram=None, sleep=time.sleep):
        self.cfg, self.sleep = cfg, sleep
        self.adb = adb or Adb()
        self.telegram = telegram or (lambda text: deployer.send_telegram(cfg, text))
        self.builds = cfg.home / "builds" / "firetv"
        self.state_file = cfg.home / "firetv-state.json"
        try:
            self.state = json.loads(self.state_file.read_text())
        except FileNotFoundError:
            self.state = {}

    def sticks(self):
        out = {}
        for p in sorted((self.cfg.home / "local" / "firetv").glob("*.json")):
            host = json.loads(p.read_text())["host"]
            out[p.stem] = host if ":" in host else f"{host}:5555"
        return out

    def sign(self, sha, apk):
        """The APK signed with the sticks' key, kept as builds/firetv/<sha>.apk (the newest three)."""
        self.builds.mkdir(parents=True, exist_ok=True)
        out = self.builds / f"{sha}.apk"
        ks = Path(os.environ.get("HOME", str(self.cfg.home))) / ".android" / "debug.keystore"
        r = subprocess.run(["apksigner", "sign", "--ks", str(ks), "--ks-pass", "pass:android",
                            "--key-pass", "pass:android", "--ks-key-alias", "androiddebugkey",
                            "--out", str(out), str(apk)], capture_output=True, text=True)
        if r.returncode != 0:
            raise RuntimeError(f"apksigner: {(r.stdout + r.stderr).strip()[-300:]}")
        for p in sorted(self.builds.glob("*.apk"), key=lambda p: p.stat().st_mtime)[:-3]:
            if p.stem not in (sha, self.state.get("deployed")):
                p.unlink(missing_ok=True)
        return out

    def shell(self, target, command):
        return self.adb("-s", target, "shell", command)

    def install(self, target, apk, version):
        """None when the new version runs on the stick, else what went wrong. Without a version
        (the rollback to a build from before the deploys, e.g. the baseline) running is enough."""
        if "connected" not in self.adb("connect", target, timeout=20).stdout:
            return "nicht erreichbar"
        core = self.shell(target, f"run-as {PKG} ls files/secrets.json").returncode == 0
        mic = "RECORD_AUDIO: granted=true" in self.shell(target, f"dumpsys package {PKG}").stdout
        # as from Amazon's Appstore, so Fire OS' home screen lists the assistant (install.sh)
        r = self.adb("-s", target, "install", "-r", "-i", "com.amazon.venezia", str(apk), timeout=300)
        if "Success" not in r.stdout:
            return "Installation: " + (r.stdout + r.stderr).strip()[-200:]
        self.shell(target, f"appops set {PKG} SYSTEM_ALERT_WINDOW allow")
        self.shell(target, f"settings put global sysui_mic_disclosure_exempt {PKG}")
        if mic:
            self.shell(target, f"pm grant {PKG} android.permission.RECORD_AUDIO")
        # The app starts itself after an update (BootReceiver); else its service is started.
        for i in range(12):
            self.sleep(5)
            running = self.shell(target, f"pidof {PKG}").stdout.strip() != ""
            if i == 4 and not running:
                self.shell(target, f"am start-foreground-service -n {PKG}/.ScreenService")
            if not running:
                continue
            if not core or version is None:
                return None
            log = self.shell(target, f"run-as {PKG} cat files/run/assistant.log").stdout
            if f"starting, version {version}" in log:
                return None
        return "läuft nicht" if not core else f"Version {version} startet nicht"

    def run(self, sha, apk):
        version = sha[:12]
        sticks = self.sticks()
        if not sticks:
            self.telegram("Fire TV: keine Sticks eingetragen, nichts installiert.")
            return True
        try:
            signed = self.sign(sha, apk)
        except Exception as e:
            self.telegram(f"Fire TV: Deploy von {version} abgebrochen ({e})")
            return False
        done, offline, failed = [], [], {}
        for name, target in sticks.items():
            why = self.install(target, signed, version)
            if why is None:
                done.append(name)
            elif why == "nicht erreichbar":
                offline.append(name)
            else:
                failed[name] = why
        good = self.state.get("deployed")
        lines = []
        if done:
            lines.append(f"Fire TV: Version {version} aktiv auf {', '.join(done)}.")
        for name, why in failed.items():
            back = ""
            if good and (self.builds / f"{good}.apk").exists():
                ok = self.install(sticks[name], self.builds / f"{good}.apk", None) is None
                back = f", zurückgerollt auf {good[:12]}" if ok else f", Zurückrollen ging AUCH schief"
            lines.append(f"Fire TV {name}: fehlgeschlagen ({why}){back}.")
        if offline:
            lines.append(f"Nicht erreichbar, nicht aktualisiert: {', '.join(offline)}.")
        if self.cfg.run_url:
            lines.append(self.cfg.run_url)
        if done and not failed:
            self.state["deployed"] = sha
            tmp = self.state_file.with_suffix(".tmp")
            tmp.write_text(json.dumps(self.state, indent=1))
            tmp.replace(self.state_file)
        self.telegram("\n".join(lines))
        return not failed


def main():
    if len(sys.argv) != 3:
        sys.exit("usage: firetv_deploy.py <commit> <apk>")
    cfg = deployer.Config({**deployer.read_env("/etc/assistant-deployer.env"), **os.environ})
    sys.exit(0 if FireTV(cfg).run(sys.argv[1], Path(sys.argv[2])) else 1)


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""The assistant's deploy on the Fire TV sticks, on the Raspberry Pi: a release goes onto every
stick, as the Echos get it (deployer.py, issue #15).

    python3 /opt/jarvis-deployer/firetv_deploy.py <version>    the release onto the sticks of its channel
    python3 /opt/jarvis-deployer/firetv_deploy.py --catch-up   the sticks that missed one (timer)

The release's Fire TV APK comes from the release store (release_store.py, signature and sha256
checked) and is re-signed with the key of the app already on the sticks (the PC's debug
keystore, ~/.android/debug.keystore; Android installs an update only with the same key), then
installed over network ADB as devices/android/tools/install.sh does it, as an update: the app
keeps its settings and secrets, so nothing secret is needed here. The Pi's ADB key is the PC's
(~/.android/adbkey), which the sticks already allow. A stick whose app runs an assistant of its
own (files/secrets.json) must log "starting, version <version>"; one that fails gets the release
it ran before back (a downgrade: adb install -d, the sticks run debug builds). A dev release goes
to the sticks of the channel dev only. A stick that is switched off gets the current release of
its channel from the catch-up as soon as it answers again. Telegram reports the outcome. Sticks:
/opt/jarvis-deployer/local/firetv/<name>.json with {"host": "<address>", "channel": "rel"|"dev"}.
Offline test: python3 test_firetv_deploy.py
"""
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path

import deployer
import release
import release_store

PKG = "dev.jarvis.screen"
APK = "jarvis-firetv.apk"
OFFLINE = "nicht erreichbar"


class Adb:
    def __call__(self, *args, timeout=120):
        return subprocess.run(["adb", *args], capture_output=True, text=True, timeout=timeout)


class FireTV:
    def __init__(self, cfg, adb=None, telegram=None, sleep=time.sleep, store=None):
        self.cfg, self.sleep = cfg, sleep
        self.adb = adb or Adb()
        self.telegram = telegram or (lambda text: deployer.send_telegram(cfg, text))
        self.store = store or release_store.Store(cfg.releases, cfg.public_key)
        self.builds = cfg.home / "builds" / "firetv"
        self.state_file = cfg.home / "firetv-state.json"
        self.state = deployer.load_state(self.state_file)

    def save(self):
        deployer.save_state(self.state_file, self.state)

    def sticks(self):
        """{name: (adb target, channel)}"""
        folder, out = self.cfg.home / "local" / "firetv", {}
        for name, ch in deployer.channels(folder).items():
            host = json.loads((folder / f"{name}.json").read_text())["host"]
            out[name] = (host if ":" in host else f"{host}:5555", ch)
        return out

    def resign(self, apk, out):
        ks = Path(os.environ.get("HOME", str(self.cfg.home))) / ".android" / "debug.keystore"
        r = subprocess.run(["apksigner", "sign", "--ks", str(ks), "--ks-pass", "pass:android",
                            "--key-pass", "pass:android", "--ks-key-alias", "androiddebugkey",
                            "--out", str(out), str(apk)], capture_output=True, text=True)
        if r.returncode != 0:
            raise RuntimeError(f"apksigner: {(r.stdout + r.stderr).strip()[-300:]}")

    def apk(self, version):
        """The APK of a version signed with the sticks' key: builds/firetv/<version>.apk, made from
        the release store (the newest three and every one a stick runs are kept). The one from
        before the releases (baseline.apk, a stick's own) only from builds/."""
        out = self.builds / f"{version}.apk"
        if out.exists() or not release.is_version(version):
            return out
        m = self.store.manifest(version)
        if APK not in {a["file"] for a in m["artifacts"]}:
            raise release.ReleaseError(f"{version} has no Fire TV APK")
        self.builds.mkdir(parents=True, exist_ok=True)
        self.resign(self.store.artifact(version, APK), out)
        keep = {version, self.state.get("deployed"), *self.state["devices"].values(), *self.state["good"].values()}
        for p in sorted(self.builds.glob("*.apk"), key=lambda p: p.stat().st_mtime)[:-3]:
            if p.stem not in keep:
                p.unlink(missing_ok=True)
        return out

    def shell(self, target, command):
        return self.adb("-s", target, "shell", command)

    def connect(self, target):
        return "connected" in self.adb("connect", target, timeout=20).stdout

    def installed(self, target):
        """The app's versionName on the stick, or None."""
        found = re.search(r"versionName=(\S+)", self.shell(target, f"dumpsys package {PKG}").stdout)
        return found.group(1) if found else None

    def install(self, target, apk, version):
        """None when the new version runs on the stick, else what went wrong. Without a version
        (the rollback to the build from before the releases, the baseline) running is enough."""
        if not self.connect(target):
            return OFFLINE
        core = self.shell(target, f"run-as {PKG} ls files/secrets.json").returncode == 0
        mic = "RECORD_AUDIO: granted=true" in self.shell(target, f"dumpsys package {PKG}").stdout
        # as from Amazon's Appstore, so Fire OS' home screen lists the app (install.sh); -d: a
        # rollback is an older versionCode
        r = self.adb("-s", target, "install", "-r", "-d", "-i", "com.amazon.venezia", str(apk), timeout=300)
        if "Success" not in r.stdout:
            return "Installation: " + (r.stdout + r.stderr).strip()[-200:]
        self.shell(target, f"appops set {PKG} SYSTEM_ALERT_WINDOW allow")
        self.shell(target, f"settings put global sysui_mic_disclosure_exempt {PKG}")
        if mic:
            self.shell(target, f"pm grant {PKG} android.permission.RECORD_AUDIO")
        started = re.compile(rf"starting, version {re.escape(version)}(\s|$)") if version else None
        # The app starts itself after an update (BootReceiver); else its service is started.
        for i in range(12):
            self.sleep(5)
            running = self.shell(target, f"pidof {PKG}").stdout.strip() != ""
            if i == 4 and not running:
                self.shell(target, f"am start-foreground-service -n {PKG}/.ScreenService")
            if not running:
                continue
            if not core or started is None:
                return None
            if started.search(self.shell(target, f"run-as {PKG} cat files/run/jarvis.log").stdout):
                return None
        return "läuft nicht" if not core else f"Version {version} startet nicht"

    def put(self, name, target, version):
        """Installs a version on one stick: None or what went wrong."""
        try:
            apk = self.apk(version)
        except Exception as e:
            return f"APK {version}: {e}"
        if not apk.exists():
            return f"keine APK für {version}"
        why = self.install(target, apk, version if release.is_version(version) else None)
        if why is None:
            self.state["devices"][name] = version
        return why

    def rollback(self, name, target, prev):
        if not prev:
            return ", keine frühere Version zum Zurückrollen"
        return f", zurückgerollt auf {prev}" if self.put(name, target, prev) is None else ", Zurückrollen ging AUCH schief"

    def run(self, version):
        try:
            m = self.store.manifest(version)
        except release.ReleaseError as e:
            self.telegram(f"Fire TV: Deploy von {version} abgebrochen ({e})")
            return False
        sticks = {n: t for n, (t, ch) in self.sticks().items() if release.accepts(ch, m["channel"])}
        if not sticks:
            self.telegram(f"Fire TV: keine Sticks im Kanal {m['channel']}, nichts installiert.")
            return True
        done, offline, failed = [], [], {}
        for name, target in sticks.items():
            prev = self.state["devices"].get(name) or self.state.get("deployed")
            why = self.put(name, target, version)
            if why is None:
                done.append(name)
            elif why == OFFLINE:
                offline.append(name)
            else:
                failed[name] = why + self.rollback(name, target, prev)
            self.save()
        lines = []
        if done:
            lines.append(f"Fire TV: Version {version} aktiv auf {', '.join(done)}.")
        for name, why in failed.items():
            lines.append(f"Fire TV {name}: fehlgeschlagen ({why}).")
        if offline:
            lines.append(f"Nicht erreichbar, holt sie nach, sobald er wieder da ist: {', '.join(offline)}.")
        if self.cfg.run_url:
            lines.append(self.cfg.run_url)
        if done and not failed:
            self.state["good"][m["channel"]] = version
            self.save()
        self.telegram("\n".join(lines))
        return not failed

    def catch_up(self):
        """The sticks that do not run their channel's release yet; quiet when nothing happened."""
        lines, ok = [], True
        for name, (target, ch) in self.sticks().items():
            want = deployer.target(self.state, ch)
            if not want or self.state["devices"].get(name) == want or not self.connect(target):
                continue
            running = self.installed(target)
            if running == want:
                self.state["devices"][name] = want
                self.save()
                continue
            prev = self.state["devices"].get(name) or self.state.get("deployed")
            why = self.put(name, target, want)
            if why is None:
                lines.append(f"Fire TV {name}: {want} nachgeholt (lief {running}).")
            elif why != OFFLINE:
                ok = False
                lines.append(f"Fire TV {name}: {want} nachholen fehlgeschlagen ({why}"
                             f"{self.rollback(name, target, prev)}).")
            self.save()
        if lines:
            self.telegram("\n".join(lines))
        return ok


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: firetv_deploy.py <version> | --catch-up")
    cfg = deployer.Config({**deployer.read_env("/etc/jarvis-deployer.env"), **os.environ})
    if sys.argv[1] == "--catch-up":
        with deployer.lock(cfg, wait=False) as free:
            sys.exit(0 if not free or FireTV(cfg).catch_up() else 1)
    release.parse(sys.argv[1])
    with deployer.lock(cfg):
        sys.exit(0 if FireTV(cfg).run(sys.argv[1]) else 1)


if __name__ == "__main__":
    main()

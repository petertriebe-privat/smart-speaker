"""Offline test of the Fire TV deploy: fake sticks over a fake adb; python3 test_firetv_deploy.py"""
import json
import subprocess
import tempfile
import unittest
from pathlib import Path

import deployer
import firetv_deploy

PKG = firetv_deploy.PKG


def done(stdout="", code=0):
    return subprocess.CompletedProcess([], code, stdout, "")


class FakeSticks:
    """adb against sticks: each has core or not, may be offline or never start the new version."""

    def __init__(self):
        self.sticks = {}  # target -> dict(core, offline, broken, installed, running)

    def add(self, target, core=True, offline=False, broken=False):
        self.sticks[target] = dict(core=core, offline=offline, broken=broken, installed=None)

    def __call__(self, *args, timeout=120):
        if args[0] == "connect":
            s = self.sticks[args[1]]
            return done("failed to connect" if s["offline"] else f"connected to {args[1]}")
        target, verb = args[1], args[2]
        s = self.sticks[target]
        if verb == "install":
            s["installed"] = Path(args[-1]).stem
            return done("Performing Streamed Install\nSuccess\n")
        command = args[3]
        if command.endswith("ls files/secrets.json"):
            return done(code=0 if s["core"] else 1)
        if command.startswith("dumpsys package"):
            return done("android.permission.RECORD_AUDIO: granted=true\n")
        if command.startswith("pidof"):
            return done("1234\n")
        if command.endswith("cat files/run/jarvis.log"):
            v = s["installed"][:12] if s["installed"] and not (s["broken"] and s["installed"] == "b" * 40) else "old"
            return done(f"[Jarvis] Fire TV starting, version {v}\n")
        return done()


class FireTVTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        home = Path(self.tmp.name)
        (home / "local" / "firetv").mkdir(parents=True)
        self.cfg = deployer.Config({"DEPLOYER_HOME": self.tmp.name, "GITHUB_RUN_ID": "9",
                                    "GITHUB_REPOSITORY": "o/jarvis"})
        self.adb, self.sent = FakeSticks(), []
        self.apk = home / "in.apk"
        self.apk.write_bytes(b"PK")

    def tearDown(self):
        self.tmp.cleanup()

    def stick(self, name, host, **kw):
        (self.cfg.home / "local" / "firetv" / f"{name}.json").write_text(json.dumps({"host": host}))
        self.adb.add(f"{host}:5555", **kw)

    def firetv(self):
        f = firetv_deploy.FireTV(self.cfg, adb=self.adb, telegram=self.sent.append, sleep=lambda s: None)

        def sign(sha, apk):  # no apksigner here: the "signed" APK is a copy
            f.builds.mkdir(parents=True, exist_ok=True)
            out = f.builds / f"{sha}.apk"
            out.write_bytes(apk.read_bytes())
            return out
        f.sign = sign
        return f

    def test_every_stick_gets_it(self):
        self.stick("tv-a", "10.0.0.1")
        self.stick("tv-b", "10.0.0.2", core=False)
        self.assertTrue(self.firetv().run("a" * 40, self.apk))
        self.assertIn("Fire TV: Version aaaaaaaaaaaa aktiv auf tv-a, tv-b.", self.sent[-1])
        self.assertEqual(self.adb.sticks["10.0.0.1:5555"]["installed"], "a" * 40)

    def test_offline_stick_is_reported_not_failed(self):
        self.stick("tv-a", "10.0.0.1")
        self.stick("tv-b", "10.0.0.2", offline=True)
        self.assertTrue(self.firetv().run("a" * 40, self.apk))
        self.assertIn("Nicht erreichbar, nicht aktualisiert: tv-b.", self.sent[-1])

    def test_failed_stick_rolls_back(self):
        self.stick("tv-a", "10.0.0.1", broken=True)
        self.firetv().run("a" * 40, self.apk)
        self.assertFalse(self.firetv().run("b" * 40, self.apk))
        self.assertIn("Fire TV tv-a: fehlgeschlagen (Version bbbbbbbbbbbb startet nicht), zurückgerollt auf aaaaaaaaaaaa",
                      self.sent[-1])
        self.assertEqual(self.adb.sticks["10.0.0.1:5555"]["installed"], "a" * 40)
        self.assertEqual(self.firetv().state["deployed"], "a" * 40)

    def test_no_sticks(self):
        self.assertTrue(self.firetv().run("a" * 40, self.apk))
        self.assertIn("keine Sticks", self.sent[-1])


if __name__ == "__main__":
    unittest.main()

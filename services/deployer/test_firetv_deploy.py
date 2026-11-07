"""Offline test of the Fire TV deploy: fake sticks over a fake adb; python3 test_firetv_deploy.py"""
import json
import subprocess
import tempfile
import unittest
from pathlib import Path

import deployer
import firetv_deploy
import release

PKG = firetv_deploy.PKG


def done(stdout="", code=0):
    return subprocess.CompletedProcess([], code, stdout, "")


class FakeSticks:
    """adb against sticks: each has core or not, may be offline or never start a broken version."""

    def __init__(self):
        self.sticks = {}  # target -> dict(core, offline, installed)
        self.broken = set()
        self.installs = []

    def add(self, target, core=True, offline=False, installed="baseline"):
        self.sticks[target] = dict(core=core, offline=offline, installed=installed)

    def __call__(self, *args, timeout=120):
        if args[0] == "connect":
            s = self.sticks[args[1]]
            return done("failed to connect" if s["offline"] else f"connected to {args[1]}")
        target, verb = args[1], args[2]
        s = self.sticks[target]
        if verb == "install":
            assert "-d" in args, "a rollback is a downgrade"
            s["installed"] = Path(args[-1]).stem
            self.installs.append((target, s["installed"]))
            return done("Performing Streamed Install\nSuccess\n")
        command = args[3]
        if command.endswith("ls files/secrets.json"):
            return done(code=0 if s["core"] else 1)
        if command.startswith("dumpsys package"):
            return done(f"    versionName={s['installed']}\n    android.permission.RECORD_AUDIO: granted=true\n")
        if command.startswith("pidof"):
            return done("1234\n")
        if command.endswith("cat files/run/jarvis.log"):
            v = "old" if s["installed"] in self.broken else s["installed"]
            return done(f"[Assistant] Fire TV starting, version {v} commit abc\n")
        return done()


class FakeStore:
    def __init__(self, versions):
        self.versions = versions

    def manifest(self, version):
        if version not in self.versions:
            raise release.ReleaseError(f"{version} is not in the store")
        return {"version": version, "channel": release.channel(version), "commit": "c" * 12,
                "artifacts": [{"file": firetv_deploy.APK}]}

    def artifact(self, version, name):
        return Path(version)


class FireTVTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        home = Path(self.tmp.name)
        (home / "local" / "firetv").mkdir(parents=True)
        self.cfg = deployer.Config({"DEPLOYER_HOME": self.tmp.name, "GITHUB_RUN_ID": "9",
                                    "GITHUB_REPOSITORY": "o/jarvis"})
        self.adb, self.sent = FakeSticks(), []
        self.store = FakeStore({"v1.0.0", "v1.1.0", "v1.1.0-dev.1", "v1.0.0-dev.9"})
        builds = home / "builds" / "firetv"
        builds.mkdir(parents=True)
        (builds / "baseline.apk").write_bytes(b"PK")  # pulled from a stick before the releases

    def tearDown(self):
        self.tmp.cleanup()

    def stick(self, name, host, channel="rel", **kw):
        (self.cfg.home / "local" / "firetv" / f"{name}.json").write_text(json.dumps({"host": host, "channel": channel}))
        self.adb.add(f"{host}:5555", **kw)

    def firetv(self):
        f = firetv_deploy.FireTV(self.cfg, adb=self.adb, telegram=self.sent.append, sleep=lambda s: None,
                                 store=self.store)
        f.resign = lambda apk, out: out.write_bytes(b"PK")  # no apksigner here
        return f

    def installed(self, host):
        return self.adb.sticks[f"{host}:5555"]["installed"]

    def test_every_stick_gets_it(self):
        self.stick("tv-a", "10.0.0.1")
        self.stick("tv-b", "10.0.0.2", core=False)
        self.assertTrue(self.firetv().run("v1.0.0"))
        self.assertIn("Fire TV: Version v1.0.0 aktiv auf tv-a, tv-b.", self.sent[-1])
        self.assertEqual(self.installed("10.0.0.1"), "v1.0.0")
        self.assertEqual(self.firetv().state["good"], {"rel": "v1.0.0"})

    def test_a_dev_release_goes_to_the_dev_sticks_only(self):
        self.stick("tv-a", "10.0.0.1", channel="dev")
        self.stick("tv-b", "10.0.0.2")
        self.assertTrue(self.firetv().run("v1.1.0-dev.1"))
        self.assertEqual(self.adb.installs, [("10.0.0.1:5555", "v1.1.0-dev.1")])

    def test_the_version_must_match_exactly(self):
        self.stick("tv-a", "10.0.0.1")
        f = self.firetv()
        # the stick logs "starting, version v1.0.0-dev.9 commit ...": that is not v1.0.0
        self.assertEqual(f.install("10.0.0.1:5555", Path("x/v1.0.0-dev.9.apk"), "v1.0.0"),
                         "Version v1.0.0 startet nicht")
        self.assertIsNone(f.install("10.0.0.1:5555", Path("x/v1.0.0.apk"), "v1.0.0"))

    def test_offline_stick_is_reported_and_catches_up(self):
        self.stick("tv-a", "10.0.0.1")
        self.stick("tv-b", "10.0.0.2", offline=True)
        self.assertTrue(self.firetv().run("v1.0.0"))
        self.assertIn("Nicht erreichbar, holt sie nach, sobald er wieder da ist: tv-b.", self.sent[-1])
        sent = len(self.sent)
        self.assertTrue(self.firetv().catch_up())
        self.assertEqual(len(self.sent), sent)
        self.adb.sticks["10.0.0.2:5555"]["offline"] = False
        self.assertTrue(self.firetv().catch_up())
        self.assertIn("Fire TV tv-b: v1.0.0 nachgeholt (lief baseline).", self.sent[-1])
        self.assertEqual(self.installed("10.0.0.2"), "v1.0.0")

    def test_catch_up_records_a_stick_that_runs_it(self):
        self.stick("tv-a", "10.0.0.1")
        self.firetv().run("v1.0.0")
        self.stick("tv-new", "10.0.0.3", installed="v1.0.0")
        self.adb.installs.clear()
        self.firetv().catch_up()
        self.assertEqual(self.adb.installs, [])
        self.assertEqual(self.firetv().state["devices"]["tv-new"], "v1.0.0")

    def test_failed_stick_rolls_back(self):
        self.stick("tv-a", "10.0.0.1")
        self.firetv().run("v1.0.0")
        self.adb.broken.add("v1.1.0")
        self.assertFalse(self.firetv().run("v1.1.0"))
        self.assertIn("Fire TV tv-a: fehlgeschlagen (Version v1.1.0 startet nicht, zurückgerollt auf v1.0.0).",
                      self.sent[-1])
        self.assertEqual(self.installed("10.0.0.1"), "v1.0.0")
        self.assertEqual(self.firetv().state["good"], {"rel": "v1.0.0"})

    def test_the_first_release_rolls_back_to_the_baseline(self):
        self.cfg.home.joinpath("firetv-state.json").write_text('{"deployed": "baseline"}')
        self.stick("tv-a", "10.0.0.1")
        self.adb.broken.add("v1.0.0")
        self.assertFalse(self.firetv().run("v1.0.0"))
        self.assertIn("zurückgerollt auf baseline", self.sent[-1])
        self.assertEqual(self.installed("10.0.0.1"), "baseline")

    def test_no_sticks(self):
        self.assertTrue(self.firetv().run("v1.0.0"))
        self.assertIn("keine Sticks", self.sent[-1])

    def test_not_in_the_store(self):
        self.stick("tv-a", "10.0.0.1")
        self.assertFalse(self.firetv().run("v9.9.9"))
        self.assertIn("abgebrochen", self.sent[-1])


if __name__ == "__main__":
    unittest.main()

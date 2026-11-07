"""Offline test of the Echo deploy: fake Echos, a fake store; python3 test_deployer.py"""
import io
import os
import shutil
import tarfile
import tempfile
import unittest
from pathlib import Path

import deployer


class FakeEchos:
    """Echos by name: channel, reachable, the version each runs, the versions that break."""

    def __init__(self):
        self.echos, self.broken, self.installs = {}, set(), []
        self.releases = {}  # version -> commit

    def add(self, host, channel="rel", running=None, offline=False):
        self.echos[host] = dict(channel=channel, running=running, offline=offline)

    def hosts(self):
        return {h: e["channel"] for h, e in sorted(self.echos.items())}

    def manifest(self, version):
        if version not in self.releases:
            raise deployer.release.ReleaseError(f"{version} is not in the store")
        return {"version": version, "channel": deployer.release.channel(version), "commit": self.releases[version]}

    def install(self, host, version):
        e = self.echos[host]
        if e["offline"]:
            return "offline", f"{host}: not reachable"
        self.installs.append((host, version))
        if version in self.broken:
            return "failed", f"{host}: version {version} is not running"
        e["running"] = version
        return "ok", f"{host}: version {version} running"

    def status(self, host, version):
        e = self.echos[host]
        return None if e["offline"] else e["running"]


class DeployerTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.cfg = deployer.Config({"DEPLOYER_HOME": self.tmp.name, "SOURCE_REPOSITORY": "o/code",
                                    "GITHUB_REPOSITORY": "o/deploy", "GITHUB_RUN_ID": "9"})
        self.echos, self.sent = FakeEchos(), []
        for v in ("v1.0.0", "v1.1.0", "v1.2.0-dev.1"):
            self.echos.releases[v] = "c" * 40

    def tearDown(self):
        self.tmp.cleanup()

    def deployer(self):
        return deployer.Deployer(self.cfg, echos=self.echos, telegram=self.sent.append)

    def test_a_release_goes_to_every_echo(self):
        self.echos.add("dot-dev", "dev")
        self.echos.add("echo-a")
        self.assertTrue(self.deployer().run("v1.0.0"))
        self.assertIn("Neue Version v1.0.0 aktiv auf dot-dev, echo-a.", self.sent[-1])
        self.assertIn("https://github.com/o/code/releases/tag/v1.0.0", self.sent[-1])
        state = self.deployer().state
        self.assertEqual(state["good"], {"rel": "v1.0.0"})
        self.assertEqual(state["devices"], {"dot-dev": "v1.0.0", "echo-a": "v1.0.0"})

    def test_a_dev_release_goes_to_the_dev_echos_only(self):
        self.echos.add("dot-dev", "dev")
        self.echos.add("echo-a")
        self.assertTrue(self.deployer().run("v1.2.0-dev.1"))
        self.assertEqual(self.echos.installs, [("dot-dev", "v1.2.0-dev.1")])
        self.assertEqual(self.deployer().state["good"], {"dev": "v1.2.0-dev.1"})

    def test_a_failure_rolls_back_the_echos_it_reached(self):
        self.echos.add("dot-dev")
        self.echos.add("echo-a")
        self.echos.add("echo-b")
        self.deployer().run("v1.0.0")
        self.echos.broken.add("v1.1.0")
        self.echos.installs.clear()
        self.assertFalse(self.deployer().run("v1.1.0"))
        # the first fails: it goes back, the others never get it
        self.assertEqual(self.echos.installs, [("dot-dev", "v1.1.0"), ("dot-dev", "v1.0.0")])
        self.assertIn("fehlgeschlagen auf dot-dev", self.sent[-1])
        self.assertIn("dot-dev: zurückgerollt auf v1.0.0.", self.sent[-1])
        self.assertIn("actions/runs/9", self.sent[-1])
        self.assertEqual(self.deployer().state["good"], {"rel": "v1.0.0"})

    def test_a_failure_without_an_earlier_release(self):
        self.echos.add("echo-a")
        self.echos.broken.add("v1.0.0")
        self.assertFalse(self.deployer().run("v1.0.0"))
        self.assertIn("keine frühere Release", self.sent[-1])

    def test_a_release_not_in_the_store(self):
        self.echos.add("echo-a")
        self.assertFalse(self.deployer().run("v9.0.0"))
        self.assertIn("abgebrochen", self.sent[-1])
        self.assertEqual(self.echos.installs, [])

    def test_a_switched_off_echo_catches_up(self):
        self.echos.add("echo-a")
        self.echos.add("echo-b", running="v0.9.0", offline=True)
        self.assertTrue(self.deployer().run("v1.0.0"))
        self.assertIn("Nicht erreichbar, holt sie nach, sobald es wieder da ist: echo-b.", self.sent[-1])
        sent = len(self.sent)
        self.assertTrue(self.deployer().catch_up())  # still off: quiet
        self.assertEqual(len(self.sent), sent)
        self.echos.echos["echo-b"]["offline"] = False
        self.assertTrue(self.deployer().catch_up())
        self.assertIn("echo-b: v1.0.0 nachgeholt (lief v0.9.0).", self.sent[-1])
        self.assertEqual(self.deployer().state["devices"]["echo-b"], "v1.0.0")
        self.echos.installs.clear()
        self.assertTrue(self.deployer().catch_up())  # nothing more to do
        self.assertEqual(self.echos.installs, [])

    def test_catch_up_records_an_echo_that_already_runs_it(self):
        self.echos.add("echo-a")
        self.deployer().run("v1.0.0")
        self.echos.add("echo-new", running="v1.0.0")
        self.echos.installs.clear()
        sent = len(self.sent)
        self.deployer().catch_up()
        self.assertEqual(self.echos.installs, [])
        self.assertEqual(len(self.sent), sent)
        self.assertEqual(self.deployer().state["devices"]["echo-new"], "v1.0.0")

    def test_a_dev_echo_follows_the_newest(self):
        self.echos.add("dot-dev", "dev", offline=True)
        d = self.deployer()
        d.run("v1.2.0-dev.1")  # nobody reachable: not marked good
        self.echos.add("echo-a")
        d.run("v1.0.0")
        self.assertEqual(deployer.target(d.state, "dev"), "v1.0.0")
        d.state["good"]["dev"] = "v1.2.0-dev.1"
        self.assertEqual(deployer.target(d.state, "dev"), "v1.2.0-dev.1")
        self.assertEqual(deployer.target(d.state, "rel"), "v1.0.0")

    def test_the_old_state_is_read(self):
        self.cfg.state_file.write_text('{"deployed": "' + "a" * 40 + '"}')
        state = self.deployer().state
        self.assertEqual((state["devices"], state["good"]), ({}, {}))

    def test_links_go_to_the_code_not_the_workflow(self):
        self.assertEqual(self.cfg.repo, "o/code")
        self.assertEqual(self.cfg.run_url, "https://github.com/o/deploy/actions/runs/9")
        self.assertEqual(deployer.Config({"GITHUB_REPOSITORY": "o/code"}).repo, "o/code")

    def test_read_env(self):
        f = Path(self.tmp.name) / "env"
        f.write_text("# comment\nTELEGRAM_CHAT_ID=42\nEMPTY=\n")
        self.assertEqual(deployer.read_env(f), {"TELEGRAM_CHAT_ID": "42", "EMPTY": ""})
        self.assertEqual(deployer.read_env(Path(self.tmp.name) / "missing"), {})

    def test_channels(self):
        folder = Path(self.tmp.name) / "devices"
        folder.mkdir()
        (folder / "b.json").write_text('{"channel": "dev"}')
        (folder / "a.json").write_text('{"room": "x"}')
        (folder / "c.json").write_text('{"channel": "beta"}')
        self.assertEqual(list(deployer.channels(folder).items()), [("a", "rel"), ("b", "dev"), ("c", "rel")])


class FakeStore:
    def __init__(self, folder):
        self.folder = Path(folder)

    def manifest(self, version):
        return {"version": version, "commit": "d" * 40, "artifacts": [{"file": deployer.BUNDLE}]}

    def artifact(self, version, name):
        return self.folder / name


@unittest.skipIf(os.name == "nt" or not shutil.which("sh"), "the Pi's sh and symlinks")
class BundleTest(unittest.TestCase):
    """The bundle's own deploy.sh runs with the version, the commit and the binary."""

    def test_unpack_and_run(self):
        with tempfile.TemporaryDirectory() as tmp:
            cfg = deployer.Config({"DEPLOYER_HOME": tmp})
            script = b'#!/bin/sh\necho "$ECHO_HOST $VERSION $COMMIT $(cat "$JARVIS_BIN") $*"\n' \
                     b'[ "${1:-}" != --status ] || echo "version=v1.0.0 commit=x state=idle"\n'
            with tarfile.open(Path(tmp) / deployer.BUNDLE, "w:gz") as t:
                for name, data in (("jarvis", b"binary"), ("devices/echo/tools/deploy.sh", script)):
                    info = tarfile.TarInfo(name)
                    info.size = len(data)
                    t.addfile(info, io.BytesIO(data))
            echos = deployer.Echos(cfg)
            echos.store = FakeStore(tmp)
            result, text = echos.install("echo-a", "v1.0.0")
            self.assertEqual((result, text), ("ok", "echo-a v1.0.0 dddddddddddd binary"))
            self.assertTrue((cfg.work / "v1.0.0" / "devices/echo/local").is_symlink())
            self.assertEqual(echos.status("echo-a", "v1.0.0"), "v1.0.0")


if __name__ == "__main__":
    unittest.main()

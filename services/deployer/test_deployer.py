"""Offline test of the deploy: fake checkout and deploy.sh; python3 test_deployer.py"""
import tempfile
import unittest
from pathlib import Path

import deployer


class FakeWorkspace:
    def __init__(self):
        self.deploys, self.results, self.output = [], [], ""

    def subject(self, sha):
        return f"subject of {sha}"

    def deploy(self, sha, binary):
        assert binary.exists(), binary
        self.deploys.append(sha)
        ok = self.results.pop(0) if self.results else True
        if not ok:
            return False, "dot-dev: version x running\necho-dev: version x is not running"
        if self.output:
            return True, self.output
        return True, "\n".join(f"{h}: version {sha[:12]} running" for h in ("dot-dev", "echo-dev", "echo2"))


class DeployerTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        home = Path(self.tmp.name)
        self.binary = home / "jarvis"
        self.binary.write_bytes(b"\x7fELF")
        (home / "builds").mkdir()
        self.cfg = deployer.Config({"DEPLOYER_HOME": self.tmp.name, "GITHUB_REPOSITORY": "o/jarvis",
                                    "GITHUB_RUN_ID": "9"})
        self.ws, self.sent = FakeWorkspace(), []

    def tearDown(self):
        self.tmp.cleanup()

    def deployer(self):
        return deployer.Deployer(self.cfg, ws=self.ws, telegram=self.sent.append)

    def test_success_reports_every_echo(self):
        self.assertTrue(self.deployer().run("a" * 40, self.binary))
        self.assertIn("Neue Version aaaaaaaaaaaa aktiv auf dot-dev, echo-dev, echo2", self.sent[-1])
        self.assertIn("https://github.com/o/jarvis/commit/" + "a" * 40, self.sent[-1])
        self.assertEqual(self.deployer().state["deployed"], "a" * 40)

    def test_failure_rolls_back_to_the_last_good(self):
        self.deployer().run("a" * 40, self.binary)
        self.ws.results = [False, True]
        self.assertFalse(self.deployer().run("b" * 40, self.binary))
        self.assertEqual(self.ws.deploys, ["a" * 40, "b" * 40, "a" * 40])
        self.assertIn("Zurückgerollt auf aaaaaaaaaaaa", self.sent[-1])
        self.assertIn("is not running", self.sent[-1])
        self.assertIn("actions/runs/9", self.sent[-1])
        self.assertEqual(self.deployer().state["deployed"], "a" * 40)

    def test_an_unreachable_echo_is_reported(self):
        self.ws.output = "dot-dev: not reachable, skipped\necho-dev: version aaaaaaaaaaaa running"
        self.assertTrue(self.deployer().run("a" * 40, self.binary))
        self.assertIn("aktiv auf echo-dev.\nNicht erreichbar, bekommt sie beim nächsten Deploy: "
                      "dot-dev.", self.sent[-1])

    def test_a_failed_checkout_is_reported_not_raised(self):
        cfg = deployer.Config({"GITHUB_WORKSPACE": self.tmp.name})  # not a git checkout
        ok, out = deployer.Workspace(cfg).deploy("c" * 40, self.binary)
        self.assertFalse(ok)
        self.assertIn("checkout of cccccccccccc failed", out)

    def test_failure_without_an_earlier_version(self):
        self.ws.results = [False]
        self.assertFalse(self.deployer().run("a" * 40, self.binary))
        self.assertEqual(self.ws.deploys, ["a" * 40])
        self.assertIn("keine frühere Version", self.sent[-1])

    def test_keeps_three_builds_and_the_running_one(self):
        d = self.deployer()
        for c in "abcde":
            d.run(c * 40, self.binary)
        self.assertEqual(len(list(self.cfg.builds.iterdir())), 3)
        self.assertTrue((self.cfg.builds / ("e" * 40) / "jarvis").exists())

    def test_links_go_to_the_code_not_the_workflow(self):
        cfg = deployer.Config({"SOURCE_REPOSITORY": "owner/code", "GITHUB_REPOSITORY": "owner/deploy",
                               "GITHUB_RUN_ID": "5"})
        self.assertEqual(cfg.repo, "owner/code")
        self.assertEqual(cfg.run_url, "https://github.com/owner/deploy/actions/runs/5")
        self.assertEqual(deployer.Config({"GITHUB_REPOSITORY": "owner/code"}).repo, "owner/code")

    def test_read_env(self):
        f = Path(self.tmp.name) / "env"
        f.write_text("# comment\nTELEGRAM_CHAT_ID=42\nEMPTY=\n")
        self.assertEqual(deployer.read_env(f), {"TELEGRAM_CHAT_ID": "42", "EMPTY": ""})
        self.assertEqual(deployer.read_env(Path(self.tmp.name) / "missing"), {})


if __name__ == "__main__":
    unittest.main()

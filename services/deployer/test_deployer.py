"""Offline test of the deploy: fake checkout and deploy.sh; python3 test_deployer.py"""
import tempfile
import unittest
from pathlib import Path

import deployer


class FakeWorkspace:
    def __init__(self):
        self.deploys, self.results = [], []

    def subject(self, sha):
        return f"subject of {sha}"

    def deploy(self, sha, binary):
        assert binary.exists(), binary
        self.deploys.append(sha)
        ok = self.results.pop(0) if self.results else True
        if not ok:
            return False, "dot-dev: version x running\necho-dev: version x is not running"
        return True, "\n".join(f"{h}: version {sha[:12]} running" for h in ("dot-dev", "echo-dev", "echo2"))


class DeployerTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        home = Path(self.tmp.name)
        self.binary = home / "assistant"
        self.binary.write_bytes(b"\x7fELF")
        (home / "builds").mkdir()
        self.cfg = deployer.Config({"DEPLOYER_HOME": self.tmp.name, "GITHUB_REPOSITORY": "o/assistant",
                                    "GITHUB_RUN_ID": "9"})
        self.ws, self.sent = FakeWorkspace(), []

    def tearDown(self):
        self.tmp.cleanup()

    def deployer(self):
        return deployer.Deployer(self.cfg, ws=self.ws, telegram=self.sent.append)

    def test_success_reports_every_echo(self):
        self.assertTrue(self.deployer().run("a" * 40, self.binary))
        self.assertIn("Neue Version aaaaaaaaaaaa aktiv auf dot-dev, echo-dev, echo2", self.sent[-1])
        self.assertIn("https://github.com/o/assistant/commit/" + "a" * 40, self.sent[-1])
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
        self.assertTrue((self.cfg.builds / ("e" * 40) / "assistant").exists())

    def test_pr_message(self):
        env = {"PR_NUMBER": "7", "PR_TITLE": "Fix timers", "PR_BRANCH": "agent/timers",
               "PR_SHA": "a" * 40, "PR_URL": "https://gh/pr/7", "RUN_URL": "https://ci/run/1",
               "ECHO_RESULT": "success", "FIRETV_RESULT": "skipped"}
        text = deployer.pr_message(env)
        for part in ("PR #7: Fix timers", "Test-Build ok (Echos), Version aaaaaaaaaaaa", "https://ci/run/1",
                     "Merge = Deploy auf Echos.", "https://gh/pr/7"):
            self.assertIn(part, text)
        env["FIRETV_RESULT"] = "success"
        self.assertIn("Merge = Deploy auf Echos und Fire TV.", deployer.pr_message(env))
        env["FIRETV_RESULT"] = "failure"
        self.assertIn("Build fehlgeschlagen: Fire TV (failure)", deployer.pr_message(env))
        env.update(ECHO_RESULT="skipped", FIRETV_RESULT="skipped")
        self.assertIn("deployt nichts", deployer.pr_message(env))

    def test_read_env(self):
        f = Path(self.tmp.name) / "env"
        f.write_text("# comment\nTELEGRAM_CHAT_ID=42\nEMPTY=\n")
        self.assertEqual(deployer.read_env(f), {"TELEGRAM_CHAT_ID": "42", "EMPTY": ""})
        self.assertEqual(deployer.read_env(Path(self.tmp.name) / "missing"), {})


if __name__ == "__main__":
    unittest.main()

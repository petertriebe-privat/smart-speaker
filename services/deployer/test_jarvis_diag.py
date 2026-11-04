"""Offline test of the diagnostics: fake speakers; python3 test_jarvis_diag.py"""
import io
import subprocess
import tempfile
import unittest
from pathlib import Path

import jarvis_diag as diag


class DiagTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.devices = Path(self.tmp.name)
        for h in ("dot-dev", "echo-dev", "echo2"):
            (self.devices / f"{h}.json").write_text("{}")
        self.calls = []

    def tearDown(self):
        self.tmp.cleanup()

    def run_fake(self, host, command, timeout=60):
        self.calls.append((host, command))
        if command.startswith("test -f /etc/echo-dot-dev-release"):
            return subprocess.CompletedProcess([], 0 if host == "dot-dev" else 1, "", "")
        return subprocess.CompletedProcess([], 0, f"out of {host}\n", "")

    def diag(self, request):
        out = io.StringIO()
        code = diag.main(request, run=self.run_fake, devices=self.devices, out=out)
        return code, out.getvalue()

    def test_status_per_board(self):
        self.assertEqual(self.diag("echo2 status")[0], 0)
        self.assertEqual(self.calls[-1], ("echo2", "/usr/local/echo-dev/jarvis/jarvis ctl status"))
        self.diag("dot-dev log 50")
        self.assertEqual(self.calls[-1], ("dot-dev", "tail -n 50 /run/echo-dot-dev/jarvis.sh.log"))

    def test_all(self):
        code, out = self.diag("all status")
        self.assertEqual(code, 0)
        for h in ("dot-dev", "echo-dev", "echo2"):
            self.assertIn(f"== {h}\nout of {h}", out)

    def test_grep_is_quoted_and_limited(self):
        self.diag("echo-dev grep 'Live closed' 20")
        self.assertEqual(self.calls[-1][1], "grep -F -- 'Live closed' /mnt/echo-net/jarvis.log | tail -n 20")
        for bad in ("echo-dev grep '$(reboot)'", "echo-dev grep 'a;reboot'", "echo-dev grep 'a`id`'",
                    "echo-dev grep \"a'b\""):
            code, out = self.diag(bad)
            self.assertEqual(code, 2, bad)
            self.assertIn("plain TEXT", out)

    def test_refuses_everything_else(self):
        for bad, why in (("echo-dev reboot", "unknown command"), ("echo-dev log 10;reboot", "not a line count"),
                         ("echo3 status", "unknown host"), ("echo-dev status now", "no arguments"),
                         ("dot-dev health", "no health log"), ("echo-dev", "usage")):
            code, out = self.diag(bad)
            self.assertEqual(code, 2, bad)
            self.assertIn(why, out, bad)
        executed = [c for _, c in self.calls if not c.startswith("test -f")]
        self.assertEqual(executed, [])

    def test_line_count_capped(self):
        self.diag("echo-dev log 999999")
        self.assertEqual(self.calls[-1][1], "tail -n 5000 /mnt/echo-net/jarvis.log")

    def test_help_and_hosts(self):
        self.assertIn("Read-only", self.diag("")[1])
        self.assertEqual(self.diag("hosts")[1].split(), ["dot-dev", "echo-dev", "echo2"])


if __name__ == "__main__":
    unittest.main()

"""Offline test of the release versions, manifests and signatures; python3 test_release.py"""
import json
import shutil
import tempfile
import unittest
from pathlib import Path

import release

VECTORS = Path(__file__).resolve().parents[2] / "shared" / "release" / "testdata"
HAVE_OPENSSL = shutil.which("openssl") is not None


def make_key(folder):
    """A throwaway P-256 key pair for the test."""
    key, pub = Path(folder) / "k.pem", Path(folder) / "k.pub"
    assert release.openssl("ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", str(key))[0] == 0
    assert release.openssl("ec", "-in", str(key), "-pubout", "-out", str(pub))[0] == 0
    return key, pub


class VersionTest(unittest.TestCase):
    def test_parse_and_order(self):
        self.assertEqual(release.parse("v1.2.3"), (1, 2, 3, None))
        self.assertEqual(release.parse("v1.3.0-dev.4"), (1, 3, 0, 4))
        for bad in ("1.2.3", "v1.2", "v01.2.3", "v1.2.3-dev.0", "v1.2.3-rc.1", "abc123", ""):
            self.assertFalse(release.is_version(bad), bad)
        order = ["v1.2.0", "v1.2.1-dev.1", "v1.2.1", "v1.3.0-dev.2", "v1.3.0-dev.10", "v1.3.0", "v2.0.0-dev.1"]
        self.assertEqual(sorted(reversed(order), key=release.key), order)
        self.assertEqual(release.channel("v1.3.0-dev.4"), "dev")
        self.assertEqual(release.channel("v1.3.0"), "rel")

    def test_next_version(self):
        tags = ["v1.2.0", "v1.3.0-dev.1", "v1.3.0-dev.2", "not-a-version"]
        self.assertEqual(release.next_version([], "rel", "minor"), "v0.1.0")
        self.assertEqual(release.next_version(tags, "rel", "patch"), "v1.2.1")
        self.assertEqual(release.next_version(tags, "rel", "minor"), "v1.3.0")
        self.assertEqual(release.next_version(tags, "rel", "major"), "v2.0.0")
        self.assertEqual(release.next_version(tags, "dev", "minor"), "v1.3.0-dev.3")
        self.assertEqual(release.next_version(tags + ["v1.3.0"], "dev", "minor"), "v1.4.0-dev.1")
        with self.assertRaises(release.ReleaseError):  # below the newest dev: dev devices never get it
            release.next_version(tags, "dev", "patch")
        self.assertEqual(release.previous(tags, "dev"), "v1.3.0-dev.2")
        self.assertEqual(release.previous(tags, "rel"), "v1.2.0")
        self.assertIsNone(release.previous([], "rel"))

    def test_version_code_rises(self):
        self.assertEqual(release.version_code("v1.2.0"), 1020099)
        self.assertEqual(release.version_code("v1.3.0-dev.4"), 1030004)
        order = ["v0.1.0", "v1.2.0", "v1.3.0-dev.1", "v1.3.0-dev.98", "v1.3.0", "v10.0.0"]
        codes = [release.version_code(v) for v in order]
        self.assertEqual(codes, sorted(codes))
        with self.assertRaises(release.ReleaseError):
            release.version_code("v1.100.0")

    def test_channels(self):
        self.assertTrue(release.accepts("dev", "dev"))
        self.assertTrue(release.accepts("dev", "rel"))
        self.assertTrue(release.accepts("rel", "rel"))
        self.assertFalse(release.accepts("rel", "dev"))
        idx = {"current": {"rel": "v1.2.0"}}
        self.assertEqual(release.current(idx, "dev"), "v1.2.0")
        self.assertIsNone(release.current({"current": {}}, "rel"))

    def test_replay_is_refused(self):
        release.check_sequence({"sequence": 5}, None)
        release.check_sequence({"sequence": 5}, 5)
        with self.assertRaises(release.ReleaseError):
            release.check_sequence({"sequence": 4}, 5)


class ManifestTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def manifest(self, version, sequence=1):
        f = self.dir / "echo-bundle.tar.gz"
        f.write_bytes(version.encode())
        return release.make_manifest(version, "a" * 12, sequence, [f], date="2026-01-01T00:00:00Z")

    def test_manifest(self):
        m = self.manifest("v1.3.0-dev.1")
        self.assertEqual(m["channel"], "dev")
        self.assertEqual(m["artifacts"][0]["size"], len("v1.3.0-dev.1"))
        release.check_artifacts(m, self.dir)
        (self.dir / "echo-bundle.tar.gz").write_bytes(b"changed")
        with self.assertRaises(release.ReleaseError):
            release.check_artifacts(m, self.dir)
        with self.assertRaises(release.ReleaseError):
            release.make_manifest("v1.0.0", "not a commit", 1, [])

    def test_index_keeps_the_newest_per_channel(self):
        versions = ["v1.0.0", "v1.1.0", "v1.2.0", "v1.3.0-dev.1", "v1.3.0-dev.2"]
        pairs = [(self.manifest(v), "0" * 64) for v in versions]
        idx = release.make_index(pairs, 7, keep=2)
        self.assertEqual([e["version"] for e in idx["releases"]],
                         ["v1.3.0-dev.2", "v1.3.0-dev.1", "v1.2.0", "v1.1.0"])
        self.assertEqual(idx["current"], {"rel": "v1.2.0", "dev": "v1.3.0-dev.2"})
        self.assertEqual(idx["sequence"], 7)
        # a release after the dev releases is the dev channel's too
        idx = release.make_index(pairs + [(self.manifest("v1.3.0"), "0" * 64)], 8)
        self.assertEqual(idx["current"], {"rel": "v1.3.0", "dev": "v1.3.0"})

    @unittest.skipUnless(HAVE_OPENSSL, "openssl")
    def test_sign_and_verify(self):
        key, pub = make_key(self.dir)
        f = self.dir / "manifest.json"
        f.write_bytes(release.dumps(self.manifest("v1.2.0")))
        self.assertFalse(release.verify(f, pub))  # unsigned
        release.sign(f, key)
        self.assertTrue(release.verify(f, pub))
        self.assertEqual(release.load_signed(f, pub)["version"], "v1.2.0")
        (self.dir / "other").mkdir()
        _, other = make_key(self.dir / "other")
        self.assertFalse(release.verify(f, other))  # someone else's key
        f.write_bytes(f.read_bytes().replace(b"v1.2.0", b"v1.2.1"))
        self.assertFalse(release.verify(f, pub))
        with self.assertRaises(release.ReleaseError):
            release.load_signed(f, pub)


@unittest.skipUnless(HAVE_OPENSSL, "openssl")
class VectorTest(unittest.TestCase):
    """The vectors every device's verifier shares (shared/release/testdata)."""

    def test_vectors_verify(self):
        pub = VECTORS / "test-key.pub"
        m = release.load_signed(VECTORS / "manifest.json", pub)
        idx = release.load_signed(VECTORS / "releases.json", pub)
        self.assertEqual(m["version"], "v1.2.0")
        self.assertEqual(idx["releases"][0]["manifest_sha256"], release.sha256(VECTORS / "manifest.json"))
        self.assertEqual(release.current(idx, "rel"), "v1.2.0")

    def test_a_changed_byte_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            for name in ("releases.json", "releases.json.sig"):
                shutil.copy(VECTORS / name, Path(tmp) / name)
            p = Path(tmp) / "releases.json"
            data = json.loads(p.read_bytes())
            data["sequence"] += 1
            p.write_bytes(release.dumps(data))
            self.assertFalse(release.verify(p, VECTORS / "test-key.pub"))


if __name__ == "__main__":
    unittest.main()

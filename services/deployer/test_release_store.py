"""Offline test of the Pi's release store; python3 test_release_store.py"""
import shutil
import tempfile
import unittest
from pathlib import Path

import release
import release_store
from test_release import HAVE_OPENSSL, make_key


@unittest.skipUnless(HAVE_OPENSSL, "openssl")
class StoreTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self.tmp.name)
        self.key, self.pub = make_key(self.dir)
        self.github = {}  # version -> folder of the "published" release
        self.fetched = []
        self.store = release_store.Store(self.dir / "releases", self.pub, self.fetch)
        self.seq = 100

    def tearDown(self):
        self.tmp.cleanup()

    def fetch(self, version, name, target):
        self.fetched.append((version, name))
        shutil.copy(self.github[version] / name, target)

    def publish(self, version):
        """A signed release as the workflow makes it; returns its folder."""
        folder = self.dir / "gh" / version
        folder.mkdir(parents=True)
        (folder / "echo-bundle.tar.gz").write_bytes(f"bundle {version}".encode())
        self.seq += 1
        m = release.make_manifest(version, "b" * 12, self.seq, [folder / "echo-bundle.tar.gz"])
        (folder / "manifest.json").write_bytes(release.dumps(m))
        release.sign(folder / "manifest.json", self.key)
        self.github[version] = folder
        return folder

    def incoming(self, version, listed, sequence=None):
        """The workflow's hand-over: the new release and an index of `listed`."""
        self.seq += 1
        folder = self.dir / "in" / f"{version}-{self.seq}"
        shutil.copytree(self.github[version], folder)
        pairs = [(release.load_signed(self.github[v] / "manifest.json", self.pub),
                  release.sha256(self.github[v] / "manifest.json")) for v in listed]
        (folder / "releases.json").write_bytes(release.dumps(release.make_index(pairs, sequence or self.seq)))
        release.sign(folder / "releases.json", self.key)
        return folder

    def test_add_and_serve_the_new_release(self):
        self.publish("v1.0.0")
        self.assertEqual(self.store.add(self.incoming("v1.0.0", ["v1.0.0"])), [])
        self.assertEqual(self.store.manifest("v1.0.0")["version"], "v1.0.0")
        self.assertEqual(self.store.index()["current"], {"rel": "v1.0.0", "dev": "v1.0.0"})
        self.assertEqual(self.fetched, [])

    def test_a_missing_listed_release_is_fetched(self):
        self.publish("v1.0.0")
        self.publish("v1.1.0-dev.1")
        self.store.add(self.incoming("v1.1.0-dev.1", ["v1.0.0", "v1.1.0-dev.1"]))
        self.assertEqual(self.store.versions(), ["v1.0.0", "v1.1.0-dev.1"])
        self.assertIn(("v1.0.0", "echo-bundle.tar.gz"), self.fetched)
        self.store.manifest("v1.0.0")

    def test_an_older_index_is_refused(self):
        self.publish("v1.0.0")
        self.store.add(self.incoming("v1.0.0", ["v1.0.0"]))
        with self.assertRaises(release.ReleaseError):
            self.store.add(self.incoming("v1.0.0", ["v1.0.0"], sequence=50))

    def test_a_changed_artifact_is_refused(self):
        folder = self.incoming(self.publish("v1.0.0").name, ["v1.0.0"])
        (folder / "echo-bundle.tar.gz").write_bytes(b"something else")
        with self.assertRaises(release.ReleaseError):
            self.store.add(folder)
        self.assertEqual(self.store.versions(), [])

    def test_a_foreign_signature_is_refused(self):
        folder = self.incoming(self.publish("v1.0.0").name, ["v1.0.0"])
        (self.dir / "x").mkdir()
        other, _ = make_key(self.dir / "x")
        release.sign(folder / "releases.json", other)
        with self.assertRaises(release.ReleaseError):
            self.store.add(folder)

    def test_a_stored_artifact_that_changed_is_refused(self):
        self.publish("v1.0.0")
        self.store.add(self.incoming("v1.0.0", ["v1.0.0"]))
        self.store.artifact("v1.0.0", "echo-bundle.tar.gz").write_bytes(b"tampered")
        with self.assertRaises(release.ReleaseError):
            self.store.manifest("v1.0.0")

    def test_prune_keeps_what_devices_run(self):
        for v in ("v1.0.0", "v1.1.0", "v1.2.0"):
            self.publish(v)
            self.store.add(self.incoming(v, [v]), in_use={"v1.0.0"})
        self.assertEqual(self.store.versions(), ["v1.0.0", "v1.2.0"])

    def test_in_use(self):
        home = self.dir / "home"
        home.mkdir()
        (home / "state.json").write_text('{"devices": {"echo-a": "v1.0.0"}, "good": {"rel": "v1.1.0"}}')
        (home / "firetv-state.json").write_text('{"devices": {"tv": "v0.9.0"}, "deployed": "baseline"}')
        self.assertEqual(release_store.in_use(home), {"v1.0.0", "v1.1.0", "v0.9.0"})


if __name__ == "__main__":
    unittest.main()

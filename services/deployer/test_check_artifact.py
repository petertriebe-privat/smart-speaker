"""Offline test of the secrets check of release artifacts; python3 test_check_artifact.py"""
import io
import tarfile
import unittest
import zipfile

import check_artifact


def tgz(files):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as t:
        for name, data in files.items():
            info = tarfile.TarInfo(name)
            info.size = len(data)
            t.addfile(info, io.BytesIO(data))
    return buf.getvalue()


def apk(files):
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        for name, data in files.items():
            z.writestr(name, data)
    return buf.getvalue()


# put together here, so the repository holds no key-shaped string
FAKE_OPENAI = b"sk-" + b"A1b2" * 10
FAKE_TELEGRAM = b"123456789:AA" + b"x" * 33


class CheckTest(unittest.TestCase):
    def test_a_clean_bundle(self):
        data = tgz({"jarvis": b"\x7fELF binary", "devices/echo/settings.json": b'{"volume": 40}'})
        self.assertEqual(check_artifact.problems("echo-bundle.tar.gz", data), [])

    def test_household_files(self):
        data = tgz({"devices/echo/local/devices/echo-a.json": b"{}", "secrets.json": b"{}", "x/release.pem": b""})
        found = check_artifact.problems("b.tar.gz", data)
        self.assertEqual(len(found), 3, found)

    def test_key_shaped_strings_also_in_nested_archives(self):
        inner = tgz({"notes.txt": b"token " + FAKE_TELEGRAM})
        data = apk({"lib/armeabi-v7a/libjarvis.so": b"x" + FAKE_OPENAI + b"x", "assets/x.tgz": inner})
        found = check_artifact.problems("app.apk", data)
        self.assertTrue(any("OpenAI key" in f for f in found), found)
        self.assertTrue(any("Telegram bot token" in f for f in found), found)

    def test_a_private_key(self):
        data = apk({"res/raw/k": b"-----BEGIN EC " + b"PRIVATE KEY-----\n"})
        self.assertTrue(check_artifact.problems("app.apk", data))


if __name__ == "__main__":
    unittest.main()

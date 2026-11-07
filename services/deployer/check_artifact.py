#!/usr/bin/env python3
"""No secrets in a public release artifact (issue #15): the Echo bundle (.tar.gz) and the APKs.

    python3 check_artifact.py ARTIFACT...

Fails on a file from local/ or one that only holds household values (secrets.json, device.json,
secrets.h, keys and keystores), and on key-shaped strings in any file, archives inside the
archive included (the core in an APK). Run by CI on the builds and by the owner's release
workflow before it publishes. Offline test: python3 test_check_artifact.py
"""
import io
import re
import sys
import tarfile
import zipfile
from pathlib import PurePosixPath

NAMES = re.compile(r"^(secrets\.json|jarvis-secrets\.json|device\.json|secrets\.h|persona_local\.h|"
                   r"adbkey|.*\.(pem|key|keystore|jks|p12))$")
PATTERNS = {
    "private key": rb"-----BEGIN [A-Z ]*PRIVATE KEY-----",
    # no word boundary in front: a binary keeps its strings back to back
    "OpenAI key": rb"sk-(proj-)?[A-Za-z0-9_-]{32,}",
    "GitHub token": rb"(ghp|gho|ghs|ghu)_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{50,}",
    "Google API key": rb"\bAIza[0-9A-Za-z_-]{35}\b",
    "AWS key": rb"\bAKIA[0-9A-Z]{16}\b",
    "Slack token": rb"\bxox[abpr]-[A-Za-z0-9-]{10,}",
    "Telegram bot token": rb"\b\d{8,10}:AA[A-Za-z0-9_-]{33}\b",
}


def members(name, data):
    """(path, bytes) of every file in an archive, nested archives unpacked."""
    yield name, data
    lower = name.lower()
    try:
        if lower.endswith((".apk", ".zip", ".jar", ".aar")):
            with zipfile.ZipFile(io.BytesIO(data)) as z:
                for info in z.infolist():
                    if not info.is_dir():
                        yield from members(f"{name}!{info.filename}", z.read(info))
        elif lower.endswith((".tar.gz", ".tgz", ".tar")):
            with tarfile.open(fileobj=io.BytesIO(data)) as t:
                for info in t.getmembers():
                    if info.isfile():
                        yield from members(f"{name}!{info.name}", t.extractfile(info).read())
    except (zipfile.BadZipFile, tarfile.TarError) as e:
        yield f"{name} (not readable: {e})", b""


def problems(name, data):
    out = []
    for path, content in members(name, data):
        inner = path.rsplit("!", 1)[-1]
        parts = PurePosixPath(inner).parts
        if inner != path and ("local" in parts[:-1] or NAMES.match(parts[-1] if parts else "")):
            out.append(f"{path}: household file")
        if "not readable" in path:
            out.append(path)
        for what, pattern in PATTERNS.items():
            if re.search(pattern, content):
                out.append(f"{path}: {what}")
    return out


def main(paths):
    bad = []
    for p in paths:
        with open(p, "rb") as f:
            bad += problems(p, f.read())
    for line in bad:
        print(line)
    if bad:
        sys.exit(f"{len(bad)} secret-shaped finding(s): not publishable")
    print(f"no secrets in {', '.join(paths)}")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit("usage: check_artifact.py ARTIFACT...")
    main(sys.argv[1:])

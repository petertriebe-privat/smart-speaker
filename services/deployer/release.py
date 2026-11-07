#!/usr/bin/env python3
"""Releases of the assistant (issue #15): versions, the signed manifest and the signed index.

Used by the owner's release workflow (the private deploy repository) and on the Raspberry Pi
(release_store.py, deployer.py, firetv_deploy.py). Standard library and the openssl command only.

Versions are tags vMAJOR.MINOR.PATCH (the channel rel: a GitHub release) or
vMAJOR.MINOR.PATCH-dev.N (the channel dev: a pre-release). A dev release comes before the
release of the same number: v1.3.0-dev.4 < v1.3.0.

manifest.json (one per release) and releases.json (the index of the releases on the Pi) are
signed with ECDSA P-256 over SHA-256: the file's exact bytes, the signature next to it as
<file>.sig in DER (ASN.1), which Go's ecdsa.VerifyASN1, Java's SHA256withECDSA and mbedTLS'
mbedtls_pk_verify all read. The private key is the owner's (a secret of the private deploy
repository), never in this repository; the devices and the Pi hold the public key.

    release.py next --channel dev|rel --bump patch|minor|major [--version V] < tags   the next version
    release.py previous --channel dev|rel < tags                         the channel's newest tag
    release.py manifest --version V --commit C --sequence N --out F [--min-version V] FILE...
    release.py index --sequence N --out F [--keep K] MANIFEST...
    release.py sign --key KEY.pem FILE...
    release.py verify --pub PUB.pem FILE...
    release.py version-code V                                            Android's versionCode

Offline test: python3 test_release.py
"""
import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

CHANNELS = ("rel", "dev")
VERSION = re.compile(r"^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-dev\.([1-9]\d*))?$")


class ReleaseError(Exception):
    pass


def parse(version):
    """(major, minor, patch, dev or None); ReleaseError for anything that is no release version."""
    m = VERSION.match(version or "")
    if not m:
        raise ReleaseError(f"not a release version: {version!r}")
    major, minor, patch, dev = m.groups()
    return int(major), int(minor), int(patch), int(dev) if dev else None


def is_version(version):
    return bool(VERSION.match(version or ""))


def key(version):
    """Sort key: v1.3.0-dev.4 < v1.3.0 < v1.3.1-dev.1."""
    major, minor, patch, dev = parse(version)
    return major, minor, patch, dev if dev is not None else float("inf")


def channel(version):
    return "dev" if parse(version)[3] is not None else "rel"


def newest(versions):
    versions = [v for v in versions if v]
    return max(versions, key=key) if versions else None


def accepts(device_channel, release_channel):
    """A dev device takes every release, a rel device only releases."""
    return device_channel == "dev" or release_channel == "rel"


def version_code(version):
    """Android's versionCode, rising with the version: MMmmppdd, dd = 99 for a release
    (v1.2.0 -> 1020099, v1.3.0-dev.4 -> 1030004). Minor and patch below 100, dev below 99.
    devices/android/build.gradle computes the same."""
    major, minor, patch, dev = parse(version)
    if minor > 99 or patch > 99 or (dev or 0) > 98:
        raise ReleaseError(f"{version}: minor and patch must stay below 100, dev below 99")
    return ((major * 100 + minor) * 100 + patch) * 100 + (99 if dev is None else dev)


def next_version(tags, channel_, bump):
    """The next version after the existing tags: the bump of the newest release, for dev with the
    next -dev.N of that number."""
    if channel_ not in CHANNELS or bump not in ("patch", "minor", "major"):
        raise ReleaseError(f"channel {channel_!r}, bump {bump!r}")
    tags = [t for t in tags if is_version(t)]
    base = newest([t for t in tags if channel(t) == "rel"]) or "v0.0.0"
    major, minor, patch, _ = parse(base)
    if bump == "major":
        major, minor, patch = major + 1, 0, 0
    elif bump == "minor":
        minor, patch = minor + 1, 0
    else:
        patch += 1
    target = f"v{major}.{minor}.{patch}"
    if channel_ == "rel":
        return target
    devs = [parse(t)[3] for t in tags if t.startswith(target + "-dev.")]
    candidate = f"{target}-dev.{max(devs, default=0) + 1}"
    # a dev device runs the newest of all: a dev release below the newest dev would never arrive
    top = newest(tags)
    if top and key(candidate) < key(top):
        raise ReleaseError(f"{candidate} would be older than {top}: bump minor or major")
    return candidate


def previous(tags, channel_):
    """The newest tag of a channel (the start of a release's notes), or None."""
    return newest([t for t in tags if is_version(t) and channel(t) == channel_])


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def dumps(obj):
    """The bytes that are signed: stable, readable."""
    return (json.dumps(obj, indent=1, sort_keys=True, ensure_ascii=False) + "\n").encode()


def now():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def make_manifest(version, commit, sequence, files, date=None, min_version=None):
    parse(version)
    if not re.fullmatch(r"[0-9a-f]{7,40}", commit or ""):
        raise ReleaseError(f"not a commit id: {commit!r}")
    m = {"version": version, "channel": channel(version), "commit": commit, "date": date or now(),
         "sequence": int(sequence),
         "artifacts": [{"file": Path(f).name, "size": Path(f).stat().st_size, "sha256": sha256(f)}
                       for f in sorted(files, key=lambda f: Path(f).name)]}
    if min_version:
        parse(min_version)
        m["min_version"] = min_version
    return m


def make_index(manifests, sequence, keep=5, date=None):
    """releases.json from (manifest dict, sha256 of its file) pairs: the newest `keep` of each
    channel, and `current` per channel (a dev device runs the newest of all)."""
    entries = {}
    for m, digest in manifests:
        entries[m["version"]] = {"version": m["version"], "channel": channel(m["version"]),
                                 "commit": m["commit"], "date": m["date"], "manifest_sha256": digest}
    kept = []
    for ch in CHANNELS:
        same = sorted((e for e in entries.values() if e["channel"] == ch), key=lambda e: key(e["version"]))
        kept += same[-keep:]
    kept.sort(key=lambda e: key(e["version"]), reverse=True)
    current = {"rel": newest(e["version"] for e in kept if e["channel"] == "rel"),
               "dev": newest(e["version"] for e in kept)}
    return {"sequence": int(sequence), "date": date or now(),
            "current": {ch: v for ch, v in current.items() if v}, "releases": kept}


def current(index, device_channel):
    """The version a device of this channel should run, or None."""
    return index.get("current", {}).get(device_channel) or (
        index.get("current", {}).get("rel") if device_channel == "dev" else None)


def check_sequence(new, last):
    """An index or manifest older than the last one seen is a replay."""
    if last is not None and int(new["sequence"]) < int(last):
        raise ReleaseError(f"sequence {new['sequence']} is older than {last}: refused")


def openssl(*args):
    r = subprocess.run(["openssl", *args], capture_output=True)
    return r.returncode, (r.stdout + r.stderr).decode(errors="replace").strip()


def sign(path, private_key):
    """Writes <path>.sig: ECDSA P-256 over SHA-256 of the file, DER."""
    rc, out = openssl("dgst", "-sha256", "-sign", str(private_key), "-out", f"{path}.sig", str(path))
    if rc != 0:
        raise ReleaseError(f"signing {path}: {out}")


def verify(path, public_key, signature=None):
    """True when <path>.sig (or signature) is the owner's signature of the file's bytes."""
    sig = Path(signature or f"{path}.sig")
    if not sig.exists() or not Path(public_key).exists():
        return False
    rc, _ = openssl("dgst", "-sha256", "-verify", str(public_key), "-signature", str(sig), str(path))
    return rc == 0


def load_signed(path, public_key):
    """The JSON of a signed file, or ReleaseError."""
    if not verify(path, public_key):
        raise ReleaseError(f"{path}: signature missing or wrong")
    return json.loads(Path(path).read_bytes())


def check_artifacts(manifest, folder):
    """ReleaseError unless every artifact of the manifest is in folder with its size and sha256."""
    for a in manifest["artifacts"]:
        p = Path(folder) / a["file"]
        if Path(a["file"]).name != a["file"]:
            raise ReleaseError(f"artifact name {a['file']!r}")
        if not p.exists():
            raise ReleaseError(f"{manifest['version']}: {a['file']} missing")
        if p.stat().st_size != a["size"] or sha256(p) != a["sha256"]:
            raise ReleaseError(f"{manifest['version']}: {a['file']} does not match the manifest")


def write(path, data):
    tmp = Path(f"{path}.tmp")
    tmp.write_bytes(data)
    os.replace(tmp, path)


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    sub = p.add_subparsers(dest="cmd", required=True)
    n = sub.add_parser("next")
    n.add_argument("--channel", required=True, choices=CHANNELS)
    n.add_argument("--bump", required=True, choices=("patch", "minor", "major"))
    n.add_argument("--version", help="this version instead of the bump: checked, and new")
    pv = sub.add_parser("previous")
    pv.add_argument("--channel", required=True, choices=CHANNELS)
    m = sub.add_parser("manifest")
    m.add_argument("--version", required=True)
    m.add_argument("--commit", required=True)
    m.add_argument("--sequence", required=True, type=int)
    m.add_argument("--min-version")
    m.add_argument("--out", required=True)
    m.add_argument("files", nargs="+")
    i = sub.add_parser("index")
    i.add_argument("--sequence", required=True, type=int)
    i.add_argument("--keep", type=int, default=5)
    i.add_argument("--out", required=True)
    i.add_argument("manifests", nargs="+")
    s = sub.add_parser("sign")
    s.add_argument("--key", required=True)
    s.add_argument("files", nargs="+")
    v = sub.add_parser("verify")
    v.add_argument("--pub", required=True)
    v.add_argument("files", nargs="+")
    c = sub.add_parser("version-code")
    c.add_argument("version")
    a = p.parse_args(argv)
    try:
        if a.cmd == "next":
            tags = sys.stdin.read().split()
            if a.version:
                if channel(a.version) != a.channel or a.version in tags:
                    raise ReleaseError(f"{a.version}: not a new version of the channel {a.channel}")
                print(a.version)
            else:
                print(next_version(tags, a.channel, a.bump))
        elif a.cmd == "previous":
            print(previous(sys.stdin.read().split(), a.channel) or "")
        elif a.cmd == "manifest":
            write(a.out, dumps(make_manifest(a.version, a.commit, a.sequence, a.files, min_version=a.min_version)))
        elif a.cmd == "index":
            pairs = [(json.loads(Path(f).read_bytes()), sha256(f)) for f in a.manifests]
            write(a.out, dumps(make_index(pairs, a.sequence, a.keep)))
        elif a.cmd == "sign":
            for f in a.files:
                sign(f, a.key)
        elif a.cmd == "verify":
            bad = [f for f in a.files if not verify(f, a.pub)]
            if bad:
                raise ReleaseError("signature wrong: " + ", ".join(bad))
        elif a.cmd == "version-code":
            print(version_code(a.version))
    except ReleaseError as e:
        sys.exit(f"release: {e}")


if __name__ == "__main__":
    main()

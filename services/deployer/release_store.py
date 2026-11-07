#!/usr/bin/env python3
"""The Raspberry Pi's release store (issue #15): every release the devices may run, signed.

    /opt/jarvis-deployer/releases/releases.json(.sig)            the signed index
    /opt/jarvis-deployer/releases/<version>/manifest.json(.sig)   the signed manifest
    /opt/jarvis-deployer/releases/<version>/<artifact>            the Echo bundle, the APKs

    python3 release_store.py add <folder>   a release from the owner's workflow: <folder> holds
                                            releases.json(.sig) and the new release's
                                            manifest.json(.sig) and artifacts
    python3 release_store.py serve          the store over HTTP for the home network (service
                                            jarvis-releases, install.sh)
    python3 release_store.py list           what the store holds

Nothing goes in without the owner's signature (release.py): the index's, each manifest's, and
every artifact's sha256 from its manifest; an index older than the stored one is refused. A
release the index lists but the folder lacks is fetched from the public GitHub release and
checked the same way. The store keeps what the index lists and every version a device still
runs (state.json, firetv-state.json), and drops the rest. HTTP without TLS: the signature, not
the transport, is the trust. Standard library and openssl only. Offline test:
python3 test_release_store.py
"""
import functools
import http.server
import json
import logging
import os
import shutil
import sys
import urllib.request
from pathlib import Path

import release

log = logging.getLogger("release-store")
INDEX = "releases.json"
MANIFEST = "manifest.json"


def github_fetch(repo):
    """fetch(version, file, target): downloads an asset of the public GitHub release."""
    def fetch(version, name, target):
        url = f"https://github.com/{repo}/releases/download/{version}/{name}"
        with urllib.request.urlopen(url, timeout=120) as r, open(target, "wb") as f:
            shutil.copyfileobj(r, f)
    return fetch


class Store:
    def __init__(self, root, public_key, fetch=None):
        self.root, self.public_key, self.fetch = Path(root), Path(public_key), fetch

    def index(self):
        """The stored index (signature checked), or None."""
        p = self.root / INDEX
        return release.load_signed(p, self.public_key) if p.exists() else None

    def versions(self):
        return sorted((p.name for p in self.root.iterdir() if release.is_version(p.name)), key=release.key) \
            if self.root.exists() else []

    def manifest(self, version):
        """The checked manifest of a stored release: signature and every artifact."""
        folder = self.root / version
        m = release.load_signed(folder / MANIFEST, self.public_key)
        if m.get("version") != version:
            raise release.ReleaseError(f"{folder}: the manifest is for {m.get('version')}")
        release.check_artifacts(m, folder)
        return m

    def artifact(self, version, name):
        return self.root / version / name

    def _put(self, folder, digest=None):
        """Copies a checked release from folder into the store; returns its version."""
        m = release.load_signed(Path(folder) / MANIFEST, self.public_key)
        version = m["version"]
        release.parse(version)
        if digest and release.sha256(Path(folder) / MANIFEST) != digest:
            raise release.ReleaseError(f"{version}: the manifest is not the one the index lists")
        release.check_artifacts(m, folder)
        target, tmp = self.root / version, self.root / f".{version}.tmp"
        shutil.rmtree(tmp, ignore_errors=True)
        tmp.mkdir(parents=True)
        for name in [MANIFEST, MANIFEST + ".sig"] + [a["file"] for a in m["artifacts"]]:
            shutil.copy(Path(folder) / name, tmp / name)
        shutil.rmtree(target, ignore_errors=True)
        tmp.rename(target)
        return version

    def _download(self, entry):
        """A release the index lists, from the public GitHub release."""
        version, tmp = entry["version"], self.root / f".{entry['version']}.download"
        shutil.rmtree(tmp, ignore_errors=True)
        tmp.mkdir(parents=True)
        try:
            for name in (MANIFEST, MANIFEST + ".sig"):
                self.fetch(version, name, tmp / name)
            m = release.load_signed(tmp / MANIFEST, self.public_key)
            for a in m["artifacts"]:
                if Path(a["file"]).name != a["file"]:
                    raise release.ReleaseError(f"artifact name {a['file']!r}")
                self.fetch(version, a["file"], tmp / a["file"])
            return self._put(tmp, entry["manifest_sha256"])
        finally:
            shutil.rmtree(tmp, ignore_errors=True)

    def add(self, folder, in_use=()):
        """Takes a new signed index (and the release next to it); returns the problems that did
        not stop it (a listed release that could not be fetched)."""
        folder = Path(folder)
        self.root.mkdir(parents=True, exist_ok=True)
        idx = release.load_signed(folder / INDEX, self.public_key)
        old = self.index()
        release.check_sequence(idx, old["sequence"] if old else None)
        listed = {e["version"]: e for e in idx["releases"]}
        if (folder / MANIFEST).exists():
            m = release.load_signed(folder / MANIFEST, self.public_key)
            if m["version"] not in listed:
                raise release.ReleaseError(f"{m['version']} is not in the index")
            self._put(folder, listed[m["version"]]["manifest_sha256"])
        problems = []
        for version, entry in listed.items():
            have = self.root / version / MANIFEST
            if have.exists() and release.sha256(have) == entry["manifest_sha256"]:
                continue
            if not self.fetch:
                problems.append(f"{version}: not in the store")
                continue
            try:
                self._download(entry)
            except Exception as e:
                problems.append(f"{version}: {e}")
        for name in (INDEX, INDEX + ".sig"):
            release.write(self.root / name, (folder / name).read_bytes())
        self.prune(set(listed) | set(in_use))
        return problems

    def prune(self, keep):
        for version in self.versions():
            if version not in keep:
                log.info("dropping %s", version)
                shutil.rmtree(self.root / version, ignore_errors=True)


def in_use(home):
    """Every version a device still runs or a deploy may roll back to (the deploy states)."""
    out = set()
    for name in ("state.json", "firetv-state.json"):
        try:
            state = json.loads((Path(home) / name).read_text())
        except (FileNotFoundError, ValueError):
            continue
        for group in ("devices", "good"):
            out |= {v for v in state.get(group, {}).values() if isinstance(v, str)}
    return out


class Handler(http.server.SimpleHTTPRequestHandler):
    """Files only: no listing, no hidden folders (an unfinished copy)."""

    def list_directory(self, path):
        self.send_error(404)

    def send_head(self):
        if "/." in self.path:
            self.send_error(404)
            return None
        return super().send_head()

    def log_message(self, fmt, *args):
        log.info("%s %s", self.address_string(), fmt % args)


def serve(root, bind, port):
    handler = functools.partial(Handler, directory=str(root))
    server = http.server.ThreadingHTTPServer((bind, port), handler)
    log.info("serving %s on %s:%d", root, bind, port)
    server.serve_forever()


def main():
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
    import deployer  # the Pi's configuration (/etc/jarvis-deployer.env)
    cfg = deployer.Config({**deployer.read_env("/etc/jarvis-deployer.env"), **os.environ})
    store = Store(cfg.releases, cfg.public_key, github_fetch(cfg.repo) if cfg.repo else None)
    cmd = sys.argv[1] if len(sys.argv) > 1 else ""
    try:
        if cmd == "add" and len(sys.argv) == 3:
            for problem in store.add(sys.argv[2], in_use(cfg.home)):
                log.warning("%s", problem)
            idx = store.index()
            print(f"store: {', '.join(e['version'] for e in idx['releases'])}; current {idx['current']}")
        elif cmd == "serve":
            serve(cfg.releases, cfg.env.get("RELEASE_BIND", "0.0.0.0"), int(cfg.env.get("RELEASE_PORT", "8380")))
        elif cmd == "list":
            idx = store.index()
            print(json.dumps(idx["current"] if idx else {}), " ".join(store.versions()))
        else:
            sys.exit("usage: release_store.py add <folder> | serve | list")
    except release.ReleaseError as e:
        sys.exit(f"release store: {e}")


if __name__ == "__main__":
    main()

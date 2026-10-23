#!/usr/bin/env python3
"""Checksum actual release bytes, retaining the existing filenames contract."""
import hashlib
import json
import os
from pathlib import Path
import re
import sys


def digest(path):
    result = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            result.update(chunk)
    return result.hexdigest()


def write_manifest(root, environment):
    source = environment["GITHUB_SHA"]
    if not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("full source SHA required")
    payloads = sorted(path for path in root.iterdir()
                      if path.is_file() and path.name not in ("SHA256SUMS", "release-manifest.json"))
    for path in payloads:
        if path.is_symlink() or not re.fullmatch(r"[A-Za-z0-9._+-]+", path.name):
            raise ValueError("release asset must have a safe regular filename")
    records = [{"name": path.name, "sha256": digest(path), "size_bytes": path.stat().st_size}
               for path in payloads]
    checksums = root / "SHA256SUMS"
    checksums.write_text("".join(f'{record["sha256"]}  {record["name"]}\n' for record in records), encoding="utf-8")
    records.append({"name": checksums.name, "sha256": digest(checksums), "size_bytes": checksums.stat().st_size})
    manifest = {
        "schema": 1, "repository": environment["GITHUB_REPOSITORY"], "commit": source,
        "tag": environment.get("RELEASE_TAG") or environment.get("GITHUB_REF_NAME", "snapshot"),
        "image_refs": environment.get("IMAGE_REFS", "").splitlines(),
        "chart_refs": environment.get("CHART_REFS", "").splitlines(),
        "files": [record["name"] for record in records], "artifacts": records,
    }
    (root / "release-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    write_manifest(Path(sys.argv[1]), os.environ)

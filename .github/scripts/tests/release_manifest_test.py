import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
import sys

sys.dont_write_bytecode = True

spec = importlib.util.spec_from_file_location("release_manifest", Path(__file__).resolve().parents[1] / "write-release-manifest.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ReleaseManifestTest(unittest.TestCase):
    def test_actual_bytes_source_and_idempotent_manifest(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            payload = b"isolated migratectl asset fixture"
            (root / "migratectl-linux-amd64").write_bytes(payload)
            environment = {"GITHUB_SHA": "a" * 40, "GITHUB_REPOSITORY": "fixture/accounts", "RELEASE_TAG": "v0.0.0-test"}
            module.write_manifest(root, environment)
            manifest = json.loads((root / "release-manifest.json").read_text())
            self.assertEqual(manifest["commit"], environment["GITHUB_SHA"])
            self.assertEqual(manifest["files"], ["migratectl-linux-amd64", "SHA256SUMS"])
            for record in manifest["artifacts"]:
                self.assertEqual(record["sha256"], hashlib.sha256((root / record["name"]).read_bytes()).hexdigest())
            self.assertEqual((root / "SHA256SUMS").read_text(), hashlib.sha256(payload).hexdigest() + "  migratectl-linux-amd64\n")
            before = (root / "release-manifest.json").read_bytes()
            module.write_manifest(root, environment)
            self.assertEqual(before, (root / "release-manifest.json").read_bytes())
            (root / "migratectl-linux-amd64").write_bytes(b"tampered")
            self.assertNotEqual(manifest["artifacts"][0]["sha256"], module.digest(root / "migratectl-linux-amd64"))

    def test_reject_mutable_source_or_unsafe_asset(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with self.assertRaises(ValueError):
                module.write_manifest(root, {"GITHUB_SHA": "main"})
            (root / "unsafe name").touch()
            with self.assertRaises(ValueError):
                module.write_manifest(root, {"GITHUB_SHA": "a" * 40})


if __name__ == "__main__":
    unittest.main()

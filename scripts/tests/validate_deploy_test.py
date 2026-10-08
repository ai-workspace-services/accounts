import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class RegionalReleaseValidationTest(unittest.TestCase):
    def validate(self, metadata, image_ref):
        with tempfile.TemporaryDirectory() as directory:
            curl = Path(directory) / "curl"
            curl.write_text('#!/bin/sh\nprintf "%s" "$TEST_PING_JSON"\n')
            curl.chmod(0o755)
            env = dict(os.environ, PATH=directory + os.pathsep + os.environ["PATH"],
                       TEST_PING_JSON=json.dumps(metadata))
            script = Path(__file__).resolve().parents[1] / "github-actions/validate-deploy.sh"
            return subprocess.run(["bash", str(script), image_ref, "https://example.test"],
                                  env=env, capture_output=True, text=True)

    def test_requires_matching_image_and_compiled_regional_contract(self):
        commit = "a" * 40
        image = "registry.example/accounts:sha-" + commit
        payload = {"status": "ok", "image": image, "tag": "sha-" + commit,
                   "commit": commit, "version": "sha-" + commit,
                   "regional_discovery": "availability-v1"}
        self.assertEqual(self.validate(payload, image).returncode, 0)
        old = dict(payload)
        del old["regional_discovery"]
        result = self.validate(old, image)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing filtered regional discovery", result.stderr)
        wrong = dict(payload, image="registry.example/accounts:sha-" + "b" * 40)
        self.assertNotEqual(self.validate(wrong, image).returncode, 0)
        self.assertNotEqual(self.validate(payload, "registry.example/accounts:latest").returncode, 0)
        self.assertNotEqual(self.validate(payload, "registry.example/accounts:sha-aaaaaaa").returncode, 0)


if __name__ == "__main__":
    unittest.main()

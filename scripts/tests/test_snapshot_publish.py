"""Exercise the Accounts publication gate with real shell execution."""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[2]


class SnapshotPublishTest(unittest.TestCase):
    def enabled(self, target, event='push', ref_type='tag', ref_name='daily-build-2026.10.16', opt_in='false', **overrides):
        workflow = (ROOT / '.github/workflows/ci-pipeline.yml').read_text()
        step = re.search(r'      - name: Resolve publish target\n(.*?)(?=\n      - name:)', workflow, re.S).group(1)
        script = textwrap.dedent(step.split('        run: |\n', 1)[1])
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'output'
            env = {**os.environ, 'PUBLISH_TARGET': target, 'PUSH_IMAGE': 'true',
                   'EVENT_NAME': event, 'REF_TYPE': ref_type, 'REF_NAME': ref_name,
                   'DEPLOY_ENV': 'uat', 'SOURCE_REF': '', 'IMAGE_TAG': '',
                   'PUBLISH_ARTIFACT_REGISTRY': opt_in, 'GITHUB_OUTPUT': str(output), **overrides}
            subprocess.run(['bash', '-euo', 'pipefail', '-c', script], env=env, check=True, capture_output=True)
            return output.read_text().strip() == 'enabled=true'

    def test_daily_snapshot_publishes_ghcr_and_release_without_google_cloud(self):
        for event in ('push', 'workflow_dispatch'):
            with self.subTest(event=event):
                self.assertTrue(self.enabled('github', event=event))
                self.assertTrue(self.enabled('release', event=event))
                self.assertFalse(self.enabled('artifact_registry', event=event))

    def test_cloud_run_publication_requires_explicit_opt_in(self):
        self.assertTrue(self.enabled('artifact_registry', event='workflow_dispatch', opt_in='true'))
        self.assertFalse(self.enabled('artifact_registry', ref_type='branch', ref_name='main', opt_in='true'))
        promotion = {'event': 'workflow_dispatch', 'ref_type': 'branch', 'ref_name': 'main',
                     'DEPLOY_ENV': 'prod', 'SOURCE_REF': 'v1.2.3', 'IMAGE_TAG': 'v1.2.3'}
        self.assertFalse(self.enabled('artifact_registry', **promotion))
        self.assertTrue(self.enabled('artifact_registry', opt_in='true', **promotion))

    def test_non_snapshot_branch_does_not_publish_a_release(self):
        self.assertFalse(self.enabled('release', event='workflow_dispatch', ref_type='branch', ref_name='main'))


if __name__ == '__main__':
    unittest.main()

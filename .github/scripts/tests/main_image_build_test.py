import importlib.util
from pathlib import Path
import unittest

p=Path(__file__).resolve().parents[1]/'write-main-image-build.py'
spec=importlib.util.spec_from_file_location('main_image_build',p)
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
SHA='a'*40;DIGEST='sha256:'+'b'*64
ENV={'GITHUB_SHA':SHA,'GITHUB_REPOSITORY':'ai-workspace-services/accounts','GITHUB_EVENT_NAME':'push',
     'GITHUB_REF':'refs/heads/main','GITHUB_RUN_ID':'123','GITHUB_RUN_ATTEMPT':'2'}
IMAGE='ghcr.io/ai-workspace-services/accounts:sha-'+SHA
LOG='layer: Pushed\nsha-'+SHA+': digest: '+DIGEST+' size: 2203\n'

class MainImageBuild(unittest.TestCase):
    def test_exact_push_digest_is_bound_to_run_and_source(self):
        d=m.document(LOG,IMAGE,ENV,SHA)
        self.assertEqual(d['image'],'ghcr.io/ai-workspace-services/accounts@'+DIGEST)
        self.assertEqual(d['source_sha'],SHA);self.assertEqual(d['run_id'],123);self.assertEqual(d['attempt'],2)
        self.assertEqual(d['platform'],'linux/amd64')

    def test_wrong_event_ref_repo_source_or_tag_rejected(self):
        for key,value in [('GITHUB_EVENT_NAME','pull_request'),('GITHUB_REF','refs/tags/daily-build-2026.10.25'),
                          ('GITHUB_REPOSITORY','fork/accounts'),('GITHUB_SHA','c'*40),('GITHUB_RUN_ID','0')]:
            with self.subTest(key=key):
                self.assertRaises(ValueError,m.document,LOG,IMAGE,dict(ENV,**{key:value}),SHA)
        self.assertRaises(ValueError,m.document,LOG,IMAGE,ENV,'c'*40)
        self.assertRaises(ValueError,m.document,LOG,IMAGE.replace(':sha-'+SHA,':latest'),ENV,SHA)

    def test_missing_duplicate_foreign_or_mutable_push_proof_rejected(self):
        for value in ('',LOG+LOG,LOG.replace(SHA,'c'*40),LOG.replace(DIGEST,'latest'),LOG.replace('size: 2203','error: denied')):
            self.assertRaises(ValueError,m.document,value,IMAGE,ENV,SHA)

if __name__=='__main__':unittest.main()

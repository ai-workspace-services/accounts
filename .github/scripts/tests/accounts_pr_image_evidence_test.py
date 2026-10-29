#!/usr/bin/env python3
import copy
import importlib.util
from pathlib import Path
import unittest

p = Path(__file__).resolve().parents[1] / 'accounts-pr-image-evidence.py'
spec = importlib.util.spec_from_file_location('evidence', p)
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)

class EvidenceTest(unittest.TestCase):
    def setUp(self):
        self.sha = 'a' * 40
        self.pull = {'state':'open', 'merged':False, 'head':{'sha':self.sha,'ref':'fix','repo':{'full_name':m.REPOSITORY}}, 'base':{'ref':'main','repo':{'full_name':m.REPOSITORY}}}
        self.run = {'id':1,'run_attempt':1,'event':'workflow_dispatch','head_branch':'fix','head_sha':self.sha,'path':m.WORKFLOW,'status':'completed','conclusion':'success','repository':{'full_name':m.REPOSITORY},'head_repository':{'full_name':m.REPOSITORY}}
        self.jobs = [{'name':'Build Accounts image (linux/amd64)','status':'completed','conclusion':'success'}, {'name':'Publish GitHub Container Registry','id':2,'status':'completed','conclusion':'success','steps':[{'name':x,'conclusion':'success'} for x in ('Resolve checked out source commit','Verify built Accounts image','Push Accounts image to GHCR')]}]
    def validate(self): return m.validate(self.run, self.jobs, self.pull, self.sha, 1, 1, 'blob', 'blob')
    def test_valid_and_digest(self):
        self.assertEqual(self.validate(), 2)
        self.assertEqual(m.digest_from_log(('sha-'+self.sha+': digest: sha256:'+'1'*64).encode(),self.sha), 'sha256:'+'1'*64)
    def test_rejects_moved_or_forked_pr_and_untrusted_workflow(self):
        for mutation in ('head','repo','merged','workflow'):
            with self.subTest(mutation=mutation):
                pull=copy.deepcopy(self.pull)
                if mutation=='head': pull['head']['sha']='b'*40
                if mutation=='repo': pull['head']['repo']['full_name']='fork/accounts'
                if mutation=='merged': pull['merged']=True
                with self.assertRaises(ValueError):
                    m.validate(self.run,self.jobs,pull,self.sha,1,1,'blob','other' if mutation=='workflow' else 'blob')
    def test_rejects_failed_or_skipped_push_and_ambiguous_digest(self):
        self.jobs[1]['steps'][-1]['conclusion']='skipped'
        with self.assertRaises(ValueError): self.validate()
        with self.assertRaises(ValueError): m.digest_from_log(b'no digest', self.sha)
        line='sha-'+self.sha+': digest: sha256:'
        with self.assertRaises(ValueError): m.digest_from_log((line+'1'*64+'\n'+line+'2'*64).encode(), self.sha)

if __name__ == '__main__': unittest.main()

#!/usr/bin/env python3
"""Record a main-run image digest from the successful existing GHCR push output."""
import argparse
import json
import os
from pathlib import Path
import re


def document(push_log, image_ref, env, checked_out_sha):
    sha = env['GITHUB_SHA']
    if (env['GITHUB_REPOSITORY'] != 'ai-workspace-services/accounts' or
        env['GITHUB_EVENT_NAME'] != 'push' or env['GITHUB_REF'] != 'refs/heads/main' or
        not re.fullmatch(r'[a-f0-9]{40}', sha) or checked_out_sha != sha):
        raise ValueError('only the exact checked-out main push source may publish UAT evidence')
    if image_ref != 'ghcr.io/ai-workspace-services/accounts:sha-'+sha:
        raise ValueError('authoritative build image source differs')
    for name in ('GITHUB_RUN_ID', 'GITHUB_RUN_ATTEMPT'):
        if not re.fullmatch(r'[1-9][0-9]*', env[name]):
            raise ValueError('run-bound identity required')
    if len(push_log.encode()) > 1_000_000:
        raise ValueError('oversized push transcript')
    matches = re.findall(r'^sha-'+re.escape(sha)+r': digest: (sha256:[a-f0-9]{64}) size: [1-9][0-9]*\s*$', push_log, re.MULTILINE)
    if len(matches) != 1:
        raise ValueError('one successful digest record for the exact pushed SHA tag required')
    return {'schema':'main-image-build/v1', 'source_repository':env['GITHUB_REPOSITORY'],
            'source_sha':sha, 'event':'push', 'ref':'refs/heads/main',
            'run_id':int(env['GITHUB_RUN_ID']), 'attempt':int(env['GITHUB_RUN_ATTEMPT']),
            'platform':'linux/amd64', 'image':'ghcr.io/ai-workspace-services/accounts@'+matches[0]}


def main():
    p=argparse.ArgumentParser()
    p.add_argument('--push-log', required=True);p.add_argument('--image-ref', required=True)
    p.add_argument('--checked-out-sha', required=True);p.add_argument('--output', required=True)
    a=p.parse_args()
    try:
        result=document(Path(a.push_log).read_text(),a.image_ref,os.environ,a.checked_out_sha)
        Path(a.output).write_text(json.dumps(result,sort_keys=True)+'\n')
    except (OSError,ValueError,KeyError):
        raise SystemExit('Main image evidence blocked: source/run/push digest mismatch.') from None

if __name__=='__main__':main()

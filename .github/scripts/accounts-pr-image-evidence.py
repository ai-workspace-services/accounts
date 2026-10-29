#!/usr/bin/env python3
"""Read-only evidence for a fixed Accounts PR image; never publishes images."""
import hashlib
import json
import os
from pathlib import Path
import re
import select
import subprocess
import sys
import time

REPOSITORY = 'ai-workspace-services/accounts'
IMAGE = 'ghcr.io/ai-workspace-services/accounts'
WORKFLOW = '.github/workflows/ci-pipeline.yml'
LIMIT = 4_000_000


def bounded(args):
    process = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    data = bytearray()
    deadline = time.monotonic() + 120
    try:
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise ValueError('retrieval timed out')
            ready, _, _ = select.select([process.stdout], [], [], min(remaining, 1))
            if not ready:
                continue
            chunk = os.read(process.stdout.fileno(), min(65536, LIMIT - len(data) + 1))
            if not chunk:
                break
            data.extend(chunk)
            if len(data) > LIMIT:
                raise ValueError('retrieval exceeds size limit')
        if process.wait(timeout=max(.01, deadline - time.monotonic())) != 0:
            raise ValueError('retrieval failed')
        return bytes(data)
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        process.stdout.close()


def api(path):
    return json.loads(bounded(['gh', 'api', '--hostname', 'github.com', path]))


def validate(run, jobs, pull, source_sha, run_id, attempt, workflow_head, workflow_base):
    if (pull.get('state') != 'open' or pull.get('merged') is not False or
            pull.get('head', {}).get('sha') != source_sha or
            pull.get('head', {}).get('repo', {}).get('full_name') != REPOSITORY or
            pull.get('base', {}).get('repo', {}).get('full_name') != REPOSITORY or
            pull.get('base', {}).get('ref') != 'main'):
        raise ValueError('PR is not the fixed open same-repository head')
    if (not workflow_head or workflow_head != workflow_base or
            run.get('id') != run_id or run.get('run_attempt') != attempt or
            run.get('event') != 'workflow_dispatch' or
            run.get('head_branch') != pull['head']['ref'] or run.get('head_sha') != source_sha or
            run.get('path', '').split('@')[0] != WORKFLOW or
            run.get('status') != 'completed' or run.get('conclusion') != 'success' or
            run.get('repository', {}).get('full_name') != REPOSITORY or
            run.get('head_repository', {}).get('full_name') != REPOSITORY):
        raise ValueError('source build provenance mismatch')
    if not isinstance(jobs, list):
        raise ValueError('missing jobs')
    builds = [job for job in jobs if job.get('name') == 'Build Accounts image (linux/amd64)']
    pushes = [job for job in jobs if job.get('name') == 'Publish GitHub Container Registry']
    if len(builds) != 1 or len(pushes) != 1:
        raise ValueError('unexpected build job set')
    for job in builds + pushes:
        if job.get('status') != 'completed' or job.get('conclusion') != 'success':
            raise ValueError('required job did not succeed')
    job = pushes[0]
    if type(job.get('id')) is not int or job['id'] <= 0:
        raise ValueError('invalid push job')
    for name in ('Resolve checked out source commit', 'Verify built Accounts image', 'Push Accounts image to GHCR'):
        selected = [step for step in job.get('steps', []) if step.get('name') == name]
        if len(selected) != 1 or selected[0].get('conclusion') != 'success':
            raise ValueError('required build step did not succeed')
    return job['id']


def digest_from_log(raw, source_sha):
    matches = set(re.findall(rb'(?m)\bsha-' + source_sha.encode() + rb': digest: (sha256:[0-9a-f]{64})\b', raw))
    if len(matches) != 1:
        raise ValueError('source SHA tag digest missing or ambiguous')
    return matches.pop().decode('ascii')


def main():
    try:
        env = os.environ
        owner_sha = env['GITHUB_SHA']
        if (env['GITHUB_REPOSITORY'] != REPOSITORY or env['GITHUB_EVENT_NAME'] != 'workflow_dispatch' or
                env['GITHUB_REF'] != 'refs/heads/main' or not re.fullmatch(r'[0-9a-f]{40}', owner_sha)):
            raise ValueError('evidence workflow must run on Main')
        values = [env['BUILD_RUN_ID'], env['BUILD_RUN_ATTEMPT'], env['PR_NUMBER']]
        if not all(re.fullmatch(r'[1-9][0-9]*', value) for value in values):
            raise ValueError('numeric producer run, attempt and PR required')
        run_id, attempt, number = map(int, values)
        pull = api(f'repos/{REPOSITORY}/pulls/{number}')
        source_sha = pull.get('head', {}).get('sha', '')
        if not re.fullmatch(r'[0-9a-f]{40}', source_sha):
            raise ValueError('fixed PR SHA required')
        run = api(f'repos/{REPOSITORY}/actions/runs/{run_id}')
        jobs = api(f'repos/{REPOSITORY}/actions/runs/{run_id}/attempts/{attempt}/jobs?per_page=100')
        workflow_head = api(f'repos/{REPOSITORY}/contents/{WORKFLOW}?ref={source_sha}').get('sha')
        workflow_base = api(f'repos/{REPOSITORY}/contents/{WORKFLOW}?ref={owner_sha}').get('sha')
        job_id = validate(run, jobs.get('jobs'), pull, source_sha, run_id, attempt, workflow_head, workflow_base)
        log = bounded(['gh', 'api', '--hostname', 'github.com', '--allow-escape-sequences',
                       f'repos/{REPOSITORY}/actions/jobs/{job_id}/logs'])
        digest = digest_from_log(log, source_sha)
        raw = bounded(['docker', 'buildx', 'imagetools', 'inspect', '--raw', IMAGE + '@' + digest])
        if 'sha256:' + hashlib.sha256(raw).hexdigest() != digest:
            raise ValueError('registry manifest digest mismatch')
        # Recheck the PR after retrieving build and registry evidence.
        current = api(f'repos/{REPOSITORY}/pulls/{number}')
        if current.get('state') != 'open' or current.get('head', {}).get('sha') != source_sha or current.get('base', {}).get('sha') != pull['base']['sha']:
            raise ValueError('PR moved during evidence retrieval')
        proof = {'schema': 1, 'repository': REPOSITORY, 'source_sha': source_sha, 'image': IMAGE + '@' + digest,
                 'pr': number, 'base_sha': pull['base']['sha'], 'producer_run_id': run_id,
                 'producer_attempt': attempt, 'producer_job_id': job_id, 'producer_workflow': WORKFLOW,
                 'evidence_kind': 'github-pr-run-and-registry'}
        Path(env['EVIDENCE_PATH']).write_text(json.dumps(proof, sort_keys=True) + '\n', encoding='utf-8')
        return 0
    except Exception:
        print('::error::Accounts PR build or registry provenance could not be verified', file=sys.stderr)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())

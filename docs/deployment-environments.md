# Deployment environments

This service only automatically deploys stable releases: a `release/**` branch
or `v*` tag resolves to `prod`. A `main` push does not deploy accounts.

Use `workflow_dispatch` to select `dev`, `uat`, or `prod`. The deploy and
validation jobs bind to the matching GitHub Environment, which is where
approval rules and environment-scoped secrets belong.

Configure these repository variables before dispatching an environment:

| Environment | Target variable | Service URL variable |
| --- | --- | --- |
| dev | `DEV_TARGET_HOST` | `DEV_SERVICE_URL` |
| uat | `UAT_TARGET_HOST` | `UAT_SERVICE_URL` |
| prod | `PROD_TARGET_HOST` | `PROD_SERVICE_URL` (defaults to `https://accounts.svc.plus`) |

The resolver fails closed when a host or service URL is absent; it never sends
UAT or dev work to production. UAT hosts must come from the CMDB output of
`ai-workspace-infra/site-migration-toolkit` after its UAT resource and DNS
creation completes.

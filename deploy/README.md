# Deployment Configuration Guide

This directory contains the deployment configurations and procedures for the Accounts Service (`accounts.svc.plus`) on Google Cloud Run. The existing VPS `docker run`/Compose flow remains unchanged.

## Environments

### 1. Production Environment
- **Service Name**: `accounts-svc-plus`
- **Repository**: [https://github.com/cloud-neutral-toolkit/accounts.svc.plus.git](https://github.com/cloud-neutral-toolkit/accounts.svc.plus.git)
- **Branch**: `release/v0.1`
- **Configuration File**: `deploy/gcp/cloud-run/prod-service.yaml`
- **Deployment Status**: [Production URL](https://accounts-svc-plus-266500572462.asia-northeast1.run.app)

### 2. Preview Environment
- **Service Name**: `preview-accounts-svc-plus`
- **Repository**: [https://github.com/cloud-neutral-toolkit/accounts.svc.plus.git](https://github.com/cloud-neutral-toolkit/accounts.svc.plus.git)
- **Branch**: `main`
- **Configuration File**: `deploy/gcp/cloud-run/preview-service.yaml`
- **Deployment Status**: [Preview URL](https://preview-accounts-svc-plus-266500572462.asia-northeast1.run.app)

---

## Deployment Procedures

The Makefile is the recommended entrypoint because it selects the environment
manifest and renders the selected Artifact Registry image into both the image
field and the runtime `IMAGE` metadata:

```bash
make cloudrun-build CLOUD_RUN_ENV=preview
make cloudrun-deploy CLOUD_RUN_ENV=preview
```

Use `CLOUD_RUN_ENV=prod` for production. Set `GCP_PROJECT`, `GCP_REGION`, or
`CLOUD_RUN_IMAGE` to override the defaults.

### Build and Deploy Preview (from `main`)
```bash
# 1. Switch to main branch
git checkout main

# 2. Build image via Cloud Build
gcloud builds submit --tag asia-northeast1-docker.pkg.dev/xzerolab-480008/cloud-run-source-deploy/accounts.svc.plus/preview-accounts-svc-plus:latest --project xzerolab-480008

# 3. Apply Cloud Run configuration
gcloud run services replace deploy/gcp/cloud-run/preview-service.yaml --project xzerolab-480008 --region asia-northeast1

# 4. Ensure public access
gcloud run services add-iam-policy-binding preview-accounts-svc-plus --project xzerolab-480008 --region asia-northeast1 --member="allUsers" --role="roles/run.invoker"
```

### Build and Deploy Production (from `release/v0.1`)
```bash
# 1. Switch to release branch
git checkout release/v0.1

# 2. Build image via Cloud Build
gcloud builds submit --tag asia-northeast1-docker.pkg.dev/xzerolab-480008/cloud-run-source-deploy/accounts.svc.plus/accounts-svc-plus:v0.1 --project xzerolab-480008

# 3. Apply Cloud Run configuration
# Note: Ensure the image path in service.yaml matches the versioned tag
gcloud run services replace deploy/gcp/cloud-run/prod-service.yaml --project xzerolab-480008 --region asia-northeast1
```

## Infrastructure Components
- **Stunnel Sidecar**: Used for secure connection to the PostgreSQL database. Configuration is stored in Secret Manager as `stunnel-config`.
- **Secrets**:
  - `postgres-password`: Database access.
  - `internal-service-token`: RPC/Internal communication.
  - `stunnel-config`: Sidecar tunnel settings.
  - `smtp-username` / `smtp-password`: Email delivery.

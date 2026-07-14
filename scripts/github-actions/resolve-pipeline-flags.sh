#!/usr/bin/env bash
set -euo pipefail

BASE_IMAGES_EXISTS=false
RUN_BASE_IMAGES=false
PUSH_BASE_IMAGES=false
PUSH_IMAGE=true
PUSH_LATEST=false
IMAGE_TAG=""
BASE_IMAGE_REGISTRY="ghcr.io"
BASE_IMAGE_ORG="${IMAGE_REPO_OWNER:-${GITHUB_REPOSITORY_OWNER:-}}"
DOCKERHUB_NAMESPACE="${DOCKERHUB_NAMESPACE:-cloudneutral}"
DEFAULT_TARGET_HOST="${DEFAULT_TARGET_HOST:?DEFAULT_TARGET_HOST is required}"
PROD_TARGET_HOST="${PROD_TARGET_HOST:-${DEFAULT_TARGET_HOST}}"
UAT_TARGET_HOST="${UAT_TARGET_HOST:-}"
RUN_APPLY=true

# Resolve the deploy environment from the git ref:
#   refs/tags/v*        -> prod   (release tags)
#   refs/heads/release/* -> prod  (release branches)
#   refs/heads/main     -> uat    (continuous)
# workflow_dispatch uses INPUT_DEPLOY_ENV (default uat). Pull requests do not
# deploy (push_image=false below), so their env is informational only.
resolve_deploy_env() {
  if [[ "${GITHUB_EVENT_NAME}" == "workflow_dispatch" ]]; then
    printf '%s' "${INPUT_DEPLOY_ENV:-uat}"
    return
  fi
  case "${GITHUB_REF:-}" in
    refs/tags/v*)        printf 'prod' ;;
    refs/heads/release/*) printf 'prod' ;;
    *)                   printf 'uat' ;;
  esac
}

DEPLOY_ENV="$(resolve_deploy_env)"

# Map env -> host. prod falls back to the historical host; uat must be
# configured (UAT_TARGET_HOST) so main can never silently deploy to prod.
if [[ "${DEPLOY_ENV}" == "prod" ]]; then
  TARGET_HOST="${PROD_TARGET_HOST}"
else
  TARGET_HOST="${UAT_TARGET_HOST}"
fi

if [[ -d deploy/base-images ]] && find deploy/base-images -type f | grep -q .; then
  BASE_IMAGES_EXISTS=true
fi

if [[ "${GITHUB_EVENT_NAME}" == "workflow_dispatch" ]]; then
  TARGET_HOST="${INPUT_TARGET_HOST:-${TARGET_HOST}}"
  [[ "${INPUT_RUN_APPLY:-true}" == "true" ]] && RUN_APPLY=true || RUN_APPLY=false
  [[ "${INPUT_PUSH_IMAGE:-true}" == "true" ]] && PUSH_IMAGE=true || PUSH_IMAGE=false
  [[ "${INPUT_PUSH_LATEST:-false}" == "true" ]] && PUSH_LATEST=true || PUSH_LATEST=false
  [[ "${INPUT_RUN_BASE_IMAGES:-false}" == "true" ]] && RUN_BASE_IMAGES=true || RUN_BASE_IMAGES=false
  [[ "${INPUT_PUSH_BASE_IMAGES:-true}" == "true" ]] && PUSH_BASE_IMAGES=true || PUSH_BASE_IMAGES=false
  BASE_IMAGE_REGISTRY="${INPUT_BASE_IMAGE_REGISTRY:-${BASE_IMAGE_REGISTRY}}"
  BASE_IMAGE_ORG="${INPUT_BASE_IMAGE_ORG:-${BASE_IMAGE_ORG}}"
  DOCKERHUB_NAMESPACE="${INPUT_DOCKERHUB_NAMESPACE:-${DOCKERHUB_NAMESPACE}}"
  if [[ "${BASE_IMAGES_EXISTS}" != "true" ]]; then
    RUN_BASE_IMAGES=false
    PUSH_BASE_IMAGES=false
  fi
else
  if [[ "${GITHUB_EVENT_NAME}" == "pull_request" ]]; then
    PUSH_IMAGE=false
  fi

  # 'latest' tracks the uat line (main) only, so a prod release tag/branch
  # never clobbers the pointer uat continuous deploys rely on. Release images
  # are still pushed under their commit-sha tag (used for the actual deploy).
  if [[ "${GITHUB_EVENT_NAME}" == "push" && "${DEPLOY_ENV}" == "uat" ]]; then
    PUSH_LATEST=true
  fi

  if [[ "${BASE_IMAGES_EXISTS}" == "true" ]]; then
    if [[ "${GITHUB_EVENT_NAME}" == "pull_request" ]]; then
      base_ref="${PR_BASE_SHA:-}"
      head_ref="${PR_HEAD_SHA:-}"
    else
      base_ref="${GITHUB_BEFORE:-}"
      head_ref="${GITHUB_SHA:-}"
    fi

    if [[ -n "${base_ref}" && "${base_ref}" != "0000000000000000000000000000000000000000" ]]; then
      if git diff --name-only "${base_ref}" "${head_ref}" | grep -q '^deploy/base-images/'; then
        RUN_BASE_IMAGES=true
        if [[ "${GITHUB_EVENT_NAME}" == "push" ]]; then
          PUSH_BASE_IMAGES=true
        fi
      fi
    fi
  fi
fi

# A real deploy (image pushed + apply) must have a resolved host. This trips
# when main -> uat runs without UAT_TARGET_HOST configured, and fails loudly
# instead of silently doing nothing or defaulting to the prod host.
if [[ "${PUSH_IMAGE}" == "true" && "${RUN_APPLY}" == "true" && -z "${TARGET_HOST}" ]]; then
  echo "resolve-pipeline-flags: no target host for deploy env '${DEPLOY_ENV}'." >&2
  if [[ "${DEPLOY_ENV}" == "uat" ]]; then
    echo "Set the UAT_TARGET_HOST repository variable (main deploys to uat)." >&2
  else
    echo "Set the PROD_TARGET_HOST repository variable." >&2
  fi
  exit 1
fi

cat <<EOF
base_images_exists=${BASE_IMAGES_EXISTS}
run_base_images=${RUN_BASE_IMAGES}
push_base_images=${PUSH_BASE_IMAGES}
base_image_registry=${BASE_IMAGE_REGISTRY}
base_image_org=${BASE_IMAGE_ORG}
dockerhub_namespace=${DOCKERHUB_NAMESPACE}
target_host=${TARGET_HOST}
deploy_env=${DEPLOY_ENV}
run_apply=${RUN_APPLY}
image_tag=${IMAGE_TAG}
push_image=${PUSH_IMAGE}
push_latest=${PUSH_LATEST}
EOF

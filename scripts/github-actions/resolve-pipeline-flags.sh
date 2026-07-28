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

# Resolve the deploy environment from the git ref:
#   refs/tags/v*        -> prod   (release tags)
#   refs/heads/release/* -> prod  (release branches)
#   refs/heads/main     -> uat    (continuous)
#   refs/heads/daily-build-* -> input environment (snapshot dispatch)
# workflow_dispatch uses INPUT_DEPLOY_ENV (default uat). Pull requests do not
# deploy (push_image=false below), so their env is informational only.
resolve_deploy_env() {
  if [[ "${GITHUB_EVENT_NAME}" == "pull_request" ]]; then
    printf 'sit'
    return
  fi
  case "${GITHUB_REF:-}" in
    refs/tags/v*|refs/heads/release/*)
      if [[ "${GITHUB_EVENT_NAME}" == "workflow_dispatch" && -n "${INPUT_DEPLOY_ENV:-}" ]]; then
        printf '%s' "${INPUT_DEPLOY_ENV}"
      else
        printf 'prod'
      fi
      ;;
    refs/heads/main|refs/heads/daily-build-*)
      if [[ "${GITHUB_EVENT_NAME}" == "workflow_dispatch" && -n "${INPUT_DEPLOY_ENV:-}" ]]; then
        printf '%s' "${INPUT_DEPLOY_ENV}"
      else
        printf 'uat'
      fi
      ;;
    *)
      # Vault OIDC roles for uat and prod require approved refs.
      # Custom or feature branches must use sit to satisfy Vault claim validation.
      printf 'sit'
      ;;
  esac
}

DEPLOY_ENV="$(resolve_deploy_env)"

if [[ -d deploy/base-images ]] && find deploy/base-images -type f | grep -q .; then
  BASE_IMAGES_EXISTS=true
fi

if [[ "${GITHUB_EVENT_NAME}" == "workflow_dispatch" ]]; then
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

cat <<EOF
base_images_exists=${BASE_IMAGES_EXISTS}
run_base_images=${RUN_BASE_IMAGES}
push_base_images=${PUSH_BASE_IMAGES}
base_image_registry=${BASE_IMAGE_REGISTRY}
base_image_org=${BASE_IMAGE_ORG}
dockerhub_namespace=${DOCKERHUB_NAMESPACE}
deploy_env=${DEPLOY_ENV}
image_tag=${IMAGE_TAG}
push_image=${PUSH_IMAGE}
push_latest=${PUSH_LATEST}
EOF

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
TARGET_HOST=""
DEPLOYMENT_ENVIRONMENT=""
SERVICE_URL=""
RUN_APPLY=true

if [[ -d deploy/base-images ]] && find deploy/base-images -type f | grep -q .; then
  BASE_IMAGES_EXISTS=true
fi

if [[ "${GITHUB_EVENT_NAME}" == "workflow_dispatch" ]]; then
  DEPLOYMENT_ENVIRONMENT="${INPUT_DEPLOYMENT_ENVIRONMENT:-uat}"
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

  if [[ "${GITHUB_EVENT_NAME}" == "push" ]]; then
    PUSH_LATEST=true
    if [[ "${REF_NAME:-}" == "main" ]]; then
      DEPLOYMENT_ENVIRONMENT="uat"
    elif [[ "${REF_NAME:-}" == release/* || "${REF_TYPE:-}" == "tag" ]]; then
      DEPLOYMENT_ENVIRONMENT="prod"
    else
      echo "Unsupported automatic deployment ref: ${REF_NAME:-}" >&2
      exit 1
    fi
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

if [[ -z "${DEPLOYMENT_ENVIRONMENT}" ]]; then
  # Pull-request builds never deploy. Still return a stable value for job outputs.
  DEPLOYMENT_ENVIRONMENT="dev"
fi

case "${DEPLOYMENT_ENVIRONMENT}" in
  dev)
    TARGET_HOST="${INPUT_TARGET_HOST:-${DEV_TARGET_HOST:-}}"
    SERVICE_URL="${DEV_SERVICE_URL:-}"
    ;;
  uat)
    TARGET_HOST="${INPUT_TARGET_HOST:-${UAT_TARGET_HOST:-}}"
    SERVICE_URL="${UAT_SERVICE_URL:-}"
    ;;
  prod)
    TARGET_HOST="${INPUT_TARGET_HOST:-${PROD_TARGET_HOST:-}}"
    SERVICE_URL="${PROD_SERVICE_URL:-https://accounts.svc.plus}"
    ;;
  *)
    echo "Unsupported deployment environment: ${DEPLOYMENT_ENVIRONMENT}. Use dev, uat, or prod." >&2
    exit 1
    ;;
esac

if [[ "${GITHUB_EVENT_NAME}" != "pull_request" ]]; then
  if [[ -z "${TARGET_HOST}" ]]; then
    echo "${DEPLOYMENT_ENVIRONMENT^^}_TARGET_HOST must be configured before deploying ${DEPLOYMENT_ENVIRONMENT}." >&2
    exit 1
  fi
  if [[ -z "${SERVICE_URL}" ]]; then
    echo "${DEPLOYMENT_ENVIRONMENT^^}_SERVICE_URL must be configured before deploying ${DEPLOYMENT_ENVIRONMENT}." >&2
    exit 1
  fi
fi

cat <<EOF
base_images_exists=${BASE_IMAGES_EXISTS}
run_base_images=${RUN_BASE_IMAGES}
push_base_images=${PUSH_BASE_IMAGES}
base_image_registry=${BASE_IMAGE_REGISTRY}
base_image_org=${BASE_IMAGE_ORG}
dockerhub_namespace=${DOCKERHUB_NAMESPACE}
target_host=${TARGET_HOST}
deployment_environment=${DEPLOYMENT_ENVIRONMENT}
service_url=${SERVICE_URL}
run_apply=${RUN_APPLY}
image_tag=${IMAGE_TAG}
push_image=${PUSH_IMAGE}
push_latest=${PUSH_LATEST}
EOF

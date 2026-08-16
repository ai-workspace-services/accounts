#!/usr/bin/env bash
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_common.sh"

: "${GCP_PROJECT:?GCP_PROJECT 不能为空}"
: "${GCP_REGION:?GCP_REGION 不能为空}"
: "${CLOUD_RUN_SERVICE:?CLOUD_RUN_SERVICE 不能为空}"
: "${CLOUD_RUN_SERVICE_YAML:?CLOUD_RUN_SERVICE_YAML 不能为空}"
: "${CLOUD_RUN_IMAGE:?CLOUD_RUN_IMAGE 不能为空}"

if [ ! -f "${CLOUD_RUN_SERVICE_YAML}" ]; then
  echo "Cloud Run service manifest not found: ${CLOUD_RUN_SERVICE_YAML}" >&2
  exit 1
fi

rendered_manifest="$(mktemp)"
trap 'rm -f "${rendered_manifest}"' EXIT
awk -v image="${CLOUD_RUN_IMAGE}" -v service="${CLOUD_RUN_SERVICE}" '
  $0 == "metadata:" { top_metadata = 1; print; next }
  top_metadata && $0 ~ /^  name:/ {
    sub(/name: .*/, "name: " service)
    top_metadata = 0
  }
  top_metadata && $0 !~ /^  / { top_metadata = 0 }
  $0 ~ /^      - name:/ {
    container_count++
    app_container = (container_count == 1)
    image_env = 0
  }
  app_container && $0 ~ /^        image:/ { sub(/image: .*/, "image: " image) }
  app_container && $0 ~ /^        - name: IMAGE$/ { image_env = 1 }
  app_container && image_env && $0 ~ /^          value:/ {
    if ($0 ~ /value: "/) sub(/value: ".*"/, "value: \"" image "\"")
    else sub(/value: .*/, "value: " image)
    image_env = 0
  }
  { print }
' "${CLOUD_RUN_SERVICE_YAML}" > "${rendered_manifest}"

gcloud run services replace "${rendered_manifest}" --region "${GCP_REGION}" --project "${GCP_PROJECT}"

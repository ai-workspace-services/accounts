#!/usr/bin/env bash
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_common.sh"

: "${GCP_PROJECT:?GCP_PROJECT 不能为空}"
: "${CLOUD_RUN_IMAGE:?CLOUD_RUN_IMAGE 不能为空}"

gcloud builds submit --project "${GCP_PROJECT}" --tag "${CLOUD_RUN_IMAGE}" .

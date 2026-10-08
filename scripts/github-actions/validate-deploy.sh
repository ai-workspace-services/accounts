#!/usr/bin/env bash
set -euo pipefail

IMAGE_REF="${1:?image_ref is required}"
BASE_URL="${2:-https://accounts.svc.plus}"

image_ref="$(printf '%s' "${IMAGE_REF}" | tr -d '\n' | xargs)"
if [[ -z "${image_ref}" ]]; then
  echo "image_ref is required" >&2
  exit 1
fi

image_no_digest="${image_ref%@*}"
tag="${image_no_digest##*:}"
if [[ "${image_no_digest}" == "${tag}" ]]; then
  tag=""
fi

commit=""
version="${tag}"

if [[ "${tag}" =~ ^[0-9a-f]{40}$ ]]; then
  commit="${tag}"
elif [[ "${tag}" =~ ^sha-([0-9a-f]{40})$ ]]; then
  commit="${BASH_REMATCH[1]}"
fi

if [[ -z "${commit}" ]]; then
  echo "image_ref must identify a full 40-character commit SHA" >&2
  exit 1
fi

ping_json="$(
  curl \
    --silent \
    --show-error \
    --fail \
    --location \
    --max-time 20 \
    "${BASE_URL}/api/ping"
)"

PING_JSON="${ping_json}" python3 - "${image_ref}" "${tag}" "${commit}" "${version}" <<'PY'
import json
import os
import sys

image_ref, tag, commit, version = sys.argv[1:5]
payload = json.loads(os.environ["PING_JSON"])

if payload.get("status") != "ok":
    raise SystemExit("ping status not ok")

if payload.get("regional_discovery") != "availability-v1":
    raise SystemExit("runtime is missing filtered regional discovery with closure reasons")

runtime_image = payload.get("image") or ""
runtime_tag = payload.get("tag") or ""
runtime_commit = payload.get("commit") or ""
runtime_version = payload.get("version") or ""

if not runtime_image:
    raise SystemExit(
        "runtime /api/ping response did not include IMAGE-derived image identity; "
        "expected the deployed container to receive IMAGE=<service_image_ref>"
    )

if runtime_image != image_ref:
    raise SystemExit(f"expected image {image_ref!r}, got {runtime_image!r}")

if tag and runtime_tag != tag:
    raise SystemExit(f"expected tag {tag!r}, got {runtime_tag!r}")

if commit and runtime_commit != commit:
    raise SystemExit(f"expected commit {commit!r}, got {runtime_commit!r}")

if version and runtime_version != version:
    raise SystemExit(f"expected version {version!r}, got {runtime_version!r}")
PY

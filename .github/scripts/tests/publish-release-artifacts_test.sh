#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script_path="${repository_root}/scripts/publish-release-artifacts.sh"
test_temp_dir="$(mktemp -d)"

cleanup() {
  rm -rf "${test_temp_dir}"
}
trap cleanup EXIT

export RUNNER_TEMP="${test_temp_dir}"
export GITHUB_REPOSITORY="ai-workspace-services/accounts"
export GITHUB_SHA="b2c89b53e774c398cb0653e124ca0121f0a014b7"
export GITHUB_REF_TYPE="tag"
export RELEASE_TAG="daily-build-2026.08.17-r2"
export GITHUB_RELEASE_MAX_ATTEMPTS=4
export GITHUB_RELEASE_RETRY_DELAY_SECONDS=0

gh() {
  local attempt_file
  local attempt

  case "$1 $2" in
    "release view")
      attempt_file="${test_temp_dir}/view-attempts"
      attempt=0
      [[ ! -f "${attempt_file}" ]] || read -r attempt < "${attempt_file}"
      attempt=$((attempt + 1))
      printf '%s\n' "${attempt}" > "${attempt_file}"
      if [[ "${TEST_MODE:-retry}" == "forbidden" ]]; then
        printf 'HTTP 403 Forbidden\n' >&2
        return 1
      fi
      if [[ "${attempt}" -le 2 ]]; then
        printf 'non-200 OK status code: 503 Service Unavailable\n' >&2
        return 1
      fi
      printf 'release not found\n' >&2
      return 1
      ;;
    "release create")
      attempt_file="${test_temp_dir}/create-attempts"
      attempt=0
      [[ ! -f "${attempt_file}" ]] || read -r attempt < "${attempt_file}"
      printf '%s\n' "$((attempt + 1))" > "${attempt_file}"
      return 0
      ;;
    "release upload")
      attempt_file="${test_temp_dir}/upload-attempts"
      attempt=0
      [[ ! -f "${attempt_file}" ]] || read -r attempt < "${attempt_file}"
      attempt=$((attempt + 1))
      printf '%s\n' "${attempt}" > "${attempt_file}"
      if [[ "${attempt}" -eq 1 ]]; then
        printf 'non-200 OK status code: 503 Service Unavailable\n' >&2
        return 1
      fi
      return 0
      ;;
  esac

  printf 'unexpected gh invocation: %s\n' "$*" >&2
  return 2
}

sleep() {
  :
}

source "${script_path}"
main

read -r view_attempts < "${test_temp_dir}/view-attempts"
read -r create_attempts < "${test_temp_dir}/create-attempts"
read -r upload_attempts < "${test_temp_dir}/upload-attempts"
[[ "${view_attempts}" -eq 3 ]]
[[ "${create_attempts}" -eq 1 ]]
[[ "${upload_attempts}" -eq 2 ]]

rm -f "${test_temp_dir}/view-attempts" "${test_temp_dir}/create-attempts" "${test_temp_dir}/upload-attempts"
TEST_MODE=forbidden
if main; then
  printf 'expected an immediate failure for a non-retryable release lookup\n' >&2
  exit 1
fi
read -r view_attempts < "${test_temp_dir}/view-attempts"
[[ "${view_attempts}" -eq 1 ]]
[[ ! -e "${test_temp_dir}/create-attempts" ]]
[[ ! -e "${test_temp_dir}/upload-attempts" ]]

printf 'publish-release-artifacts retry behavior verified\n'

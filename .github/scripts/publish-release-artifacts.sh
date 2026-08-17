#!/usr/bin/env bash
set -euo pipefail

is_retryable_github_failure() {
  local output="$1"
  [[ "${output}" =~ (HTTP[[:space:]]+|status[[:space:]]+code:[[:space:]]*)(429|5[0-9]{2}) ]] ||
    [[ "${output}" =~ (connection[[:space:]]+reset|connection[[:space:]]+refused|TLS[[:space:]]+handshake[[:space:]]+timeout|EOF) ]]
}

is_missing_release() {
  local output="$1"
  [[ "${output}" =~ (HTTP[[:space:]]+|status[[:space:]]+code:[[:space:]]*)404 ]] ||
    [[ "${output}" =~ release[[:space:]]+not[[:space:]]+found ]]
}

run_github_release_command() {
  local description="$1"
  shift

  local attempt=1
  local max_attempts="${GITHUB_RELEASE_MAX_ATTEMPTS:-4}"
  local retry_delay_seconds="${GITHUB_RELEASE_RETRY_DELAY_SECONDS:-5}"
  local output
  local status

  while :; do
    set +e
    output="$("$@" 2>&1)"
    status=$?
    set -e

    if [[ "${status}" -eq 0 ]]; then
      [[ -z "${output}" ]] || printf '%s\n' "${output}"
      return 0
    fi

    if ! is_retryable_github_failure "${output}" || [[ "${attempt}" -ge "${max_attempts}" ]]; then
      printf '%s\n' "${output}" >&2
      return 2
    fi

    printf 'GitHub release %s failed transiently (attempt %s/%s); retrying in %ss.\n' \
      "${description}" "${attempt}" "${max_attempts}" "${retry_delay_seconds}" >&2
    sleep "${retry_delay_seconds}"
    attempt=$((attempt + 1))
  done
}

release_exists() {
  local release_tag="$1"
  local attempt=1
  local max_attempts="${GITHUB_RELEASE_MAX_ATTEMPTS:-4}"
  local retry_delay_seconds="${GITHUB_RELEASE_RETRY_DELAY_SECONDS:-5}"
  local output
  local status

  while :; do
    set +e
    output="$(gh release view "${release_tag}" 2>&1)"
    status=$?
    set -e

    if [[ "${status}" -eq 0 ]]; then
      return 0
    fi

    if is_missing_release "${output}"; then
      return 1
    fi

    if ! is_retryable_github_failure "${output}" || [[ "${attempt}" -ge "${max_attempts}" ]]; then
      printf '%s\n' "${output}" >&2
      return 2
    fi

    printf 'GitHub release lookup failed transiently (attempt %s/%s); retrying in %ss.\n' \
      "${attempt}" "${max_attempts}" "${retry_delay_seconds}" >&2
    sleep "${retry_delay_seconds}"
    attempt=$((attempt + 1))
  done
}

copy_release_files() {
  local dir
  IFS=':' read -r -a dirs <<< "${RELEASE_INPUT_DIRS:-dist:build:bin:release}"
  for dir in "${dirs[@]}"; do
    [[ -d "${dir}" ]] || continue
    while IFS= read -r -d '' file; do
      cp "${file}" "${assets_dir}/$(basename "${file}")"
    done < <(find "${dir}" -type f \( -name '*.bin' -o -name '*.zip' -o -name '*.tgz' -o -name '*.tar.gz' -o -name '*.tar.zst' \) -print0)
  done
}

main() {
  local release_tag="${RELEASE_TAG:-${GITHUB_REF_NAME:-snapshot}}"
  local assets_dir="${RUNNER_TEMP}/release-assets"
  local files_json

  mkdir -p "${assets_dir}"

  if [[ -n "${RELEASE_BUILD_COMMAND:-}" ]]; then
    bash -c "${RELEASE_BUILD_COMMAND}"
  fi

  copy_release_files

  if [[ -n "${CHART_DIR:-}" ]]; then
    helm package "${CHART_DIR}" --destination "${assets_dir}" >/dev/null
  fi

  files_json="$(find "${assets_dir}" -maxdepth 1 -type f -print | sed 's#^.*/##' | jq -Rsc 'split("\\n") | map(select(length > 0))')"
  jq -n \
    --arg repository "${GITHUB_REPOSITORY}" \
    --arg commit "${GITHUB_SHA}" \
    --arg tag "${release_tag}" \
    --arg image_refs "${IMAGE_REFS:-}" \
    --arg chart_refs "${CHART_REFS:-}" \
    --argjson files "${files_json}" \
    '{schema: 1, repository: $repository, commit: $commit, tag: $tag, image_refs: ($image_refs | split("\\n") | map(select(length > 0))), chart_refs: ($chart_refs | split("\\n") | map(select(length > 0))), files: $files}' \
    > "${assets_dir}/release-manifest.json"

  printf '%s\n' "${assets_dir}"

  if [[ "${GITHUB_REF_TYPE:-}" == "tag" ]]; then
    if release_exists "${release_tag}"; then
      :
    else
      local release_lookup_status=$?
      if [[ "${release_lookup_status}" -ne 1 ]]; then
        return "${release_lookup_status}"
      fi
      run_github_release_command "creation" \
        gh release create "${release_tag}" --title "${GITHUB_REPOSITORY} ${release_tag}" --notes "Automated CI release for ${GITHUB_SHA}."
    fi
    run_github_release_command "asset upload" \
      gh release upload "${release_tag}" "${assets_dir}"/* --clobber
  fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi

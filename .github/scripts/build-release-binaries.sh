#!/usr/bin/env bash
set -euo pipefail

: "${RUNNER_TEMP:?RUNNER_TEMP is required}"
: "${GITHUB_SHA:?GITHUB_SHA is required}"
[[ "$GITHUB_SHA" =~ ^[0-9a-f]{40}$ ]] || { echo 'full source SHA required' >&2; exit 2; }
[[ "$(git rev-parse HEAD)" == "$GITHUB_SHA" ]] || { echo 'checkout does not match release source SHA' >&2; exit 2; }
git diff --quiet HEAD -- || { echo 'release source checkout must be clean' >&2; exit 2; }
[[ -z "$(git ls-files --others --exclude-standard)" ]] || { echo 'untracked source files are not release inputs' >&2; exit 2; }
assets_dir="${RUNNER_TEMP}/release-assets"
mkdir -p "$assets_dir"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o "$assets_dir/accounts" ./cmd/accountsvc
zip -j "$assets_dir/accounts-${RELEASE_TAG:-${GITHUB_REF_NAME:?release tag required}}.zip" "$assets_dir/accounts"
for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  target_os="${platform%/*}"
  target_arch="${platform#*/}"
  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build -trimpath -ldflags='-s -w' \
    -o "$assets_dir/migratectl-${target_os}-${target_arch}" ./cmd/migratectl
done

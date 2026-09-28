#!/usr/bin/env bash
# P0-Q regression gate. Uses in-process stores only and never mutates UAT.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$script_dir/_common.sh"

go test -count=1 ./api ./internal/store \
  -run 'Test(AdminPlanGroupPreviewApplyUsesOneTimeTokenAndSynchronizesQuota|AdminPlanGroupBatchPreservesUsageAndQuotaAcrossPlanToggles|AdminPlanGroupBatchStaleMiddleTargetDoesNotPartiallyApply)$'

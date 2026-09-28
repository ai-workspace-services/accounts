#!/usr/bin/env bash
# P0-S regression gate. Uses the test store only and never calls Stripe.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$script_dir/_common.sh"

go test -count=1 ./api -run 'Test(PaidSubscriptionAndManualValidityOutrankExpiredSnapshot|CanceledSubscriptionDowngradesAtPeriodEndAndIdempotently|PaymentFailureGraceAndQuotaPeriodResetAreIdempotent|SubscriptionUpgradeAppliesImmediatelyWithoutResettingUsageTwice|SubscriptionSweepProcessesBoundedPagesUntilAllUsersAreReconciled|AgentClientListUsesPostReconciliationQuotaBlock|ManualValidityExpiresAtNextUTCDateWithoutGrace|RefundReconciliationRequiresExplicitCompletedFullRefund)$'

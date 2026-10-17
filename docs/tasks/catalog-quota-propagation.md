# Catalog quota changes follow existing plan assignments

Saving an active billing plan now commits its catalog row and the quota of
existing profiles assigned through `pricing_rule_version = plan:<PLAN_ID>` in
one transaction. PostgreSQL locks the profile/quota pairs before calculating the
remaining quota. Memory-store behavior follows the same rules.

Usage is the larger of the previously consumed allocation and the current
period's traffic buckets. The resulting remainder is clamped to zero. Traffic
buckets, financial ledgers, balances, billing period dates, explicit suspensions,
group memberships, and subscription records are not rewritten. Subscription
records with individually agreed monthly or annual quotas retain their own
allocation for paid plans. Missing profile/quota pairs are not provisioned by a
catalog edit. Inactive plans do not propagate changes.

For example, changing FREE from 3 GiB to 5 GiB gives an account that has consumed
1 GiB a remaining quota of 4 GiB. Re-saving the same plan does not grant another
allocation. Existing group identifiers stay stable even if their names contain
the previous quota. Agent eligibility is re-evaluated by the existing users/SSE
revision calculation from the committed quota state; explicit billing pauses
remain in effect.

No schema migration is required. Once released to UAT, re-save FREE at its
configured 5 GiB quota with an audit reason, then verify management and the user
quota API after reload. This also reconciles a catalog edit made before this
fix. Promotion and any production reconciliation require their normal release
workflow; this PR does not modify live accounts.

Regression commands:

```sh
go test ./internal/store ./api
go test -race ./internal/store -run '^TestCatalogQuotaPropagation$'
CATALOG_QUOTA_TEST_DSN='<empty disposable PostgreSQL database>' \
  go test ./internal/store -run '^TestCatalogQuotaPropagationPostgres$' -count=1 -v
```

The native regression refuses a database that already contains `public.users`.
It checks propagation, preserved account state, replay, quota changes in both
directions, independent subscriptions, and atomic rollback on an injected write
failure.

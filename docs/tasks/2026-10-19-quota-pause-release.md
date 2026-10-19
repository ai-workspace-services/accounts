# Quota pause / Free group cleanup release gates

Scope: V3 only. V2 is a read-only synchronization source. Keep the UI layout,
public DNS, user identities, subscription attributes, balances and ledgers intact.

## Canonical Free group

- Desired current identifier: `segment:quota:free-5gb`.
- `docs/migrations/proposed_2026101903_canonical_free_quota_group.up.sql` replaces the legacy identifier
  and collapses duplicates while preserving the first occurrence order.
- This SQL changes only `users.groups`. It does not reset quota, change expiry,
  delete users, or modify payments, refunds or usage history.
- Do not rewrite already executed migration SQL, migration checksums or audits.
- The migration is NOT deployment-ready until both Accounts and Billing accept
  its reviewed checksum and application group readers/writers are switched.
- Rollout must use an old/new-compatible application first; migrate only after
  an encrypted backup has passed isolated restore. Remove temporary legacy
  read handling only after proving no current legacy groups remain.

## Outstanding release gates

- Durable pause/recovery event and transactional notification outbox; repeated
  polling must not send duplicate notifications. SMTP unknown outcomes must
  not be automatically retried as if known failures.
- Restore on paid renewal / natural-month grant / quota increase; never clear
  operator pauses, billing suspensions or archive state automatically.
- Resolve existing refund usage gate (less than 5% of included allowance) vs
  published overage-excluded policy before claiming full refund-policy parity.
- Confirm V2-to-V3 synchronization scope. Never full-restore V2 over V3 Stripe
  Test catalog, local financial history or reviewed migration metadata.
- V3 tag/digest publication, Playbook selection, Doco-CD reconciliation and
  authenticated page / notification acceptance remain separate release gates.

No live deployment or data transfer is implied by these local source changes.

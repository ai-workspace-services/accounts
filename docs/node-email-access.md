# Public node email exceptions

For public shared/free/plus/pro nodes, eligibility is the existing tier rule OR a normalized configured email match. Plus/Pro users keep their original access; named Free users gain this node only. User plans, quotas and other nodes are unchanged. Account activity, access verification policy, pauses, blacklists, quota exhaustion and dedicated-team isolation remain mandatory.

`allowedEmails` defaults to an empty list. An omitted list or `[]` adds nobody. It works independently of `customAllowFree`; enabling that flag must never widen the exception to unlisted Free users. See [Free access rules](custom-node-free-access.md) for the public/custom distinction.

The deployment credential must have public-node configuration permission. Requests carry `X-Agent-Email-Policy: public-union-v1` and `X-Agent-Allowed-Emails` as a JSON array. The resolved scope acknowledges the policy and SHA-256 digest of the normalized, sorted, deduplicated email list (joined with newline). Missing/mismatched receipts must be rejected by the updated Agent. Email markers are internal policy bookkeeping, omitted from displayed admin groups.

The same authenticated policy supplies user snapshots, revision/SSE events and regional node discovery used by user nodes/config and subscription exports. Status reports cannot independently grant emails: public-node email metadata comes from the resolved request policy. Removing the list revokes the exception after reconciliation/status refresh. Session termination continues to follow existing data-plane revocation behavior.

Custom nodes retain allowlist restriction and default-off customAllowFree. For public nodes, a listed email can access that node regardless of tier without changing plan or quota. Team nodes reject email extensions and retain strict membership isolation. `customAllowFree` may be set on team nodes but never grants cross-team access. No SQL schema change or user migration is required. Deploy Accounts before Agent; remove public-node exceptions and reconcile before rollback.

Related Agent task: https://github.com/ai-workspace-xstream/xconnect-edge-agent/issues/132

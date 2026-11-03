# Custom Free opt-in

Related task: https://github.com/ai-workspace-xstream/xconnect-edge-agent/issues/127

Agent YAML `agent.customAllowFree` defaults to false and may be combined with `shared`, `free`, `plus`, `pro`, `custom`, or `team`. `allowedEmails` works independently: a matching address receives access to that public node even below its normal tier threshold. An empty or omitted list adds nobody. `customAllowFree` never opens a public node to every Free user; it only has an effect when an email allowlist is configured, and for `custom` it allows listed Free users while the list remains mandatory. Users meeting the normal tier threshold retain access. For `team`, the flag never bypasses enrolled team membership or tenant isolation. Restricted credentials cannot enable the flag; a team credential can only enable it for its own matching team node. Malformed flags fail closed.

The resolved scope includes `customAllowFree`, and status normalizes the capability from the authenticated request. On public nodes, only an email match adds a user below the tier threshold; `customAllowFree` does not widen that audience. The email exception does not change the user's plan. Omitting the header or setting false removes an earlier opt-in on the next status update. Existing scope, account activation, verification policy, proxy pauses, blacklists, quotas and dedicated-team isolation remain enforced. No user plan/state migration or SQL changes are needed.

Deploy Accounts first, then the compatible Agent. Closing the switch and reconciling nodes precedes backend rollback. Default nodes and existing clients retain their prior behavior.

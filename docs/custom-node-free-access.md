# Custom Free opt-in

Related task: https://github.com/ai-workspace-xstream/xconnect-edge-agent/issues/127

Agent YAML `agent.customAllowFree` defaults to false and can be combined with `shared`, `free`, `plus`, `pro`, `custom`, or `team`. For public classes it adds Free users to the node's eligible audience. For `custom`, the nonempty email allowlist remains mandatory and limits the audience. For `team`, the flag never bypasses the enrolled team membership or tenant boundary. Restricted credentials cannot enable the override; a team credential can only enable it for its own matching team node. Malformed flags fail closed.

The resolved scope includes `customAllowFree`, status normalizes the capability from the authenticated request, and regional node visibility uses the resolved scope plus email allowlist. On public nodes, `allowedEmails` is an additional node-only exception that can bypass the tier threshold without changing the user's plan. Omitting the header or false removes an earlier opt-in on the next status update. Existing scope, account activation, verification policy, proxy pauses, blacklists, quotas and dedicated-team isolation remain enforced. No user plan/state migration or SQL changes are needed.

Deploy Accounts first, then the compatible Agent. Closing the switch and reconciling nodes precedes backend rollback. Default nodes and existing clients retain their prior behavior.

# Custom Free opt-in

Related task: https://github.com/ai-workspace-xstream/xconnect-edge-agent/issues/127

Agent YAML `agent.customAllowFree` defaults to false. With a custom node's authenticated public-pool management credential, `X-Agent-Custom-Allow-Free: true` adds Free users to its eligible audience; the nonempty email allowlist continues to narrow that audience. Restricted and team credentials cannot enable the override. Malformed flags fail closed.

The resolved scope includes `customAllowFree`, status normalizes the capability from the authenticated request, and regional node visibility uses the resolved scope plus email allowlist. Omitting the header or false removes an earlier opt-in on the next status update. Existing scope, account activation, verification policy, proxy pauses, blacklists, quotas and dedicated-team isolation remain enforced. No user plan/state migration or SQL changes are needed.

Deploy Accounts first, then the compatible Agent. Closing the switch and reconciling nodes precedes backend rollback. Default nodes and existing clients retain their prior behavior.

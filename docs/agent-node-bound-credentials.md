# Node-bound Agent credentials

Companion task: https://github.com/ai-workspace-xstream/xconnect-edge-agent/issues/130

An optional `nodeId` on an Agent credential binds its token to one logical
Agent ID and one stable node ID. Status remains indexed by Agent ID, so several
credentials may report from the same host/IP. Tokens must be provisioned by the
existing secret configuration mechanism; never commit real token values.

For a bound credential:

- `id` cannot be `*`; nodeId and tokens must be unique in the configured registry.
- Groups declare an explicit resource class or team. Request metadata cannot
  change it. Credential-owned team behavior is preserved. Shared-token managed team
  approval/revocation remains on its existing enrollment path; this change does
  not convert a static team credential into managed enrollment.
- Agent IDs in header, query and status body must all match the credential ID.
  Status xray.nodeId must match its nodeId. Legacy shared aliases cannot take over
  the bound identity.
- A custom credential requires an enrolled `allowedEmails` list. Requests cannot
  expand it. The server uses this list for users, events and status metadata used
  by route/subscription authorization. Other classes forbid this field.
- To enroll the existing customAllowFree opt-in, include `custom:allow-free` with
  `resource:custom`. It permits only enrolled Free users under the existing
  active/paused/access rules; it does not upgrade their subscription. A request
  cannot enable this permission if it is absent from enrolled groups.
- Successful user snapshots add agentId/nodeId receipts. Old clients ignore these
  additive fields. Pending team snapshots retain their existing empty-user format.

Credentials without nodeId retain legacy behavior. Do not infer that legacy
shared tokens are now node-scoped. The binding is read from configured secret
material on startup; no token issuer, production configuration change, SQL
migration or virtual-node runtime is included. Roll back code/config together
before adding bound credentials: older binaries ignore the new fields.

This is the Accounts prerequisite only. Agent multicontext execution, exporter
ownership/epoch, atomic Billing accounting and dual-node UAT remain separate
work. No deployment or migration was performed.

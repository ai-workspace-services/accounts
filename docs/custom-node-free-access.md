# Node Free access and email allowlists

Related task: https://github.com/ai-workspace-xstream/xconnect-edge-agent/issues/141

## Access rules

1. Normal tier eligibility remains in effect. Plus/Pro users retain access according to the node tier; plans, quotas and tenant isolation are unchanged.
2. On public shared/free/plus/pro nodes, `allowedEmails` is an independent node-level exception. Matching addresses may bypass the tier threshold without `customAllowFree`. The default list is empty; an omitted list or `[]` adds nobody.
3. `customAllowFree` defaults to false and never admits all Free users. It requires a nonempty email allowlist to grant additional Free access, and any additional access remains limited to that list. On public nodes, disabling the flag does not revoke the independently effective email exception.
4. A `custom` node requires a nonempty `allowedEmails` list for every user. Listed Free users additionally require `customAllowFree: true`; listed Plus/Pro users retain the existing custom-node access rules.
5. Account activity, access verification policy, traffic pauses, blacklists, expiry, quotas and team isolation remain mandatory. No plan changes, bulk user-state updates or SQL schema changes are involved. Team nodes retain membership-based isolation and reject email extensions.

An empty allowlist does not remove access already granted by the node's normal tier. Free/shared nodes continue to admit users under their existing tier rules.

## Public Plus/Pro nodes

The table describes additional Free access; normal tier-qualified users are unaffected.

| `customAllowFree` | `allowedEmails` | Additional Free users |
| --- | --- | --- |
| omitted or false | omitted or `[]` | none |
| true | `[]` | none |
| omitted or false | `[alice@example.com]` | Alice only |
| true | `[alice@example.com]` | Alice only |

For a `custom` node, the same list admits Free users only when the flag is true. `allowedEmails: [""]` is invalid, not an empty list.

```yaml
agent:
  resourceClass: plus
  teamId: ""
  customAllowFree: false
  allowedEmails: []
```

To add specific users independently of the flag:

```yaml
agent:
  resourceClass: plus
  teamId: ""
  allowedEmails:
    - alice@example.com
    - bob@example.com
```

To admit a listed Free user on a custom node:

```yaml
agent:
  resourceClass: custom
  teamId: ""
  customAllowFree: true
  allowedEmails:
    - alice@example.com
```

## Reconciliation and release

User snapshots, revision/SSE events, regional node discovery and subscription exports must apply the same policy. The authenticated request supplies the scope and normalized public email policy; Agent verifies the returned scope and email digest. Restricted credentials cannot enable the override; team credentials remain bound to their enrolled node and team. Malformed flags fail closed.

Deploy Accounts before a compatible Agent. Confirm the actual client list and node visibility for listed and unlisted users; a healthy process or a client count alone does not prove correct authorization. Documentation records the intended contract; runtime conformance requires separate verification of the deployed image.

Revoking public-node email exceptions requires removing the emails and reconciling; setting the flag to false alone does not revoke an independent allowlist. For custom nodes, closing the flag revokes listed Free access after reconciliation. Existing data-plane session-revocation behavior still applies.

# Agent managed configuration

Accounts exposes a read-only `GET /api/agent-server/v1/managed-config` endpoint. Set
`AGENT_MANAGED_CONFIG_PATH` to a JSON file mounted from a reviewed GitOps revision
to enable it. Leave the variable unset to return 404. Publish the complete file
atomically so each request reads one revision. A missing, unreadable, oversized
or invalid file returns 503.

The input contract for version 1 is:

```json
{
  "schemaVersion": 1,
  "revision": "0123456789012345678901234567890123456789",
  "templates": {
    "free-shared": {"statusInterval": "60s"},
    "plus": {},
    "pro": {},
    "custom": {}
  },
  "nodeOverrides": {
    "example-node": {"statsEnabled": false}
  }
}
```

Use the full Git commit SHA of the reviewed source as the revision. All four
templates are required. Free and Shared use the same template. Each object
supports optional `region`, `pool`, `statusInterval` (a positive Go duration),
and `statsEnabled`. An override replaces only the fields it specifies.

Send the existing Agent credential in the Authorization header. Accounts derives
the node ID and resource class from the credential. Request headers and query
parameters cannot select another node or template. Legacy and Team credentials
are outside this endpoint's supported scope.

The response contains `schemaVersion`, `revision`, `nodeId`, `resourceClass`,
`template`, and `agent`. The `agent` object contains the effective overlay
fields. Treat the other fields as response metadata. Keep the manifest free of
secrets. Configure Agent fetch and application separately.

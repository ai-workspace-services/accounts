# Migration authentication with the internal service token

Each migration listener delegates authorization to its own Accounts instance via
`--accounts-auth-url`. The client supplies each environment's existing
`INTERNAL_SERVICE_TOKEN` as a Bearer token through the source and target token
environment variables. Do not place credential values in Git, command arguments,
logs, or documentation.

`POST /api/admin/migration/authorize` accepts the configured service token for
`source.read`, `target.plan`, and `target.apply`. An unset or incorrect token
is rejected. Accounts administrator JWTs and sessions are not accepted by this
endpoint. The returned operator identifier represents the internal service, so
it does not identify an individual administrator. Everyone holding the shared
token has all three migration scopes; protect and rotate it accordingly.

The listener's existing schema, database identity, conflict, backup and apply
gates remain in effect. Deploy each environment's matching Accounts and listener
configuration before attempting a transfer.

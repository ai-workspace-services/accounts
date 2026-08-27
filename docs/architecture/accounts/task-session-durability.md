# Task-session durability ownership

Status: accepted (2026-08-28)

Accounts is the only durable source of truth for cross-device XWorkmate
namespaces, sessions, ordered events, lightweight message context, task-run
state, scheduler leases, and opaque Bridge task references.

The canonical PostgreSQL tables are `public.task_namespaces`,
`public.task_sessions`, `public.task_session_events`, and `public.task_runs`.
Messages are facts in the ordered event stream. `context_summary` contains only
a bounded recent-message projection for fast attach; clients recover complete
history from `task_session_events` by sequence number.

Bridge remains an execution router and artifact owner. It resolves a stable
Accounts principal from credential introspection, dispatches ACP work, and
reports lightweight run state back to Accounts. Bridge must not create a second
session schema or treat its in-memory session map as cross-device truth.

Accounts never stores artifact bytes, file contents, attachments, base64,
working-directory paths, manifests, download URLs, or full tool logs. The only
artifact-domain value allowed in this control plane is
`task_runs.bridge_task_ref`, an opaque lookup reference.

All public session reads and writes derive `accountId` from the authenticated
principal. Namespace and session identifiers supplied by clients are selectors,
not authority; resources owned by another account return the same `404`
envelope as missing resources.

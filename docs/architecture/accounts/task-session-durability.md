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

## Amendment: task scheduler for the orchestrator worker (2026-10-13)

AI Workspace v2.1 moves role selection and dispatch from Bridge into the DSH
orchestrator (`aiw-orchestrator`). Accounts stays the only durable task state;
the orchestrator keeps none.

- A message may carry `routing` hints (`role`, `roleMode`, `roleModel`,
  `productMode`, `specialty`, `dataClass`, `projectId`, `workspace`,
  `attachments`, `followUp`, `previousTurnFailed`). They are validated, stored
  on the existing `task_runs.routing` column and handed to the worker that
  claims the run. Unknown keys and wrong types are rejected. `workspace` and
  `projectId` are opaque keys, never paths: the worker host maps `workspace` to
  a directory it has authorized, so this rule against working-directory paths
  still holds.
- The orchestrator uses `/api/internal/task-scheduler` (`claim`,
  `runs/{id}/renew`, `runs/{id}/events`) with its own bearer credential
  (`TASK_SCHEDULER_TOKEN`), accepted only for the accounts in
  `TASK_SCHEDULER_ACCOUNT_IDS`. The general internal service token does not open
  these routes. Every renew and event is checked against the run's fence, lease
  holder and lease token hash; a fenced-out worker cannot write.
- Workers append only `run.progressed`, `assistant.message.created`,
  `run.completed`, `run.failed` and `run.cancelled`.
- `POST /api/v1/sessions/{sessionId}/runs/{runId}/cancel` cancels a queued run
  outright (`run.cancelled`) and records `run.cancel_requested` for a running
  one; the lease holder sees it on its next renewal and settles the run.
- No schema change: routing uses the existing column, and a cancel request is an
  event in the ordered stream.

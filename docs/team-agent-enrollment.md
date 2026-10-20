# Managed Team node enrollment

A shared deployment credential may register a node with `X-Agent-Resource-Class: team` and an optional suggested `X-Agent-Team-ID`. The node remains pending and receives an empty user snapshot. Only a root administrator can assign an existing team and approve it through `GET/PUT /api/auth/admin/agents/team-enrollments` (status read and settings write permissions respectively).

PUT fields: `agentId`, `status` (`approved`, `rejected`, `revoked`), `teamId` (approval only), required `reason`, and the exact `updatedAt` returned by GET. Conflicting decisions return 409. Node suggestions never replace administrator assignments. Legacy dedicated credentials remain bound to their original scope and cannot be taken over by the deployment token.

Responses from managed `/api/agent-server/v1/users` contain `enrollmentStatus` and an authoritative `scope`. Pending/rejected/revoked responses contain zero clients and a distinct revision, which must be applied to remove old users. Approval is not public discovery readiness: the node must report application of its current approval revision. Reassignment requires explicit revocation followed by a healthy zero-user acknowledgement of that exact revocation revision before the next approval. Persisted assignment decisions overlay all discovery and full-configuration node selection.

Apply migration `2026102002` before upgrading Team Agents, Accounts and Console. This changes no VLESS transport paths. Database changes and production deployment are separate release gates; this implementation has only been locally tested.

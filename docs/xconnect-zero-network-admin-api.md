# XConnect Zero network administration API

## Delete an owned network

`DELETE /api/overlay/v1/admin/networks/{network_id}` deletes a network owned by
the authenticated account. The caller must have `xconnect.zero.manage`; the
handler derives the owner from the authenticated session and never accepts an
owner ID from the request.

The request must repeat the exact network ID as a destructive-action guard:

```http
DELETE /api/overlay/v1/admin/networks/net_shared_vault
Authorization: Bearer <account-session-token>
Content-Type: application/json

{"confirm_network_id":"net_shared_vault"}
```

Success returns `204 No Content`. A missing network or a network belonging to
another account returns `404`; an incorrect/missing confirmation returns
`400`; ambiguous device IDs that could make cleanup affect credentials from a
different network return `409` without deleting anything.

The database transaction removes the network together with its invites,
registrations, devices, device credentials, enrollment sessions, signed-config
acknowledgements, and legacy network/node records when those tables exist. This
is permanent and invalidates stored control-plane credentials. It does not
remotely erase configuration already installed on a Gateway or One; stop or
revoke those clients before deletion, and verify migration backups first.
Create a replacement network with the same ID only after its owner has
confirmed the deletion and its member clients are ready to re-enroll.

# Email consent and self-service links

Root-only management: `GET/PUT /api/auth/admin/email-management` stores the real sender company, postal address, contact mailbox and HTTPS console origin. `POST /api/auth/admin/email-management/preview` renders a footer and links for the current Root account only. It never sends mail or changes another recipient's consent.

User preferences: `GET/PUT /api/auth/email-preferences` accepts an account session or a `key` capability scoped to preferences. Categories default to off; a save records the explicit choices, policy version, source and timestamp. Updates require the current version. No administrator API can opt another user in.

Unsubscribe: `POST /api/auth/email-unsubscribe?key=…` accepts an unsubscribe-only capability and disables all marketing categories. This endpoint supports RFC 8058 form POSTs without a login or redirect. GET is not registered, so link scanners cannot unsubscribe a user by fetching the URL. Portal's `/email/unsubscribe` page requires a single confirmation before the POST. Unsubscribe increments the preference version, preventing an already-open form from accidentally restoring revoked consent.

Capabilities contain 256 random bits; only SHA-256 hashes are persisted. Preference links expire after 30 days, unsubscribe links after 180 days. Both are bound to the current account and original mailbox; changing the account mailbox invalidates old links. Do not log or retain the `key` query parameter. Configure ingress/access-log redaction before sending real campaigns. Retention cleanup should delete expired token records; consent evidence must follow the operator's documented retention policy.

All relevant responses use `Cache-Control: no-store`; Portal capability pages disable third-party analytics and use `Referrer-Policy: no-referrer`. Root previews contain sensitive bearer links and should not be forwarded or included in screenshots/support tickets.

`EmailMessage.Marketing` marks promotional messages with one account ID, category and digest/important classification. Producers must call `deliverEmail`, which reloads consent and frequency choices, resolves the recipient from the account store (ignoring caller-supplied To), appends the footer to both MIME alternatives, and supplies the List-Unsubscribe and List-Unsubscribe-Post headers. It checks the preference version again immediately before the SMTP call. Changes already accepted by SMTP cannot be recalled. The SMTP provider must DKIM-sign the unsubscribe headers where required. Existing transactional messages remain independent of these choices.

No campaign composer, bulk-send endpoint, renewal/expiry scheduler, send-frequency ledger, quota policy change or live migration is included. A future campaign queue must add its own frequency ledger, event deduplication, retries, bounce/complaint suppression and final eligibility check. This foundation does not by itself certify jurisdiction-specific marketing compliance.

## Schema and local verification

The additive `2026101702_email_preferences` migration creates four independent tables. The PostgreSQL 17 native artifact and full-business catalog include them; earlier sources may omit them, in which case recipients remain opted out. No subscription, quota or payment facts are changed.

Local checks:

- `go test ./...`
- `EMAIL_PREFERENCES_TEST_DSN=<isolated local email_preferences_test database> go test ./internal/service -run TestEmailPreferencesPostgresMigration -count=1`
- `MIGRATECTL_NATIVE_TEST_DSN=<isolated local PostgreSQL17 account database> go test ./sql ./internal/migrate -run 'TestNative|TestFullBusinessContractNativeCoverage' -count=1`

These tests and the Portal browser fixture do not demonstrate deployment, real-mail delivery, automatic scheduling or production acceptance.

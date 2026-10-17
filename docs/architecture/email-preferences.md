# Email campaigns, consent and welcome journeys

Root manages `/panel/management` → **邮件与通知 / Email & notifications**. Ordinary administrators and operators cannot read or change its backend endpoints. Sender identity requires the real legal company name, postal address, contact mailbox and HTTPS console origin. Existing transactional mail remains separate.

## Announcement campaigns

`/api/auth/admin/email-campaigns` supports list/create, draft-only updates, content/audience preview, explicit launch confirmation, cancellation and delivery results. Supported kinds are new feature, app release and announcement. All three are marketing under the `productUpdates` category; no broadcast can claim to be a mandatory security or account message. Drafts accept plain text and optional HTTPS CTA links; outgoing HTML escapes operator and recipient text.

A campaign targets all, Free, individual subscribers or Team accounts. Active verified normal accounts are eligible; Root, administrator, operator and demo accounts are excluded. Team membership and subscription validity are read from persisted account facts, never inferred from temporary unlimited quota overrides. Preview excludes missing consent, the wrong frequency group, blacklist/suppression entries and frequency caps. Launch must match both the reviewed campaign version and the recipient-identity digest, and snapshots at most 10,000 recipients. Saving drafts and previewing never sends an email.

Each recipient receives a separate durable delivery, not a shared To/CC/BCC message. The SMTP-boundary check verifies the current preference version. Category, frequency, current account/mailbox, segment, suppression and cancellation are checked again before sending. Marketing has an `AD:` subject prefix, a personalized preference/unsubscribe footer and RFC 8058 List-Unsubscribe headers. `Message-ID` contains the durable delivery ID and uses the configured SMTP From domain; it helps correlate an uncertain result with provider records, but does not guarantee provider deduplication.

Marketing limits are global per recipient across campaign and tutorial deliveries: one per 7 days, three per 30 days, and at most one monthly-digest message per UTC calendar month. A campaign frequency group must match the recipient's selected frequency. Capped campaign messages are recorded as skipped; they are not silently rolled into a later, potentially stale announcement.

## Welcome and guided onboarding

Root configures three steps via `/api/auth/admin/email-onboarding`: `hello` (immediate), `first_steps` (default +24h) and `discover_more` (default +72h). No step is enabled by default. Enabling requires an SMTP sender and valid sender identity. Content and CTA are editable; `{{name}}` is supported.

Password registration and creation of a new verified OAuth account enroll the journey without waiting for SMTP. First Hello is a one-time account-service guide with a preference-management link. Root must not put promotions or upgrade sales in that service template. Follow-ups are marketing under `tutorials`, requiring explicit consent. They remain ordered and honor the recipient's frequency; a cap defers the next tutorial, rather than discarding it or changing the preference. Each step/account identity has a unique constraint, independent of the opaque dedupe-key string, so copied/remapped accounts cannot be accidentally enrolled twice.

Each step's activation timestamp excludes pre-existing accounts. A bounded account reconciliation pass recovers enrollment after a failed post-registration queue write. Disabling or reactivating a step stops pending deliveries from its prior activation. Content changes apply to future enrollments; already queued messages keep their reviewed snapshot. Bulk-imported historical users are not backfilled.

## Dispatch, retries and operational results

The application starts a bounded 10-second worker only where the existing `DatabaseRuntime.MayRunBackgroundWriters()` policy permits it. Replicas coordinate with a durable per-recipient lease and compare-and-swap delivery state. The worker handles at most 10 deliveries per iteration. Root can process a bounded batch through `/api/auth/admin/email-dispatch`, including runtimes without a continuously running background worker. Serverless scheduling must be explicitly operated; this change does not provision a Cloud Scheduler or other external automation.

Known temporary SMTP rejection (4xx) and safe pre-SMTP failures retry with backoff, up to three attempts. A connection error or process crash after the SMTP boundary becomes `uncertain`; it is never automatically replayed. Root must inspect provider records and explicitly acknowledge possible duplicate delivery before requeueing it. This avoids claiming exactly-once delivery over SMTP. `sent` means SMTP accepted the message, not that it reached an inbox.

Root can cancel unsent campaign deliveries and view outcomes. In-flight SMTP messages cannot be recalled. `/api/auth/admin/email-suppressions` records provider-confirmed bounces or complaints; these block both marketing and welcome delivery. The initial integration is manual registration of provider events, not an unauthenticated public webhook. A suppression cannot be removed through a subscriber preference link.

## Recipient controls and privacy

`GET/PUT /api/auth/email-preferences` accepts an account session or a preferences-only capability. Every category defaults to off. Explicit saves record choices, policy version, source and timestamp. Version checks prevent stale forms from restoring consent after unsubscribe. There is no Root endpoint to subscribe another user.

`POST /api/auth/email-unsubscribe?key=…` accepts an unsubscribe-only capability. The Portal page requires one confirmation; GET cannot change state. RFC 8058 form POST needs neither login nor redirect. Preference capabilities expire after 30 days, unsubscribe capabilities after 180 days; only SHA-256 hashes are persisted, and links are bound to the original mailbox. Account email changes invalidate old links.

Responses are not cached. Capability pages disable third-party analytics and use no-referrer/noindex metadata. Configure ingress/request-log redaction for `key` before real campaigns. Do not save preview bearer links in screenshots, support tickets or release notes. Expired tokens and consent evidence require the operator's documented retention policy. SMTP must DKIM-sign the unsubscribe headers where required. This foundation does not itself certify jurisdiction-specific marketing compliance.

## Schema and acceptance

Additive migrations `2026101702_email_preferences` and `2026101703_email_campaigns`, the PostgreSQL 17 native artifact and full-business catalog contain preferences, campaigns, delivery/lease records, onboarding steps and suppressions. Legacy sources may omit these tables. No subscription, quota or payment facts are changed.

Local validation uses `go test ./...`, API/fake-SMTP integration tests, isolated PostgreSQL migration/concurrent-claim tests, native initialization and full-business PostgreSQL transfer tests. Portal uses lint/typecheck/build, UI confirmation tests and a fictional local account. No real campaign, live migration, production deployment or automatic opt-in is performed by implementation or tests.

Root can resolve an uncertain delivery as SMTP accepted only after provider verification; this records acceptance without sending again. A suppressed or cancelled onboarding predecessor stops later steps.

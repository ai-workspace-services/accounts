# Subscription file imports

CSV and TSV use the same email-keyed parser and transactional preview/apply operation. A preview never grants an entitlement. Only root, administrator and operator accounts may use the import API, subject to existing administrative access controls. Unknown emails are reported with original spreadsheet row numbers; accounts are never created and accounts outside the source are not changed.

## Columns and compatibility

- `邮箱` / `email`: trimmed, case-insensitive account key. Username is reference data only.
- `套餐` / `plan`: `plus`, `pro`, `custom`, `free` (legacy FREE/PLUS/UNLIMITED-BETA are retained). The legacy unnamed column immediately after `订阅结束` is recognized as `套餐`.
- `订阅类型` / `订阅周期` / `subscription_type`: `month` or `year` (legacy `payg` accepted). Contract validity and payment cycle are independent; a year-long contract does not automatically change `month` to `year`.
- Validity accepts ISO dates and the existing supported date formats. End dates remain inclusive in UTC. Import records supply validity on native databases without legacy user validity columns.
- Monthly/annual quotas accept bytes or GB/GiB/MB/MiB. Existing binary unit conversion is retained. PRO uses 50 GiB per month; an explicit conflicting PRO allowance is rejected.
- Fees accept yuan amounts or existing `fee_minor` export values. Importing a paid status records a commercial fact; it does not create a Stripe payment or credit a wallet.
- Empty commercial fields retain existing record values. A zero value explicitly sets numeric fields to zero. Invalid dates, duplicate emails/headers, unknown plan/payment values and oversized files fail validation.

PRO preserves its name and profile identity while using the existing paid quota classification; effective quota is taken from the account billing profile, not the legacy classification name. CUSTOM keeps the existing package, node-access groups, team restrictions and rating multipliers. Actual team memberships still require team management. Dedicated-only personal accounts may import CUSTOM records. Inactive/unpaid/future CUSTOM records do not grant a new allowance.

Source plan provenance is stored as a non-entitlement `subscription:source-plan:*` label in the existing user groups JSON. Subscription cycle remains a separate field. Exports preserve source plan identity even when the effective plan has expired. Export with `includeHistory=true` includes current historical commercial rows; immutable previous versions remain in audit history. No schema changes are needed.

Applied imports preserve consumed usage, including an active monthly usage-reset boundary. Identical reads return `noChanges` and cannot be applied. Existing preview expiry, idempotency and concurrent entitlement/usage change checks remain active. Audit entries retain prior and new commercial records.


There is no Google integration or credential requirement. Export the spreadsheet as CSV/TSV and use the existing file-import action.

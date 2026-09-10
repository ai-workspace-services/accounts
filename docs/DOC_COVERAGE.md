# Documentation Coverage Matrix

This matrix tracks the bilingual canonical documentation set for `accounts.svc.plus` and maps it back to the current codebase and older docs.

该矩阵用于跟踪 `accounts.svc.plus` 的双语规范文档，并将其与当前代码状态和历史文档对应起来。

| Category | EN | ZH | Current status | Existing references | Next check |
| --- | --- | --- | --- | --- | --- |
| Architecture | Yes | Yes | Seeded from current codebase and existing docs. | `api/overview.md`<br>`architecture/components.md`<br>`architecture/design-decisions.md`<br>`architecture/overview.md`<br>`architecture/roadmap.md`<br>`architecture/transactional-email.md`<br>`development/code-structure.md` | Keep diagrams and ownership notes synchronized with actual directories, services, and integration dependencies. |
| Design | Yes | Yes | Seeded from current codebase and existing docs. | `architecture/design-decisions.md` | Promote one-off implementation notes into reusable design records when behavior, APIs, or deployment contracts change. |
| Deployment | Yes | Yes | Seeded from current codebase and existing docs. | `Runbook/Feature-Sandbox-Mode-and-Sync-Fix.md`<br>`Runbook/Fix-Agent-404-And-UUID-Change.md`<br>`Runbook/Fix-CloudRun-Stunnel-Startup-Failure.md`<br>`Runbook/Fix-Rotating-UUID-Sync-Archive-2026-02-06.md`<br>`Runbook/README.md`<br>`Runbook/Security-Scrubbing-Archive-2026-02-06.md`<br>`SMTP_GMAIL_SETUP.md`<br>`development/dev-setup.md` | Verify deployment steps against current scripts, manifests, CI/CD flow, and environment contracts before each release. |
| User Guide | Yes | Yes | Seeded from current codebase and existing docs. | `api/overview.md`<br>`architecture/overview.md`<br>`getting-started/concepts.md`<br>`getting-started/installation.md`<br>`getting-started/introduction.md`<br>`getting-started/quickstart.md`<br>`usage/cli.md`<br>`usage/config.md` | Prefer workflow-oriented examples and keep screenshots or terminal snippets aligned with the latest UI or CLI behavior. |
| Developer Guide | Yes | Yes | Seeded from current codebase and existing docs. | `api/auth.md`<br>`api/endpoints.md`<br>`api/errors.md`<br>`api/overview.md`<br>`development/code-structure.md`<br>`development/contributing.md`<br>`development/dev-setup.md`<br>`development/testing.md` | Keep setup and test commands tied to actual package scripts, Make targets, or language toolchains in this repository. |
| Vibe Coding Reference | Yes | Yes | Seeded from current codebase and existing docs. | `Runbook/Fix-Agent-404-And-UUID-Change.md`<br>`api/auth.md`<br>`api/endpoints.md`<br>`api/errors.md`<br>`api/overview.md` | Review prompt templates and repo rules whenever the project adds new subsystems, protected areas, or mandatory verification steps. |

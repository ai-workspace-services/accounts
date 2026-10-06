# Repository Agent Guide

Default local skill references for this repository:

- Release traceability: [skills/release-traceability/SKILL.md](skills/release-traceability/SKILL.md)

## Default Rule

For any change related to CI/CD, image tags, deployment contracts, release validation, or runtime version reporting, follow the release traceability skill above as the default repository policy.

## What This Means

- Treat `service_image_ref` as the single source of truth for release identity.
- Keep commit traceability tied to full `GITHUB_SHA`.
- Ensure deploy uses prebuilt images rather than building on the target host.
- Ensure `/api/ping` and validate both report and verify the same runtime image-derived version data.

# Changelog

Hive did not historically maintain a complete changelog. This file starts a pragmatic, forward-looking record for operators and contributors. We are not reconstructing every old change retroactively; instead, maintainers should add notable user-facing changes as PRs land.

## How we maintain this file

- Add entries under `Unreleased` for user-visible features, fixes, security changes, migrations, deprecations, and breaking changes.
- Move entries into a dated release section when a release is cut.
- Link issues or PRs when useful, but keep entries readable for operators who are not following every PR.
- Do not include routine refactors, test-only changes, or dependency churn unless they affect users.

## Unreleased

### Added

- Contributor onboarding, governance, templates, and local development documentation for the `v2` Go codebase.

## Recent v2 notable changes

### Added

- Timezone-aware time-of-day governor cadences so hives can vary agent activity by local operating windows.
- Merger contributor tier and configurable automerge queue/label behavior for safer merge delegation.
- ClankeR contributor role claiming and owner-assigned agent-role grants.
- Multi-hub relay subscriptions so one contributor session can subscribe to more than one hub.
- Custom dashboard CSS support with sanitizer hardening and operator-facing feedback about dropped rules.
- Replicated agent instances for scaling selected agent roles.

### Changed

- Dashboard and contributor operations views received queue, pause/resume, management, and live-activity improvements.
- Contributor Kubernetes workload generation now supports headless deployments for relay contributors.

### Fixed

- Automerge sweep behavior no longer treats pending CI as green.
- Multi-hub relay control frames are isolated per subscription.
- Custom CSS popovers and dashboard fallback UI received usability fixes.

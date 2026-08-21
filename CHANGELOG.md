# Changelog

Hive did not historically maintain a complete changelog. This file starts a pragmatic, forward-looking record for operators and contributors. We are not reconstructing every old change retroactively; instead, maintainers should add notable user-facing changes as PRs land.

## How we maintain this file

- Add entries under `Unreleased` for user-visible features, fixes, security changes, migrations, deprecations, and breaking changes.
- Move entries into a dated release section when a release is cut. If the repository has no release tag for the change, use the merge date rather than inventing a version.
- Link issues or PRs when useful, but keep entries readable for operators who are not following every PR.
- Do not include routine refactors, test-only changes, or dependency churn unless they affect users.

## Unreleased

### Added

- Hive quadrant: every hive is scored on four axes — trust, efficiency, satisfaction and productivity — shown as a small shape in the hub's hive table, as a labelled chart with scores when you hover it, and as a fleet aggregate above the table that follows whatever filter is applied. Scores are percentiles against the hives in the current view rather than absolute grades, so the picture recalibrates as the fleet moves, and any axis can be sorted on to pull a worklist ("weakest efficiency" first). An axis with too little evidence renders as a collapsed spoke and reports why, rather than scoring zero — a new hive is never marked down for data it has not had time to produce. Satisfaction currently reports no signal by design: nothing in the platform measures it yet, and it is left visibly empty rather than filled with a proxy ([#4384](https://github.com/kubestellar/hive/pull/4384)). Seven already-collected spoke metrics now ride the heartbeat to feed it, adding no GitHub API calls.
- User location: hub users can carry a country, shown as a flag beside their avatar. It is set from a dropdown when a hive is requested, changed later by the signed-in user, or assigned by an admin from the Users table for accounts that predate the feature; where nothing is stated it is inferred from the browser's language header, and an unknown country renders nothing rather than a guess. A deliberate choice always outranks an admin's, which outranks an inference, so nobody's stated country is quietly overwritten. The admin Users table gains a country column and a fleet breakdown that reports how many users are still unknown ([#4371](https://github.com/kubestellar/hive/pull/4371), [#4374](https://github.com/kubestellar/hive/pull/4374), [#4373](https://github.com/kubestellar/hive/pull/4373), [#4386](https://github.com/kubestellar/hive/pull/4386)).

### Security

- Token mint: the `/mint` HTTP endpoint gains a caller-identity seam, per-caller entitlements, and per-caller audit records ([#4436](https://github.com/kubestellar/hive/pull/4436), [#3915](https://github.com/kubestellar/hive/issues/3915)). Default behaviour is unchanged (shared-secret gate, no entitlement bound — a warning is now logged for this posture); once `Entitlements` are configured the mint is deny-by-default per verified identity, refusing any subject, audience or scope outside the caller's grant, and every mint and refusal is logged with the caller's identity.

### Fixed

- Governor threshold scaling now reaches hives that have applied an ACMM level. Applying a pack seeds explicit `surge`/`busy`/`quiet` thresholds, and because "an explicit threshold is never scaled" could not tell a pack-seeded default from a number an operator typed, scaling by repo count silently did nothing on what is the normal path — including, plausibly, the large multi-repo fleets it was built for. Config now records where the thresholds came from: pack-seeded values are treated as per-repo *bases* and scale, while anything you set yourself is still used verbatim. Existing hives are unchanged on upgrade; re-apply the level to turn scaling on, and editing any threshold hands the whole set back to you ([#4037](https://github.com/kubestellar/hive/issues/4037)).
- The agent Terminal now says when it is showing scrollback rather than live output. Scrolling with the mouse wheel puts the pane into tmux copy-mode, which stops it following new output — and because that state lives on the server, closing and reopening the terminal returned to the same frozen view, so an idle-looking agent was indistinguishable from a stuck one. The status bar now reads `[live]` or `[SCROLLBACK - not following live output - press q to resume]`, and the clock beside it is labelled `now` so it is not mistaken for a timestamp of the content on screen ([#4399](https://github.com/kubestellar/hive/issues/4399)).
- `DASHBOARD_AUTH_TOKEN` was documented as holding a Kubernetes secret *name*; it holds the token *value* itself. An operator following the old wording would have set their dashboard token to the literal name of the Secret ([#4427](https://github.com/kubestellar/hive/pull/4427)).

## 2026-08-21

Covers user-facing changes merged between 2026-08-11 and 2026-08-21 (the previous dated entry closed at 2026-08-10). This section was reconstructed from merged-PR history after the record fell behind.

### Added

- Convergence engine (default **off**): a new `convergence.mode` rollout knob (`off`/`shadow`/`enforce`) with admission diagnostics, canonical outcome identity and desired-generation status, exact-subject GitHub proof, selective proof invalidation, and a fenced, idempotent mutation journal ([#4356](https://github.com/kubestellar/hive/pull/4356), [#4362](https://github.com/kubestellar/hive/pull/4362), [#4366](https://github.com/kubestellar/hive/pull/4366), [#4372](https://github.com/kubestellar/hive/pull/4372), [#4382](https://github.com/kubestellar/hive/pull/4382), [#4388](https://github.com/kubestellar/hive/pull/4388), [#4389](https://github.com/kubestellar/hive/pull/4389)). Behavior is unchanged unless the mode is turned on.
- Podman deployment path: host preflight checks (engine, root mode, cgroups, SELinux, mounts, secrets, ports, subordinate IDs), Quadlet units for the Hive service, gateway, network boundary, and data volume, a rootful/rootless × enforcing/advisory support matrix, digest-pinned manual update and rollback, and backup/restore plus Docker-to-Podman migration guidance ([#4284](https://github.com/kubestellar/hive/pull/4284), [#4286](https://github.com/kubestellar/hive/pull/4286), [#4307](https://github.com/kubestellar/hive/pull/4307), [#4340](https://github.com/kubestellar/hive/pull/4340), [#4342](https://github.com/kubestellar/hive/pull/4342), [#4355](https://github.com/kubestellar/hive/pull/4355), [#4380](https://github.com/kubestellar/hive/pull/4380), [#4383](https://github.com/kubestellar/hive/pull/4383), [#4385](https://github.com/kubestellar/hive/pull/4385), [#4407](https://github.com/kubestellar/hive/pull/4407), [#4409](https://github.com/kubestellar/hive/pull/4409)).
- `just contribute-move`: a supported path to move a contributor relay to another machine by reissuing its credential, instead of hand-copying `contributor.env` ([#4418](https://github.com/kubestellar/hive/pull/4418)).
- Explain mode: per-agent `explain_mode: brief|full` asks the agent to record its own reasoning in its run log for debugging, with `?explain=only|hide` log filters ([#3897](https://github.com/kubestellar/hive/pull/3897)).
- Budget history: per-window token budget history is recorded so past resets stay visible, and the dashboard draws a budget-window history graph with labeled trend axes ([#4320](https://github.com/kubestellar/hive/pull/4320), [#4331](https://github.com/kubestellar/hive/pull/4331)). Operators can also reset the budget window from the dashboard ([#3614](https://github.com/kubestellar/hive/pull/3614)).
- Release channels: stable/candidate/edge image channels are published from `v4`, with a channel badge and picker in the hub and spoke dashboards ([#3702](https://github.com/kubestellar/hive/pull/3702), [#3703](https://github.com/kubestellar/hive/pull/3703), [#3762](https://github.com/kubestellar/hive/pull/3762)).
- Work sources (Phase 1): read-only adapters for GitHub Projects v2, Jira Cloud, and Linear alongside the default GitHub Issues source, with a Work Source settings tab ([#4189](https://github.com/kubestellar/hive/pull/4189)–[#4196](https://github.com/kubestellar/hive/pull/4196)).
- Hub Manage Access improvements: user search, provider identity icons, GitHub display names/avatars, last-active timestamps, bulk role change/remove, CSV export, time-limited grants, and an append-only permission audit log ([#4138](https://github.com/kubestellar/hive/pull/4138)–[#4163](https://github.com/kubestellar/hive/pull/4163)).
- Multi-provider human login on the hub, including OIDC providers, with enriched user identities ([#3664](https://github.com/kubestellar/hive/pull/3664), [#4115](https://github.com/kubestellar/hive/pull/4115)).
- `project.issue_filter`: label-gate which issues agents may initiate work on ([#4040](https://github.com/kubestellar/hive/pull/4040)).
- Durable per-kick agent run logs with history in the dashboard ([#4308](https://github.com/kubestellar/hive/pull/4308)), and model/reasoning-effort attribution on PRs and Live Activity events ([#4088](https://github.com/kubestellar/hive/pull/4088), [#4091](https://github.com/kubestellar/hive/pull/4091)).
- Weekly (Tuesday 1pm ET) auto-upgrade mode for hosted hives ([#4381](https://github.com/kubestellar/hive/pull/4381)).
- Documentation: a v2 → v4 migration guide ([#4300](https://github.com/kubestellar/hive/pull/4300)) and a beginner "Zero to Automation" getting-started guide ([#4090](https://github.com/kubestellar/hive/pull/4090) and follow-ups).

### Fixed

- Hosted App-key delivery resolves the cluster hub-record-first, matching the alert and registry, so a claimed hive receives its App key on the first heartbeat instead of stalling on `key-missing` ([#4333](https://github.com/kubestellar/hive/pull/4333)).
- Advisory alerts: the stale-advisory alert names repos with Issues disabled ([#4330](https://github.com/kubestellar/hive/pull/4330)), the digest never adopts a foreign-authored comment the App cannot edit ([#4332](https://github.com/kubestellar/hive/pull/4332)), and Hive detects repos the App installation does not cover instead of blaming an undelivered key ([#4364](https://github.com/kubestellar/hive/pull/4364)).
- Dashboard restart-count and budget-window resets no longer flicker back to stale values after a successful reset ([#4315](https://github.com/kubestellar/hive/pull/4315), [#4351](https://github.com/kubestellar/hive/pull/4351)).
- Contributor relay reliability: completion/idle detection across Codex, agy, and Gemini backends, in-flight task resume on reconnect, and pane-stall handling no longer interrupts a live CLI ([#4026](https://github.com/kubestellar/hive/pull/4026), [#4067](https://github.com/kubestellar/hive/pull/4067), [#4086](https://github.com/kubestellar/hive/pull/4086), [#4265](https://github.com/kubestellar/hive/pull/4265), [#4287](https://github.com/kubestellar/hive/pull/4287)).

### Security

- Sandbox egress gate: closed the IPv6 bypass of the IPv4-only forced-proxy gate ([#4327](https://github.com/kubestellar/hive/pull/4327)), without failing closed on pods that have no IPv6 egress ([#4350](https://github.com/kubestellar/hive/pull/4350)).
- A multi-wave security audit (2026-08-10 → 2026-08-14) hardened hub/spoke authentication (per-hive heartbeat bearers, revocable Ed25519 sessions, master-key generations and rotation), restored owner-only dashboard gates, closed SSRF/symlink/redirect holes, and digest-pinned supply-chain images; the accompanying documentation sweep is in [#3830](https://github.com/kubestellar/hive/pull/3830).
- `/metrics` fails closed when `HIVE_METRICS_TOKEN` is unset ([#3804](https://github.com/kubestellar/hive/pull/3804)).

## 2026-08-10

### Added

- Contributor onboarding, governance, templates, and local development documentation for the `v2` Go codebase.
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

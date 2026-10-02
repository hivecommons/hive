# Data collection and telemetry

Hive sends a small number of operational signals outside the running hive only through the paths below. This page documents what is collected, where it goes, who can read it, retention where the code defines it, and how an operator can disable each path. The Linux Foundation telemetry policy says project software should not collect telemetry by default without LF legal review and prior user consent; Hive therefore treats any phone-home style path as a review item before changing defaults.

## Summary

| Path | What leaves the hive/browser | Destination and visibility | Default / consent | Retention | Disable path | Code references |
|---|---|---|---|---|---|---|
| Hub heartbeat | Hive ID; org/repos/primary repo; configured AI author; GitHub API/base host; ACMM level; agent names, state, backend/model counts; governor mode and queue counts; process/version/image/uptime; dashboard URL/snapshot URL/public flag/cluster ID; health summaries; token/budget counters; contributor counts, leaderboard usernames/avatar URLs/task counts/current task; active/engaged dashboard usernames and last-action timestamps; repo activity counts; PR/CVE/fleet counters; advisory/error strings; non-secret GitHub App IDs/slugs/installation IDs/key fingerprints; dashboard-token hash; file-descriptor counts; cluster-health status. No raw GitHub tokens, App private keys, dashboard tokens, session IDs, or heartbeat secrets are sent. | Configured hub at `hub.url` / `HIVE_HUB_URL`; hub UI/API access is hub-admin/operator gated. | Sent every two minutes when the hive is hub-linked. A standalone hive with no hub URL does not heartbeat. Once hub-linked, sensitive identifier classes can be withheld with `hub.heartbeat_omit` (see [Withholding heartbeat identifiers](#withholding-heartbeat-identifiers)); other fields are not configurable. | Hub registry retention is operational state, not currently documented as a fixed expiry. | Remove the hub link (`hub.url` / `HIVE_HUB_URL`) to stop all hub features. `hub.heartbeat_omit` withholds identifier classes per hive; an all-or-nothing heartbeat/task-status disable switch is still pending maintainer direction (tracking issue: [#10202](https://github.com/hivecommons/hive/issues/10202)). | `src/cmd/hive/boot_heartbeat_deps.go`; `src/cmd/hive/main.go` (`bootHeartbeatWith`); `src/pkg/hub/spoke/heartbeat.go` (`HeartbeatPayload`); `src/pkg/hub/hub_keys.go` (`SpokeHeartbeatKey`). |
| Task-status push / contributor relay status | Hive ID, contributor summary, and leaderboard entries: GitHub username, avatar URL, trust tier, tasks completed/failed, active flag, and current task title. | Same configured hub, authenticated with the per-hive heartbeat bearer. | Runs only when the hive is hub-linked. | Operational hub state; no fixed expiry documented. | Disable the hub link. No separate task-status disable switch. | `src/cmd/hive/main.go` (`StartTaskStatusPush` collector); `src/pkg/hub/spoke/heartbeat.go` (`TaskStatusPayload`). |
| Dashboard feedback reports | User-entered title/description/screenshots; optional diagnostics if the user keeps **Include diagnostics** checked: version, commit, channel, ACMM level, hive ID, hosted/hub-linked flags, agent count, agent names/backend/model/state, browser user agent/platform/language, screen/window size, dashboard page, plus recent console errors and failed local `/api` calls. Project org/repo is sent only if the user checks **Also include project org/repos**. Tokens, bearer strings, emails, and secret-looking key/value text are redacted before submission. | For hub-linked hives, the configured hub relays to GitHub issues in `hivecommons/hive` or `hivecommons/docs`; otherwise the user's local GitHub token creates the issue, or Hive returns a prefilled GitHub URL. | User-initiated only. The modal previews exactly what will be sent; diagnostics can be unchecked; project org/repos require a second opt-in checkbox. | GitHub issue retention follows GitHub/project issue retention. The spoke stores up to 50 local feedback issue references in `/data/feedback-submissions.json`. | Do not submit; uncheck diagnostics/project context; remove screenshots. Operators can also avoid hub relay by not hub-linking. | `src/pkg/dashboard/static/index.html` (`FEEDBACK_DIAGNOSTIC_FIELDS`, preview, checkboxes); `src/pkg/dashboard/feedback.go`. |
| NPS prompt | Score 1-4, optional free text, hive ID, and dashboard version. The signed-in username is used only locally for rate limiting and is not sent. | Hub `POST /api/nps/ingest`, or an explicitly configured NPS relay for standalone hives; only hub admins can read stored responses. | Hosted spokes default on; self-hosted/federated/standalone default off. Standalone relay requires explicit `hub.nps_enabled`/`HIVE_NPS_ENABLED` and `hub.nps_relay_url`/`HIVE_NPS_RELAY_URL`. | Hub store keeps at most 1,000 responses per hive and 10,000 total, oldest dropped first. | `hub.nps_enabled: false` or `HIVE_NPS_ENABLED=false`; leave relay URL unset. | `src/docs/nps.md`; `src/docs/env-vars.md`; `src/pkg/dashboard/nps.go`; `src/pkg/hub/nps.go`. |
| Hosted hub web analytics | GA4 page/event data from hub-hosted pages, including custom hub events such as hive creation/deletion. Existing event code includes hive IDs and creation inputs such as org, primary repo, ACMM level, and cluster ID. | Google Analytics via `www.googletagmanager.com`, property `G-4707R797K3`. | Loaded by hosted hub static HTML pages. This is website analytics, not spoke software telemetry, but it still needs privacy review because custom events may include project identifiers. | Google Analytics retention is controlled in the GA property, not in this repository. | Browser tracking protections or removing the hosted hub GA script; there is no repository config flag. | `src/pkg/hub/assets/dashboard.html`; `src/pkg/hub/saas_pages.go`; `src/pkg/hub/static/*.html` pages such as `reading.html` and `cncf-reference-architecture.html`. |
| Crash/error reporting | No Sentry/PostHog/Segment/Plausible crash reporter was found in `src` during the onboarding audit. Errors are logged locally; selected log-safe health/error strings can appear in hub heartbeats as described above. | Local logs and, for selected operational state, the configured hub. | No third-party crash reporting by default. | Local log/PVC retention is deployment-specific. | N/A for third-party crash reporting; disable hub link for heartbeat-carried operational errors. | Audit search: `telemetry|analytics|metrics push|phone.?home|posthog|segment|plausible|gtag|googletagmanager|sentry` over Go/HTML/JS/Markdown. |

## Linux Foundation telemetry-policy status

| Policy requirement | Hive status | Gap / follow-up |
|---|---|---|
| Do not collect telemetry by default without Linux Foundation review and approval. | Standalone hives do not heartbeat or send NPS relay data unless configured. Hub-linked/hosted hives always send operational heartbeats to the configured hub. Hosted hub pages load GA4. | Heartbeats and hosted GA4/custom events should be reviewed with CNCF/LF before this onboarding item is marked complete. |
| Specify exactly what is collected. | NPS is documented in `src/docs/nps.md`; feedback diagnostics disclose fields in the UI. This page now documents heartbeats, task-status, feedback, NPS, analytics, and crash reporting. | Keep this page updated when heartbeat or feedback fields change. |
| Demonstrate anonymization and avoid personal/end-user/confidential data. | No raw secrets are intended in the heartbeat; feedback redacts token/email/secret-looking text; NPS intentionally omits user identity. | Heartbeats can include GitHub usernames, org/repo names, dashboard usernames, current task titles, project identifiers, hive IDs, and GA4 custom event identifiers. These are not fully anonymized and may be business-sensitive. |
| Notify users and require consent before collection starts. | Feedback is explicit and previewed; project org/repo diagnostics require a separate checkbox. NPS has an enable switch and documented defaults. | Hub heartbeats are tied to hub-link configuration and are not separately consented or field-configurable; hosted GA4 notice/consent is not documented in this repo. |
| Document storage and project-community access. | NPS retention and admin-only read path are documented. Heartbeat and task-status visibility is hub-side operational/admin UI. | Fixed retention for heartbeat/task-status registry data and GA4 retention are not documented. |
| Secure the collection mechanism. | Heartbeats and task-status use per-hive derived bearer credentials; heartbeat responses are signed; feedback/NPS size caps and redaction are implemented. | Security posture depends on the configured hub and GA4; continue to review any new phone-home path before enabling it by default. |
| Make approved telemetry data available to project participants. | NPS is admin-only by design; hub operational state is hub-admin/operator visible. | If LF approves any project telemetry program, define what aggregate data is shared with the community and where. |

## Heartbeats are not a crash reporter

The heartbeat is an operational control-plane link between a spoke and its configured hub. It is used for liveness, health, upgrade/config delivery, hosted-spoke lifecycle, and fleet views. It should not be treated as an anonymous metrics program: the payload contains stable project and hive identifiers and may contain GitHub usernames. Operators that do not want this link should run without `hub.url`/`HIVE_HUB_URL`; Hive does not currently provide a per-field heartbeat opt-out (tracking issue: [#10202](https://github.com/hivecommons/hive/issues/10202)).

---

## Withholding heartbeat identifiers

Set `hub.heartbeat_omit` in `hive.yaml` to a list of classes to strip from every hub heartbeat and task-status push. Hive ID, version/commit, cluster ID and auto-upgrade state are never removed, so upgrade and config delivery keep working. Unknown class names fail config validation. The dashboard **Settings → Hub → Telemetry** section shows which classes are currently withheld (read-only).

```yaml
hub:
  heartbeat_omit: [repos, users, task_titles, dashboard_urls]
```

| Class | Withheld fields | Tradeoff when omitted |
|---|---|---|
| `repos` | `repos`, `primary_repo`, `repo_activity`, `repo_target_issue` | Hub cannot list the hive's repositories or per-repo activity, and cannot flag repo-target misconfiguration. |
| `users` | `owner`, `ai_author`, `ai_author_effective`, active/engaged dashboard usernames, per-user last actions, the whole leaderboard (heartbeat and task-status) | Hub loses owner display, the contributor leaderboard and per-user time-in-hive. |
| `task_titles` | Leaderboard `current_task` | Hub leaderboard shows who is active but not what they are working on. |
| `dashboard_urls` | `dashboard_url`, `snapshot_url`, public-URL self-check | Hub cannot link to the hive's dashboard or snapshot, and cannot probe URL reachability. |

## Telemetry agent

The telemetry agent audits and, once opted in, improves the **observability of the managed project** — the repositories the hive watches, not the hive's own internal metrics/tracing (see [Config.OTel/Tracing](operator-reference.md) for that, a deliberately separate concern). It is an L5/L6-only agent: absent from the roster below L5 and paused in every governor mode at L5/L6 until an operator explicitly opts in.

## What the telemetry agent does

On each kick, telemetry follows its ACMM-level policy template (`telemetry-advisory.md`, `telemetry-holdgated.md`, or `telemetry-full.md`) and:

- Inspects tracing, metrics, structured logging, scrape targets, dashboards, monitoring custom resources, collector/exporter configuration, and web analytics in the repositories it is authorized to audit (`$HIVE_REPOS`).
- Detects the project's *existing* observability stack before recommending anything, and prefers OpenTelemetry as a vendor-neutral spine — but only acts on a backend an operator has explicitly configured.
- Flags unbounded metric labels, high-cardinality span attributes, missing scrape targets, inconsistent span names, and dashboards that aren't source-controlled.
- Never reveals credentials, endpoints, API keys, or secret values in its output — it refers only to environment-variable or secret names.

At **advisory** level (ACMM L2–L4, though telemetry does not actually appear in the roster until L5 — see [ACMM level gating](#acmm-level-gating) below), it writes each confirmed finding as an advisory bead owned by `telemetry` and returns an `AgentReport` with `kind: "findings"`. It never creates GitHub issues, branches, commits, or pull requests in this mode.

At **hold-gated** (L5) and **full** (L6) modes, telemetry runs in `ISSUES_AND_PRS` and, in addition to filing findings, can open bounded, hold-gated pull requests: OpenTelemetry SDK wiring and request-path spans, bounded metrics and `/metrics` endpoints, structured logging, dashboard JSON, alert-rule YAML, `ServiceMonitor`/`PodMonitor` resources, collector/exporter configuration, dashboard-lint CI, and GA4 wiring for an identified web property. It re-verifies and closes only stale beads it owns, and files findings with a `[telemetry]` title.

Telemetry must never, at any PR-capable level: commit credentials, literal collector endpoints, API keys, or secret values; add an exporter that sends data off-box without an explicitly configured backend; introduce unbounded labels or span attributes; merge its own PR; or (at L5) remove a `hold`/`on-hold`/`do-not-merge` label. At L6 it still never merges its own PR, and it must not modify work already labeled `hold`, `on-hold`, or `do-not-merge`.

## ACMM level gating

Telemetry and operations are **L5/L6-only opt-in agents**. Per the built-in ACMM packs (`src/pkg/config/packs/level-5.yaml`, `level-6.yaml`):

- They are absent from the roster entirely at L1–L4 — no pack below L5 defines a `telemetry` entry, so the agent does not appear in the dashboard, does not spawn a pane, and cannot be kicked.
- At L5 and L6 they are present but their cadence is `paused` in **every** governor mode (`surge`, `busy`, `quiet`, `idle`) until an operator opts in. This is not a description of typical defaults — the pack literally sets `telemetry: paused` in all four cadence tables at both levels.
- `kick_template` is `telemetry-holdgated.md` at L5 and `telemetry-full.md` at L6, matching the hold-gated-vs-full PR behavior described above.

## When to enable telemetry

Enable telemetry once you're comfortable with the rest of your L5/L6 roster and want automated observability hardening for the *managed* project — bounded metrics, tracing, and dashboards-as-code, on a backend you've explicitly named. Leave it paused if you have no observability backend in mind yet: with nothing configured, its policies fail closed, so it will only detect and report the existing stack rather than propose changes.

## How to opt in: `governor.project_observability`

Un-pausing the agent alone does nothing useful — telemetry's PR-capable policies fail closed without a confirmed target stack. Configure the opt-in under **Settings → Project Observability** in the dashboard:

```yaml
governor:
  project_observability:
    open_source: [opentelemetry, prometheus, grafana]
    kube_native: [servicemonitor]
    commercial: [honeycomb]
    references:
      honeycomb:
        endpoint_env: OTEL_EXPORTER_OTLP_ENDPOINT
        credential_secret: observability/honeycomb-key
```

Reference fields accept **names only** — an environment-variable name or a `secret-name/key` reference. Literal endpoints, tokens, and API keys are rejected. Selecting platforms and saving persists them under `governor.project_observability` — and nothing else: saving does **not** un-pause the agent (#7261 removed the enable toggles this tab used to carry). The tab shows a read-only status line per agent reporting its `Enabled` flag and real per-mode cadences; to actually run telemetry, un-pause it yourself from **Agents → Cadences** (a conservative `24h` in each mode is a good starting point).

After telemetry's first advisory run, platforms mentioned in its findings are preselected as suggestions in the Project Observability tab. They stay unsaved until an operator reviews and clicks **Save** — only then does the persisted declaration govern future telemetry (and operations) work.

## How telemetry interacts with other agents

Telemetry's lane keywords (`observability`, `opentelemetry`, `prometheus`, `grafana`, `tracing`, `metrics`, `structured-logging`, `servicemonitor`, `podmonitor`) are disjoint from operations' (health, SLO, runbook, incident, rollback, alerting terms), so the two agents do not compete for the same issues. Both share the same `${PROJECT_OBSERVABILITY}` prompt section and the same `governor.project_observability` configuration — telemetry adds the instrumentation; operations builds the health/SLO/runbook layer on top of it.

## Configuration reference

Registered defaults (`applyKnownAgentDefaults` in `src/pkg/config/config.go`), applied when a field is left blank in your own config:

| Field | Default |
|-------|---------|
| `emoji` | 📡 |
| `color` | `#00a8cc` |
| `aliases` | `["tm"]` |
| `bead_role` | `worker` |
| `sort_order` | `65` |
| `include_repos` | `true` |
| `lane_keywords` | `observability`, `opentelemetry`, `prometheus`, `grafana`, `tracing`, `metrics`, `structured-logging`, `servicemonitor`, `podmonitor` |
| `detect_keywords` | `telemetry`, `observability`, `opentelemetry`, `prometheus` |

ACMM packs additionally set `backend: copilot`, `model: claude-sonnet-4-6`, `mode: ISSUES_AND_PRS`, `stale_timeout: 28800`, and the level-appropriate `kick_template`.

## Cadence and budget considerations

Telemetry is a heavyweight, PR-capable agent once enabled. Follow the same guidance as every other agent un-paused at L5/L6: set all modes to `12h` or `1d` first, watch its output for a few cycles, and only shorten the cadence once you understand what it's producing and how much budget it consumes per run.

## What to read next

- **[Operations Agent](operations.md)** — the companion L5/L6 opt-in agent for operational readiness (health checks, SLOs, runbooks).
- **[Agent Configuration](agent-configuration.md)** — every agent field, the ACMM level packs, and `project_observability` details.
- **[ACMM Policy Matrix](acmm-policy-matrix.md)** — the full per-level, per-agent policy table, including the L5/L6-only gating note.
- **[Getting Started](getting-started.md)** — when to opt in as part of the level-climbing journey.

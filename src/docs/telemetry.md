# Data collection and telemetry

Hive sends a small number of operational signals outside the running hive only through the paths below. This page documents what is collected, where it goes, who can read it, retention where the code defines it, and how an operator can disable each path. The Linux Foundation telemetry policy says project software should not collect telemetry by default without LF legal review and prior user consent; Hive therefore treats any phone-home style path as a review item before changing defaults.

## Summary

| Path | What leaves the hive/browser | Destination and visibility | Default / consent | Retention | Disable path | Code references |
|---|---|---|---|---|---|---|
| Hub heartbeat | Hive ID; org/repos/primary repo; configured AI author; GitHub API/base host; ACMM level; agent names, state, backend/model counts; governor mode and queue counts; process/version/image/uptime; dashboard URL/snapshot URL/public flag/cluster ID; health summaries; token/budget counters; contributor counts, leaderboard usernames/avatar URLs/task counts/current task; active/engaged dashboard usernames and last-action timestamps; repo activity counts; PR/CVE/fleet counters; advisory/error strings; non-secret GitHub App IDs/slugs/installation IDs/key fingerprints; dashboard-token hash; file-descriptor counts; cluster-health status. No raw GitHub tokens, App private keys, dashboard tokens, session IDs, or heartbeat secrets are sent. | Configured hub at `hub.url` / `HIVE_HUB_URL`; hub UI/API access is hub-admin/operator gated. | Sent every two minutes when the hive is hub-linked. A standalone hive with no hub URL does not heartbeat. Once hub-linked, sensitive identifier classes can be withheld with `hub.heartbeat_omit` (see [Withholding heartbeat identifiers](#withholding-heartbeat-identifiers)); other fields are not configurable. | Hub registry retention is operational state, not currently documented as a fixed expiry. | Remove the hub link (`hub.url` / `HIVE_HUB_URL`) to stop all hub features. `hub.heartbeat_omit` withholds identifier classes per hive; a core-heartbeat disable switch is still pending maintainer direction (tracking issue: [#10202](https://github.com/hivecommons/hive/issues/10202)). | `src/cmd/hive/boot_heartbeat_deps.go`; `src/cmd/hive/main.go` (`bootHeartbeatWith`); `src/pkg/hub/spoke/heartbeat.go` (`HeartbeatPayload`); `src/pkg/hub/hub_keys.go` (`SpokeHeartbeatKey`). |
| Task-status push / contributor relay status | Hive ID, contributor summary, and leaderboard entries: GitHub username, avatar URL, trust tier, tasks completed/failed, active flag, and current task title. | Same configured hub, authenticated with the per-hive heartbeat bearer. | Defaults on when the hive is hub-linked; controlled by `hub.task_status_push` / `HIVE_HUB_TASK_STATUS_PUSH`. | Operational hub state; no fixed expiry documented. | Set `hub.task_status_push: false` or `HIVE_HUB_TASK_STATUS_PUSH=false` and restart; this stops only the separate task-status loop. | `src/cmd/hive/main.go` (`StartTaskStatusPush` collector); `src/pkg/hub/spoke/heartbeat.go` (`TaskStatusPayload`). |
| Dashboard feedback reports | User-entered title/description/screenshots; optional diagnostics if the user keeps **Include diagnostics** checked: version, commit, channel, ACMM level, hive ID, hosted/hub-linked flags, agent count, agent names/backend/model/state, browser user agent/platform/language, screen/window size, dashboard page, plus recent console errors and failed local `/api` calls. Project org/repo is sent only if the user checks **Also include project org/repos**. Tokens, bearer strings, emails, and secret-looking key/value text are redacted before submission. | The diagnostic bundle is committed to the `hive-feedback` branch of `hivecommons/hive` or `hivecommons/docs` with the GitHub App installation token (hub-less: the spoke's; hub-linked: minted by the hub for that spoke). The issue itself is opened by the user from a prefilled `issues/new` URL that links to the bundle; Hive never authors feedback with a user, bot, or operator token, and dashboard login requests no OAuth scope (#1927). | User-initiated only. The modal previews exactly what will be sent; diagnostics can be unchecked; project org/repos require a second opt-in checkbox. | GitHub issue retention follows GitHub/project issue retention. The spoke stores up to 50 local feedback issue references in `/data/feedback-submissions.json`. | Do not submit; uncheck diagnostics/project context; remove screenshots. Operators can also avoid hub relay by not hub-linking. | `src/pkg/dashboard/static/index.html` (`FEEDBACK_DIAGNOSTIC_FIELDS`, preview, checkboxes); `src/pkg/dashboard/feedback.go`. |
| Build-containment lookup | A commit SHA (the hive's running commit and the commit being asked about) or a PR number, sent as part of a GitHub REST `compare` or `pulls` request. Only when an operator runs a lookup (`GET /api/saas/build-contains` on the hub, `GET /api/version/contains` on a hive). No hive identifier, org, repo name or user is in the request. | `api.github.com`, the public `hivecommons/hive` repository, using the hub's read-only GitHub identity or the hive's own configured GitHub client. The answer is shown to the caller only. | On demand only; no background polling. A hive with no GitHub client answers `unknown` and sends nothing. | None stored; compare answers are cached in memory, for the life of the process. | Do not run the lookup; there is no setting because nothing is sent otherwise. | `src/pkg/buildcontains/buildcontains.go`; `src/pkg/hub/build_contains.go`; `src/pkg/dashboard/api_version_contains.go`. |
| NPS prompt | Score 1-4, optional free text, hive ID, and dashboard version. The signed-in username is used only locally for rate limiting and is not sent. | Hub `POST /api/nps/ingest`, or an explicitly configured NPS relay for standalone hives; only hub admins can read stored responses. | Hosted spokes default on; self-hosted/federated/standalone default off. Standalone relay requires explicit `hub.nps_enabled`/`HIVE_NPS_ENABLED` and `hub.nps_relay_url`/`HIVE_NPS_RELAY_URL`. | Hub store keeps at most 1,000 responses per hive and 10,000 total, oldest dropped first. | `hub.nps_enabled: false` or `HIVE_NPS_ENABLED=false`; leave relay URL unset. | `src/docs/nps.md`; `src/docs/env-vars.md`; `src/pkg/dashboard/nps.go`; `src/pkg/hub/nps.go`. |
| Hosted hub web analytics | GA4 page/event data from hub-hosted pages, including custom hub events such as hive creation/deletion. Existing event code includes hive IDs and creation inputs such as org, primary repo, ACMM level, and cluster ID. | Google Analytics via `www.googletagmanager.com`, property `G-4707R797K3`. | Loaded by hosted hub static HTML pages. This is website analytics, not spoke software telemetry, but it still needs privacy review because custom events may include project identifiers. | Google Analytics retention is controlled in the GA property, not in this repository. | Browser tracking protections or removing the hosted hub GA script; there is no repository config flag. | `src/pkg/hub/assets/dashboard.html`; `src/pkg/hub/saas_pages.go`; `src/pkg/hub/static/*.html` pages such as `reading.html` and `cncf-reference-architecture.html`. |
| Ask AI help (optional) | The question a signed-in person typed, after a credential-shape check that refuses rather than sends; if enabled, a fixed dashboard section label; the Kapa.ai integration ID; and, for a follow-up, the vendor thread ID. Kapa also sees the calling server's IP. Hive sends no login, email, page content, URL, hive, repo or agent name as context. | Kapa.ai Chat API (`api.kapa.ai`), called server-side by the hub, or by a self-hosted hive in `ask.mode: direct`; a hosted hive in `ask.mode: hub` sends the same fields (without the integration ID) to its hub relay over the per-hive heartbeat bearer. The project's Kapa administrators can read questions in Kapa analytics. | Off by default (`ask.enabled` / `HIVE_ASK_ENABLED`); inert until a key file, project ID and integration ID are configured. Each person confirms a disclosure before their first question and can withdraw. | Kapa retains conversations under its own terms; Hive stores no question or answer text, only the per-person consent record. | Leave `HIVE_ASK_ENABLED` unset, or withdraw consent in the Ask panel. | [Ask AI help](#ask-ai-help); `src/pkg/askai/service.go`; `src/pkg/askai/kapa.go`; `src/pkg/askai/relay.go`; `src/pkg/hub/ask.go`; `src/pkg/dashboard/api_ask.go`; [guide](ask-ai-help.md), [design](design/dashboard-ask-ai-help.md). |
| Customers enrichment: gitdm snapshot | Nothing about any person. A download of the public cncf/gitdm affiliation files (the same files for every hive), with a generic User-Agent and no hive identity. Matching against confirmed customers runs on the hive. | `github.com` / `raw.githubusercontent.com`, public `cncf/gitdm` repository. | **Off by default** (`customers.enrichment.enabled` and `customers.enrichment.gitdm.enabled`, both needed). Refreshed every `customers.enrichment.gitdm.refresh_interval` (default 24h), with ETags so an unchanged file is not downloaded again. | The snapshot is cached in `/data/customers/colony-cache`; matches are kept in `/data/customers/enrichment.json` until the person is forgotten. | Leave either switch off, or set `HIVE_CUSTOMERS_ENRICHMENT_ENABLED=false`. | [Customers enrichment](customers-enrichment.md); `src/cmd/hive/customers_enrichment.go` (`runCustomersEnrichmentPass`); `src/pkg/customers/enrichment.go`; hivecommons/colony `gitdm`. |
| Customers enrichment: devstats lookup | A confirmed customer's public GitHub login, never an email, name or id, with a generic User-Agent and no hive identity. | CNCF devstats (`devstats.cncf.io`). | **Off by default** (`customers.enrichment.enabled` and `customers.enrichment.devstats.enabled`). At most `customers.enrichment.devstats.hourly_limit` lookups an hour (default 30). Suggested people are never looked up, and a person without a confirmed GitHub login is never looked up. Sent as a POST to the public devstats API by the hivecommons/colony client (generic User-Agent, no redirects, rate limited). | Results are kept in `/data/customers/enrichment.json` until the person is forgotten. | Leave either switch off, or set `HIVE_CUSTOMERS_ENRICHMENT_DEVSTATS_ENABLED=false`. | [Customers enrichment](customers-enrichment.md); `src/cmd/hive/customers_enrichment.go` (`runDevstatsStep`); hivecommons/colony `devstats`. |
| Customers enrichment: crowd.dev lookup | A confirmed customer's public GitHub login, never an email, name or id, and the owner-supplied crowd.dev API token. | crowd.dev / LFX Insights API. | **Off by default** (`customers.enrichment.enabled` and `customers.enrichment.crowddev.enabled`) and inert without a token. Not active: LFX Insights has no usable public API (hivecommons/colony#5), so no request is made. | Results are kept in `/data/customers/enrichment.json` until the person is forgotten. | Leave either switch off, clear the token, or set `HIVE_CUSTOMERS_ENRICHMENT_CROWDDEV_ENABLED=false`. | [Customers enrichment](customers-enrichment.md); `src/pkg/dashboard/api_customers_enrichment.go`. |
| Hub affiliation: colony gitdm snapshot | Nothing about any person. The hub downloads the public cncf/gitdm affiliation files (the same files for every hub), with a generic User-Agent and no hub or user identity. Each user's public github.com login is matched against them on the hub; emails are never used for matching. | `raw.githubusercontent.com`, public `cncf/gitdm` repository. | **On by default** on the hub (`HIVE_HUB_AFFILIATION_COLONY_GITDM`), because no data about anyone leaves the hub. Revalidated every `HIVE_HUB_AFFILIATION_COLONY_GITDM_REFRESH` (default 24h) with ETags, so an unchanged file is not downloaded again. | The files are cached in `/data/saas/colony-cache`. A match is cached on the user record (`affiliation`, `affiliation_source: colony:gitdm`, `affiliation_confidence`) until the next daily refresh and is shown only in the hub admin Users table. | Set `HIVE_HUB_AFFILIATION_COLONY_GITDM=false` on the hub. | [Hub affiliation from colony](#hub-affiliation-from-colony); `src/pkg/hub/affiliation_colony.go`; `src/pkg/hub/top_repo.go` (`refreshUserTopRepo`); hivecommons/colony `gitdm`; [#19851](https://github.com/hivecommons/hive/issues/19851). |
| Hub affiliation: colony devstats lookup | A hub user's public github.com login, never an email, name, hub id or GHE login, with a generic User-Agent and no hub identity. | CNCF DevStats (`devstats.cncf.io`). | **Off by default** on the hub (`HIVE_HUB_AFFILIATION_COLONY_DEVSTATS`). At most `HIVE_HUB_AFFILIATION_COLONY_DEVSTATS_HOURLY_LIMIT` lookups an hour across all users (default 30, at most 120), one per user per daily refresh. Sent as a POST to the public DevStats API by the hivecommons/colony client (generic User-Agent, no redirects, rate limited). | The CNCF-wide contribution count is cached on the user record (`cncf_contributions`) until the next refresh, and cleared when the switch is off. Shown only in the hub admin Users table. | Leave `HIVE_HUB_AFFILIATION_COLONY_DEVSTATS` unset or `false`. | [Hub affiliation from colony](#hub-affiliation-from-colony); `src/pkg/hub/affiliation_colony.go`; hivecommons/colony `devstats`. |
| Customers hub import (optional) | From the hive: its hive ID and a bounded batch of salted HMAC-SHA-256 hashes of the GitHub logins of its **confirmed** customers (never a suggested person, never plaintext, never any other identifier or profile field). From the hub, back to the hive: for a person who consented for this hive, the real name they entered themselves on the hub and the fields they switched on; for a grant holder of this hive, a labeled public country and organization without a consent record (only when the hive sends the `X-Hive-Customer-Match-Labels: 1` header, which it always does); for a person previously delivered and since withdrawn, only the names of the fields to clear. | The configured hub, `POST /api/customers/match`, authenticated with the per-hive heartbeat bearer. The hub compares hashes only against consenting people and never stores or logs a non-matching hash; for a match it learns that this hive has that person as a customer. Values received are visible to the hive owner and anyone with Customers access on that hive, who can export them. | Off by default at every layer: the hub switch `HIVE_HUB_CUSTOMERS_SHARE_ENABLED`, the hub's trust-domain allowlist `HIVE_HUB_CUSTOMERS_TRUSTED_HIVES` (empty by default; a hive not on it gets an empty answer whatever its own switch says), the hive switch `customers.hub_import.enabled` / `HIVE_CUSTOMERS_HUB_IMPORT_ENABLED` (Settings, Features), and consent on the hub's Profile sharing page for the real name, per person, per hive, with nothing pre-checked. Country and organization are treated as public and need no per-person consent once both switches are on. With the hive switch off no request is made. | On the hive, a hub value not refreshed within 72 hours is cleared. On the hub, a consent record lives with the person (deleted with the hub user); a withdrawn record is kept at most 90 days while a clear may be owed. | Turn the hive switch off; the person withdraws on the hub (takes effect on the hive at its next hourly sync); or leave the hub switch off. | `src/pkg/customers/hub_import.go`; `src/cmd/hive/customers_hub_import.go`; `src/pkg/hub/customer_match.go`; `src/pkg/hub/customer_consent.go`; [design](design/customers-hub-consent.md). |
| Crash/error reporting | No Sentry/PostHog/Segment/Plausible crash reporter was found in `src` during the onboarding audit. Errors are logged locally; selected log-safe health/error strings can appear in hub heartbeats as described above. | Local logs and, for selected operational state, the configured hub. | No third-party crash reporting by default. | Local log/PVC retention is deployment-specific. | N/A for third-party crash reporting; disable hub link for heartbeat-carried operational errors. | Audit search: `telemetry|analytics|metrics push|phone.?home|posthog|segment|plausible|gtag|googletagmanager|sentry` over Go/HTML/JS/Markdown. |

## Linux Foundation telemetry-policy status

| Policy requirement | Hive status | Gap / follow-up |
|---|---|---|
| Do not collect telemetry by default without Linux Foundation review and approval. | Standalone hives do not heartbeat or send NPS relay data unless configured. Hub-linked/hosted hives always send operational heartbeats to the configured hub. Hosted hub pages load GA4. | Heartbeats and hosted GA4/custom events should be reviewed with CNCF/LF before this onboarding item is marked complete. |
| Specify exactly what is collected. | NPS is documented in `src/docs/nps.md`; feedback diagnostics disclose fields in the UI. This page now documents heartbeats, task-status, feedback, NPS, analytics, crash reporting, and the Customers hub import. | Keep this page updated when heartbeat or feedback fields change. |
| Demonstrate anonymization and avoid personal/end-user/confidential data. | No raw secrets are intended in the heartbeat; feedback redacts token/email/secret-looking text; NPS intentionally omits user identity. | Heartbeats can include GitHub usernames, org/repo names, dashboard usernames, current task titles, project identifiers, hive IDs, and GA4 custom event identifiers. These are not fully anonymized and may be business-sensitive. |
| Notify users and require consent before collection starts. | Feedback is explicit and previewed; project org/repo diagnostics require a separate checkbox. NPS has an enable switch and documented defaults. The Customers hub import is off by default on hub and hive and sends a person's fields only after that person consents, per hive and per field, on a versioned disclosure. | Hub heartbeats are tied to hub-link configuration and are not separately consented or field-configurable; hosted GA4 notice/consent is not documented in this repo. |
| Document storage and project-community access. | NPS retention and admin-only read path are documented. Heartbeat and task-status visibility is hub-side operational/admin UI. | Fixed retention for heartbeat/task-status registry data and GA4 retention are not documented. |
| Secure the collection mechanism. | Heartbeats and task-status use per-hive derived bearer credentials; heartbeat responses are signed; feedback/NPS size caps and redaction are implemented. | Security posture depends on the configured hub and GA4; continue to review any new phone-home path before enabling it by default. |
| Make approved telemetry data available to project participants. | NPS is admin-only by design; hub operational state is hub-admin/operator visible. | If LF approves any project telemetry program, define what aggregate data is shared with the community and where. |

## Ask AI help

These disclosures apply only where Ask is supported and enabled. A signed-in
person must accept the panel's disclosure before sending a question. The
hub stores its users' consent records with hub data. A hive keeps consent
for its own dashboard users, even when the hub relays their questions. Consent is scoped to the dashboard
identity and disclosure version; a changed disclosure requires fresh
consent. An operator enabling the feature does not consent on anyone's
behalf.

Kapa does not receive dashboard content, URLs, repo or hive names, user
identifiers, cookies, or session tokens as request context. It does receive
the free text that you type, so do not enter any of those details yourself.
The browser does not load a Kapa widget or contact Kapa from the panel.

Hive's operational audit records outcomes and counts, not the question,
answer, source links, or vendor thread ID. Withdrawing consent prevents the
next request and does not delete past questions from Kapa. See
[Kapa's retention documentation](https://docs.kapa.ai/security/certifications).

## Heartbeats are not a crash reporter

The heartbeat is an operational control-plane link between a spoke and its configured hub. It is used for liveness, health, upgrade/config delivery, hosted-spoke lifecycle, and fleet views. It should not be treated as an anonymous metrics program: the payload contains stable project and hive identifiers and may contain GitHub usernames. Operators that do not want this link should run without `hub.url`/`HIVE_HUB_URL`; Hive does not currently provide a per-field heartbeat opt-out (tracking issue: [#10202](https://github.com/hivecommons/hive/issues/10202)).

---

## Disabling task-status pushes

The separate task-status loop can be disabled without losing the core heartbeat, liveness, hub-managed upgrade policy, or config delivery:

```yaml
hub:
  task_status_push: false
```

Alternatively set `HIVE_HUB_TASK_STATUS_PUSH=false`. The environment variable overrides the config and accepts `1/true/yes/on` and `0/false/no/off` (case-insensitive, ignoring surrounding whitespace). Empty or invalid values fall back to the config. An absent config value defaults to `true`, preserving existing behavior. Restart the hive after changing this setting; it is evaluated at startup, not hot-reloaded.

This switch stops only the separate `StartTaskStatusPush` loop. The core heartbeat still sends its normal payload, including contributor counts and leaderboard fields. Use `hub.heartbeat_omit` below to withhold sensitive identifier classes from that payload; disabling task-status pushes is not a blanket telemetry opt-out.

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

## Activity digest

`hub.activity` controls the bounded, read-only activity digest the heartbeat carries for the hub-wide Activity Monitor ([#15963](https://github.com/hivecommons/hive/issues/15963), RFC [#15861](https://github.com/hivecommons/hive/issues/15861)). It is built in process from the dashboard's stage tracker: repo, number, kind, title, author kind, stage and modifier pills of open items, plus totals by stage and modifier.

```yaml
hub:
  activity:
    enabled: true        # default: on for hosted spokes (hub.hive_type: hosted) not withholding repos; off for self-hosted
    max_items: 100       # default 100; clamped to the hub's 250-item ceiling
    share_private: false # private-repository items are dropped (still counted in totals) unless true
```

When the cap applies, items needing a human or a decision are kept first, then stalled ones, then the furthest stage, and the number left out is reported as `truncated`. A repository counts as public only when the hub lists the hive as public (`hub.is_public`, its project repos) or it is on `HIVE_PUBLIC_REPOS`. Once the hub acknowledges a digest's etag, later beats send only `etag` and `generated_at` until the tracker changes. A tracker change also sends an activity-only update over the heartbeat endpoint and bearer, at most one every 15 seconds; changes arriving meanwhile are merged into it. The block also carries `events` ([#15964](https://github.com/hivecommons/hive/issues/15964)): the stage hops, opens and merges the spoke saw since the hub last acknowledged them, derived by diffing successive tracker snapshots (the first one only seeds the baseline), each with a sequence number. At most 200 are buffered; the oldest are dropped first and `events_dropped` discloses how many. Events stay buffered, and are resent, until a heartbeat or update response's `activity_events_ack` covers them. Private-repository items yield no events unless `share_private` is on. The hub keeps at most 1,000 events across the fleet in memory only, and no event is kept longer than 24 hours; when the ring is full it drops the oldest event of the hive holding the most events, so one hive cannot evict another hive's events. `hub.heartbeat_omit` applies: `repos` withholds every item and event (totals remain) and `task_titles` blanks titles.

## Hub affiliation from colony

The hub admin Users table shows who each signup appears affiliated with. Its
daily refresh (`src/pkg/hub/top_repo.go`) derives that from the user's public
GitHub or GHE footprint, and also asks the
[hivecommons/colony](https://github.com/hivecommons/colony) providers
(`src/pkg/hub/affiliation_colony.go`, phase 1 of the
Colony customer-data backend design ([#19851](https://github.com/hivecommons/hive/issues/19851))):

- **gitdm (on by default).** The hub downloads the public cncf/gitdm files and
  matches the user's public github.com login against them on the hub. Nothing
  about any person is sent. Emails are never used for matching on the hub, so
  no value derived from an email address, private-domain or otherwise, enters
  the Affiliation. A GHE login is never matched, because gitdm lists github.com
  logins.
- **devstats (off by default).** When switched on, each user's public
  github.com login is sent to CNCF DevStats, at most once per user per daily
  refresh and within an hourly cap. It adds the CNCF-wide contribution count
  to the Affiliation tooltip, and an affiliation should a later colony release
  return one. It is off by default because it is a per-person request to a
  third party and, with colony v0.2.0, it returns contribution activity rather
  than an affiliation.

Which value is shown, strongest first: an organization the user set on the hub,
one an admin set for them, the company the person wrote on their own GitHub or
GHE profile, colony gitdm, colony devstats, then the footprint heuristics
(contributed-to org, org membership, top repo owner) and the GHE and IdP
fallbacks. A colony value is labelled `colony:gitdm` or `colony:devstats` with
colony's confidence. None of these values are sent to any hive by this path;
the customer match answer still uses only the hub organization field and listed
organization email domains (see [Customers hub import](#customers-hub-import)).

| Setting (hub environment) | Default | Meaning |
|---|---|---|
| `HIVE_HUB_AFFILIATION_COLONY_GITDM` | `true` | gitdm provider |
| `HIVE_HUB_AFFILIATION_COLONY_GITDM_REFRESH` | `24h` | How often the gitdm files are revalidated (Go duration, at least `1h`) |
| `HIVE_HUB_AFFILIATION_COLONY_DEVSTATS` | `false` | devstats provider |
| `HIVE_HUB_AFFILIATION_COLONY_DEVSTATS_HOURLY_LIMIT` | `30` | devstats lookups across all users in one hour (at most `120`) |
| `HIVE_HUB_AFFILIATION_COLONY_TIMEOUT` | `2m` | Time budget for one user's colony lookups (at most `10m`); the first gitdm load downloads every file |

All are read on each refresh, so a change needs no restart.

## Customers hub import

The Customers section is spoke-local: nothing in it is read from the hub or sent
to the hub **unless** the hive owner turns on the hub import and a person opts in
on the hub. That opt-in path is the only exception, and it is off by default at
every layer ([#19088](https://github.com/hivecommons/hive/issues/19088), design:
[customers-hub-consent](design/customers-hub-consent.md)).

- **Trust domain.** The hub answers only the project's own hives, listed by
  hive ID in `HIVE_HUB_CUSTOMERS_TRUSTED_HIVES` on the hub (empty by default).
  Any other hive gets an empty match list, even with its import switch on and
  a person's consent recorded for it. The hub decides by the hive identity the
  per-hive bearer proves.
- **Request (spoke to hub).** Hashes only. Each is an HMAC of a confirmed
  customer's GitHub login under a per-hive match key (`HIVE_CUSTOMER_MATCH_KEY`,
  derived from the hub secret and hive ID), never the heartbeat bearer. At most
  200 hashes per request and four requests per hourly pass, and once more when a
  customer is accepted. A suggested person is never asked about.
- **Answer (hub to spoke).** The real name crosses only for a person with an
  effective consent for this hive, and only if they entered it themselves.
  Country and organization are treated as public
  ([#19306](https://github.com/hivecommons/hive/issues/19306)): for a person who
  holds a grant on this hive and is not blocked they cross without a consent
  record, each labeled with its source (entered by the person, set by a hub
  admin, inferred from browser language, derived from a listed organization
  email domain, or not recorded). The hive shows the label as "from hub: ...".
  An organization is derived from an email domain only for domains the hub
  operator listed in `HIVE_HUB_CUSTOMERS_ORG_DOMAINS` (empty by default); the
  email address and the domain never cross. Notes, email, Slack ids and
  location never cross. A person who is not a grant holder, is blocked or is
  unknown gets the same silence, so the answer cannot be used to learn who is on
  the hub.
- **Consent first (real name).** The person sees what is sent, to whom, what is
  never sent, how fast withdrawal takes effect and that exported copies cannot
  be recalled, then confirms per hive. A material change to that text suspends
  sharing of the real name until they confirm again.
- **What the hub learns.** For a person who matches (consenting, or a grant
  holder answered with public fields), that this hive has them as a customer. Non-matching hashes are dropped unlogged.
- **Withdrawal.** Immediate on the hub; the hive clears the real name at its
  next sync. A public country or organization the hub stops sending (for example
  because the person cleared it) is removed by the hive's 72 hour expiry, not at
  once. A value someone edited by hand on the hive is left alone. A hive that
  cannot reach the hub clears hub values after 72 hours.
- **Who sees what.** The person sees, per hive, what was last sent and when. A
  hub admin sees only consent counts per hive. The hive owner and anyone with
  Customers access on that hive see, and can export, the values received.

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

ACMM packs define `backend: copilot` (though the effective backend is subject to the allow/deny fallback), `model: claude-sonnet-4-6`, `mode: ISSUES_AND_PRS`, `stale_timeout: 28800`, and the level-appropriate `kick_template`.

## Cadence and budget considerations

Telemetry is a heavyweight, PR-capable agent once enabled. Follow the same guidance as every other agent un-paused at L5/L6: set all modes to `12h` or `1d` first, watch its output for a few cycles, and only shorten the cadence once you understand what it's producing and how much budget it consumes per run.

## What to read next

- **[Operations Agent](operations.md)** — the companion L5/L6 opt-in agent for operational readiness (health checks, SLOs, runbooks).
- **[Agent Configuration](agent-configuration.md)** — every agent field, the ACMM level packs, and `project_observability` details.
- **[ACMM Policy Matrix](acmm-policy-matrix.md)** — the full per-level, per-agent policy table, including the L5/L6-only gating note.
- **[Getting Started](getting-started.md)** — when to opt in as part of the level-climbing journey.

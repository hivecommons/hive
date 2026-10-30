# Fleet self-reporting (`governor.fleet_report`)

A hive can report *its own* hive-attributable failures upstream to
`hivecommons/hive`, so a defect that shows up across many hives gets noticed
without every operator filing the same issue by hand.

Because this files issues on a public repository from your hive, it is
**off by default** and the dry-run preview is designed to be read before you
turn it on. This page is the reference for what it sends, when, and what it
never sends.

## The opt-in

```yaml
governor:
  fleet_report:
    enabled: true              # tri-state; unset = hosted on, self-hosted off
    repo: hivecommons/hive     # default
    max_per_day: 5             # cap on NEW issues per 24h (joins never count)
    file_upstream: false       # legacy alias for enabled: true
```

`enabled` is a tri-state ([#11868](https://github.com/hivecommons/hive/issues/11868)):

- **unset** (the default) resolves to **on** for hub-managed hives — those
  booted with `HIVE_HEARTBEAT_KEY` by the hub, which run under the
  hivecommons-hive GitHub App and so can write to `hivecommons/hive` — and
  to **off** (dry-run) for self-hosted hives whose PAT is scoped to their
  own repos;
- `enabled: false` forces dry-run everywhere;
- `enabled: true` (or the legacy `file_upstream: true`) forces filing.

Dry-run means the hive evaluates everything, shows you the reports it
*would* file on the dashboard, and writes nothing upstream. The publish path
returns immediately when dry-run is set, before any GitHub client is touched.
There is one knob, and it is all-or-nothing: there is no per-trigger opt-in.
The same settings are exposed in the dashboard under Settings → Governor →
"Self-Reported Issues" (owner role) and over
`GET`/`PUT /api/config/governor/fleet-report`.

## The three triggers

A report is only ever built from evidence marked **attributable** — evidence
the hive believes points at hive's own behaviour rather than at your repos,
your prompts, or your infrastructure. Non-attributable evidence never produces
the report under a trigger.

### `acmm-shortfall`

Fires when an ACMM criterion has been unmet for **two or more weekly epochs**
(`PersistentUnmetEpochs = 2`) *and* there is attributable evidence to explain
it. The two-epoch requirement means a single bad week never files anything.

The point of this trigger is the pairing: "this hive cannot reach an ACMM bar,
and here is the hive-attributable reason why". A shortfall with no attributable
evidence is treated as an **operator** concern instead — it is surfaced to you
on the dashboard and is not reported upstream.

**Always the self-report tracker, never a standalone issue**
([#13470](https://github.com/hivecommons/hive/issues/13470)). A shortfall is
filed in one of two ways, and neither is a `Fleet report:` issue:

- When this hive already has an open `[selfreport]` tracker about the same
  component (a `runtime-error` entry, or an earlier shortfall that opened the
  tracker itself — open, not recovered, not found closed), the shortfall
  posts one short comment there — "ACMM shortfall rolled up from **<hive>**
  (<version>): `<component>` (<error class>) is blocking criterion
  `<criterion>` — <count>" — and its state entry points at that tracker. The
  roll-up is re-posted only when its text changes.
- Otherwise the shortfall **opens the tracker**: a
  `[selfreport] acmm-shortfall: <component> blocks <criterion>` issue with the
  same identity rows, owners, spoke link, `Affected hives` block and
  (when the evidence carries any) scrubbed diagnostics a `runtime-error`
  report has, plus an `## ACMM shortfall` section naming the criterion. A
  second criterion blocked by the same component then rolls up onto it. When
  the tracker is closed the next persistent shortfall opens a fresh one; a
  fingerprint closed upstream inside the 7-day cooldown is not re-filed.

The standalone `Fleet report: <component> blocks <criterion> [<fingerprint>]`
format is retired. Nothing on a current build renders it, the hub relay
refuses it from a spoke that still does (below), and the hive that watches
the report repo closes any that still land.

### Retired: `hive-code-defect` and every standalone `Fleet report:` issue

Until [#13470](https://github.com/hivecommons/hive/issues/13470) a third
trigger synthesised "agent stall", "agent crash loop" and "CI-poll nudge"
evidence from bare dashboard counters (`StallNudges`, `Restarts`,
`CIPollNudges`) and filed it as `Fleet report: … hive-code defect` issues with
no identity, owners, spoke link or diagnostics — nobody could act on them
(hivecommons/hive#13465 is the canonical example; over 14 days 12 of 18 such
reports were closed by the bot itself and about a third reached a fix,
against about 82% for self-reports). Those conditions are now `runtime-error`
classes (`agent_stall`, `agent_crash_loop`, `ci_poll_nudge`, below), observed
at the moment the agent manager or watchdog sees them, with the kick/pane
context attached. A persisted state entry for the retired trigger is pruned
on the next evaluation; nothing is filed or recovered under it.

A spoke still running a pre-#13470 build (hivecommons/hive#13567 came from
`5.186.4-22-g20cec0241`) can still *ask* for a standalone issue. Two guards
make sure it never gets one:

- **Hub relay.** `POST /api/selfreport/ingest` recognises the retired
  `Fleet report:` title. With no open issue for the fingerprint it looks for
  the relaying hive's open `[selfreport]` tracker (open, `hive/selfreport`,
  body carrying that hive's `<!-- hive:selfreport … hive=<id> -->` marker)
  and folds the payload onto it as one comment (the original report inside
  `<details>`), answering `200 {"joined": true}`. With no tracker it answers
  `410 Gone {"reason": "legacy_standalone_retired"}` and logs the spoke's
  version; the spoke records an upstream-write failure and retries next
  cycle, which is a log line, not an issue. Neither path creates an issue.
- **Governor sweep.** A spoke whose own credential can write to the report
  repo files directly and never touches the hub, so the hive that watches
  `hivecommons/hive` runs an hourly sweep: one search for open issues titled
  `Fleet report:` authored by a Bot account and carrying the
  `<!-- hive-fleet-fingerprint:… -->` marker, each closed as `not_planned`
  with a comment pointing at #13470 (at most 5 per pass). An issue a person
  reopened (`state_reason: reopened`), a human-authored one, or one without
  the marker is left alone.

### `runtime-error`

Unusual runtime errors the hive notices in itself. "Unusual" is an explicit
allow-list of classes defined in `pkg/fleetreport/runtime.go`; a condition
only becomes a report once it crosses that class's threshold, and the report
title is `[selfreport] <class>: <short message>`.

| Class | Fires when | Severity |
|---|---|---|
| `restart_storm` | the restart-storm breaker pauses an agent. Message carries the pause reason (`restart-storm: <cause>`); diagnostics add the restart count inside the breaker window against the pause threshold, the recent restart reasons with their counts (at most five, remainder summarised), and the agent's last error when it differs from the cause. Everything is bounded and scrubbed before storage | high |
| `kick_failed_repeatedly` | ≥3 consecutive undeliverable kicks for one agent *after* the busy-no-activity stall restart has been attempted (or that restart itself errored) — i.e. the recovery did not bring the session back; kicks that fail before the restart are a dashboard/log warning only | medium |
| `panic_recovered` | a dashboard handler recovered from a panic | high |
| `model_discovery_degraded` | model discovery has been failing for >1h (excluded, audit-log only: copilot serving the HTTP catalog because the SDK helper is absent (#11868), and copilot `Not authenticated` because the operator has not completed the dashboard Copilot Login (#12947), and copilot `not licensed to use Copilot` 403s, a seat problem on the reporting hive (#13206)) | medium |
| `inference_auth_failed` | 401/403 from an inference backend (LiteLLM, Copilot) | high |
| `advisory_post_failed` | the advisory digest could not be posted (rate-limit skips excluded) | medium |
| `agent_stall` | ≥3 consecutive "continue" nudges sent to one agent after a kick without the pane moving; a credited stall recovery (the pane moves before the next kick) clears the streak. Message carries the nudge count, last kick time and frozen pane hash; diagnostics carry agent, lane, backend, counters and a scrubbed 15-line pane tail (#13470, was the hive-code-defect "agent stall") | medium |
| `agent_crash_loop` | the watchdog latches an agent as crash-looping and pauses it (acting mode only — observe mode would re-count every sweep); the healthy-window reset clears it. Diagnostics carry the classifier detail, failure count/cap, backoff ladder and healthy-reset window (#13470, was "agent crash loop") | high |
| `ci_poll_nudge` | the harness sent a non-monitoring agent the stop-polling-CI nudge (at most one per kick); reviewer, CI-maintainer and quality lanes are exempt because reading CI status across PRs and repositories is their assigned work. Only a command line run more than once since the kick counts as polling (#14016): a sweep that reads each of several PRs' checks once does not cross the count threshold. A self-report fires only after an eligible agent ran ≥2 such repeated polls since the kick — a single nudged poll or a sweep does not trigger a report. One per 24h window; message carries polls since kick / time since kick / nudges since boot, diagnostics the scrubbed pane tail plus the repeated CI-status command lines with their counts (#14289; #13470, was "CI-poll nudge") | low |
| `issue_request_stuck` | an issue/comment/label request file in the relay queue (`issue-requests/`) has failed ≥3 attempts **or** has been retrying for ≥30 min. Key is the request file name; message carries kind, attempts and age, diagnostics the last error (scrubbed). Clears when the request completes or is quarantined (`.failed`/`.bad`) | medium |
| `effect_ledger_op_stuck` | the mutation boundary refused a retry because the journaled external effect (issue create, PR open, …) has been `Unknown`/`Planned` for ≥30 min — the caller keeps replaying the same logical op and it never resolves. Key is the op's logical ID; message carries kind, status and age, diagnostics the attempt count, repo and claim target. Clears when the op is applied, replayed as already-applied, or reconciled | medium |
| `agent_dir_setup_failed` | the harness failed to set up the per-agent sandbox directory structure — create XDG directories, restore or tighten directory permissions on interactive/inference home directories. Log-fed: keywords include `failed to create per-agent xdg dir`, `failed to restore per-agent dir mode`, `failed to restore interactive home mode`, `failed to tighten per-agent dir mode`, `failed to tighten interactive home mode`, `failed to restore inference home mode`, `failed to tighten inference home mode` (`crux/agent/manager.go`), repeated ≥5 times in 10 min. Log-fed: same repeat threshold and idle clear as `agent_pane_timeout` | medium |
| `agent_pane_timeout` | the agent-readiness wait timed out and the harness dumped the tmux pane (`prompt timeout — dumping pane` / `pane content at timeout`, `pkg/agent/manager.go`), and the startup kick dropped because the CLI never became ready or reached its input prompt (`startup kick dropped: …`, `crux/agent/manager_kick.go`, the same event as the pane dump). Log-fed: same ≥5-in-10-min repeat threshold and 20-min idle clear as the catch-all below; pane excerpts are scrubbed and bounded before storage | low |
| `agent_login_required` | the login scanner saw the agent CLI asking for `/login` (`login required detected`, `pkg/loginscan`) — the agent's auth lapsed and it is paused until an operator re-authenticates. Log-fed: same repeat threshold and idle clear as `agent_pane_timeout` | medium |
| `pr_request_head_unpushed` | the pr-request watcher found the PR head branch missing on the remote (`head branch is not on the remote`) — the agent's push did not authenticate or never ran. Log-fed: same repeat threshold and idle clear as `agent_pane_timeout` | medium |
| `bead_fact_ingest_failed` | the knowledge system failed to ingest a bead fact from an agent (`failed to ingest bead fact`, `pkg/knowledge`), repeated ≥5 times in 10 min. Log-fed: same repeat threshold and idle clear as `agent_pane_timeout` | medium |
| `feedback_issue_refresh_failed` | the dashboard's feedback-issue watcher failed to refresh or update a feedback issue (`feedback: issue refresh failed`, `pkg/dashboard`), repeated ≥5 times in 10 min. Log-fed: same repeat threshold and idle clear as `agent_pane_timeout` | medium |
| `github_app_repo_not_covered` | the GitHub App recheck (`state=repo-not-covered`) found the installation does not include every configured repo — an operator configuration condition (tick the repos in the App's repository access), not a hive defect. Log-fed: same ≥5-in-10-min repeat gate and 20-minute idle clear as `agent_pane_timeout` | low |
| `github_app_not_installed` | a per-agent scoped-token mint was attempted while the GitHub App has no installation ID (`installation_id` 0): the App is not installed on the hive's org, or discovery never found it. An operator configuration condition — install the App or set `github.installation_id` — not a hive defect. Key is the agent name; reported on the first observation and cleared by the next successful mint. The runtime logs the matching WARN once per agent instead of every supervisor tick (#14186) | medium |
| `ioscan_classifier_unavailable` | the ioscan model classifier failed closed (`ioscan classifier failed closed; text withheld`, `pkg/scheduler/ioscan_enforce.go`) or, under `ioscan.classifier.fail_mode: open`, passed text on unclassified (`ioscan classifier failed open; text passes on unclassified`) — typically no model endpoint resolved — so untrusted text is withheld. The scheduler logs `ioscan fail-closed blocked kick` (`pkg/scheduler/templates.go`) or `ioscan fail-closed blocked held-PR coordination kick` (`pkg/scheduler/policy_overlays.go`) when it withholds a kick for the same reason, which maps to this class too. The WARN is already deduped per failure by `warnOnce` (once per process), so it is reported on the first observation (`MinCount` 1) with the normal idle clear; this is a real degradation, not noise. Log-fed | medium |
| `github_fetch_transient` | a non-critical GitHub fetch failed and the feature degrades gracefully (`channel commit date: fetch failed — the row will render without a timestamp`, `pkg/hub/channel_commit_date.go`) — typically a rate limit or transient API error, not a hive defect. Reported only when persistent: log-fed, same ≥5-in-10-min repeat threshold and idle clear as `agent_pane_timeout`. The sibling `SHA poll: commit message fetch non-200` has its own class, `sha_poll_commit_message_unavailable` | low |
| `label_provisioning_failed` | provisioning the issue stage labels on a repo failed (`issue stage labels: provisioning failed`, `pkg/github/issue_stage_labels.go`) — typically the App lacking label permission on that repo or a transient API error; diagnostics carry the repo and scrubbed error. Log-fed: same repeat threshold and idle clear as `agent_pane_timeout` | low |
| `agent_cli_restart_loop` | the agent CLI crashed (bare shell) or sat on a consent screen and the harness restarted it before the kick (`agent CLI crashed or stuck on consent screen, restarting before kick`, `crux/agent/manager_kick.go`, `kick_async.go`), repeated ≥5 times in 10 min — a restart loop, not transient noise. Log-fed: same repeat threshold and idle clear as `agent_pane_timeout` | medium |
| `proxy_connect_dial_failed` | the hive proxy could not dial the upstream host of a CONNECT request (`proxy: CONNECT dial failed`, `crux/proxy/github_proxy_connect.go`) — typically DNS or network unreachability of an agent-requested host, repeated ≥5 times in 10 min. Log-fed: same repeat threshold and idle clear as `agent_pane_timeout` | medium |
| `proxy_tls_handshake_failed` | the hive proxy's TLS handshake with an upstream host failed (`proxy client tls handshake failed`, `transparent proxy tls handshake failed`, `copilot sniff: client tls handshake failed`, `crux/proxy/github_proxy_connect.go`, `crux/proxy/github_proxy_copilot.go`) — typically a certificate validation failure or encryption mismatch on an agent-requested host, repeated ≥5 times in 10 min. Log-fed: same repeat threshold and idle clear as `agent_pane_timeout` | medium |
| `review_evidence_write_failed` | the github-client failed to write a review evidence bundle to persistent storage (`review evidence: could not write bundle`, `pkg/github`), repeated ≥5 times in 10 min. Log-fed: same repeat threshold and idle clear as `agent_pane_timeout` | medium |
| `selfreport_cleared_comment_failed` | the fleet report publisher failed to post the "condition cleared" comment on a self-report issue (`fleet report: cleared comment failed`, `pkg/github`), repeated ≥5 times in 10 min. Log-fed: same repeat threshold and idle clear as `agent_pane_timeout` | medium |
| `inference_caller_unidentified` | the inference translator refused a caller whose identity could not be established (`inference request from unidentified caller refused`, `crux/proxy/github_proxy_inference.go`). The request remains rejected with HTTP 401; this is distinct from backend credential failures. Check the per-agent UID map and socket attribution for the calling process; the log carries the remote address, method and path. Log-fed: ≥5 refusals in 10 min, clears after 20 quiet minutes | medium |
| `inference_upstream_error` | a failed or non-2xx response from the configured inference backend (LiteLLM, vLLM/llm-d) on the Anthropic-to-OpenAI translator or MITM reroute path (`inference upstream error` / `inference upstream failed`, `crux/proxy/github_proxy_inference.go`), repeated ≥5 times in 10 min — covers transport failures and statuses other than the dedicated 401/403 auth signal. Log-fed: same repeat threshold and idle clear as `agent_pane_timeout` | medium |
| `proxy_plain_forward_refused` | the hive proxy refused a plain-HTTP forward to a non-globally-routable destination (`proxy: plain HTTP forward refused`, `crux/proxy/github_proxy_connect.go`) — a client ignored its proxy bypass for a private address (e.g. the Docker bridge gateway). Log-fed: same ≥5-in-10-min repeat threshold and 20-minute idle clear as `agent_pane_timeout`. Operator diagnostics: fix the client's `NO_PROXY` or target address; the refusal is counted as a proxy violation | low |
| `sha_poll_commit_message_unavailable` | the SHA poller's commit-message fetch got a non-200 from GitHub (typically a 403 rate limit or token scope; `SHA poll: commit message fetch non-200`, `pkg/hub/saas_sha_poller.go`). The poller backs off per SHA and keeps serving the SHA without a message. The WARN itself is deduped to once per hour per (SHA, status) (`warnGitHubFetchOnce`), so one stuck commit logs one line and never trips this class; it is reported only when ≥5 distinct SHA/status pairs fail within 10 min (a broad outage such as an exhausted rate limit or a revoked token). Its main job is keeping the residual WARN out of `unclassified_error_repeated`. Log-fed: same repeat threshold and idle clear as `agent_pane_timeout` | low |
| `tmux_session_check_failed` | the agent's tmux session `has-session` check failed (`tmux has-session failed`, `crux/agent/manager.go`) — typically the session was lost or the tmux server is unhealthy, repeated ≥5 times in 10 min. Log-fed: same repeat threshold and idle clear as `agent_pane_timeout` | medium |
| `transcript_scan_slow` | a Copilot transcript activity scan took longer than its interval (`transcript activity scan outlived its interval`, `crux/agent/busy_visibility.go`) — agent liveness reads may lag. Log-fed: same ≥5-in-10-min repeat threshold and 20-minute idle clear as `agent_pane_timeout`. Operator diagnostics: check the agent's transcript/session volume; it is bounded by concurrency, so repeated hits indicate slow disk or very large session directories | low |
| `kick_freshness_withheld` | a kick was withheld because every issue/PR ref failed the pre-kick freshness check (`kick withheld: every ref failed the freshness check`, `cmd/hive/kick_freshness.go`). The kick is retried on the next cycle; reported only when the same withholding repeats ≥5 times in 10 min (refs persistently stale or unreadable). Deferred API-budget refusals log at INFO and never trip it. Its main job is keeping the residual WARN out of `unclassified_error_repeated`. Log-fed: same repeat threshold and idle clear as `agent_pane_timeout` | low |
| `unclassified_error_repeated` | **generic catch-all**: the same WARN/ERROR log line (numbers, hashes, paths and quoted strings collapsed) repeated ≥5 times in 10 min. Fed by a `slog.Handler` tee (`pkg/fleetreport/slogfeed`) on the hive's root logger, so a repeating defect is noticed even with no hook site. Skips benign lines (read/keepalive i/o timeouts, WARNs whose `error` is a GitHub rate-limit refusal or a 502/503/504 gateway failure — the caller retries next tick, context cancellation, the permissions watcher's `fixed …` self-healing lines) and any message that already matches another class's keywords. Clears automatically after 20 quiet minutes (2× the window), so a "cleared" comment follows once the symptom stops | low |

The generic feed also skips the exact `review pending expired` message: it
records the review planner's bounded retry, and a burst across unrelated PRs
must not become a repeated-error report. The original expiry log and its
diagnostics remain available. `review dispatch exhausted; holding PR for a
human` still qualifies for reporting when repeated.

It also skips the constant `policies: seeded policy differs from the
embedded default with no seed record…` warning (`ReconcileSeededDefaults`,
`pkg/policies/seed_reconcile.go`, hivecommons/hive#13825): a hive whose
policy directory predates the seed-manifest tracking added in
hivecommons/hive#9428 has every embedded template diverge on its first
post-upgrade reconcile — up to one WARN per template in the same boot — and
a burst across unrelated templates is not a repeated runtime defect
(hivecommons/hive#14005). The per-template warning stays in the container
log, already deduped per path per process.

It also skips `copilot CLI substituted the configured model`
(`logCopilotModelFallback`, `src/crux/agent/manager_poll.go`): each copilot
agent warns once per (requested, running) model tuple (hivecommons/hive#13659)
and shows the substitution on its dashboard `LastError`, so a hive whose
copilot agents share one model id the account's Copilot CLI rejects emits one
WARN per agent in the same boot. That is an operator configuration condition —
change the agents' model — not a repeated runtime defect
(hivecommons/hive#14020).

The re-engagement sweep logs `re-engagement kick undeliverable; budget not
charged` at INFO rather than WARN when the cause is `shared session pool is
busy` (`deliverReEngagement`, `src/cmd/hive/main_sweeps.go`): a saturated shared CLI
session pool refusing another session is backpressure. The kick can be retried
on a later sweep and consumes no repair budget, so a burst across PRs is not a
repeated runtime defect (hivecommons/hive#15350). Other undeliverable-kick
causes stay at WARN and remain reportable.

Deferred to follow-ups (defined, not yet wired): `upgrade_stalled`,
`data_disk_pressure`, `github_app_token_mint_failed`,
`unexpected_http_5xx_from_hub`.

The queue-health classes exist because of a real incident: eleven
placeholder request files (`repo: "owner/repo"`, title `t`, body `b`) sat in
the relay queue and were retried for six hours — 175 identical warnings
sharing one effect-ledger op stuck in `Unknown` — while the allow-list,
having no hook for the watcher, reported nothing. The generic feed catches
the *shape* of that failure (one message, many repeats) for any component;
the two explicit classes name the queues an operator actually has to drain.

Each report attaches the class, severity, count and window, the normalised
message (numbers, hashes, paths and quoted strings replaced by placeholders)
and a redacted diagnostics excerpt inside `<details>`. Secrets are never
collected; the diagnostics go through the same scrubber as the other
triggers.


### `observation`

Nous detector findings use the `observation` self-report class and the
`hive-observation` label. The body contains only hive metadata (hive id/slug,
project organization, ACMM level, hive version, and `cluster_id`) plus
aggregate evidence and a JSON daily series suitable for a sparkline. Watched
repository names, issue numbers, and file paths are not part of this report.

Observation reports never use the spoke's GitHub client. A hub-managed spoke
posts them to `POST /api/selfreport/ingest`; the hub forces the destination to
`hivecommons/hive`, adds the configured needs-decision label, and searches the
open issue body for the detector fingerprint before creating or commenting.

## Exactly what leaves the hive

Each report — `acmm-shortfall` and `runtime-error` alike, since
[#13470](https://github.com/hivecommons/hive/issues/13470) — carries:

| Field | Value |
|---|---|
| hive | the hub-assigned hive id on a hub-managed spoke; the anonymous instance digest otherwise |
| hive name, spoke | hub-managed spokes only (hivecommons/hive#13211): the hive's display name and a markdown link (name → dashboard URL) — see [Which hive](#which-hive); `—` when unknown |
| anonymous instance | first **16 hex characters of the SHA-256** of your hive ID |
| owner(s) | the GitHub login(s) of the hive's owner(s) — `dashboard.authorized_users` entries with the owner role plus the device-flow operator — as `@login`; `(anonymous)` when `include_owners: false` |
| hive version, commit | build stamp, short commit |
| mode | the hive's mode |
| ACMM level | e.g. `L3` |
| unmet criterion | name and key (`acmm-shortfall` only; also repeated in the `## ACMM shortfall` section) |
| component, agent, lane | which part of the hive produced the evidence |
| error class | the classified error, not the raw message |
| count + window | how many occurrences, over what window |
| detected periodicity | if the failures look periodic |
| repos, owners | the `owner/repo` names this hive watches (`project.repos`) and the distinct owners derived from them, as `N: a/b, c/d` — see [Repositories and repo owners](#repositories-and-repo-owners) |
| repo, ref | `runtime-error` only: the item the error was scoped to, when the class carries one (today `advisory_post_failed` → the digest's repo); `—` otherwise |
| self-recovered | `no` on a report, `yes` on a recovery |

Alongside the rendered body, each report — and the hub relay request to
`POST /api/selfreport/ingest` — carries a structured `hive_state` block: the
hive's 24 h / 7 d state trends from `pkg/hivestate` (median/p90 queue depth
for issues, PRs and held items, SLA-violation rate, agent idle ratio,
kicks/day, tokens/day; see
[Hive state time series](strategy-lab.md#hive-state-time-series)). It contains
only aggregate counts and rates — no agent names, issue numbers, or repository
content — and is kept out of the body, so moving trends never make an open
report look changed. It is omitted when the hive has no snapshot in the last
7 days.

Every report carries the same label set: `fleet-report`, `hive/selfreport`,
`origin/selfreport`, `kind/bug`, `needs-triage`, `severity:…`, `trigger:…`,
plus `class:…` for runtime errors and `criterion:…` for shortfalls. The
per-instance `component:…`/`version:…`/`instance:…` labels are gone — the
report repo rejected them and the retry used to drop *every* label
(hivecommons/hive#13465 landed with none). If GitHub rejects a label set the
hub relay retries with the core five, then with `hive/selfreport` alone, never
with none. Observation issues additionally carry `hive-observation` and
`observation:<kind>`.

### What it does not send

- A hive that is not hub-managed sends only an anonymous instance digest;
  the raw hive id, name and spoke URL never leave it.
- Observation reports use the hub-provided hive id/slug and project
  organization metadata required for maintainer triage, but never send watched
  repository names, issue numbers, or file paths.
- **No raw log lines, error messages, or repository content.** Evidence is
  reduced to a classified error *class*, a component, and counts.
- Titles and bodies are passed through `logscrub` before leaving the hive,
  which redacts credential-shaped substrings (GitHub tokens, AWS access keys,
  bearer tokens, JWTs, private-key blocks). This is a backstop, not the primary
  protection — the report is built from structured fields, not from free text.

## Deduplication, comments, and recovery

Every report gets a deterministic **fingerprint**. A `runtime-error`
fingerprint is a SHA-256 over the error class and the normalised message
(numbers, hashes, paths and quoted strings collapsed) — it names the defect,
not the build, so the same fault on every version and every hive produces
the same fingerprint and a close by a maintainer keeps suppressing it across
the daily channel rollout (#14256 was the case where a per-version
fingerprint produced one new issue per build). An ACMM shortfall
fingerprint covers the class, component, version and criterion key.

Before filing, the hive searches upstream for an existing issue carrying that
fingerprint:

- **Found** → it adds a comment and a 👍 reaction instead of opening a
  duplicate. The reaction is how "N hives see this" gets counted.
- **Not found** → it opens one issue.

- **Found** → the hive adds itself the way a self-report does (#13470): the
  short `Also seen on …` comment (not a re-post of the whole body), the
  `hive/selfreport` label, and its line in the `Affected hives` block below.

For `runtime-error` reports the hive goes further and **joins first**. If no
fingerprint match exists it looks for an open issue labelled
`hive/selfreport` for the same class, then for a human-filed issue whose
title or body mentions the class keywords (for example "restart storm",
"model discovery degraded", "LiteLLM 401"). The catch-all
`unclassified_error_repeated` class says nothing about the defect, so its
class search is additionally scoped to the leading words of the message in
the title; with no usable message it does not join by class at all. When
any of those matches, the hive adds itself to that issue instead of opening
a new one:

- one comment `Also seen on **<hive>** (v<version>, <cluster>) at <time>`
  carrying a `<!-- hive:selfreport-seen hive=<slug> fp=… -->` marker and its
  diagnostics excerpt — at most once per hive per 24h;
- the `hive/selfreport` and `origin/selfreport` labels (the per-hive `hive/<slug>` label is retired, #12539; `hive/selfreport` is deprecated in favour of the `origin/selfreport` provenance label and is kept only so existing filters and the joiner's `label:hive/selfreport` search keep working);
- a bump to the `Affected hives (N)` block in the issue body, edited
  idempotently between `<!-- hive:selfreport-affected -->` markers: the
  joining hive gets (or updates) its own line naming its owner(s), and its
  repos are merged into the block's `Affected repos (N)` /
  `Affected repo owners (N)` union (below).

### Who is affected

So a maintainer can tell **who** is affected and ping them — not just which
namespace-style hive name — every hive renders on its own line of the
affected block (hivecommons/hive#12529 follow-up):

```
<!-- hive:selfreport-affected -->
**Affected hives (2):**
- hosted-aslom (instance b04ca59b7312017e) — owner(s): @alice, @bob — seen 3, last 2026-10-17T11:00:00Z
- hosted-oke-11-r05x — owner(s): (anonymous) — seen 1, last 2026-10-16T19:56:47Z
seen_count: 4
<!-- /hive:selfreport-affected -->
```

- `owner(s)` are the GitHub logins the spoke knows for its owner:
  `dashboard.authorized_users` entries holding the owner role (the first
  entry without an explicit role, plus any `:owner` entry) and the operator
  who completed the device flow (`/data/gh-user-token`). They travel in the
  report as an explicit `owners` field; non-GitHub identities (`ibmid:…`)
  and anything that is not a plausible login are dropped before they leave
  the hive. `unknown` means the hive sent none (a pre-owners spoke).
- `seen N, last <time>` are per hive; `seen_count` stays the total.
- **Privacy.** `governor.fleet_report.include_owners: false` (default
  `true`) renders `(anonymous)` in both the evidence table and the hive's
  line, sends no logins, and a later report never re-attaches them.
- When a spoke that predates the field relays through the hub, the hub
  fills the line from its own record of the hive owner (unless the spoke
  opted out). Existing issues with the older inline
  `**Affected hives (1):** <hive>` list are upgraded to the per-hive form on
  the hive's next join, so an old issue gains the owner on the next sweep.

### Which hive

So a maintainer can tell **which** hive a report came from — and open it —
without translating a hub id through the hives listing, every report
carries the hive's display name and dashboard URL
(hivecommons/hive#13211), both in the evidence table:

```
| hive | hosted-available-oke-260812-0nzc-available-ok-ba44 |
| hive name | kellya |
| spoke | [kellya](https://kellya.hive.hivecommons.dev) |
```

and on the hive's affected-block line, as `<name> (<hive id>)` plus
an `open` link cell:

```
- kellya (hosted-available-oke-260812-0nzc-available-ok-ba44) (instance 2e281aad9b0e1e15) — [open](https://kellya.hive.hivecommons.dev) — owner(s): @clubanderson — seen 1, last 2026-10-20T16:53:31Z
```

- **Where they come from.** The spoke sends its own view (`project.name`,
  and `dashboard.public_url` else the hub-delivered `hub.dashboard_url`) as
  explicit `hive_name` / `spoke_url` fields. When the report goes through
  the hub relay, the hub overlays what its hives listing shows — the
  operator-set `project_name` (renames land on the hub, not the spoke) and
  the validated vanity URL — and fills both for a spoke that predates the
  fields, falling back to the org or primary repo for the name and to the
  heartbeat-reported dashboard URL, then the placeholder host, for the
  link. A later join refreshes the line, so a renamed hive shows its new
  name on the next report.
- **Privacy.** Both fields leave the hive only when it is hub-managed — the
  same condition under which the hive id (rather than the anonymous digest)
  is named today. A hub-less hive stays anonymous: the rows render `—`, the
  affected line is unchanged, and nothing beyond the name and URL is added.
  Names are reduced to a single line without markdown structure before they
  leave; URLs must be absolute `http(s)` URLs and lose any credentials,
  query or fragment.
- **Older issues and spokes.** A report without the fields renders exactly
  as before (`| hive name | — |`, `| spoke | — |`, the pre-existing bullet
  form); the hub completes the hive's line and rows when it files or joins
  on the spoke's behalf.

### Repositories and repo owners

So a maintainer can tell whether a fleet issue is confined to one project or
one org, every report names the repositories the reporting hive
serves (hivecommons/hive#12338). The evidence table carries the hive's own
view; the affected block carries the union across every hive that has
reported, re-derived idempotently on each join:

```
<!-- hive:selfreport-affected -->
**Affected hives (2):**
- hosted-aslom (instance b04ca59b7312017e) — owner(s): @alice — seen 2, last 2026-10-17T11:00:00Z
- hosted-available-oke-11-placeholder-r05x — owner(s): @carol — seen 1, last 2026-10-16T19:56:47Z
**Affected repos (3):** acme/widgets, hivecommons/hive, hivecommons/pluk
**Affected repo owners (2):** acme, hivecommons
seen_count: 3
<!-- /hive:selfreport-affected -->
```

- `Affected repos` is the sorted union of every reporting hive's watched repos
  plus any triggering `repo`; `Affected repo owners` (the orgs/users owning
  those repos — distinct from the per-hive `owner(s)` logins above) is
  always re-derived from it, never parsed back. A hive that re-reports with nothing new leaves the body
  untouched; a known hive bringing a new repo edits only the block.
- Repo names are validated to exactly `owner/name` (lower-cased, GitHub-safe
  characters) before they leave the hive; anything else is dropped.
- **None of this enters the fingerprint.** The fingerprint stays class +
  normalised message + version, so two hives watching different repos still
  dedupe onto one issue.
- The lines are omitted entirely (and the table cells render `—`) when no
  reporting hive contributed repos, so pre-#12338 hives and opted-out hives
  produce exactly the block they always did.

**Privacy.** The report repo is public and `hive.yaml` does not record
whether a watched repo is private, so the hive cannot filter private repos
for you. `governor.fleet_report.include_repos` (default `true`; Settings →
Governor → "Self-Reported Issues" → "Name the watched repositories in
self-reports") turns the whole feature off for a hive whose `project.repos`
include private repositories: the table shows `—`, the report carries no
repos, and the hive's joins never add to the union. The hub relay forwards
the body as built, so the switch governs relayed reports too.

A new issue is opened only when nothing matches. Rate limits apply to **new**
filings only: at most `max_per_day` per hive per 24h, at least 1h between
new filings of different classes, and a fingerprint that was closed upstream
is never re-filed by the same hive while the hive keeps observing the
condition — a close by anyone (maintainer, scanner, close-on-merge) is an
instruction to that reporter to be quiet about that fingerprint. The 7-day
cooldown runs from the later of the close and the last tick that still saw
the condition, so it only expires once the condition has been absent for a
whole week; a recurrence after that is a regression and gets a fresh issue.
The hub relay applies the same rule for relayed reports (a recently closed
fingerprint answers `closed_upstream` and writes nothing). For the scanner
and maintainers this means "already fixed on v5" is a safe reason to close a
self-report even when the reporting hive's build (named in the body)
predates the fix: cite the fixing PR once and close; the reporter will not
re-file or comment again, and picks the fix up on its next build. Filing is also
deferred while the GitHub API budget is not `normal`. Hub-managed hives whose credential is refused by the report repo
(403/404) relay the report to the hub's `POST /api/selfreport/ingest` with
their heartbeat bearer, and the hub files or joins under the App identity.

The hive also remembers the body hash of what it last posted, so a condition
that persists unchanged does not re-comment every cycle. It re-reports only
when the body actually changes, when the issue is gone, or when a previously
recovered condition comes back.

When the condition clears, the hive posts a **recovery** comment
(`self-recovered: yes`). If the hive opened that issue itself, it also closes
it; if a human opened the issue, the hive comments but **leaves it open** —
it will not close someone else's issue.

`runtime-error` issues are never auto-closed, even by the hive that opened
them: when the condition clears for that hive it posts a
`cleared on <hive> at <time>` comment — at most one per 24h when the
condition flaps, and none once the issue is closed — and leaves the issue
for a human to confirm the fix.

## State

Report state lives at `/data/fleet-report-state.json`: the per-criterion epoch
history that drives the two-epoch threshold, and the open-issue records
(number, URL, whether the hive opened it, component, body hash, recovery
flag, and for a rolled-up shortfall the fingerprint of the self-report
tracker it commented on). Deleting
it resets the epoch counters and makes the hive forget which upstream issues
are already open, so a persisting condition can file again.

## Previewing before you opt in

The evaluation runs on every dashboard status refresh **regardless of the
knob** — dry-run changes only whether anything is written upstream. So you can
leave `enabled` unset (self-hosted) or `enabled: false`, run normally, and read the reports the hive would
have filed on the dashboard's fleet-report panel.

That is the intended way to adopt this: watch the dry-run output until you are
satisfied that what it would send is both accurate and something you are
willing to publish, then opt in.

## See also

- [Fleet health: the verdict and remediation hints](fleet-health.md) — the
  per-hive verdict on `/fleet`, which is about *your* hive's state rather than
  upstream reporting.
- [Fleet drift signals](fleet-drift-signals.md) — per-hive deviation badges.

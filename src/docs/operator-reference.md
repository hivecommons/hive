# Hive operator reference

This page is a concise operator reference for fields and runtime knobs that are
easy to miss in `hive.yaml.example`. It was checked against `pkg/config/config.go`
and `cmd/hive/main.go` on branch `v4`.

For the full centralized environment variable table, including hub, backup,
inference, deployment, contributor, and legacy helper-script variables, see
[Environment variable reference](env-vars.md).

## Agent Go toolchain shim

Container images install `bin/go-wrapper.sh` as `go` in the runtime Go toolchain
and keep the real compiler beside it as `go-real`. When `HIVE_AGENT` or
`HIVE_AGENT_ID` is set, the shim blocks `go test`, `go vet`, and `go tool vet`
inside the hive pod and points agents to CI instead, because those commands can
read or mutate live `/data` state and have killed agent sessions. Human
operators can debug inside the pod by setting `HIVE_ALLOW_LOCAL_GO_TEST=1`; all
other Go subcommands pass through unchanged.

## Minimum required configuration

Most of `hive.yaml.example` is optional. The smallest config the hive will start
with (enforced by `Config.Validate`) is:

- **`project.org`** — the GitHub org the hive works on. Startup crash-loops with
  `project.org is required` if missing.
- **at least one repo** under `project.repos` (bare repo name; the org is
  `project.org`).
- **GitHub credentials** — one of `github.token`, `github.app_id` (App auth), or
  `github.forge`. Missing all three fails with
  `github.token, github.app_id or github.forge is required`.
- **at least one agent** under `agents:` (a bare `name: { backend, model }` is
  enough; defaults fill in the rest — see [agent-configuration.md](agent-configuration.md)).

Everything else — `governor` cadences, `knowledge`, `notifications`,
`dashboard.auth_token`, gateways, `hub` — is optional and has working defaults.
A minimal `hive.yaml`:

```yaml
project:
  org: my-org
  repos: [my-repo]
github:
  token: ${HIVE_GITHUB_TOKEN}
  pr_detail_ttl_s: 1800
agents:
  scanner:
    backend: copilot
    model: claude-sonnet-4-6
```

## GitHub API quota — who is spending it?

Every hive exposes `GET /api/gh-rate-limits` behind dashboard auth. The payload
contains GitHub's core/search/GraphQL rate-limit windows, ETag cache counters,
and a one-hour REST top-consumer list grouped by caller, method, and normalized
endpoint. Operators can collect a hosted fleet view with:

```bash
make quota-report
# or:
bash src/scripts/gh-quota-report.sh --context hive-oke --top 3
```

For a single dashboard, use `--local <url> --token <dashboard-token>`; for saved
fixtures or CI-free formatting checks, use `--from-file <json>`. The command
prints one row per spoke plus the hub when available. The hub endpoint may lag a
spoke rollout; a 404 or other unavailable hub response is reported as
`endpoint-missing` without hiding spoke data. If none of the mounted/configured
dashboard-token candidates authenticates, the row reports `auth-required`; the
script does not forge `X-Hive-User` for localhost because the dashboard
middleware strips unproved identity headers unless the hub proxy proof is valid.

Interpret the top-consumer columns this way:

- `charged` counts REST requests that consumed core quota in the last hour.
- `not_modified` counts conditional `GET` requests answered from GitHub as
  `304 Not Modified` and replayed by Hive's ETag cache; these are useful signal
  but did not spend quota.
- `requests` is `charged + not_modified` plus any other observed responses.
- Any `rate_limited` entry, or any spoke below 10% remaining core quota, makes
  the script exit non-zero so it can be used as a smoke check.

The known hot endpoint in hosted fleets is
`/repos/{owner}/{repo}/pulls/{number}`. It should be watched first when charged
usage climbs, because repeated PR detail refreshes can dominate the one-hour
window even when list endpoints are mostly free ETag revalidations.

Hosted spokes authenticate as their GitHub App installation and normally show a
5,000 requests/hour `core.limit` (`app/token` in the report). A 60/hour limit is
anonymous and means credentials were not applied. The hub must use the App or
installation identity too; do not configure a personal access token on the hub,
because one operator's PAT would become a shared fleet bottleneck and audit
liability.

Before reference, captured 2026-10-08 from the hosted fleet:

| Metric | Value |
|---|---:|
| Total charged REST requests/hour | 12995 |
| Hub endpoint | `endpoint-missing` |
| Top fleet consumer | `hive GET /repos/{owner}/{repo}/pulls/{number} 4259/9505` |
| Second fleet consumer | `hive GET /repos/{owner}/{repo}/issues/{number}/comments 1819/3298` |
| Third fleet consumer | `hive:enrich_pr_ci GET /repos/{owner}/{repo}/pulls/{number} 1157/2325` |

## OpenTelemetry: GitHub API spend and cache freshness

Hive emits OpenTelemetry spans and metrics for every GitHub call, the governor
eval cycle, and the ForgeState cache ([#13380](https://github.com/hivecommons/hive/issues/13380),
Phase 3 of [#13373](https://github.com/hivecommons/hive/issues/13373)). Both
signals share one switch and are **off by default**; nothing is exported and
no behaviour changes until an endpoint is configured.

| Setting | Effect |
|---|---|
| `otel.enabled: true` (alias `tracing.enabled`) | Turns the pipeline on. Without an endpoint it stays a no-op. |
| `otel.endpoint` | OTLP/HTTP collector URL. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Standard OTel env var; used when `otel.endpoint` is empty. Traces and metrics go to the same collector. |
| `otel.headers`, `otel.service_name`, `otel.insecure` | Exporter headers, `service.name` resource attribute, plaintext HTTP. |

Spans:

| Span | Where | Attributes |
|---|---|---|
| `github.request` | Outermost layer of the GitHub HTTP transport chain (one per REST/GraphQL call, including ETag replays and pacer refusals) | `http.request.method`, `http.response.status_code`, `github.endpoint` (templated path, e.g. `/repos/{owner}/{repo}/pulls/{number}`; no ids), `github.repo`, `hive.caller` (REST caller tag, `unknown` when untagged), `hive.pacing_class` (`high` / `normal` / `low`), `github.cache_hit` (304 / ETag-cache served), `github.ratelimit.remaining`, `github.ratelimit.bucket` (`core`, `search`, `graphql`, …), `github.shed` (request refused by the pacer) |
| `governor.eval_cycle` | One per governor eval cycle | `hive.id`, `hive.acmm_level` |
| `governor.enumerate_actionable` | child of `eval_cycle` | items found |
| `governor.stage_labels` | child of `eval_cycle` | `hive.stage_labels.items`, `hive.stage_labels.writes` |
| `governor.kick` | child of `eval_cycle` | `hive.kicks.assembled`, `hive.kicks.review_delivered`; `agent.kick` children per dispatched agent |
| `governor.automerge` | Sibling of `eval_cycle` (the sweep runs on its own cadence) | `hive.automerge.seen`, `hive.automerge.merged`, `hive.automerge.skipped` |

GitHub calls made inside those spans attach as `github.request` children, so a
trace answers "which stage of which cycle spent the quota".

Metrics (OTLP, exported on the SDK's periodic reader):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `github.requests` | counter | `caller`, `endpoint`, `status` | Every GitHub call; `status` is the HTTP code, `shed`, or `error`. |
| `github.ratelimit.remaining` | gauge | `bucket` | Last `X-RateLimit-Remaining` seen per bucket (unauthenticated 60/hour buckets are ignored). |
| `github.secondary_limit_hits` | counter | — | 403/429 responses GitHub flagged as secondary/abuse limits. |
| `forgestate.staleness_s` | observable gauge | `repo` | Seconds since each cached repo was last reconciled or patched. |
| `forgestate.drift` | counter | `repo`, `kind` (`issue`, `pr`) | Entries the reconcile pass found differing between cache and API. |
| `webhook.event_to_patch_ms` | histogram | `repo`, `writer` (`forgestate`, `cache_invalidate`) | Latency from webhook receipt to the cache patch being applied. |

Adaptive reconcile: `forgestate.reconcile_interval_s` is the base cadence. The
store stretches it from the latest `core` bucket observation
(`forgestate.AdaptiveInterval`): below 20% remaining → 2×, below 10% → 4×,
below 5% → 8×. Webhook patches keep the cache fresh in the meantime, and the
interval snaps back once the window resets. When a hive has not yet seen a
rate-limit header the base interval is used unchanged.

## Configuration blocks

Top-level YAML keys accepted by `config.Config`:

| Block | Purpose | Notes |
|---|---|---|
| `project` | Managed GitHub org, repos, primary repo, AI author. | Required for normal operation. |
| `project.agent_issue_decision_policy` | `resolve`. | `resolve` keeps Hive-authored issues from parking at `stage/045-needs-decision` for non-risk engineering questions (sequencing, naming, numbering, config shape, scope, approach A vs B); agents record `Decision taken by hive: …` and the relay downgrades to `stage/010-actionable`. `escalate` parks every Hive-authored `--needs-decision` request. Risk parks still require `Risk: <category>`. |
| `policies` | Optional git/local policy prompt source. | Hot-reloaded when configured. |
| `agents` | Agent definitions and behavior metadata. | Per-agent overlays may also live under `data.agents_dir`. |
| `governor` | Cadences, labels, sensing, health, budgets, inference gateways, trajectory review. | See notable fields below. |
| `github` | PAT or GitHub App credentials and forge URLs. | Use one auth method. |
| `notifications` | ntfy, Slack, and Discord webhooks. | All optional; see [notifications.md](notifications.md). |
| `notifications.discord.rfc_channel_id` | empty (off). | Discord channel that receives RFC lifecycle notices (RFC issue opened, closed, or taken off hold) through `notifications.discord.bot_token`. Env override `HIVE_DISCORD_RFC_CHANNEL_ID`; see [RFC lifecycle notices](notifications.md#rfc-lifecycle-notices-discord-bot). |
| `dashboard` | Web UI port, snapshots, auth token, frame allowlist, authorized users. | `auth_token` can come from `HIVE_DASHBOARD_TOKEN`. |
| `agent_sandbox` | Podman-rootless sandbox launcher for hub/pod agents (`pkg/sandbox`). | Opt-in and **two-gate**: this block's own `enabled: true` sandboxes nothing by itself — each agent also needs `sandbox.enabled: true` under `agents.<name>`. The dashboard's Security tab writes only this global flag, so enabling it there alone can leave every agent unconfined — but the tab now shows this: `security.sandboxWarnings` in `GET /api/config/governor` carries `config.AgentSandboxGateWarnings`'s diagnosis (also logged at WARN at boot/reload), rendered both in the page's coherence-warnings box and inline under the toggle, naming the still-unconfined agents and the fixing key (#4918). See [sandbox-isolation.md](sandbox-isolation.md) and the [getting-started confinement section](getting-started.md#where-agents-actually-run-read-this-before-l3). |
| `data` | Metrics, logs, session, and agent overlay directories. | Defaults are `/data/...` in containers. |
| `knowledge` | Wiki layers, vaults, git/document sources, connectors (`knowledge.connectors`, see [knowledge-connectors.md](knowledge-connectors.md)), primer, curator, bead synthesizer. | Disabled unless `enabled: true`. |
| `hub` | Hub/spoke hosted-hive metadata. | Usually provisioner-owned. |
| `hive_id` | Stable spoke identifier. | Usually provisioner-owned. |
| `acmm_level` | Current ACMM pack level. | May be set by hub/dashboard. |
| `quality` | Optional quality-lane capabilities. | `formal: true` enables agent-authored Spin models only at ACMM L5/L6; see [Formal verification](formal-verification.md). |
| `runs` | Long-running run settings: `runs.external.flue`, the report-only external-execution binding pilot, and `runs.external.omp`, the OMP workbench host behind the same adapter. | Both default OFF and independent. `enabled: true` with no `mode` runs in `shadow` (persists admissions, observes, never starts external work); `mode: report-only` dispatches. Flue dispatches to a runtime at `endpoint` pinned to `workflow_version` and needs a build with the `extwork_flue` tag; OMP offers work over the contributor relay to a workbench peer declaring `ext-exec/omp` whose declared version matches `workflow_version`, and needs the `extwork_omp` tag. Neither host receives a repository credential, dashboard token, or publication tool; OMP (tier T3) is additionally refused any write-capable stage. See [External workflow admission](design/external-workflow-admission.md). |
| `auto_merge` | The App self-merge sweep: whether and how the Forge App merges its **own** CI-green PRs. | Default ON, but inert below `acmm_level: 6`. See [App self-merge sweep](#app-self-merge-sweep-auto_merge) below. |
| `issues.close_on_merge` | `true`. | When a PR merges that was authored by Hive or a trusted author, or that Hive itself merged (the merge actor is the configured Hive identity, resolved from `merged_by` or a PR detail lookup when cached metadata omits it; [#15148](https://github.com/hivecommons/hive/issues/15148)), and closing keywords or Hive claim metadata show it fixed an open issue, Hive comments `Fixed by #<pr> (merged <sha>). Closing. Reply /reopen if the problem persists.` and closes the issue as `completed`. Issues labelled `needs-reporter-confirmation`, `epic`, or `needs-human` stay open: Hive comments that the fix merged, adds `hive/likely-done`, and waits for `/fixed` or more instructions. Both comments carry `<!-- hive-close-on-merge -->`, Hive's already-handled marker: an issue with that comment is never closed on merge again, so `/reopen` after a Hive close sticks. An issue reopened after the PR merged stays open even when GitHub's closing keyword, not Hive, closed it: before closing, Hive reads the issue timeline since that PR's merge, and a `reopened` event by anyone but Hive (including `github-actions[bot]` from `/reopen`) skips the close and posts one comment carrying `<!-- hive-reopened-after-merge pr=<pr> -->`, so later sweeps skip that PR without rereading the timeline. The marker is per PR: a different PR that merges after the reopen can still close the issue. If the timeline cannot be read, Hive leaves the issue open and retries on the next sweep. |
| Issue↔PR link safeguard | Always on with issue stage and claim-label sweeps. | GitHub shows its issue-list PR icon only when a default-branch PR body contains a closing keyword. When Hive sees a Hive-authored default-branch PR covering an issue and the PR body does not say the issue stays open, Hive appends `Fixes #N` to that PR body. Existing Hive-authored default-branch PRs that used `Refs #N` but are recorded by Hive stage/claim metadata as the implementation PR are retrofitted once with the same closer and `<!-- hive:issue-link-fixed -->`; human-authored PR bodies are never edited. If the body says `Part of #N` or otherwise leaves the issue open, Hive does not add a closer; it posts one stage-entry comment on the issue with the PR link and explains that GitHub will not show the icon without a closing keyword. Non-default-branch PRs get the same issue comment. |
| `issues.close_on_merge_backfill_interval` | `1h`. | How often Hive scans open issues carrying Hive PR-linkage signals (`stage/040-covered-by-pr` or the retired `🐝 covered by PR`, `hive/likely-done`, claim/covered comments, or timeline PR references) and applies the same close-on-merge policy to PRs that merged before the current process saw them. |
| `issues.close_on_merge_interval` | `10m`. | How often the close-on-merge **ledger sweep** walks every merged-PR claim in the duplicate-PR claim ledger. Passes in between (every governor eval cycle and manual rescan) only look at claims whose PR merged since the last full walk, so a fresh merge still closes its issue on the next pass without re-reading the whole ledger ([#11778](https://github.com/hivecommons/hive/issues/11778)). Both the ledger sweep and the backfill pause while the GitHub API budget mode is not `normal`; Hive logs one line on each pause and resume. Env override: `HIVE_CLOSE_ON_MERGE_INTERVAL` (Go duration). |
| `dedupe.advisory_autoclose` | `true`. | The close-on-merge backfill works the dedupe agent's advisory checklists itself ([#15754](https://github.com/hivecommons/hive/issues/15754)): for every open hive-authored issue whose body opens with `<!-- hive:dedupe-checklist repo=… -->` it ticks each `- [ ] #N …` task line whose #N is closed or merged (one body edit per sweep, idempotent) and, once every line is ticked, comments `All N items resolved (#a closed, #b merged …) — closing this advisory.` and closes the issue as `completed`. A checklist with open items expires after seven days. The Activity Monitor never parks an advisory at 045 regardless. `false` leaves checklists untouched (they still expire). Env/dashboard: yaml-only. |
| `issues.close_on_merge_recheck` | `24h`. | How long a merged-PR claim keeps its settled close-on-merge verdict before the ledger sweep re-verifies it against the API. Once a sweep reaches a final verdict for a claim — issue closed, already closed, Hive marker present, awaiting confirmation, untrusted author, reopened by a person, or PR not merged — it stamps `close_on_merge_settled_at` on the claim and later sweeps skip the claim with no API call until this window elapses. Transient failures (issue, timeline or comment reads/writes that errored) never settle and retry on the next sweep as before. The marker follows the claim while the same PR claims the issue and resets when a different PR takes it over. Env override: `HIVE_CLOSE_ON_MERGE_RECHECK` (Go duration). |
| `issues.reporter_confirmation` | `false`. | Legacy opt-in switch for hives that want every human-filed bug-family issue to wait for reporter verification before close. Leave false for the default close-on-merge policy; add `hive: needs-confirmation` to an individual issue when only that issue needs the gate. `hive: reporter-confirmed` and `hive: close-on-merge` remain accepted legacy bypasses. |
| `issues.sha_hold_trusted_label` | `needs-sha`. | Soft label the SHA-hold sweep applies to a `kind/bug` without a commit SHA when the reporter's author association is in `project.issue_filter.reporter_trust.trusted_associations` (default OWNER/MEMBER/COLLABORATOR), instead of the hard `hold` (#12002). The notice is the same; the issue stays actionable. A SHA-hold `hold` already on such an issue is swapped for this label. Set to `hold` to restore the hard hold for every reporter. |
| `issues.lift_hive_hard_suppress` | `true`. | Runs the hard-suppress reconcile (#12002): a `hold` or `blocked` label on an open issue is lifted when Hive provably applied it (newest label event is a `labeled` by the App bot; for `hold`, paired with the `<!-- hive:sha-hold -->` notice) and a PR cross-referencing the issue merged after it. Hive posts one note carrying `<!-- hive:hard-suppress-lifted -->`. Human-applied labels are never lifted. Skipped with the other unstick steps when the API budget is critical. |
| `issues.hard_suppress_reconcile_interval` | `30m`. | Minimum time between hard-suppress reconcile passes. |
| `issues.rfc_existing_check.enabled` | `false`. | Triage existing-coverage check (#13575): new open issues labelled `rfc`, `enhancement` or `kind/feature` get one comment that lists the existing settings that may already cover the request. Each setting links to Settings with the search set to its key. The comment carries `<!-- hive:rfc-existing-coverage -->` and is never posted twice. The check never labels or closes issues. Tuning keys are `post_when_nothing_found`, `labels`, `max_matches` and `max_age`; see [Triage: does this RFC already exist?](settings.md#triage-does-this-rfc-already-exist). |
| `runs.triage.prior_art.enabled` | `true`. | Prior-art gate (#13575): before any agent claims a feature / RFC / epic issue, the scheduler searches knowledge beads, the Settings index, merged PRs (12 months), closed issues, docs and code for the issue's key terms. A human-filed issue whose best finding reaches `runs.triage.prior_art.threshold` (default `70`) is parked at `stage/017-possible-existing` with one `<!-- hive:prior-art-gate -->` comment quoting up to `max_findings` (default `5`) findings and the two replies (`/hive decision proceed — <gap>` / `/hive decision exists`); a clanker-filed one is parked the same way and closed `already-exists` after `bot_close_ttl` (default `72h`) with no reply; a hive-filed one is never parked (findings note only). `kinds` (default `feature, rfc, epic, enhancement`) picks the gated kinds; `max_parks_per_cycle` (default `5`) bounds one cycle. See [Prior-art gate](spektacular.md#prior-art-gate). |
| `contribute.help_links` | Defaults to Hive contributor docs, the Hive issue tracker, and the Twitch live stream. | Up to 5 `{label, url}` links shown on `/contribute` Onboarding, Operations, and printed once by relays on connect. URLs must be absolute `http://` or `https://`; labels are plain text and length-capped. Prefer the Hive Commons Discord redirect (`https://hivecommons.dev/discord`) over channel URLs for contributors who may not already be server members. |
| `contribute.max_parallel_fixes` | `2`. | Ceiling on how many stage-072 `fix-pr` sub-sessions one contributor's relay may run beside its primary session ([#12436](https://github.com/hivecommons/hive/issues/12436)). The relay advertises its own count (`max_parallel_fixes`, from `HIVE_MAX_PARALLEL_FIXES`); the hub clamps it to this. A relay that advertises no `parallel_sessions` always has capacity 1 (preempt mode). Fixes beyond capacity queue server-side and are served before new work. Settings → Hub → Fix budget. |
| `contribute.min_focus_window` | `30m`. | Preempt mode only: after a preempted primary task resumes, the hub does not preempt that contributor again until it has run uninterrupted for this long. Settings → Hub → Fix budget. |
| `variables` | Trusted variable resolver definitions. | Env-only substitution works without this block. |
| `classification` | Go-consumed subset of `hive-project.yaml`'s `classification:` block — today only `review_bots`, the external review-bot logins whose inline threads on hive-authored PRs the hive addresses and resolves itself. | Off unless `review_bots.logins` names a bot. The same key in `hive-project.yaml` is read when `hive.yaml` has none. See [review-bot-threads.md](review-bot-threads.md). |
| `claims` | The worker-claim ledger ([#8380](https://github.com/hivecommons/hive/issues/8380)): who is on an issue right now, ranked human > agent > contributor > external, so a person can take an issue over from a hive agent or a relay contributor and the displaced holder is told to stop. | Default ON. `enabled: false` turns recording, enforcement and `/api/claims` off. `human_ttl_s` 4h, `agent_ttl_s` 2h, `contributor_ttl_s` 30m, `max_ttl_s` 24h; the owner can also set these four from **Governor Config → Features → Work intake & issue lifecycle → Issue claims** (a value of 0 or *Reset to default* uses the default; human, agent and contributor may not exceed the max), and a saved change applies without a restart to claims made or renewed afterwards while existing claims keep their expiry ([#10981](https://github.com/hivecommons/hive/issues/10981)); `comment: false` / `label: false` suppress the GitHub issue comment / `stage/020-claimed` label mirror (`label: false` also suppresses the `stage/030-pr-in-progress` start label an agent claim mirrors — formerly `claimed` / `hive/in-progress`, [#11841](https://github.com/hivecommons/hive/issues/11841)). A governor kick only *lists* the issues it names (30-minute hold, no comment, not a claim); the agent claim, with its comment, is recorded on the agent's first start signal on an issue — its own `hive-open-issue claim` request, a `hive-open-pr` request naming it, or a comment with an explicit start statement (`<!-- hive:start -->`, "starting now", an opening "Starting …" / "Implementing …"; see the [recognized forms](hive-open-issue.md#comment)); a report-only comment (triage, verification, gate check, status), an explicit deferral ([#12387](https://github.com/hivecommons/hive/issues/12387)) or escalation, and any `label` request record no claim. `never_claim_labels` (default `[epic]`, case-insensitive; `[]` turns it off) names umbrella/tracking labels no automated producer may claim and no claim ever stages `stage/020`/`stage/030`; the same admission gate also refuses issues parked on a person (`stage/045-needs-decision`, `needs-spec`, …), `needs-human` and held issues. `escalate_after_claims` (default `2`, negative turns it off) counts those claims only: once one agent's last that-many claims on a free issue were followed by no linked PR, referencing commit, label or assignee change and no close/reopen, the issue is withheld from every kick, labelled `needs-human` and given one comment listing the claims, instead of being claimed again. A `hive/likely-done` issue an agent already started on after the merged PR (its verification) is withheld from kicks until a person comments, labels, assigns, closes or reopens it, so it is verified once rather than every cycle ([#10527](https://github.com/hivecommons/hive/issues/10527)). `reclaim_cooldown_s` (default `21600` = 6h; negative turns it off) keeps a relay contributor session off an issue it released without shipping — task failed, abandoned, or its lease revoked — while other contributors may retry at once after the short global failure cooldown; before it, a relay whose environment could not run a task took the same issue back every ten minutes, flapping its claim ([#13093](https://github.com/hivecommons/hive/issues/13093)). A relay's *disconnect* release (after the [#7838](https://github.com/hivecommons/hive/issues/7838) grace window), a revoked lease and a lease that aged out unresumed each release the issue claim on GitHub too — release marker posted, claim labels removed — so hive agents may claim the issue on the next cycle; `contributor_flap_threshold` (default `3`; negative turns it off) disconnect releases of one issue inside `contributor_flap_window_s` (default `7200` = 2h) put the issue in *contributor cool-off* for `contributor_flap_cooloff_s` (default `21600` = 6h): withheld from every contributor, immediately eligible for hive agents, work detail `contributor cool-off: N drops in W`; and `contributor_starvation_s` (default `14400` = 4h; negative turns it off) is how long contributor claims may keep hive agents off an issue with no PR before the claim becomes advisory and a hive agent may claim it anyway ([#13579](https://github.com/hivecommons/hive/issues/13579)). An issue labelled `blocked` (the review backlog's parking for a finding whose file exists only on an unmerged PR, or `hive-open-issue comment --blocked-on-pr`) is refused by the same admission gate until the tracked PR merges or closes. Driven from `hivectl claim` — see [hivectl.md](hivectl.md#claim--unclaim--claims--issue-claims). |
| `removed_agents` | Persistent tombstones for deleted agents. | Dashboard/overlay-owned; do not seed casually. |
| `good_first_issue` | Hive-curated `good first issue` advertising: a 15-minute sweep scores open, unclaimed issues and gives the few that qualify GitHub's `good first issue` + `help wanted` labels and one welcome comment pointing at this hive's contributor relay. Not a hold — a hive agent starting the issue removes the label. | Default ON; `enabled: false` opts out. Silent until `dashboard.public_url` (or a hub-assigned dashboard URL) exists. See [Hive-curated good first issues](good-first-issue.md). |
| `decisions.delegate` | "Let hive decide": the activity monitor's Decide… › *Let hive decide* action has the hive decide an issue or PR parked at *Needs human decision* itself, as the owner's delegate under the owner-delegate policy, recording it as a human decision would be (`/hive decision`, park lifted, queue approval on a PR) with an audit marker. | Default ON (`enabled: true`); `enabled: false` hides the action hive-wide; `repos.<owner/repo>.enabled` overrides per repository. `llm.enabled` (default OFF) consults the owner-delegate decider model for items the rules leave at the reversible default, on the governor reviewer endpoint; `llm.model` overrides the model. `effect_timeout` (default `30m`) is how long a recorded decision may go without its expected effect before a repeat click answers "decided, but it hasn't taken effect" with the plain reason and the next actions, and the hive posts one follow-up comment. Rails and policy in [Dashboard sections → Let hive decide](dashboard-sections.md#let-hive-decide). |
| `chat.grounded_answers` | Hive Chat grounded answers: a chat message that is not a bare command (`/agents stuck`, `/prs waiting on review`, "which agent completed last?") is answered by one read-only model turn over a capped snapshot of the hive state the dashboard already holds (agents, open PRs, actionable issues, decisions, governor, alerts, version), citing the items used or saying it does not have the data. Chat never takes actions on this path; every attempt is audited as `chat_grounded_answer`. | Default ON (`enabled: true`); `enabled: false` makes scoped questions answer with the command's usage instead. `model` overrides the governor reviewer model for the turn. See [Hive Chat](dashboard.md#hive-chat). |
| `governor.first_timer_welcome` | First-time contributor welcome: when a human whose GitHub `author_association` is `FIRST_TIME_CONTRIBUTOR`/`FIRST_TIMER` opens a PR, post one comment (per author per repo, marker `<!-- hive:first-timer-welcome -->`) that thanks them and invites them to run their own hive — profile, leaderboard, trust-tier progression, offline review-fix/rebase handling. Bots, `author/clanker`/`author/hive` PRs, drafts and known relay contributors are skipped. | Default ON; `enabled: false` opts out; `template_path` overrides the text. Silent without a public dashboard URL. See [Welcoming first-time contributors](good-first-issue.md#welcoming-first-time-contributors). |
| `sla` | Delivery SLA targets (`sla.targets.start_to_pr_h`, `pr_to_merge_h`, `open_to_merge_h`, hours; defaults 4 / 24 / 72) the dashboard's Throughput section colours its Delivery SLA spans against. | Metrics are derived from the lifecycle timeline; no extra GitHub calls. Also editable from **Settings → Thresholds**. See [Delivery SLA](#delivery-sla). |
| `sentinel` | Suspicious-activity alerts: flags open PRs (any author) that touch sensitive files or look like security overrides, privilege escalation, or codebase damage. | Default ON; observe-and-alert only. See [Suspicious-activity alerts](#suspicious-activity-alerts-sentinel). |

### Agent-runtime blocks (`config.AgentRuntimeConfig`)

The keys the agent runtime owns — the part of Hive being carved out as **Crux** ([#13536](https://github.com/hivecommons/hive/issues/13536)) — are grouped in Go into two structs that `config.Config` and `config.GovernorConfig` embed with `yaml:",inline"`. The grouping is a code-organisation boundary only: every key keeps the position it has always had in `hive.yaml`, so no file needs to change and a `Save()` writes the same shape back.

| Struct | Embedded in | YAML keys (unchanged position) |
|---|---|---|
| `AgentRuntimeConfig` (`pkg/config/agent_config.go`) | `Config` | `agents` (including the `agents.github_api_hourly_cap` scalar and the `agents.session_recycle` block), `backends`, `providers`, `tool_approval`, `agent_sandbox`, `turn` |
| `AgentRuntimeGovernorConfig` (`pkg/config/agent_config.go`) | `GovernorConfig` | `governor.watchdog`, `governor.litellm`, `governor.vllm`, `governor.llm-d`, `governor.bob`, `governor.trajectory`, `governor.kick_limits`, `governor.gateways`, `governor.rotation`, `governor.provider_budget` |

Go callers keep reading `cfg.Agents`, `cfg.Governor.Gateways` and so on through field promotion; only composite literals name the embedded struct (`config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: …}}`). `pkg/config/testdata/agent_runtime_golden.{yaml,json}` pins the parse of a representative file so the boundary cannot drift the schema.

## Notable fields

| Field | Default / behavior | Operator note |
|---|---|---|
| `github.pr_detail_ttl_s` | `1800` seconds. `HIVE_GITHUB_PR_DETAIL_TTL` overrides for tests/experiments. | Reuses `GET /pulls/{number}` detail responses while the cheap PR list still reports the same `head.sha` and `updated_at`, and GitHub has resolved `mergeable_state`. |
| `github.graphql_pr_batch` | `true` | Uses one paginated GraphQL query per repository scan to populate the PR detail cache and CI rollup cache, replacing most per-PR `GET /pulls/{number}` and `check-runs` reads. Set `false` to return to the REST-only scan path. |
| `github.graphql_pr_batch_page_size` | `50` | Page size for the GraphQL `pullRequests(first:)` batch. Values above GitHub's `100` maximum are rejected. |
| `governor.logging.kick_log_source` | `scrollback`. Also `transcript` or `auto`; unknown values mean `scrollback`. `HIVE_KICK_LOG_SOURCE` (when set to a valid value) wins over this key. | What per-kick logs (archives and the live full log) are built from: the tmux scrollback, the backend CLI's own transcript (Copilot CLI, Claude Code), or the transcript when a reader exists else the scrollback. Editable under **Settings → Logging → Kick log source**; applies to the next kick without a restart, and each log's header `source:` line (and the kick history page) shows which was used. See [agent-logging.md](agent-logging.md#per-kick-log-history). |
| `governor.labels.automerge` | Defaults to `lgtm`. | Label applied when a merger/owner queues a PR for Hive auto-merge-on-green. With Prow's commit binding (prow-github-actions v3), Hive binds `lgtm` by also setting the `prow/lgtm` commit status (needs the App's `statuses: write` permission); alternatively choose a distinct label such as `hive/auto-merge`. Distinct from the [App self-merge sweep](#app-self-merge-sweep-auto_merge), which needs no label and no human queuer. |
| `project.repo_policies[].auto_merge` | effective `false` below L6; unset = `true` at L6 | Per-repo off switch. Switching to L6 turns this on for every active repo; owners can toggle repos afterward. `false` lets Hive open PRs for that repo but blocks all Hive merge paths (`hive-merge`, App self-authored sweep, and proxy-visible direct REST/GraphQL merge attempts). The dashboard repo-card switch persists this key and takes effect without restart. |
| `project.repo_policies[].label_driven` | Off (unset) for every repo. | Opt-in for repos whose maintainers accept and park issues with labels and their own bots (for example `needs-triage` → `triage/accepted`). On such a repo the un-park sweep posts no "What to reply" notice and never removes `needs-human`, `needs-decision` or `needs-direction` on a `/hive` comment alone; instead a trusted triager applying an approval label releases the issue and counts as human sign-off — see [Label-driven approval](#label-driven-approval). Editable from the dashboard: Settings → Repos → per-repo "Label-driven approvals (Prow-style)" (write or owner role). Details in [maintainer-commands.md](maintainer-commands.md#label-driven-repositories). |
| `project.repo_policies[].label_driven_approval_labels` | Unset: inherits `project.label_driven_approval_labels`. | Per-repo approval-label list; only read when `label_driven` is set. Editable from the dashboard sub-row of the repo's label-driven toggle. |
| `project.repo_policies[].companion_repos` | Unset (no companions). | List of sibling repositories (`name` in the project org, or `org/name`) that this repo's tooling expects checked out beside it under the agent workspace root — for example a docs/website repo a spec tool registers at `<workspace>/<org>/<sibling>/…`. Contributor task prompts and resident-agent kicks carry a shallow-clone plan for each companion at `<workspace root>/<org>/<sibling>` (`$HIVE_WORKSPACE_DIR` for contributor agents, `$HOME/workspace` for resident agents), and agents are told that a tool reporting one of those paths missing means *clone it*, never park the issue. Self-references and malformed entries are dropped. See [running-at-level-6.md](running-at-level-6.md#needless-parks). |
| `project.label_driven_approval_labels` | `["triage/accepted"]` | Labels whose trusted addition approves an issue on every label-driven repo without its own list. Ignored by repos without `label_driven: true`. Dashboard: Settings → Repos → "Default approval labels". |
| `project.label_driven_trust_org_members` | `true` | Whether a public member of the repo's owning org who applies an approval label is trusted (Prow's own rule). Dashboard: Settings → Repos → "trust public org members". |
| `project.label_driven_trusted_bots` | `["kubestellar-prow[bot]"]` | Bot logins whose label additions count as a trusted triager (Prow command bots relaying `/triage accepted`). `github-actions[bot]` is not in the default because it is the shared identity of every workflow in the repo, including ones triggered by untrusted events; operators whose Prow emulation runs as `github-actions[bot]` must add it explicitly per deployment. Dashboard: Settings → Repos → "Trusted bots". |
| `project.label_driven_supersede_park_actors` | `[]` (off) | Automation logins (for example a lifecycle bot running as `github-actions[bot]`) whose park labels a later trusted acceptance supersedes on label-driven repos: Hive removes the label, says so in the release comment, and releases the issue ([#13605](https://github.com/hivecommons/hive/issues/13605)). Park labels added by people, by unlisted bots, or by a listed bot after the acceptance still block; `hold` is never superseded. Per-repo override: `project.repo_policies[].label_driven_supersede_park_actors`. |
| `auto_merge.allow_unprotected_base` | Deprecated no-op. | Accepted so older configs keep loading. `hive-merge` now merges into any protected or unprotected branch the App can write, subject to the CI-evidence gate and GitHub's own merge rules. |
| `auto_merge.no_ci_ok` | Empty by default. | Explicit repo list whose zero-CI merge-request verdict may pass; failing or pending CI evidence is still enforced. |
| `auto_merge.human_merge_paths` | Unset (off). | Per-repo map of `owner/repo` to glob patterns (same syntax as `intent.guardrail_path_patterns`) for paths a person must merge. Applies to every merge lane (App self-merge, trusted-author, label-queued, merge-request relay) whatever the intent tier; a matching PR is held with one comment, and an incomplete changed-file list fails closed. Details in the sweep key table below. |
| `auto_merge.human_merge_title_prefixes` | `["⚠️", "⚠"]` | Title prefixes (after trimming) that mark a PR as a breaking change a person must merge. Every auto-merge lane and the merge train skip the PR; the tracker parks it at 090 *Ready for human merge · human merge*. Set to `[]` to disable. |
| `auto_merge.human_merge_labels` | `["needs-owner-merge"]` | Labels (case-insensitive) that mark a PR a person must merge; same effect as the title prefixes. Set to `[]` to disable. |
| `forgestate.enabled` | `false` | Runs the per-repo ForgeState cache ([#13373](https://github.com/hivecommons/hive/issues/13373)) and switches the automerge sweep lanes (self-authored, trusted-author, contributor lane) to it: no `/pulls` list per 10 s tick, passes triggered by `check_suite`/`check_run`/`status`/`pull_request_review`/`pull_request` webhooks for the affected PR plus a 5-minute floor pass, and a 60 s freshness-bounded read-through before every merge; a stale snapshot never merges ([#13377](https://github.com/hivecommons/hive/issues/13377)). `forgestate.reconcile_interval_s` (900), `freshness_bound_s` (60), `drift_threshold` (0.05) and `search_budget` (20) tune the cache. |
| `evidence.enabled` | On by default. | The review relay writes one [review evidence bundle](review-evidence.md) per PR head to `/data/evidence/<owner>/<repo>/<number>/<head>.json` when it records a verdict or posts a review; the sentinel sweep adds its findings to the flagged head's bundle; and every merge path adds the head's CI check-run summary, the human approvals and label changes, and the merge event. Owners and mergers read bundles through `GET /api/review/evidence` and `GET /api/review/evidence/list` (see the [API reference](api-reference.md)). Set `false` to stop writing new bundles; existing ones are left in place. |
| `evidence.signing_key_file` | Unset (bundles unsigned). | Path to an Ed25519 private key (32-byte seed or 64-byte key, hex or base64). When set and readable, bundles are signed. Dashboard/API readers require a valid signature under its public half and fail closed when the configured key is unavailable; unsigned mode verifies only the bundle content hash. Writers still record `"signed": false` when the key is empty, absent or unusable and log a warning for an unusable key, so an evidence write failure never blocks review or merge work. Keep the key off shared hosts. |
| `evidence.complexity.enabled` | On by default (when `evidence.enabled` is on). | Once a month, for each managed repo, the hive downloads the default-branch tarball through the GitHub App and writes one Go cyclomatic-complexity snapshot to `/data/evidence/<owner>/<repo>/complexity/<YYYY-MM>.json` (see [Monthly complexity snapshot](review-evidence.md#monthly-complexity-snapshot-report-only)). Report-only: it never affects merge eligibility. Cheap by design — one archive download and an AST pass per repo per month, no module downloads and no compile. An existing month is never rewritten; the hive re-checks hourly and retries a failed repo on the next check. The last 24 months are kept per repo. Set `false` to stop writing snapshots; existing ones stay. |
| `evidence.complexity.static_analysis` | `false`. | Also run `go vet` and (when installed) `staticcheck` on the snapshot checkout and record finding counts per tool. Off by default because it compiles each repo and downloads its modules, which is not cheap on a spoke with many or large repos. |
| `review.all_authors` | Deprecated no-op; accepted and logged once. | Every open PR is reviewed regardless of author — hive agent, human, clanker, dependabot or any other bot — whenever review is on (`require_approval` and `fan_out`); there is no author-class opt-out below that switch ([#12974](https://github.com/hivecommons/hive/issues/12974)). The knob used to be required for human PRs. It never lets an agent push to those PRs (see the next row). Remove it from `hive.yaml`. |
| `review.fix_human_prs` | Retained for compatibility; treated as push-disabled. | No longer lets review-fix kicks push commits onto PRs whose login author is not a hive agent/App, even when the PR is hive-attributed ([#13124](https://github.com/hivecommons/hive/issues/13124)); the one exception is a PR the stage reconciler adopted in place (`stage/074-adopted-by-hive`). A `changes_requested` verdict on such a PR stops at the published review, with the proposed fix as a suggestion or patch block in the comment, and the refusal is audited as `review_fix_withheld` with the setting's actual value. When the setting is on, the review-thread reconciler still answers review-bot threads on those PRs, by suggestion only, never by checkout or push ([#12897](https://github.com/hivecommons/hive/issues/12897)). Existing configs may still store `true` for UI/audit continuity, but DCO squash attribution means Hive cannot add a compliant follow-up commit without forging the PR author's sign-off. See [review-swarm.md](review-swarm.md#who-may-be-pushed-to-all_authors-and-the-retired-fix_human_prs-push). |
| `review.severity.block_at` | Unset; owner-only. | Lowest finding priority that blocks merge: `P1` (ship fast, P0–P1 block), `P2` (strict, P0–P2 block) or `P3` (everything blocks). Also drives `classification.review_bots.min_priority` when that key is not set in `hive.yaml` or `hive-project.yaml`. Any other value fails load naming the field. Features -> Review gate -> Severity & backlog. See [review-swarm.md](review-swarm.md#blocking-line-reviewseverity-reviewbacklog). |
| `review.severity.comment_below` / `backlog_below` | `true` / `true`; owner-only. | Whether findings below the blocking line are posted as non-blocking comments and filed to `review.backlog`. |
| `review.backlog.destination` | `github_issue`; owner-only. | `github_issue`, `github_project`, `linear` or `jira`. The last three need the matching `governor.work_source.type` (`github_projects`, `linear`, `jira`); on a mismatch the hive falls back to `github_issue`. Destination fields: `project_column_id` (required for `github_project`), `linear_state`, `jira_status`. |
| `review.backlog.labels` / `max_per_pr_per_day` | `[agent/reviewer]` / `10`; owner-only. `from-review`, `origin/review`, and `origin/reviewer` are accepted and mapped to `agent/reviewer`. | Labels on every backlog item and the per-PR-per-day cap (negative fails load; 0 = default). Reviewer is an agent lane, so `agent/reviewer` is the routing/ownership/provenance label. Non-lane hive issue filers continue to use `origin/selfreport`, `origin/governor`, `origin/dashboard`, `origin/forge`, or `origin/convergence`; the one-time startup migration (`/data/origin-label-migration-v4.done`) removes redundant lane-origin labels and backfills only non-lane origins. |
| `review.reviewers.autoscale.*` | `enabled: false`; when on: `min: 1`, `max: 4` (≤ the replicas cap 5), `target_backlog_per_reviewer: 2`, `scale_up_cycles: 2`, `scale_down_cycles: 5`, `cooldown: 5m`, `cpu_per_reviewer: 1.0`, `mem_per_reviewer_mb: 2048`, `agent` unset (first `role: reviewer` agent, else `reviewer_agents[0]`); owner-only. | Sizes the reviewer lane to its backlog ([#14685](https://github.com/hivecommons/hive/issues/14685)): adds a `reviewer-N` replica after `scale_up_cycles` cycles with more PRs waiting for a free reviewer than `target_backlog_per_reviewer` per reviewer (or a wait older than the review-pending stage dwell), drains the top replica after `scale_down_cycles` idle cycles and retires it only when its turn ends. Scale-ups are refused by named guards (`at-max`, `cooldown`, `saturated` kicks, `provider-budget`, `provider-headroom`, `max-parallel-reviews`, cgroup `cpu` / `memory`). Decisions log as `reviewer autoscale: 2→3 reason=…`; state is in `/api/status.reviewerAutoscale`. See [review-swarm.md](review-swarm.md#reviewer-autoscale-reviewreviewersautoscale). |
| `review.contributor_prs.base_sync` | Off by default; owner-only. | Lets PR follow-up use GitHub's update-branch API on hive-authored lane PRs whose head is in a contributor fork, whose `mergeable_state` is `behind`/`dirty`, and whose author enabled "allow edits by maintainers". `behind` PRs get a GitHub-authored merge commit; `dirty`/422 falls back to the `hive-base-moved` repair note and kick. This requires an agent/proxy mode with `ISSUES_PRS_MERGE` because `PUT /pulls/{n}/update-branch` is merge-scoped. It does not rewrite history and does not add a human/bot-authored DCO commit; the repository's "Sign-off survives the squash" check examines the PR's own commits, not GitHub's update-branch merge commit. |
| `governor.advisory.post_retry_backoff_s` | `3600` seconds. `HIVE_ADVISORY_POST_RETRY_BACKOFF_S` overrides the default when the key is unset; a negative value disables the pause. | How long the advisory digest write is paused after GitHub refuses it with `403 Resource not accessible by integration`. That refusal is structural (foreign-authored comment, repo dropped from the installation, or repo transferred to another owner), so per-cycle retries only burn API quota. The failed edit first falls back to creating a fresh App-authored comment; if the create is refused too, the error is recorded for the hub health row — naming the new `owner/repo` when the repository has moved — and attempts pause for this window. See [advisory.md](advisory.md#when-the-write-is-refused). |
| `governor.merge_train_stall_warn_s` | `3600` seconds; `0` disables | How long the head of a merge train may sit in one stage before the Activity Monitor reads the train **stalled** — the queued chip turns to "Merge queue · stalled — waiting on #15253 (waiting for CI) · 3h", the Merge queue popover marks the train, and the activity log records one `merge train stalled` line per train per hour naming the head, its state and how long. A soaking head is on its clock, not stalled, until that clock itself overruns the window ([#15395](https://github.com/hivecommons/hive/issues/15395)). |
| `governor.trajectory.enabled` | Defaults to enabled. | The lane no-ops until a reviewer endpoint and model resolve from `governor.trajectory` or `governor.litellm`. |
| `governor.kick_limits.max_issues` / `max_prs` | `100` / `50` | Caps on every issue list and every PR list (actionable, stale drafts, merge-eligible, CI-failing) rendered into a kick prompt; a cut list ends with an explicit "… and N more" line. An absent key or a negative value means the default; an explicit `0` opts out of the cap entirely (`KickListUnlimited` — every item is listed), and anything above `500` is pinned to `500`. Editable without touching this file under **Settings → Repos → Kick prompt list caps**. Truncation drops the *newest, lowest-priority* items: issues are listed oldest first, and PRs in review-priority order (fixes, then refactors/docs, then tests, oldest first within each class), so lowering a cap never hides the most urgent work. Raise `max_prs` only if your agents actually work more than 50 PRs per turn; the tail of a long list costs tokens and delivery time on every kick without changing behaviour (#7368). |
| `project.agents_md_max_bytes` | `8192`; values under `2048` are raised to `2048`. | Caps the repo `AGENTS.md` block prepended to a kick. Whole sections (split at `#`/`##` headings; each resolved skill is its own unit) are kept or dropped, never clipped mid-rule; when any are dropped the kick carries a one-line `⚠️ AGENTS.md exceeded N bytes; … — read AGENTS.md …` warning naming them and the scheduler logs it. See [agents-md.md](agents-md.md#size-cap-whole-sections-visible-warning). |
| `project.style` | unset (built-in text applies) | Hive-wide **house style**: a short how-to-change rule list prepended to every kick on every backend, directly under the header. A managed repo's root `HIVE_STYLE.md` (read from `project.checkouts_dir`) overrides it; precedence repo > hive > built-in. Inline text wins over `project.style_file`. See [house-style.md](house-style.md). |
| `project.style_file` | unset | Path on the hive host holding the hive-wide house style; used when `project.style` is empty. |
| `project.style_max_bytes` | `4096`; values under `512` are raised to `512`. | Caps each house-style candidate. An oversize candidate is skipped whole (not clipped) and the next source applies; the skip is audited as `house_style_skip`. The repo file is also run through ioscan at the issue-body policy and refused whole on a block. |
| `dashboard.snapshot_frame_ancestors` | Empty list means CSP `frame-ancestors 'none'`. | Entries must be exact `https://` origins; paths, wildcards, credentials, query, and fragments are rejected. |
| `dashboard.authorized_users` | Empty means no per-user direct-route allowlist. | Entries can be `user` or `user:role`; roles are `read`, `read-write`, `merger`, `owner`. |
| `dashboard.public_url` | Empty means OAuth redirect URIs (Linear agent install, OpenRouter funding) fall back to `hub.dashboard_url`, then the request's forwarded/`Host` origin. | Set to this dashboard's externally reachable origin (`https://hive.example.com`, no path/query) on a standalone hive whose callback is published on a different hostname or whose ingress rewrites `Host`. Must be an absolute `http(s)://` origin; a trailing slash is trimmed and anything else fails config load. See [linear-agent.md](linear-agent.md#setup). |
| `dashboard.experiment_lab` | Off by default. | Hides the Experiment Lab (formerly Strategy Lab) while that surface is being reworked. Set `dashboard.experiment_lab: true` to show the Experiment Lab section, sidebar nav entry, and Nous status controls. |
| `dashboard.strategy_lab` | Off by default. **Deprecated** alias of `dashboard.experiment_lab`, kept for one release (#11695). | `true` still enables the Experiment Lab and logs a one-line deprecation at config load; the value is folded into `dashboard.experiment_lab` so the next config save writes the documented key. |
| `hub.contribute_announcement` | Empty/off by default. | Operator notice for contributors. Set from Governor → Hub or `/contribute` Management (owner/read-write): `text` is server-sanitised plain text capped at 500 characters and rendered only through text-safe sinks (empty clears), `level` is `info`/`warning`, `expires_at` is optional RFC3339, and the server rotates `id` whenever text changes so dismissed Operations/Profile banners reappear. Active notices show on `/contribute` Operations/Profile, above the Onboarding command block, and in connected relay terminals. |
| `hub.contribute_repo_filters` | Empty map means repos inherit the hive-wide contributor title, author, and label filters exactly as before. | Optional full `owner/repo` keys that narrow one repository's `/contribute` queue without widening the hive. Hive-wide skip labels and hive-wide filters always run first; a repo filter can only add deny matches or require an allow-mode title/author/label match for that repo. Edit from `/contribute` → Management → Repos for Contribute → Filters or Governor → Hub. Operations lists configured overrides and withheld diagnostics name repo matches, e.g. `Repo filter label "2-discussing" denied for projectbluefin/common`. |
| `hub.contribute_wall_enabled` | Off by default. | Opts the hive into the public contributor wall on `/contribute` Operations. The Management tab and Governor Hub settings mirror this switch. When off, the panel is hidden and write endpoints refuse posts. |
| `hub.contribute_wall_retention_days` | `0` means the dashboard default (90 days). | Bounds wall posts stored on the hub data volume next to contributor profiles. Older posts are pruned on load/write; use a positive number to change the retention window. |
| `hub.nps_enabled` | Unset means on for hosted spokes (`hub.hive_type: hosted`) and off for every other install. `HIVE_NPS_ENABLED` overrides it. | Enables the dashboard NPS feedback prompt, whose responses are forwarded to the hub over the spoke's authenticated hub link, without the user's identity, and are visible only to hub admins. A hive with no hub link sends nothing unless a relay is configured (below). See [nps.md](nps.md). |
| `hub.nps_relay_url` | Empty (relay disabled). `HIVE_NPS_RELAY_URL` overrides it. | Base URL of the NPS relay. A standalone spoke (NPS enabled, no hub link) self-registers a generated key there and posts signed responses to it (no token to configure); a hub pulls them from it. `https` only, except `http` to a loopback host. See [nps.md](nps.md#standalone-hives-the-nps-relay). |
| `hub.nps_relay_pull_secret` | Empty. `HIVE_NPS_RELAY_PULL_SECRET` overrides it. | Secret, hub only. The credential the hub uses to pull and ack relay entries. Prefer the env var. |
| `hub.snapshot_url` | Empty (unset): no snapshot URL is advertised in the hub heartbeat. | Public URL of this hive's snapshot page, sent to the hub in the heartbeat (normally written by hosted-hive provisioning). Must be an absolute `http(s)://` URL with an optional path; credentials are rejected and a trailing slash is trimmed. A non-`http(s)` scheme, missing host, or credentials fails config load and is a `400` from `PUT /api/config/governor/hub` (nothing from that save is applied). Query strings and fragments are also a `400` from the dashboard, but at config load a legacy value carrying one is preserved verbatim (no trailing-slash trimming) with a startup warning so existing hives keep booting; remove the suffix before editing the field through the dashboard. |
| `knowledge.connectors[]` | Empty (no connectors). | Scheduled sync of external knowledge into vault facts; types `git`, `document`, `github-wiki`, `confluence`, `notion`. Credentials only via `auth.env` / `auth.file` (inline secrets fail validation). Facts land in `/data/knowledge/connectors/<layer>`; status at `GET /api/knowledge/connectors`, owner-only `POST /api/knowledge/connectors/{name}/sync`. See [knowledge-connectors.md](knowledge-connectors.md). |
| `knowledge.connectors[].scope` (`confluence`) | `deployment` auto-detects Cloud for `*.atlassian.net`; `max_pages` `2000`; `full_sync_every` `10`. | `base_url` plus `spaces` and/or `root_page_ids`; Cloud needs `email` (Basic auth with the API token), Data Center uses the token as a bearer PAT. Give the account read-only space permission. |
| `knowledge.connectors[].scope` (`notion`) | All pages shared with the integration; `max_pages` `2000`; `full_sync_every` `10`. | Optional `root_page_ids` / `database_ids`; `include_archived` fetches bodies of archived pages. Pages must be shared with the internal integration (••• → Connections). Requests are spaced 350 ms apart. |
| `knowledge.publish` | Unset (`connector` empty): publishing off. | One-way mirror of curator-promoted facts into one connector target: `connector` (a `knowledge.connectors` name whose type can publish), `layers` (`project`/`org`/`community`; personal is never published), `root`, optional `include_types`, `dry_run`, `propose_via`. Facts are read from each layer's `knowledge.layers[].path`. Started at boot (a bad connector logs `knowledge publish mirror disabled` and leaves publishing off); state in `/data/knowledge/connector-state/_publish`. Status in the `publish` object of `GET /api/knowledge/connectors`; owner-only `POST /api/knowledge/publish/sync`. See [knowledge-connectors.md](knowledge-connectors.md#publish-mirror). |
| `knowledge.bead_synthesizer.publish` | Off (`enabled: false`). | Publishes synthesized facts into a repository through one PR on `hive/bead-synth-knowledge`: `enabled`, `repo` (empty = `project.primary_repo`), `path` (default `docs/knowledge/`; `.hive/knowledge/` is the alternative; relative, no `..`), `base_branch` (empty = repo default branch), `mode` (`pr` default or `commit`), `cadence` (`per_run` default or `daily`). ACMM L<=4 hives publish via PR only; `commit` is warned about and still opens a PR. See [knowledge-curator.md](knowledge-curator.md#repository-publish-knowledgebead_synthesizerpublish). |
| `variables.security.*` | Deny by default. | `allow_exec`, `allow_http`, and GitHub prompt-source allowlists are honored only from the trusted seed, not dashboard overlays. |
| `agents.<name>.prompt_source` (size cap) | 96 KiB (`promptsrc.DefaultMaxPromptBytes`); `HIVE_PROMPT_SOURCE_MAX_BYTES` overrides it. | A fetched kick template over the cap is **rejected whole, not truncated** ([#12130](https://github.com/hivecommons/hive/issues/12130)): the kick uses the last successfully fetched copy if there is one, otherwise the ordinary `kick_template` → pack → convention → embedded-default chain, and the scheduler logs `prompt from GitHub exceeds size cap; REJECTED whole (not truncated), falling back` with `repo`, `path`, `size_bytes` and `cap_bytes`. Saving an agent whose source is over the cap fails in the dashboard with the same sizes rather than baking a cut-off policy. A half-policy is a policy with its closing rules missing; no remote policy is the safer failure. Every shipped template fits with >2× headroom (`TestShippedTemplatesFitUnderCap`). |
| `data.claude_sessions_dir` | `/data/home/.claude/projects` | Where the dashboard reads Claude Code session JSONL for per-agent token/cost accounting. Point it at the agents' real session directory if you relocate `HOME`. |
| `data.copilot_sessions_dir` | `/data/home/.copilot/session-state` | Same, for the Copilot CLI backend's session state. |

### Label-driven approval

Opt-in per repository (`project.repo_policies[].label_driven: true`,
hivecommons/hive#11313). With it unset nothing below applies and the hive
behaves exactly as before. With it set:

- The un-park sweep treats a trusted addition of an approval label (default
  `triage/accepted`) like `/hive approve`: it removes the park labels Hive
  itself added (`needs-human`, `needs-decision`, `needs-direction`) and adds
  `approved-direction`, without requiring a `/hive` comment. Park labels a
  person added, and `hold` / `do-not-merge`, are left alone and keep the issue
  parked; Hive posts one "accepted, but still held by …" note per acceptance.
  A park label added by a lifecycle bot listed in
  `label_driven_supersede_park_actors` before the acceptance is removed too
  ([#13605](https://github.com/hivecommons/hive/issues/13605)); the list is
  empty by default.
- `pr_self_authorization` counts that label as human sign-off, the same way it
  counts `approved-direction`.
- `/hive approve` and `/hive decision` on such a repo may also be issued by
  collaborators with the GitHub **triage** role (read via `role_name`, so
  custom roles based on triage count too); elsewhere write+ is still required.

**Who is trusted to apply the label.** Only the actor of the *latest*
`labeled` timeline event for that label is checked, in this order: a bot in
`label_driven_trusted_bots` (default `kubestellar-prow[bot]` only); a collaborator whose `role_name` is `triage`,
`write`, `maintain` or `admin`; a public member of the owning org when
`label_driven_trust_org_members` is on. Any other actor, a label Hive applied
itself, or a timeline that cannot be read → not approved (fail closed, logged
once). Removing and re-adding the label re-evaluates the newest event only.

**Webhook.** An `issues` `labeled`/`unlabeled` webhook marks the issue dirty so
the next sweep runs within ~30 s instead of the 5-minute poll; polling remains
the fallback. This dirty-mark, and the check that once-only reply markers were
authored by the hive's own identity, apply to every repo and are the only
behaviour changes for repos that did not opt in.

### GitHub PR-request creation

The PR-request watcher opens the requested PR after its safety gates pass. It no
longer runs hub-side changelog, DCO, docs, or Go-test preflights and has no
hub-side PR preflight configuration. CI is the sole verdict for repository rules;
agents fix red checks on the open PR and push follow-up commits so the failure
and repair stay visible in CI history.

### Issue-request quarantine (`issue-requests/*.failed`)

The issue-request watcher (the `hive-open-issue` relay) classifies each failure
before deciding whether to retry. Permanent errors — a `--repo` that is not
`owner/name`, a documentation placeholder such as `owner/repo`, a repo outside
the hive's primary + fleet set, GitHub `404`/`410`/`422`/`401`, or a `403`
that is not a rate limit — are quarantined on the first tick. Transient errors
(`5xx`, network, rate limits, an effect-ledger entry left *Unknown*) back off
exponentially; an *Unknown* ledger entry is reconciled against the repo's open
issues and expired after one hour if the lookup keeps failing, and a `kind:
issue` create is given up after 3 consecutive reconciliation failures or 8
consecutive identical errors rather than the general 24-hour horizon.

When a request is given up the watcher renames it
`/var/run/hive-metrics/issue-requests/<agent>-<nonce>.json.failed`, writes
`<agent>-<nonce>.result.json` with `ok: false` and the final `error`, and logs
one `ERROR` — `issue-request watcher: request cannot succeed, quarantined as
.failed` — naming the path, agent, repo and reason. Malformed requests land as
`.bad`, authorization refusals as `.denied`. Delete a `.failed` file once
understood, or rename it back to `.json` to retry after fixing the cause. A
pile of `.failed` files from one agent with `repo: owner/repo` means that
agent is smoke-testing the relay live: point it at `hive-open-issue --dry-run`.
See [hive-open-issue.md](hive-open-issue.md#retries-and-what-happens-when-a-request-cant-be-fulfilled).

For runtime precedence and provenance, see [config-layering.md](config-layering.md).

## App self-merge sweep (`auto_merge`)

Three different mechanisms merge PRs automatically, and they share the word
"automerge" without sharing much configuration:

- **The human queue** — a merger/owner applies the `governor.labels.automerge`
  label (default `lgtm`) and Hive squash-merges the PR once CI is green. A
  human decision starts it. See
  [contributor-trust-and-roles.md](contributor-trust-and-roles.md).
- **The App self-merge sweep** (`SweepSelfAuthoredAutoMerges`,
  `src/pkg/github/automerge/automerge_sweep.go`) — a background loop that merges the
  App's **own** open PRs with no human queue-approval at all. This is what the
  top-level `auto_merge:` block controls.
- **The merge-request watcher** (`hive-merge`) — agents request merges through
  a result-file protocol and the watcher verifies CI evidence itself before
  merging. It reads `auto_merge.no_ci_ok` for repos with no CI by design; the
  older `allow_unprotected_base` key is now accepted but ignored. See
  [hive-merge.md](hive-merge.md).

The self-merge sweep exists because Prow structurally forbids self-approval: a
PR the Forge App itself opens can never collect the `lgtm`+`approved` labels
tide requires, since nobody but the App authored it and the App cannot review
its own work. The sweep merges such PRs directly over the GitHub REST API
(squash), bypassing tide entirely.

Neither the self-merge sweep nor the trusted-bot lane needs Prow's `lgtm` /
`approved` labels: `tide` is a status GitHub reports but branch protection does
not require, so the sweep merges once the **required** contexts are green.

**Held and never-started required checks.** A required context can be missing
from a PR head for reasons polling cannot cure. When the head was pushed by
`github-actions[bot]` (for example a GITHUB_TOKEN autofix commit onto a
Dependabot branch), GitHub may create the `pull_request` runs and hold them
for approval (`action_required`) under the repository or organization workflow
approval policy; the required contexts never appear and the PR sits at
`required-check-missing:<ctx>` or `required-check-pending:<ctx>` indefinitely
(hivecommons/hive#11552). For the App self-merge, trusted-bot, and
registry-trusted clanker lanes only, the sweep now inspects the head's workflow
runs when a required check is missing or pending:

- held runs whose head repository is the base repository cause Hive to post one
  `/ok-to-test` issue comment per head and report `workflow-approval-required:<ctx>`.
  The repository-local, self-contained `.github/workflows/hive-ok-to-test.yml`
  listens for that comment and approves the held runs with the repository
  `GITHUB_TOKEN` (not the Hive App token). It references no reusable workflow,
  so it also works under "allow select actions" policies and on GHES.
  No Prow and no Hive App Actions-write permission are required.
- if the held runs remain `action_required` after the grace period, Hive reports
  `workflow-approval-required:missing-workflow:<ctx>` so the attention panel and
  stage entry name the likely missing workflow, and the 085 entry comment
  carries the full workflow rendered for that hive's own App login. Render it
  with `hivectl ok-to-test-template` (`--app-login <app>[bot]` to override the
  login read from `/api/config`) or copy `src/docs/templates/hive-ok-to-test.yml`.
- a head with **no** `pull_request` runs at all that is older than 30 minutes is
  reported as `required-check-never-started:<ctx>`: GitHub never scheduled the
  workflows (seen on hivecommons/hive#11553), and only a new push, a
  close/reopen, or `@dependabot rebase` will create them.
- workflow-approval and never-started holds are logged once per PR head as
  `automerge hold`; later ticks only count the PR in the skip stats instead of
  emitting `automerge skip` every tick.
- fork heads, sentinel-alert PRs, and human-authored lanes are never touched; a
  first-time contributor's held run is still a person's decision — with one
  exception: a contributor-lane PR whose author the **contributor registry**
  admitted at a `require_tiers` tier has its held runs approved once per head
  by the App (`auto_merge.contributor_lane.approve_held_runs_for_registry_trusted`,
  default `true`; needs Actions: Read and write). The operator already trusts
  that author to merge, so holding their CI is pure drag (hivecommons/docs#481).

Per-repo `project.repo_policies[].auto_merge: false` is stronger than the
hive-wide L6 setting: the merge relay refuses the repo, the self-authored sweep
skips it before listing/merging, and the proxy returns 403 for direct
REST/GraphQL merge attempts that name the repo. Existing hold labels (including
`hive-pause/<hive-id>`) remain in place because disabled repos are never
advanced into the merge path.

| Key | Default | Meaning |
|---|---|---|
| `auto_merge.self_authored` | **on** when unset | The only off switch. `false` disables the sweep and App-authored PRs fall back to fully manual merges. |
| `auto_merge.max_merges` | `3` (`DefaultAutoMergeSweepMaxMerges`) when 0/unset | Caps merges per sweep pass. |
| `auto_merge.min_head_age` | `3m` | Minimum PR head age before automerge trusts an unknown required-check set. A fresh head pushed more recently than this waits with `pending: head pushed ... ago (< min_head_age)` unless `auto_merge.required_checks` is declared and every required check is complete and successful. Head age is measured from the head commit's committer date (never earlier than the PR's creation), not the PR's `updated_at`, so label and comment activity — including the hive's own — does not restart it. |
| `auto_merge.required_checks` | unset | Default operator-declared status-check contexts / check-run names (e.g. `["build-gate"]`) that the sweep's green gate requires on repos without a per-repo override. See below. |
| `auto_merge.repos.<repo>.required_checks` | inherits default | Per-repo override for heterogeneous hives. `<repo>` may be the bare name from `project.repos` or `owner/repo`; an explicit empty list declares that GitHub has no required contexts, while Hive still requires positive CI evidence from at least one real successful check-run before merge. |
| `auto_merge.allow_unprotected_base` | deprecated no-op | Accepted for compatibility only. [`hive-merge`](hive-merge.md) no longer refuses solely because a base branch has no GitHub branch protection; it may merge into any branch the App can write after positive CI evidence. |
| `auto_merge.no_ci_ok` | unset (refuse) | **Merge-request watcher key, not a sweep key.** Per-repo opt-in that downgrades only the "unverified" CI verdict (zero statuses, check runs, and workflow runs) to green, for adopted repos with no CI by design. Red and pending verdicts are never downgraded (#6281). See [hive-merge.md](hive-merge.md). |
| `auto_merge.human_merge_paths` | unset | Per-repo map of `owner/repo` to glob patterns (same syntax as `intent.guardrail_path_patterns`) for paths a person must merge, e.g. `Danathar/goodreads-mcp: [".claude/settings.json", ".claude/hooks/**"]`. Every App merge path honors it: the automerge sweep lanes (App self-merge, trusted-author, label-queued) and the agent merge-request relay refuse a PR that touches a listed path, add `hold`, and post one `<!-- hive-human-merge-path -->` comment naming the path(s); a person must merge it. If the changed-file list cannot be fetched completely for a configured repo, the merge is withheld (#11038, #11039). |
| `auto_merge.human_merge_title_prefixes` | `["⚠️", "⚠"]` | PR-title prefixes that mean "breaking change — owner merges" (U+26A0 with or without the VS16 selector by default, matched after trimming leading whitespace). One shared predicate, `automerge.RequiresHumanMerge(pr)`, runs where each lane computes eligibility — App self-merge, label-queued, trusted-author and contributor lanes, plus the refetch before a merge — so a matching PR is never merged by hive and never fronts a merge train, however green and however approved. The PR parks at stage 090 *Ready for human merge* with the `human merge` chip and reason *breaking change — owner merges*; the owner merges it with **Approve & merge as maintainer** or on GitHub. Hive posts one writeback-gated comment saying it will wait for a human. Set to `[]` to turn the title rule off. Added after a ⚠️-titled PR was auto-merged by the trusted-author lane while its owner was still deciding whether to ship it. |
| `auto_merge.human_merge_labels` | `["needs-owner-merge"]` | Labels (case-insensitive, trimmed) that carry the same "a person merges this" meaning as the title prefixes above; either match is enough. Set to `[]` to turn the label rule off. |
| `auto_merge.require_current_base` | unset | Repo list (`owner/repo`) with an explicit **on** override for the current-base merge gate (Settings → Repos → "Merge only on current base"). On a gated repo the App self-merge, trusted-author and contributor lanes only merge a head that contains the current base tip. A green head tested against an older base is updated with GitHub update-branch (a merge commit; no author commit is rewritten) and merges once CI is green on the new head, so a semantic conflict with work merged since turns that PR red instead of breaking the base ([#13634](https://github.com/hivecommons/hive/issues/13634), [#15335](https://github.com/hivecommons/hive/issues/15335)). Costs one compare call per merge candidate. Listing a repo here is also **strict**: a behind head the App cannot update (fork without maintainer edits, branch protection or a missing `contents: write`) is parked (`fork-not-editable` / `update-branch`) rather than merged as tested. Wins over `skip_current_base` and over the default. |
| `auto_merge.require_current_base_default` | `false` | Hive-wide default of the current-base gate for repos in neither `require_current_base` nor `skip_current_base` (Settings → Features → Auto-merge → "Merge only on current base"). **Off** by default: the merge train already update-branches overlapping siblings, so the gate is being soaked per repo (hive's own hive first, via `require_current_base`) before any hive turns it on everywhere. When `true`, a behind head the App cannot update merges as it was tested, with one WARN per PR (`automerge current-base gate: … merging as tested`), so fork-heavy repos are not parked wholesale. |
| `auto_merge.skip_current_base` | unset | Repo list (`owner/repo`) with an explicit **off** override for the current-base gate (Settings → Repos → "Merge only on current base" unchecked): a green head there merges even if the base has moved, whatever `require_current_base_default` says. An explicit `require_current_base` entry wins over this list. |
| `auto_merge.merge_forward_trusted_human` | unset (off) | Repo list (`owner/repo`) where a `role/trusted` + `author/human` PR on a same-repo branch that GitHub reports as conflicting (`dirty`) gets the base merged into its head as one App-authored, signed-off merge commit via the GitHub merges API (no human commit is rewritten), then merges once CI is green on the new head. A real conflict pushes nothing and posts one `<!-- hive:merge-forward-conflict head=… -->` note naming the files both sides changed. Fork heads are never written. Attempted once per head (#13636). |
| `auto_merge.merge_train.enabled` | `true` | Conflict-aware merge train in every automerge lane (hive, trusted-author, contributor) ([#15117](https://github.com/hivecommons/hive/issues/15117)). Each sweep groups the open approved / mergeable-pending PRs of one repo+base by overlapping changed files (`GET /pulls/{n}/files`, cached per head SHA) and lets ONE front per group merge — ordered readiness first: a head the lane last saw at the merge gate (green, approved, soaking or waiting on a sweep) leads, one it has not evaluated since its push comes next, one still at CI / Review trails, then clean-mergeable before behind, then oldest; recomputed every pass, so a member that turns green moves up and one that turns red moves back — while the others wait at stage/080 with lane reason `merge-train:queued-behind#<front>:<pos>/<size>:…` (tracker pill "In merge queue · 3 of 4 · ~4m", never a human stage, never `needs-rebase` as a human flag). The moment the front merges, every sibling whose branch is merely `behind` gets `PUT /pulls/{n}/update-branch`; a `dirty` one is handed to the fixer's rebase-resolve episode (`requested_by: merge-train`, the shared two-attempt guard of [#15104](https://github.com/hivecommons/hive/pull/15104); the fixer router still requires a head the hive can push to, so a fork without maintainer edits stays on the plain `conflicting` path for a person). A head the lane last saw blocked on its own (red required check, terminal hold, permanent merge refusal) never fronts, and a group whose best member is blocked imposes no ordering (fail-open) — nobody waits behind a PR that is not moving. One INFO line `merge train` per order change, not per cycle. The queue carries one PR per issue: when two open PRs declare the same `Fixes #N`, the sweep keeps one (green first, then the earlier-opened) and holds the other with lane reason `merge-train:duplicate-of#<winner>:<issue>/0:0/0:` (tracker pill "Duplicate of #15402 · needs a pick"; *Let hive decide* supersedes, [#15395](https://github.com/hivecommons/hive/issues/15395)). That pick sees one lane; when the two claimants sit in different lanes (the hive's own PR and a trusted human's PR for one issue) every lane holds its candidate with lane reason `merge-train:claim-conflict#<other>:<issue>/0:0/0:` and the log line `same-issue claim conflict undecided: #A vs #B`, so neither is admitted until a decision is recorded on one head or the other PR closes — the PR parks at 085, the chevron reads "held · duplicate of #B · needs decision", and the row's options name the concrete PRs; the hive recommends and never picks ([#15810](https://github.com/hivecommons/hive/issues/15810)). `false` restores independent per-PR evaluation. A PR GitHub reports conflicting on its own, or one at stage/072 returned-to-contributor (label or stage record), is never a member nor counted in the train's size and draws no train idle comment; a conflicting PR rides only while the train itself is repairing it (its rebase requested behind a merged front, loop guard not spent) ([#15395](https://github.com/hivecommons/hive/issues/15395)). |
| `auto_merge.merge_train.max_rebase_attempts` | `2` | Loop guard: how many rebase-resolve requests the train makes for one PR behind one front. Past it the PR moves to stage/085 with the pill "Needs human decision · rebase failed twice" and the decision option "Rebase, then merge as maintainer". |
| `auto_merge.native_merge_queue.enabled` | `false` | Opt-in: once the lane's gates pass, enqueue the PR into GitHub's native merge queue (App auth only) instead of merging directly, and withdraw only Hive's own entries when the PR loses eligibility ([#15198](https://github.com/hivecommons/hive/issues/15198)). Off keeps the pre-queue behaviour everywhere, including the `merge-queue-required` park. See [native-merge-queue.md](native-merge-queue.md). |
| `auto_merge.native_merge_queue.repos` | unset (every watched repo once enabled) | `owner/repo` allow-list for the hand-off, matched case-insensitively. |
| `auto_merge.native_merge_queue.skip_current_base_gate` | `false` | Explicitly lets a queue-enabled base bypass the `auto_merge.require_current_base` gate and the `behind` update-branch, because the merge group re-tests the merged result and a pushed head drops the PR from the queue. Off keeps both gates in force on queue bases. |
| `auto_merge.native_merge_queue.probe_ttl` | `30m` | How long one base branch's `isMergeQueueEnabled` GraphQL answer is cached before re-reading. |
| `auto_merge.squash_only` | unset (no repo) | Repo list (`owner/repo`) whose branch policy forbids merge commits. Elsewhere, a `role/trusted` PR whose head mixes the contributor's commits with App-linked, App-signed ones (the hive's own in-place adoption fixes), every commit self-signed, is labelled `merge-method/merge` and merged with a merge commit instead of parking at `stage/085` as `adoption-stalled:hive-signoff-attribution`; a listed repo keeps squash-only policy and re-homes an in-place adoption to an App-authored superseder instead (#13975). |
| `auto_merge.credit_issue_authors` | `true` | Append `Co-authored-by: <login> <id+login@users.noreply.github.com>` for the human authors of the issues a PR closes (`Fixes|Closes|Resolves #N`) to the squash (or merge) commit message on every hive-performed merge, so reporters land on the contributor graph when an external clanker fixed their issue. Bots, the PR author and logins already credited on the branch are skipped; the logins are recorded as `issue_authors=` on the merge audit entry and in review evidence. One cached `GET /issues/{n}` per newly referenced issue. `false` leaves GitHub's default squash message untouched (#14581). |
| `auto_merge.bot_takeover.enabled` | `true` | When `github-actions[bot]` is in `auto_merge.trusted_bot_authors`, re-author its same-repo PRs whose required `pull_request` checks can never run (the bot opened them with a workflow `GITHUB_TOKEN`). After the head is older than both `min_head_age` and 30 minutes with no `pull_request` run, the App pushes the same change as one signed-off commit to `hive/takeover-<n>`, opens a PR with the original title/body plus `Supersedes #<n>`, comments `Superseded by #N` on the bot PR and closes it. One attempt per bot head SHA. See [hive-merge.md](hive-merge.md#github-actionsbot-pr-takeover). |
| `auto_merge.trusted_authors.enabled` | `false` | Enables the opt-in human-authored PR tier: Hive may merge a green PR only when the author already holds the required hive role and, by default, GitHub push/maintain/admin permission on that repo. |
| `auto_merge.trusted_authors.repos` | empty = all watched repos | Optional repo allow-list for the trusted-author tier. The dashboard renders this as a watched-repo multi-select; selecting nothing preserves the all-repos default. |
| `auto_merge.trusted_authors.require_role` | `merger` | Minimum `dashboard.authorized_users` role the PR author must hold (`merger` or `owner`). |
| `auto_merge.trusted_authors.require_github_permission` | `true` | Also require GitHub to report author write access; API errors fail closed. Fork PRs still need this check even if the setting is false. |
| `auto_merge.trusted_authors.exclude_labels` | `hold`, `do-not-merge`, `needs-human` | Labels that keep a PR out of trusted-author auto-merge. |
| `auto_merge.contributor_lane.enabled` | `true` | Enables the contributor-lane tier: Hive merges a green, soaked, reviewer-approved PR whose author holds a `require_tiers` tier in the contributor registry (`registry_tiers_sufficient`), or whose linked issue carries this hive's App-authored `hive:claim kind=contributor` marker naming the PR author with a `tier=` in `require_tiers` and `verified=true`. Grants unattended merge only to contributors an operator granted such a tier and whose GitHub login was proven; set `false` to make every contributor PR wait for a human `/lgtm` + `/approve`. See [hive-merge.md](hive-merge.md#contributor-lane-auto-merge). |
| `auto_merge.contributor_lane.soak_minutes` | `30` | How long the current head must stay green before the tier merges it; a new push restarts the soak. `0`/unset means the default (30); the shortest real soak is `1`. Also editable from the Activity Monitor's ⚙ → Toasts & timing ("Pre-merge soak"), which writes through `PUT /api/config/auto-merge`; the lane re-reads it on every pass, so a running soak follows the new window. |
| `auto_merge.contributor_lane.require_reviewer_verdict` | `true` | Require an approving aggregate reviewer verdict for the current head. Independently of this key, a hive with no reviewer agent never merges through the tier (`contributor-lane:no-reviewer`). |
| `auto_merge.contributor_lane.require_tiers` | `trusted`, `merger` | Contributor trust tiers whose verified claims the tier may merge. Only operator-granted tiers (`trusted`, `merger`, `advisor`) are accepted; `newcomer` and `contributor` are rejected at validation. |
| `auto_merge.contributor_lane.registry_tiers_sufficient` | `true` | Admit a PR whose author's contributor-registry profile carries a `require_tiers` tier without a linked issue or `hive:claim` marker (logged as `contributor-lane:registry-trusted`). All other gates still apply. `false` requires the claim marker for every contributor. |
| `auto_merge.contributor_lane.auto_promotion_scope` | `hive` | Where the clean-merged-PR ratchet (`auto_merge.contributor_tier.auto_promote_after_merged`) counts an author's merges: `hive` counts across every repository the hive manages (one `search/issues` query finds the repos holding the author's merges; the PR's own repo is always walked first), `repo` counts only the PR's repository. Revert and sentinel-alert evidence blocks promotion in either scope. |
| `auto_merge.contributor_tier.auto_promote_after_merged` | `5` | Clean merged PRs (no revert, no sentinel label, not undone by a later merged revert) after which the contributor lane treats an author as `trusted` without an operator grant, unless the registry marks them `revoked`. Negative disables the ratchet. |
| `auto_merge.contributor_lane.approve_held_runs_for_registry_trusted` | `true` | When the registry path admitted the PR author and the lane stops at `required-check-missing:<ctx>`, approve the head's `action_required` workflow runs once (`POST /repos/{owner}/{repo}/actions/runs/{id}/approve`, fork heads included), post one `hive:contributor-held-runs-approved` audit comment and log `contributor-lane approved held runs`. Never acts for claim-marker, auto-promoted or association admissions, sentinel/exclude-labelled PRs, or diffs touching `.github/workflows/**`. Needs the App's **Actions: Read and write** permission. See [hive-merge.md](hive-merge.md#contributor-lane-auto-merge). |
| `auto_merge.contributor_lane.owner_approval_releases_workflow_fence` | `true` | A hive owner / trusted merger's explicit approval on the **current** head — a GitHub `APPROVED` review on that commit, or a dashboard `/hive decision approve` bound to it — releases the contributor lane's `contributor-lane:workflow-path` fence, so the lane merges a PR editing `.github/workflows/**` / `.github/actions/**` itself once CI, soak and the other gates are clear. Logs `workflow-path fence released by owner approval`, leaves one `hive:workflow-approval` audit comment per head; approval on an older head does not count. An unmarked approval binds to the PR `updated_at` pinned on the hive's first sweep of the head, not to the push, so one typed between the push and that sweep is judged stale and must be typed again ([#14754](https://github.com/hivecommons/hive/issues/14754)). Needs the App's **Workflows: Read and write**; without it the merge is refused, the PR is marked `hive:unauthorized` (`merge-workflow-path`) and parks at 090 for a person's click. `false` keeps every workflow-path PR a person's merge ([#14664](https://github.com/hivecommons/hive/issues/14664)). See [hive-merge.md](hive-merge.md#contributor-lane-auto-merge). |
| `auto_merge.contributor_lane.repos` | empty = all watched repos | Optional repo allow-list for the contributor-lane tier (bare name or `owner/repo`). |
| `auto_merge.contributor_lane.exclude_labels` | `hold`, `do-not-merge`, `needs-human`, `blocked`, `needs-rebase` | Labels that keep a PR out of contributor-lane auto-merge (`do-not-merge/*` always excluded). |

Owners can edit `auto_merge.trusted_authors.*` and `auto_merge.contributor_lane.*`
from **Settings → Features → Auto-merge**. The dashboard writes the same
owner-only `/api/config/auto-merge` overlay as the other auto-merge controls, so
hosted spoke changes survive restarts without editing `hive.yaml`.

**When a lane leaves the merge to a person (`stage/090-ready-for-human-merge`).**
A PR no lane vouches for — an untrusted author (`contributor-lane:author-association`,
`untrusted-author-role`, `author-permission-missing`, `fork-non-member`, the
claim-tier family), workflow files, a manual-merge path — parks at 090 with a
*Ready for human merge* pill whose tooltip names the reason and the fix in plain
words (the config key and where in ⚙ / 👥 it is set). The Activity Monitor's ⋯
menu and needs-human popover lead with the two actions that do what the pill
asks ([#15078](https://github.com/hivecommons/hive/issues/15078)), both
`POST /api/repos/{owner}/{repo}/items/{n}/decision` and both limited to a
verified owner or an authorized user with the `merger` role:

| Decision | What it does | Audit / ledger |
|---|---|---|
| `merge-now` — *Merge now as maintainer* (secondary; *Approve and let hive merge* leads) | Comments on the PR that @you merged it from the dashboard, then merges it at once through the hive's GitHub App (`MergePRVia`, audit path `dashboard`). Refused when the PR is merged or closed, or when you are its author. | `repo_item_decision_merge`; `last_operator_action.action: merge-now`; toast *merged*. |
| `merge-now` on a PR whose `mergeable_state` is `dirty` or `behind` — *Rebase, then merge as maintainer* | Never a GitHub 405. Records your approval first (/lgtm + /approve naming you), arms a rebase-then-merge follow-through (`/data/rebase-merge-followups.json`), kicks the rebase (update-branch on the hive's own PR when only behind; otherwise the fixer lane, which owns a parked PR with an armed follow-through), and answers `202 rebase-queued`. A 2-minute ticker then reads the PR: `mergeable_state: clean` on the rebased head → merges it with your approval attributed; still `dirty` after 2 h, CI red, or no rebase within the deadline → `failed` with the conflicting files and the options *Adopt into fresh branch* / *Close as superseded* (also left as a note on the PR). Refused on a fork whose author has not allowed maintainer edits (`head-not-pushable`). The approval is bound to the head you read: a new head on a contributor's PR counts as the rebase only when GitHub says the hive committed it — update-branch under the App (author resolved to the App bot on a commit GitHub reports as *verified*) or a committer GitHub resolves to the App bot; a committer matched by the pane identity email domain alone, with no GitHub login resolved, also needs GitHub to report the commit as *verified*, and the author counts only when GitHub resolves it to the App bot and reports the commit as *verified* — GitHub derives the author login from a free-text email anyone can set ([#15182](https://github.com/hivecommons/hive/issues/15182), [#15421](https://github.com/hivecommons/hive/issues/15421)). **Operational prerequisite:** the fixer's own rebase of a contributor PR is a plain unsigned git commit (the push broker sets `commit.gpgsign=false`, and the signed-commit reconciler only re-signs PRs the App bot authored), so on a `dirty` contributor PR the follow-through settles `failed` (`foreign-head`) unless the fixer host signs its pushes with a key GitHub verifies against a pane-domain address; *Approve the new head & merge* on the fixer's head is the way out until then. A head anyone else pushes after your click settles the follow-through as `failed` (`foreign-head`) with nothing merged, a note on the PR, and *Approve the new head & merge* / *Close as superseded* as the ways out. | `repo_item_decision_merge` with `status: rebase-queued`; `last_operator_action.action: merge-now`, outcome `pending` → `applied` / `failed`, `followed[]` steps `approval-recorded`, `rebase-kicked`, `rebased`, `ci-green`, `merged` / `failed`; row `rebase_merge.state`; toast *rebase queued, merge to follow*. |
| `queue-merge` — *Approve and let hive merge* (lead) | Admits the head (`admitted_by: operator-approval:<you>`), leaves an `APPROVED` review in your name with a note, adds the automerge label, and leaves the merge to the hive's auto-merge sweep, which merges through its own App on the next tick once CI is green and the head has soaked. On an untrusted author's PR that admission is the one thing that lets the trusted-author lane merge it: the lane's `untrusted-author-role` skip yields to the recorded approval for that exact head, and the merge comment names you (*merged by Hive on behalf of @you …*). Refused on your own PR. | `repo_item_decision_queue_merge`; `last_operator_action.action: queue-merge`; toast *approved — hive will merge on the next tick once CI is green*. |

**Approve is the trust override for that head
([#15136](https://github.com/hivecommons/hive/issues/15136)).** Before, an
owner's Approve on an untrusted-author PR was reduced to the `/lgtm` +
`/approve` comments and the contributor lane's claim-tier rule then
adopt-superseded the PR the owner had just approved. Now every Approve
variant — `approve`, `queue-merge`, `merge-now` — writes
`admitted_by: operator-approval:<login>` (with `admitted_head`,
`admitted_at`) on the PR's stage record before touching GitHub, and the
response and audit detail carry `admitted_by`. While that admission stands
for the head: the contributor lane admits the author for that head
(`contributor-lane:operator-approval`; the merge comment names who
admitted it), **no adoption or supersede is permitted** — a due adoption
records `adopt_blocked_by: operator-admission` and the row reads *not
adopted: a maintainer approved this head; the hive merges it as-is* — and
the hive merges as-is after checks and soak (or at once for `merge-now`).
A new push drops the admission and the row asks again. A
`/hive decision approve` comment by a trusted merger is the same admission;
the engine derives it from the recorded approval and writes it back. An
explicit **Adopt** / **Re-author** from the dashboard is the person choosing
re-authoring and is not refused.

**Approve means "approved to proceed", not "merge this exact head whatever
happens".** The merge-as-is hold applies only while the head *is* mergeable
as-is. When the lane cannot land the approved head unchanged — the fork is
not editable by the App and sits behind the base (`fork-not-editable`),
it conflicts (`conflicting` / `not-mergeable` / `needs-rebase`), or a
commit lacks `Signed-off-by` (`dco-missing`) — the admission does not
block adoption: the hive adopts (re-authors the commits on a hive-owned
branch) and **carries the approval to its copy** (a decision approval
recorded on the copy's head; the copy's body says *Approval carried over:
@you approved #N at head …*). Logged as `a person admitted this head but
the hive cannot merge it as-is; adopting and carrying the approval`. Before
this, an approved fork PR that fell behind `v5` deadlocked: every cycle
refused adoption (*not adopted: a maintainer approved this head; the hive
merges it as-is*) while the merge itself failed on `fork-not-editable`.

**Adopt now always wins over an earlier Approve.** Recording the forced
adoption clears the `operator-admission` blocker at once, and the adoption
engine bypasses admission, presence, grace and `maintainer_policy` on its
next pass. The response is honest about timing: `status: adopted-now` with
the copy's number when the nudged pass opened it within the handler's wait,
else `status: adoption-queued` (*Adopting… (queued): the hive opens its copy
on its next stage pass*). Either way the row is in its acted state the
moment you click — `adoption.queued_by` / `takeover.forced_by` on the
payload, chip *adopting… (queued)*, caption *Adopting… (queued by @you …)*,
no *not adopted* blocker, **Adopt now** gone from the ⋯ menu and the
popover, no decision popover asking again — until the sweep lands and the
row reads *Adopted → #N*.

| You | Head mergeable as-is | Head not mergeable as-is (fork not editable + behind, conflicting, DCO missing) |
|---|---|---|
| **Approve** | admitted; hive merges it unchanged after checks and soak; no adoption | hive adopts (supersede), approval carried to the copy |
| **Adopt** / **Re-author** | hive adopts on the next pass (grace and policy still apply to a plain Adopt) | same |
| **Adopt now** (`force: true`) | hive adopts on the next pass regardless of an earlier Approve, presence, grace, policy; row acted at once | same |

| Decision | What it does | Audit / ledger |
|---|---|---|
| `re-author` — *Re-author as hive PR* | Offered as its own lead action on a 090 PR whose head cannot merge as-is — the stage record's commit check counted commits without a `Signed-off-by` (`dco_unsigned` / `dco_total` on the row). The popover and both Approve confirms say so **before** acting (*2 of 3 commits have no Signed-off-by, so a required DCO check refuses the merge*); approving never re-authors by itself. Re-author posts its own note and the `adoption/supersede` request label; the hive re-homes the commits on a hive-owned, signed-off branch and opens a superseding PR. Disabled with the reason on a person's PR (the hive does not re-author a human's PR — ask them to `git commit -s --amend`). | `repo_item_decision_adopt` with `decision=re-author`; `last_operator_action.action: re-author`; toast *adopted*. |

*Let hive decide* on a PR parked at 090 only for its author's trust takes the
decision instead of answering that it cannot merge
([#15173](https://github.com/hivecommons/hive/issues/15173)):

| PR head | Hive decides | ✓ hover |
|---|---|---|
| review passed (no reviewing / changes-requested / requires-human `review.verdict_labels` label), CI green, mergeable | approve — the same path as *Approve and let hive merge* (needs the `merger` role): admits the head (`admitted_by: operator-approval:<you>`), approves it and adds the automerge label; no adoption, and the sweep merges the PR under its own number after soak | *hive decided: approve · review passed, CI green* |
| CI red | adopt in supersede mode | *hive decided: adopt · CI red* |
| conflicting | adopt in supersede mode to rebase | *hive decided: adopt · conflicting, needs rebase* |
| review not passed | adopt in supersede mode | *hive decided: adopt · review not passed* |

A sentinel alert, your own PR, a writer without the `merger` role on a good
head, adoption turned off, or any other 090 reason keep the plain-words
no-effect answer with the two merge buttons.


Two related repairs ride with the rebase-then-merge action
([#14822](https://github.com/hivecommons/hive/issues/14822)). The fixer
router now classifies a conflicting (`mergeable_state: dirty`) PR *before*
skipping rows parked at 085 *Needs human decision*, and a PR with an armed
follow-through is routed as pushable (like an in-place adoption), so a
parked PR that falls behind its base is no longer owned by no lane. And on
each unpark sweep the hive dismisses, with an Info log line
`dismissed self-inflicted changes_requested: source comment is hive's own /hive decision`,
any recorded changes-requested whose source comment is the hive's own
delegated `/hive decision` — the self-inflicted verdict the guard in
[#15063](https://github.com/hivecommons/hive/pull/15063) now prevents but
which older records still carried.

**Autonomous rebase-resolve** ([#15103](https://github.com/hivecommons/hive/issues/15103)).
No merge lane ever rebased a conflicting PR: the self and trusted-author
lanes stopped at `label:needs-rebase` before reaching the update-branch call,
and the fixer router only listed hive-authored or adopted PRs as rebase
work — so a PR the operator opened under their own identity through a
session agent sat `dirty` with nobody kicked to fix it. Now:

- **`behind` (clean update):** prow's `needs-rebase` label no longer blocks
  the self or trusted-author lane when GitHub says `mergeable_state: behind`
  — the lane's update-branch runs and clears the label.
- **`dirty` (real conflict):** the fixer router lists a trusted author's PR
  (`auto_merge.trusted_authors` enabled, repo allowed, author at the required
  role, head pushable, no exclude label) as rebase work alongside the hive's
  own and adopted PRs. The kick says
  *rebase-resolve (attempt n/2): `gh pr checkout`, `git rebase origin/<base>`,
  resolve, re-run build and tests, `git push --force-with-lease` to the PR's
  OWN branch; do not open a new PR*. Loop guard: `/data/rebase-attempts.json`
  counts distinct dirty heads per PR — the first dirty head is attempt 1, a
  fixer push that is still dirty is attempt 2, a third dirty head is
  exhausted: the router stops listing the PR (Warn log
  `rebase attempts exhausted; leaving the conflict to a person`) and the row's
  gate pill reads **Rebase failed · 2/2 — needs a person**. While it runs the
  pill reads **Rebasing · attempt n/2** with the working cue; the episode
  clears as soon as the PR is no longer conflicting. The row payload carries
  `rebase: {attempt, max, exhausted, head, text}`.
- **Approve on a PR that is behind or conflicting** no longer promises that
  "the merge queue takes it from here" (the gate skips `conflicting` for
  good). The approval is recorded, the rebase episode opens in your name
  (`requested_by`, which admits the PR to the fixer router whoever opened it,
  as long as the hive can push to it; a hive-authored PR that is merely
  `behind` gets update-branch on the spot), the toast reads *Approved — #N
  conflicts with base; hive will rebase it next pass (attempt 1/2)* with a
  **Rebase, then merge as maintainer** button, the response carries
  `rebase: {attempt, max, …}`, and the row's *Approving…* cue clears as soon
  as the decision records.
- **Rebase, then merge as maintainer** uses the same task: a rebased head that
  is still dirty goes back to *Rebasing* (step `still-conflicting`, the 2 h
  rebase clock restarting) while attempts remain, and settles *failed* with
  the conflicting files once the guard is exhausted.

Neither widens who the lanes trust: to stop a contributor's PRs parking at 090
give them the `trusted` / `merger` tier under 👥 Contributors
(`auto_merge.contributor_lane.require_tiers`), add them as a repository
collaborator, or list them under `dashboard.authorized_users` with the
`merger` role.

Actionable merge failures surface as dashboard system alerts, deduplicated by
repo+reason and cleared by the next successful merge in that repo. Alerts name
the operator action: grant/install the App with Contents and Pull requests write,
approve fork PR workflow runs or relax the repo's fork-workflow approval
setting, adjust review/ruleset bypass or approve the PR, make a missing
required check report, or enable/use an allowed merge method. Conflicts go back
to the agent fix loop, and rate limits do not alert.

**The ACMM gate.** `self_authored: true` (or unset) is necessary but not
sufficient: the sweep only starts when the hive's `acmm_level` is **6 or
higher** (`config.SelfMergeMinACMMLevel`). An unset `acmm_level` fails closed
— a hive never gets self-merge by accident; the boot log records
`self-authored auto-merge sweep disabled: acmm_level below minimum (or
auto_merge.self_authored is off)` when either condition blocks it. This
matches the [ACMM policy matrix](acmm-policy-matrix.md): below L6 all agent
PRs are hold-gated and nothing merges its own work. Runtime ACMM level changes
that cross this gate restart the request relay generation, so the sweep starts
or stops without a pod restart. Promotion into L6 does not release existing
level-applied holds; use `release_level_holds: true` on `PUT /api/packs/level`
for a deliberate one-off release, otherwise a human must remove `hold`. The
dashboard's L6 promotion notice lists the currently held App-authored PRs and
the watched repositories' auto-merge toggles; it is informational only.

**Eligibility per PR.** The sweep only ever considers open, non-draft PRs
authored by the App bot login itself (re-verified per PR, not just at listing
time — it never touches anyone else's PRs), that GitHub reports mergeable, and
whose head commit is green on the required checks. The head SHA is re-fetched
and re-verified at the merge step, so a push between evaluation and merge is
never squashed unchecked.

**Why `required_checks` exists.** Asking GitHub which checks a branch actually
requires (`GetRequiredStatusChecks`) needs the `administration:read` scope,
which the Hive GitHub App does not hold. Without a config-declared list the
sweep falls back to that API, then to the branch and ruleset payloads, and
finally to the operator-declared list. When the actual set is unknown, Hive
relies on GitHub's merge endpoint for required-check enforcement while ignoring
known metadata contexts and requiring real CI evidence locally. Declaring the
branch's real required set removes ambiguity. On hives that watch multiple
repositories, keep `auto_merge.required_checks` only as a default and put
repo-specific gates under `auto_merge.repos`:

```yaml
auto_merge:
  required_checks: [build-gate]   # default for repos without an override
  repos:
    console:
      required_checks: [build-gate]
    console-marketplace:
      required_checks: [static-validation, card-quality-gate, validate]
    homebrew-tap:
      required_checks: [brew-ci gate, unit tests + drift check]
    docs:
      required_checks: []
```

Repo keys may be bare names from `project.repos` or fully qualified
`owner/repo` names. If the configured list names a check that has never
reported on the PR head and GitHub's branch-protection/branch/ruleset fallback
chain returns a known actual set that does not include it, Hive logs a once per
repo warning (`automerge required_checks override does not match repo
protection`) and uses the actual set. If the actual set is unknown, Hive keeps
waiting on the configured list rather than silently weakening the gate.

When the branch reports no required checks (nothing is enforced server-side, so
a completed failing check-run or status on the head still blocks), or when the
required-check source remains unknown after the protection/branch/ruleset/config chain, Hive does not
let metadata-only contexts wedge trusted-author or self-authored merges. Known
merge/deploy metadata such as Prow `tide`, probot `DCO` when the lowercase prow
`dco` context has succeeded, and the existing ignorable allowlist are ignored
for the local status gate; when the source is unknown the merge endpoint still
enforces any server-side branch rule. Hive still fails closed with `ci-unverified` unless at least one
real, non-meta check-run has succeeded on the head SHA, so an unprotected branch
with only tide/DCO and no CI does not merge. The sweep logs the carve-out once
per PR as `automerge ignoring meta contexts on unprotected base`.

When `required_checks` is unset and GitHub's branch-protection API is not
available, the sweep also waits for every non-meta, non-ignorable PR-context
check-run name expected for the PR to appear on the new head. The reference is
the PR's previously evaluated head when known, otherwise the head SHA of the
most recently merged PR into the same base branch; base-branch commits are not
used because they include push-only workflows. Check-runs whose
`pull_requests` list is empty are excluded from the expected set. This avoids
the short post-push window where fast checks have passed but slower PR
workflows have not created check-runs yet, while `min_head_age` remains the
backstop when no PR-context reference is available.

**Rate-limit behavior.** The sweep ticks every 10 seconds on hives with ≤4
configured repos. Above that, the interval scales so the sweep's list+candidate
calls stay within 25% of the App's hourly REST allowance — a 45-repo hive on a
fixed 10s tick used to exceed the whole allowance on list calls alone and
starve every other GitHub caller, including the agents.

## Supersession sweep (`supersession_sweep`)

The supersession sweep (`src/pkg/github/pr_supersession_sweep.go`) looks for
open PRs whose claimed issue is already closed by a *different* merged PR. It
keeps one `<!-- hive:pr-supersession-sweep -->` notice per PR, edited in place.

- **Which issues count.** When the PR closes issues with a closing keyword
  (`Fixes`/`Closes`/`Resolves #N`), only those are candidates; a bare
  `Part of #N` / `Refs #N` reference is context and a closed parent reached
  that way never supersedes a PR. Only a PR with no closing keyword at all
  falls back to its branch-name issue and bare references. If **any**
  candidate issue is still open the PR is live work and the sweep leaves it
  alone — no notice, no label, no close (#12349: #12322, `Fixes` open
  #12117 and `Part of` closed #12080, was escalated to `needs-human`).
- **Hive-authored PRs** are closed immediately at ACMM L6 when every file they
  touch was also touched by the merged PR. When they touch files the merged
  PR did not, the hive resolves the overlap itself instead of asking a person
  (`pr_supersession_resolve.go`): if the head is behind the base it requests
  GitHub's update-branch (`branch_update` effect) and waits for the new head;
  once the head is current it re-reads the files, and either closes the PR
  (nothing left, or only files the merged PR already changed) or rewrites the
  body's `Closes #N` to `Refs #N (closed by #M; this PR adds <files>)`, stamps
  `<!-- hive:supersession-resolved issue=N closer=M -->`, edits the notice and
  lets the PR continue in its lane. A refused update (conflict) is noted once
  per head and left to the fix lane. The hive's own earlier `needs-human` is
  released when it takes the resolution over; one a person applied is
  honoured and the PR is left alone. Below L6 the PR is escalated instead.
- **Escalation goes through the stage engine.** Whenever the sweep hands a
  PR to a person (contributor PR with files the merged PR did not touch, or a
  hive PR below L6) it adds `needs-human` *and* submits a `Parked` fact, so
  the PR lands at `stage/085-needs-decision` in the same pass — never a bare
  `needs-human` beside an active lane stage (#13782 wore
  `stage/050-review-pending` + `needs-human` until the next auto-merge tick).
  Only a closing claim (`Closes #N`) parks a contributor PR; a PR that merely
  references the closed issue (`Refs #N`, `Part of #N`, a branch name) gets
  the notice with the files named and no park. The sweep's own `needs-human`
  clears with its cause: when the supersession it announced no longer holds
  (issue reopened, claim rewritten, link downgraded to a reference) it takes
  the label off again (`supersession-sweep-released-needs-human`); a person's
  `needs-human` stays (hivecommons/hive#14155).
  The notice names the files the merged PR did not touch. Once the notice is
  on the PR, the sweep re-escalates only when the notice changes (a different
  merged PR or reason); clearing the label for an unchanged notice sticks.
- **Human-authored PRs** get the notice and are surfaced to operators in the
  dashboard Review Queue (below); they are never closed automatically
  unless `supersession_sweep.close_contributor_prs` is set (and the repo is at L6).
  With it set, the sweep closes the PR only when **all** of these hold: every
  file it touches was also touched by the merged PR; it is
  `size/XS`–`size/M` (or under 100 changed lines when unlabeled);
  the author has no unresolved review threads; review threads could be read;
  and `grace_period` has passed since the notice announced the window with no
  human (author or maintainer) comment or review comment after the notice. Any
  doubt keeps the PR open and rewrites the notice to say why.
- A closed PR gets the `hive/superseded` label and one
  `<!-- hive:pr-supersession-autoclose -->` comment naming the merged PR and
  its merge time, with "reopen if that's wrong". Reopening and commenting
  keeps it open on the next pass.
- Every human-authored PR the sweep detects as superseded — whether or not
  automatic closure is on — is tagged `superseded` in the dashboard Review
  Queue with a link to the merged PR. When auto-close is active the
  entry also shows when the grace window ends; otherwise there is no
  countdown. An owner can **Close now** (same comment, label and close as the
  sweep) or **Keep open**, which adds `hive/keep-open`; the sweep never
  auto-closes a PR with that label.

| Key | Default | Meaning |
|---|---|---|
| `supersession_sweep.close_contributor_prs` | `false` | Allows closing superseded human-authored PRs after the grace window. Off: they get the notice and a Review Queue close/keep-open decision, but are never closed automatically. |
| `supersession_sweep.grace_period` | `24h` (`DefaultSupersessionGracePeriod`) when 0/unset | How long a human-authored PR stays open after the grace-window notice. Changing it rewrites the notice and restarts the window. |

## Stage labels (`governor.labels.stage`)

The hive owns one ordered `stage/NNN-*` label per open issue/PR
(`010-actionable` … `100-merge-queue`), a `role/*` admission-tier label and
an `author/*` authorship label (`human` / `clanker` / `hive`; names under
`governor.labels.author`) on PRs, and a red `stage/stalled` marker when nobody
but the hive has touched an item for the stage's dwell — never while checks
run or the reviewer verdict is pending, those are waits on the hive and
surface as a `hive_held` count on the review queue instead;
`label:stage/stalled` across repos is everything a person has walked away
from. Backward stage moves are a one-line comment and the review queue's
transition history (the `stage/regressed` label is retired, #12534). Default
on; `governor.labels.stage.enabled: false` stops the reconciler,
`governor.labels.stage.dwell.<slug>` re-times a stage's stalled threshold,
`governor.labels.stage.stalled.require_quiet: false` measures the dwell from
stage entry instead of last activity (Settings → Labels). When a clanker
PR enters
`stage/072-returned-to-contributor` the hive posts one nudge comment per head
(re-nudged once after `governor.labels.stage.contributor_nudge.renudge_interval`,
default = the 24h dwell; cap 2) and, for a contributor whose relay heartbeat is
within `governor.labels.stage.presence_window` (default 15m), pushes a `fix-pr`
interrupt to their relay session, bounded by the per-contributor fix budget
(`contribute.max_parallel_fixes` / `contribute.min_focus_window`).
`governor.labels.stage.contributor_nudge.enabled: false` turns the nudge and
the relay task off. The nudge and the adoption below apply to `author/clanker`
PRs only; human- and hive-authored PRs just wait at 072. A red PR whose author
is unavailable is adopted (`governor.labels.stage.adoption.enabled`, default
on) — judged against live hub presence each sweep: relay offline for
`governor.labels.stage.adoption.after_offline` (default 15m) since its last
heartbeat, no contributor profile here for `adoption.after_unregistered`
(default 2h) since the first nudge, or online but silent for
`contributor_grace_online` (default 2h; `contributor_grace_offline`, 15m, when
the hub reports no presence). `hold`/`do-not-merge` blocks adoption. The
author's repository role does not: clanker PRs are adoptable on disconnect
regardless of the owner's repo role
(`governor.labels.stage.adoption.maintainer_policy`, default `adopt`; owner
decision on hivecommons/hive#14524 — the PR a contributor's clanker opened is the
hive's to stand in for when that hive disconnects, maintainer or not).
`ask` holds a maintainer's PR for an owner: the activity monitor row says
"waiting on @author (maintainer, offline 26h) — adopt?" and offers **Adopt**
(decision API `adopt`, which sets `adoption/supersede` for the next sweep);
`never` keeps the earlier hands-off rule and releases any existing adoption
of a maintainer's head. Human-authored PRs stay unadopted under all three.
Dashboard: Settings → Labels → "When the PR author maintains the
repository". Whatever the policy and the clocks say, the row's ⋯ menu on a
072 / 071 clanker PR offers the operator **Adopt now — hive takes it over**
(decision API `adopt` + `force: true`: supersede adoption on the next nudged
sweep, bypassing presence, grace, `maintainer_policy` and an earlier
Approve's admission; `hold` still stops it; marker `hive:adopted
reason=operator-forced by=@login`; the row shows *adopting… (queued)* and
drops the action until the copy opens — see *Adopt now always wins over an
earlier Approve* above) and
**Reroute to another clanker** (decision API `reroute` + `to: <login>|any`:
the follow-up work goes to an online contributor clanker's fix queue as a
`fix-pr` interrupt — the same mechanism as the 072 nudge — with a
`hive:rerouted` note on the PR). A rerouted task the target relay has not
taken within `governor.reroute.pickup_timeout` (default `30m`) falls back to
Adopt now by itself, with a system alert and a `repo_item_reroute_fallback`
audit entry; with no clanker online the Reroute item is disabled ("no
clankers online — use Adopt now"). Both are also options in the
Needs-human popover once the nudge grace has elapsed, and "Let hive decide"
picks Adopt now then (hivecommons/hive#15038;
[dashboard-sections.md](dashboard-sections.md#hive-activity-monitor)). While
the row waits at 071 / 072 its waiting-on pill also says whether the rework
has been picked up — `· no response`, `· queued` (on the clanker's fix queue,
N ahead), `· acknowledged` (activity on the PR after the request),
`· working` (the clanker relay took the `fix-pr` task), `· re-review pending`
(a new head, routed back to 050 on the next sweep) — from the stage record,
the PR list and the fix queue the tracker already holds, with no extra
GitHub calls (hivecommons/hive#15280). Push detection needs the head saved
by a nudge: a never-nudged 071 row has only a stage-entry time, so a push
cannot produce `· re-review pending` and may appear as `· acknowledged`
from PR activity instead. The stage engine still routes the new head to
050 on its next sweep (hivecommons/hive#15286). Every PR row in the CI /
Review column also carries push progress (hivecommons/hive#15784): the
stage record keeps `waiting_push_since`, `last_push_at` and `last_push_sha`,
booked from the `pull_request` synchronize / `push` webhook re-read and, as
a fallback, from the head change the next enumerate sees. While a 071 / 072
row waits the pill ends with the wait's age (`waiting on 👤 @user to push ·
38m`), muted-amber once past `dashboard.stage_tracker.waiting_push_stale_h`
(default `4`; `0` keeps the default, negative disables the tone). Once a
push lands and CI / the re-review for that head still run, the chevron
returns to the column's own word — stage only, the 👤 clears because the
ball is back with the hive — and the gray pill names the push: `@user
pushed abc1234 · 2m` (links to the commit), `adopted from @x's #N · pushed
2m ago` on an adopted row, `pushed abc1234 · 2m` on a plain CI / Review
row; the hive line reads `push seen 2m ago → CI/re-review queued`. When the
PR allows maintainer edits (`adoption.in_place`, default on) the hive adopts
it in place: `stage/074-adopted-by-hive`, the PR's own branch filed in
`ci-failing.json` as the hive's (`adopted: true`) for the scanner's red-PR
fix-first kick. On a later head change, forgeable git names/emails never keep
ownership: the hive follows the head only when GitHub links the commit to the
App bot (the stage store only remembers SHAs that already passed that
check); otherwise it hands the PR back as a contributor push
(`governor.labels.stage.adoption.strict_attribution`, default on; first deploys
without any repo evidence log and keep adoption). Push-capable App-authored
agents use the App bot noreply email (keyed by the bot's GitHub user id, not
the App id) for git commits so fixer pushes normally carry that GitHub linkage while their commit name/sign-off still names the
lane. Once the hive's in-place head is green, reviewer-approved, DCO-clean
and soaked, the contributor lane merges it under the hive's own authority
(`contributor-lane:adopted-by-hive`) — the original author's trust tier is
not consulted, because the hive owns an adopted head
(`governor.labels.stage.adoption.auto_merge`, default on; `false` leaves an
untrusted contributor's adopted PR waiting for a maintainer `/lgtm`). If the hive cannot push in place, it
pushes `hive/adopt-<n>` at the contributor head, opens a superseding App PR
that preserves the author's commits, and — in the same pass — **closes the
original** with one note naming the copy and why ("Superseded by hive:
re-authored as #M … This PR is closed; if #M fails, hive reopens it."),
labelled `hive/superseded`, so one issue never shows two active PRs
(`governor.labels.stage.adoption.supersede.close_first`, default on;
hivecommons/hive#15135). A contributor head that **conflicts with the base**
(`needs-rebase` / `conflicting`) is first replayed onto `hive/adopt-<n>`
(a merge of the base); when that replay itself conflicts the copy still
opens, on the hive's own branch, and the fixer's rebase-resolve episode
takes it (`requested_by: adoption`, the same two-attempt guard as the
merge-train, hivecommons/hive#15104) — the row reads "Adopting · fixer
resolving conflicts". If both attempts fail the copy parks at `stage/085`
with `rebase-failed-twice` and the original (when still open) with
`adoption-stalled:rebase-failed-twice`; the Needs-human popover offers
**Adopt into fresh branch** / **Close as superseded**. A plain conflict
never hands a PR back to a contributor who cannot act (hivecommons/hive#15307):
an earlier attempt that opened nothing is re-admitted once the clanker has
stayed offline or unregistered for `governor.labels.stage.adoption.offline_grace`
(default 30m) since the attempt or its last heartbeat. (Without a fixer
wired the replay conflict is still terminal for the head: one note,
`supersede-conflict`.) If the copy is closed unmerged or its required
checks are red at two heads, the hive reopens the original, removes
`hive/superseded`, and parks it at `stage/085` with `adoption-copy-failed`
("needs a person"); at most one reopen per original per 24h. The note,
label, close and reopen are unsolicited writes on a contributor's PR, so each
first asks the write-back gate as kind `contributor_adopt` (L6 + opt-in label
by default; see [the write-back table](#what-hive-writes-to-github-and-when)):
refused, the original is not closed and stays open on the keep-open path
until the copy merges, and a due reopen waits for the next poll. A clanker-
or hive-authored original is exempt from the opt-in label: with
`governor.labels.stage.adoption.close_superseded: on_adopt` (the default;
`on_merge` keeps it open until the copy merges) the gate admits its close
as `superseded-agent-pr` once the copy carries its commits and sign-off, so
an agent's PR never lingers open beside hive's copy
(hivecommons/hive#15526) (a pause
lifting lands it; a policy change after the close is warned in the log and
leaves the PR closed until a person reopens it). `close_first:
false` keeps the earlier behaviour: the original moves to 074 and closes
only once the superseder merges. Whether that rework reaches each agent is visible per row
on Operations → Connected contributor agents: the relay's parallel capacity
(`capacity N · spawn|preempt`, or *not advertised*), one line per in-flight
item tagged **NEW** / **REWORK** with its stage label, and a red *N PRs
awaiting rework (not yet dispatched)* line when the contributor has
`stage/072` PRs the hub has not handed them (`parallel_capacity`,
`in_flight[]`, `fix_queue_depth`, `pending_rework` on `/api/contribute/fleet`;
see [contributor-relay.md](contributor-relay.md#what-the-operator-sees-operations--connected-contributor-agents)). The retired `🐝 covered by PR`,
`claimed` and `hive/in-progress` labels are removed from open items on the
first cycle after upgrade. Full table, state diagram and migration note:
[labels-and-control-signals.md](labels-and-control-signals/stage-labels.md#stage-labels-stagennn--the-pipeline-state-machine).

### Central stage engine rollout

Every stage label is derived by one `pkg/stageengine` reconciler behind
one writer ([ADR-0025](adr/0025-central-stage-engine.md)), built once per
process and shared by the issue and PR sweeps. The rollout switch is
`governor.labels.stage.engine.mode` (Settings → Labels → *Central stage
engine mode*, or `hive.yaml`); the sweeps re-read it before each pass, so
a change takes effect on the next sweep without a restart, and the hive
logs one `WARN stage engine: mode changed` per transition.

| mode | behaviour |
|---|---|
| `enforce` (default) | engine's derivation is written; out-of-order transitions are counted as anomalies |
| `shadow` | pause — the engine derives and counts but writes nothing; every `stage/*` label stays exactly as found |

An unset mode is `enforce`. The pre-rollout value `off` (legacy
pass-through) is rejected at config load since the sweeps' own derivation
path was retired (plan PR-F); set `shadow` to pause stage writes instead.
Watch `/api/status.stageEngine` (`mode`, `derivations`, `anomalies`,
`healed`), the strip at the top of the activity tracker (`engine enforce ·
N anomalies`), `hive_stage_transition_anomaly_total{kind,from,to}` and
`hive_stage_duplicate_pills_healed_total{kind}` on `/metrics`; an anomaly
pair not explained by a documented event (merge, close, human label edit)
is a deriver or transition-table fix, never a reason to leave `enforce`.

The remaining series are `hive_stage_derivations_total{kind,stage}` and
`hive_stage_writer_writes_total{kind,op}`.

## Shared-CI rerun sweep (`shared_ci_rerun`)

When a fix lane defers a red PR to a `[shared-ci] <check> failing across
<owner/repo>` incident it stamps `<!-- hive-shared-ci-<n> -->` on the PR
(see [labels and control signals](labels-and-control-signals.md)). Nothing on
the PR changes when the incident's fix lands, so the PR stays red until
someone re-runs it. The shared-CI rerun sweep
(`src/pkg/github/shared_ci_rerun.go`) closes that gap every ~15 minutes:

- It lists `[shared-ci]` issues closed in the last 72h in each watched repo,
  ignoring ones closed as "not planned". With none, it makes no other calls.
- For each open PR whose newest shared-CI marker names one of those incidents,
  it re-runs the failed jobs (`POST .../actions/runs/{id}/rerun-failed-jobs`)
  of the newest run per workflow on the PR head when that run concluded
  `failure` or `timed_out`, using the hive's App/daemon credential. It pushes
  nothing to the branch, so it applies to every author; PRs with a hold or
  `do-not-merge` label are skipped.
- It then posts one comment carrying `<!-- hive-shared-ci-rerun-<n> -->`,
  which keeps the PR to one re-run per incident. A PR with nothing red on its
  head gets no comment. At most 10 PRs are re-run per pass; each re-run is
  logged as `shared-ci rerun repo=… pr=… incident=#…` and audited as
  `shared-ci-rerun`.
- It is an unstick step (`shared_ci_rerun` in `api_budget.unstick_steps`): it
  keeps running in `conserve` and is shed only in `critical`.

| Key | Default | Meaning |
|---|---|---|
| `shared_ci_rerun.enabled` | `true` (unset) | Set `false` to stop re-running PRs when a `[shared-ci]` incident closes. |

## Open-PR dispatch guard and duplicate-fix sweep (`scanner`)

An issue whose fix is already open must not be dispatched again
([#14799](https://github.com/hivecommons/hive/issues/14799)). The claim
ledger the admission gates read is refreshed by the eval cycle, so a PR a
person opened by hand between scans was invisible to the `/contribute` relay
lane and the scanner, and the issue was handed out a second time; the two
same-issue PRs then landed at `stage/085-needs-decision` for the owner to pick
between. Two mechanisms close that gap
(`src/pkg/github/open_pr_links.go`, `src/pkg/github/pr_duplicate_fix_sweep.go`):

- **Pre-dispatch guard.** Before any automated producer claims an issue —
  the central `IssueClaimBlockReason` gate, the ledger admission check, the
  issue-request watcher and the `/contribute` relay lane — the hive lists
  the repo's open PRs (once per repo per sweep, cached for a minute) and
  parses each title/body with the claim ledger's closing-keyword grammar
  (`Fixes|Closes|Resolves #N`; a `Refs #N` is not a fix). When an open PR
  links the issue, the claim is skipped, the issue is labelled
  `has-open-pr` (created on first use; never re-added), the refusal is
  logged as `claim skipped: open PR already linked` with the PR number, URL
  and author, and the tracker's withheld list shows *An open pull request
  already links this issue (claim skipped)* with the PR as evidence
  (reason `open_pr_linked`). A failed listing fails **open**: the
  ledger-backed guards still apply, and a flaky listing must not stall every
  claim. The gate also refuses an issue the confirming read says is already
  **closed** (`issue closed`), so an issue closed between enumerations is
  never dispatched from the cached snapshot.
- **Webhook claim linking.** A `pull_request` delivery (opened, reopened,
  edited, synchronize, ready-for-review, draft, closed) is parsed with the
  same closing-keyword and branch-name grammar and written into the claim
  ledger the moment it lands
  ([#14814](https://github.com/hivecommons/hive/issues/14814);
  `src/pkg/github/webhook_pr_claims.go`): an open PR's claims are recorded
  (and the cached actionable snapshot re-filtered, so the hub's own agents
  stop seeing the issue too), a PR edited to stop naming an issue or closed
  without merging releases it, and a merged PR's claims become merged
  claims. Before this the link was only made on the full reconcile pass —
  about an hour apart on a busy spoke — and an owner PR opened with
  `Fixes #N` in between left #N offerable for that long; the `/contribute`
  lane handed [#14765](https://github.com/hivecommons/hive/issues/14765) out
  nine minutes after #14772 (Fixes #14765) opened. The receiver's response
  lists the issues as `claimed_issues`, the governor logs `webhook PR claims
  applied to claim ledger` with the PR, state and counts, and the open-PR
  listing cache for the repo is invalidated so the pre-dispatch guard
  re-lists on its next call. An `issues` closed delivery (or a PR merged
  with a closing keyword, which GitHub delivers as the issue's own close)
  already evicts the issue from the status snapshot the lane offers from.
  The same open delivery also **preempts a fixer already at work**
  ([#15469](https://github.com/hivecommons/hive/issues/15469)): the
  `/contribute` hub releases every in-flight contributor task on the
  covered issue whose session is not the PR's own author (reason
  `preempted: open PR #N by @login already covers this issue`) and hands
  the session other work, exactly as a claim takeover does. Before this
  the ledger stopped *offering* the issue but never told the contributor
  it had dispatched nine seconds after the issue was filed, so that
  fixer finished and opened a second PR
  ([#15473](https://github.com/hivecommons/hive/pull/15473)) for an issue
  the owner's PR ([#15470](https://github.com/hivecommons/hive/pull/15470))
  already covered.
- **Duplicate-fix sweep.** Every ~15 minutes (an unstick step, alongside the
  supersession sweep) the hive looks for an open issue two or more open PRs
  close and resolves the pair deterministically. The **earlier-created PR
  wins** while its checks are green or pending: the later hive-authored PR
  receives one comment — `Superseded by #<keeper> (earlier fix for #<issue>).
  Closing as duplicate.` — is labelled `hive/superseded` and closed, with the
  reason `duplicate-of:<keeper>` in the log and the `duplicate-fix-closed`
  audit entry. A later PR a person authored (`author/human`) is never
  closed: it gets the comment, with a request to close it or say why both
  should land, and stays open at its current stage
  (`duplicate-fix-commented`). A later PR by a **relay contributor
  registered on this hive** (the login has a contributor profile — the
  hive's own fixer under its contributor account) is read as the hive's
  own for the write-back gate, so the notice is never suppressed as
  unsolicited, but it is likewise left open with one comment
  (`Superseded by #<keeper> … held out of the merge queue and returned to
  its author`) rather than closed: the merge train holds it as
  `duplicate-of#<keeper>` and the stage engine parks it at
  `stage/072-returned-to-contributor` (071 for a person, 070 for the App's
  own) with the tracker pill *Duplicate of #keeper · needs a pick* — never
  "Approved; waiting its turn" — and the 07x pass neither nudges nor adopts
  it ([#15469](https://github.com/hivecommons/hive/issues/15469): a relay
  login with no `author/*` label yet read as a person, the L4 gate refused
  the notice, and both PRs rode the merge train). Only when the earlier PR is **red or stale**
  (no update for 24h) **and** the later PR is green does the hive ask the
  owner: the later PR is parked `needs-human` → `stage/085-needs-decision`
  with a comment naming both PRs and the check states
  (`duplicate-fix-escalated`). A closed issue is left to the supersession
  sweep, held / `do-not-merge` PRs are skipped, a PR already carrying the
  sweep's `<!-- hive:duplicate-fix -->` marker is never written to again,
  and at most 10 writes are made per pass. A stage-074 **adoption pair** is
  not a duplicate: the App PR whose body says `Supersedes #<original>`
  carries the original's commits and closes it when it merges, so neither
  side of the pair is closed as a duplicate of the other
  ([#14977](https://github.com/hivecommons/hive/pull/14977), the adoptee of
  #14947, was closed this way before the exemption).

| Key | Default | Meaning |
|---|---|---|
| `scanner.skip_issues_with_open_pr` | `true` (unset) | Set `false` to turn off both the pre-dispatch guard and the duplicate-fix sweep; the claim ledger alone then decides, as before. |
| `governor.auto_close_superseded` | `true` (unset) | At ACMM L6, a **hive-authored** PR whose closing targets are all closed, one of them by a different merged PR, and whose head conflicts with the base (or whose files are a subset of that PR's) is closed by the hive itself as superseded — `hive/superseded`, one notice naming the superseding PR — in the self-authored auto-merge lane (before any review cycle), the supersession sweep, and close-on-merge of the superseding PR. Human-authored PRs are never auto-closed. Set `false` to report the refusal once and leave the PR for a person, as before ([#14965](https://github.com/hivecommons/hive/pull/14965)). See [labels-and-control-signals.md § Supersession](labels-and-control-signals/stage-labels.md#supersession-closed-issue-another-merged-pr). |

## What hive writes to GitHub, and when

Besides the writes an agent asks for through the relay (see
[github-write-surface.md](github-write-surface.md)), the hive writes to GitHub
on its own schedule. Every such *unsolicited* write — a comment, label,
review, issue edit or close on an item hive did not open and nobody opted in
on — passes through one gate, `writeback.Allowed(repo, item, kind)` in
`pkg/writeback`, before it reaches the GitHub client. The gate never blocks
hive's own PRs and issues, replies to explicit `/hive …` commands, work the
operator started from the dashboard, or agent kicks on items already
labelled for hive — with one exception: a repo under an operator pause
([repo-pause.md](repo-pause.md)) refuses **every** kind, own items included,
with reason `repo-paused`. The pause also silences the advisory digest
(`advisory digest skipped: repo paused`, once per update interval), which is
not a gate kind but a write all the same. Below the gate the sweeps still *scan*: sentinel findings
appear in **Security → Suspicious Activity → Sentinel findings** and
`/api/sentinel`, hold-gate mergeability notices become dashboard-only, and
PR stage tracking continues from internal state with no `stage/*` label on
the foreign PR.

The gate is per write-back *kind*. Each kind has a minimum ACMM level and
whether the review opt-in label (`review.require_labels`, default
`hive-review`) is required on the PR; both are overridable per kind under
`writeback.kinds.<kind>.{min_level, require_label, unlabeled_min_level}`.
The rationale for each default is the same question: does the hive have, at
that level, the power the write-back exists to govern?

| Path (code) | Trigger | What it writes | Target set | Gate kind → default |
| --- | --- | --- | --- | --- |
| Sentinel sweep + webhook (`pkg/github/sentinel_sweep.go`, `pkg/sentinel`) | 15-min reconcile; `pull_request` webhook | `<!-- hive-sentinel -->` "maintainer review required" comment + `sentinel-alert` label | every open PR in `project.repos`, human or bot | `sentinel` → L5 with opt-in label, L6 without; below: dashboard/API finding only |
| Hold guard (`cmd/hive/main_helpers.go` hold-drift, `maintainHeldPRMergeability`) | each governor eval | "⚠️ Hold-gate mergeability" / drift comment, re-applied `hold` label, **branch push** merging base | every `hold`-labelled PR, incl. human prow-held ones | `hold_gate_notice` → hive-authored PRs only, L≥4; never foreign PRs |
| Stage labeler (`pkg/github/stage_labeler.go`, `stage_writer.go`, `pkg/stagelabels`) | stage engine reconcile | `stage/NNN-*` add/remove, `hive:stage-needs-decision` comment, role/author labels | every tracked PR and issue | `stage_label`, `needs_decision_comment` → L5 + opt-in label on foreign PRs; issue stage labels follow the issue filter, not this gate |
| Supersession sweep (`pr_supersession_sweep.go`, `_autoclose.go`, `_resolve.go`) | hourly sweep | supersession notice comment, `hive/superseded` close, `needs-human` label | contributor PRs overlapping a hive PR | `supersession_notice` → L5 + label; `supersession_close` → L6 + label |
| Duplicate-fix sweep (`pr_duplicate_fix_sweep.go`, `open_pr_links.go`) | hourly sweep | `hive:duplicate-fix` comment, `has-open-pr` label, `hive/superseded` close | PR pairs fixing one issue | `duplicate_fix_notice` → L5 + label |
| Reporter-trust reconcile (`pr_reporter_trust.go`, `reporter_trust_wait.go`) | PR watcher | hold comment + `needs-human` | PRs tracing to issues from untrusted reporters | `reporter_trust_hold` → L5 + label. Held issues read **Triage needed** in the Activity Monitor from the moment the gate holds them and carry `needs-triage` (`reporter_trust.awaiting_label`) once the wait notice is posted. |
| PR↔issue linker (`pr_issue_link.go`, `pr_contributor_link.go`, `linked_prs.go`) | PR watcher | "covered by PR" label on the issue, link comments | issues referenced by any PR | `covered_by_pr_label` → L5 + label |
| Verdict / decision labels (`pr_decision_operator.go`, `human_decision_label.go`, `legacy_decision_labels.go`, `contribute_verdict_settle.go`) | review verdicts, dashboard decisions | `hive:verdict-*`, decision labels | reviewed PRs | `verdict_label` → L3 + label; dashboard-triggered decisions are solicited |
| Contributor adopt / nudge / bot takeover (`automerge/automerge_contributor_*.go`, `automerge_bot_takeover.go`) | auto-merge sweep | adopt comment, update-branch push, workflow approval, nudge comments | contributor PRs in the contributor lane | `contributor_adopt` → L6 + label |
| Adoption close-first (`automerge/automerge_adoption_close_first.go`) | auto-merge sweep, when a supersede adoption opens the hive's copy; close-first poll | close-first note, `hive/superseded` label and close on the original; reopen, copy-failed note and label removal when the copy fails | the contributor's original PR | `contributor_adopt` → L6 + label (an agent-authored original closes without the label, `adoption.close_superseded: on_adopt`); refused: the original stays open until the copy merges, and a due reopen waits for the next poll |
| Claims (`prclaims.go`, `claimed_by_label.go`, `cmd/hive/claimswire.go`, `claimescalation.go`) | claim ledger changes | `claimed-by:*` labels, claim/escalation comments | claimed issues | `claim_comment` → L4 + label (hive-claimed issues are own items) |
| Issue close / task-list / decomposed parents (`issue_close.go`, `task_list_sweep.go`, `decomposed_parents.go`, `close_on_merge.go`, `tracking_issue.go`) | merges, sweeps | close, body edits (checkbox ticks), `Refs` rewrites | issues linked from hive PRs | `issue_close`, `parent_issue_edit` → L5 + label; hive-filed issues are own items |
| Gate comments (`pkg/scheduler/clarity_gate.go`, `prior_art_gate.go`, `triage.go`, `issue_blocked.go`, `issue_recheck_soak.go`) | scheduler intake | clarity/prior-art/triage questions, `blocked` labels | issues admitted by the issue filter | `gate_comment` → L5 + label; items carrying the intake label are opted in |
| Welcome / outreach (`pkg/gfi`, `gfi_forge.go`, `pr_clanker_requested.go`) | good-first-issue selection | welcome comment, `good first issue` label | issues hive advertises | `welcome_comment` → L5 with label, L6 without |
| Issue filing (`pkg/upstreamwatch/github_filer.go`, `advisory.go`, scanner/deps/regression agents, `dedupe_advisory_close.go`) | agents, advisory digest, upstream watch | **new** issues hive authors, pinned advisory issue + comments | `project.repos` | `issue_file` → L3, no label (this is what listing a repo opts in to; `per_repo.mode: issues-only` keeps only this) |
| Shared-CI rerun (`shared_ci_rerun.go`) | CI failure watcher | `rerun-failed-jobs`, `hive-shared-ci-rerun-<n>` comment | PRs with failed shared workflows | `ci_rerun` → L6 + label |
| Level hold, self-authorization & reporter-trust releases (`pr_level_hold.go`, `automerge/self_authorization_release.go`, `reporter_trust_release.go`) | PR watcher | `hold` label add/remove | agent PRs | own items (hive-authored) — not gated |
| Migrations & retirements (`hive_hold_migration.go`, `hive_id_label_retire.go`, `claimed_by_human_migration.go`, `origin_labels.go`) | boot, one-shot | label renames | items carrying hive labels | opted in by carrying hive labels |

Paths not yet routed through the gate are counted by the ratchet
(`pkg/writeback/testdata/direct-writes-baseline.txt`): a test fails on any
new direct `CreateComment` / `AddLabels` / `CreateReview` / `Edit` call
outside `pkg/writeback`, and every migration lowers the baseline.

### Configuration

```yaml
writeback:
  unsolicited:
    min_level: 5          # raises every per-kind floor; never lowers one
    require_labels: true  # foreign PRs also need review.require_labels (fallback hive-review)
    backfill: false       # never touch items that predate first_seen / first write access
  per_repo:
    torch-spyre/torch-spyre: { mode: issues-only }   # off | issues-only | labels-only | full
  kinds:
    sentinel: { min_level: 5, require_label: true, unlabeled_min_level: 6 }
  dedupe:                 # content-aware suppression, see below
    enabled: true
    same_item_ttl: 24h
    burst_items: 2
    burst_window: 60s
    hourly_cap: 60
```

Everything is editable under **⚙ → Features → Autonomy & ACMM → Write-back
containment**, which also shows the per-kind matrix, a counter of suppressed
writes in the last 24 h with a drill-down (`GET /api/writeback/suppressed`),
and the backfill banner. `/api/status` carries the same state under
`writeback`. The per-repo mode is also set from each repo's Projects card.

### Backfill guard

The gate persists `first_seen` per repo in `/data/metrics/writeback.json`
(`HIVE_WRITEBACK_STATE_PATH`), and records the moment a repo first answers
2xx after 403/404s as `writable_at`. Items created before the later of the
two are never written to unsolicited while `backfill: false`. The sweep logs
one line — `writeback: skipped N pre-existing items on <repo> (backfill
off)` — and the dashboard shows "hive can now write to `<repo>`; N existing
PRs were left untouched — enable backfill to process them".

### Duplicate-write suppression

Policy decides whether a kind may write to an item; the content layer under
`writeback.dedupe` decides whether *this* write is worth making. It applies
only to comment kinds whose body the caller handed the gate as the preview
(`sentinel`, `needs_decision_comment`, `supersession_notice`,
`duplicate_fix_notice`, `reporter_trust_hold`, `contributor_adopt`,
`claim_comment`, `gate_comment`, `welcome_comment`); `Allowed()` without a
preview — labels, closes, hold-gate upkeep, task-list edits — stays
policy-only. Three checks run after the policy gate allows a write:

| Check | Key | Suppression reason | Default |
|---|---|---|---|
| Same item | `(repo, item, kind, hash)` of the body with `<!-- hive:… -->` marker arguments, SHAs, RFC 3339 timestamps, durations and whitespace normalised away | `duplicate` — "identical `<kind>` already written to #N at `<time>`" | `same_item_ttl: 24h` |
| Burst across items | `(repo, kind, hash)` of the body additionally stripped of `#N` references, `@mentions` and digits | `burst` — "same `<kind>` already sent to #A within 60s; held to avoid a notification burst"; one WARN per burst | `burst_items: 2`, `burst_window: 60s` |
| Hourly cap | `(repo, kind)` writes in the last hour | `rate-capped` | `hourly_cap: 60` (negative disables) |

Holds are suppressions, not a queue: nothing is replayed later, the next
sweep simply asks again. Merge-path kinds are exempt from the hourly cap
(`hold_gate_notice`, `verdict_label`, `needs_decision_comment`,
`contributor_adopt`, `ci_rerun`, `issue_close`, `supersession_close`,
`issue_file` and every label kind; `writeback.RateCapExemptKinds`). The
hash table is persisted beside the repo records in `writeback.json` so a
restart does not re-fire a notice, pruned by TTL and bounded to 5000
same-item entries. Suppressions appear in the drill-down with the reasons
above, and `/api/status` → `writeback.dedupe` carries the settings plus the
last-hour counters `dedupe_same_item`, `dedupe_burst` and `rate_capped`.
`enabled: false` turns the layer off.

## Suspicious-activity alerts (`sentinel`)

The sentinel evaluates open PRs in the watched repos — human, bot, or agent —
and, when a PR matches one of the behaviors below, posts one
`<!-- hive-sentinel -->` comment per head SHA listing the matched rules and
paths (`pkg/sentinel`, `pkg/github/sentinel_sweep.go`). Since
[#13379](https://github.com/hivecommons/hive/issues/13379) it runs in two
ways. A verified `pull_request` `opened`/`synchronize`/`reopened` webhook
evaluates that one PR at once, reading only the PR and its changed files
(`SweepSentinelPR`); deliveries for the same PR coalesce while one is
pending. The full sweep (`SweepSentinel`) is the reconcile cadence: every
`github.SentinelReconcileInterval` (15 minutes) it enumerates open PRs from
the ForgeState cache when `forgestate.enabled` is on (registered at boot by
`cmd/hive/forgestate_boot.go`; with the cache off it lists the repo as
before), skips every head it has already
evaluated with no GitHub call, reads the remaining candidates once each, and
so bounds how long a PR whose delivery was lost sits unevaluated. The log
line `sentinel sweep complete` carries `cached_repos`/`listed_repos`. Both
paths share one option set and the detection, comment and label logic. For untrusted authors, it also adds the
alert label. The label is a hard hold: while present, Hive will not submit an
APPROVE review, apply LGTM/approval labels, or merge the PR through any
auto-merge lane (dashboard queue, self-authored App sweep, trusted bot authors,
or trusted authors). Trusted authors (the Hive App, configured trusted bots and
owner/trusted-author allow-list entries) default to an informational notice and
audit/dashboard finding only; no alert label is added unless the operator opts
into blocking them. A maintainer who removes the label keeps the PR clear until
the author pushes again: the sweep reads the clearance back from the alert
comment it left for that head (alert comment present, label absent), so it
survives a hub restart or a second replica and nothing else needs to be posted.
A dashboard **Approve** is the same clearance for the head it names: the hive
takes the label off as its own account, so the `unlabeled` event alone is not
read as a human clearance; the `/lgtm` + `/approve` comments the approval
posted carry a `hive:pr-approval head=<sha>` marker, and the sweep reads that
as the release for exactly that head (rewriting its alert comment to say so),
so it does not re-apply the label the owner just released (dibs#510). A later
push re-arms it.

| Behavior | Fires when |
|---|---|
| `sensitive_path` | Any changed file matches a sensitive-path glob. |
| `owner_self_nomination` | The PR author adds their own login to an OWNERS/CODEOWNERS/MAINTAINERS file. |
| `permission_escalation` | A workflow/CI file widens `permissions:` (`write-all`, `contents: write`, `id-token: write`, …), or a role/RBAC manifest grants `*`/cluster-admin. |
| `secret_exposure` | Added lines look like credentials (PEM blocks, AWS/GitHub/Slack tokens, `password=`), or a workflow echoes/uploads `secrets.*`. |
| `ci_gate_weakening` | Added `continue-on-error`, removed a required-check/branch-protection line, deleted a workflow or test job, added `--no-verify`/`skip ci`. |
| `test_removal` | Tests are deleted or heavily reduced while production code is not. |
| `security_policy_edit` | SECURITY.md, GOVERNANCE.md, `.github/rulesets/**`, dependabot/codeql config, or Hive's own policy/proxy rules change. |
| `remote_code_execution` | Added `curl … \| sh`, `wget … \| bash`, `eval $(curl …)`, base64-decoded execution, or an unpinned third-party action in a workflow. |

| Key | Default | Meaning |
|---|---|---|
| `sentinel.enabled` | **on** when unset | `false` disables the sweep entirely. |
| `sentinel.label` | `sentinel-alert` | Label added to untrusted flagged PRs (created red if missing). Neutral on purpose: it means "look closely", not "malicious". This configured label is also added to Hive's hold/exclude set and blocks Hive approval plus every auto-merge lane until a human removes it. |
| `sentinel.trusted_authors_block` | `false` | When `false`, trusted author findings are notice-only: comment, audit/dashboard record, no label, no approval/merge block. Set `true` to add the blocking label for trusted authors too. |
| `sentinel.sensitive_paths` | shipped defaults (`sentinel.DefaultSensitivePaths`) | Glob list (same syntax as `intent.guardrail_path_patterns`; `**` crosses directories) that **replaces** the defaults when set. Defaults cover OWNERS/CODEOWNERS/MAINTAINERS, SECURITY.md, GOVERNANCE.md, `.github/workflows/**`, actions, dependabot, codeql, rulesets, CI config, Makefile/Justfile, pre-commit hooks, `policies/**`, `hive.yaml*`, `gh-wrapper*`, proxy rules, Dockerfiles, `install.sh`, dependency manifests and lockfiles, release workflows, `.env*`/key material, `deploy/**`, Terraform, Helm/k8s manifests. |
| `sentinel.disabled_behaviors` | empty | Rule names from the table above to switch off. Unknown names are rejected. |
| `sentinel.exempt_logins` | empty | GitHub logins never flagged (e.g. a release bot). Use sparingly. |
| `sentinel.repos` | empty = all watched repos | Optional `owner/repo` allow-list. |
| `sentinel.max_actions` | `20` | Caps label/comment/remediation actions per pass. |

Owners edit all of this from **Settings → Security → Suspicious Activity**;
the dashboard writes the owner-only `/api/config/governor/security` overlay.
Every alert is also written to the dashboard audit log as `sentinel-alert`,
and, when `evidence.enabled` is on, recorded under `sentinel` in the flagged
head's [review evidence bundle](review-evidence.md).

On the dashboard's Projects cards, every open PR that carries the configured
sentinel label is drawn as a red pill (status chip and main pill), taking
precedence over the held, needs-human and merge colours, and the pill legend
has a matching entry. The red clears on the next status refresh after a
maintainer removes the label. Trusted-author PRs that only received a sentinel
comment are not red.

## Image provenance and tags

Pre-built images are published by [`.github/workflows/docker.yml`](../../.github/workflows/docker.yml) to `ghcr.io/hivecommons/hive` (plus `hive-contributor` and `hive-hub`) and mirrored **by digest** into the matching `ghcr.io/kubestellar/*` packages. Post-transfer, `hivecommons` is the native publishing org; the workflow retags the already-built digest into `kubestellar` so that spokes still pinned to the old org keep resolving, and both orgs serve digest-identical manifest lists for the same tag. (A missing cross-org credential is a hard failure in that direction precisely because a one-sided publish would leave `kubestellar` serving stale tags to live spokes.)

A build of a release line publishes, in one multi-architecture manifest operation:

- `ghcr.io/hivecommons/hive:<line>-latest` — the rolling tag for the current HEAD of that branch: `v4-latest` on `v4`, `v5-latest` on `v5`, `v6-latest` on `v6`;
- `ghcr.io/hivecommons/hive:<git-short-sha>` — immutable per-commit tags;
- that line's moving **release channel**, if it owns one — `v5` merge builds own `:candidate` (and `:latest`), `v6` merge builds own `:edge`; `v4` is a maintenance line and publishes **no** channel, only `v4-latest` and short-SHA tags ([#7721](https://github.com/hivecommons/hive/issues/7721) Phase 1). Channels are retags of the same digest; see [release-channels.md](release-channels.md).

> **`:latest` follows `v5`.** Only `v5` builds move the global `:latest` tag (`INCLUDE_LATEST` is `false` in `docker.yml` on `v4` and `v6`), so it tracks the same digest as `candidate`. The cross-line ambiguity previously tracked in [#6711](https://github.com/hivecommons/hive/issues/6711) — where `:latest` alternated between lines depending on which run finished last — was resolved by the [#7721](https://github.com/hivecommons/hive/issues/7721) Phase 1 channel remap. For production, still prefer a named channel (`stable`) or a digest over `:latest`.

Channel ownership is deliberately **per-line**: without the split, every merge to one line would silently re-point another line's channel back onto its own build minutes after a deliberate promotion (`src/scripts/publish-image-tags.sh`). Two consequences follow that are easy to get backwards:

- `:stable` is **not** published by a branch build at all. The separate stable-promotion workflow advances it by digest to the newest `v5` build that crossed the 24-hour line, after the [soak gate](stable-soak-policy.md) passes.
- `:edge` rides `v6`, so it is an **active-development build of the next line**, not a fresher `:stable`. It is the newest build, not the most proven one.

PR and short-lived branch builds compile the image as a CI gate, but only the long-lived release lines (`v4`, `v5`, and `v6`) push tags. Before tagging, the workflow verifies its SHA is still branch HEAD, so a stale queued build cannot move a rolling tag backward.

> **Note:** `v2-latest` was the rolling tag of the retired `v2` branch. Do not use it for new deployments — prefer `stable` for production, or pin a digest. (`src/docker-compose.yaml` was bumped off it in #4206; standalone image references now come from one source of truth, [`src/deploy/standalone-images.sh`](../deploy/standalone-images.sh).)

### One source of truth for standalone assets

Standalone deployment assets do not carry their own image references. They take
them from [`src/deploy/standalone-images.sh`](../deploy/standalone-images.sh),
which names the Hive, gateway, and auto-update-profile images in fully
qualified form. Change a reference there rather than in an individual asset.

Fully qualified is deliberate: `podman auto-update` refuses a short name, and
Podman's Quadlet generator warns on one, so a reference that resolves through
the host's `unqualified-search-registries` is not portable across the two
runtimes. Docker may still spell the same image without the `docker.io/`
prefix — the contract test compares the normalized forms, so the two spellings
are equal as long as they name the same image. Digest pins are carried through
unchanged and are checked for as well.

[`src/deploy/test_standalone_image_refs.sh`](../deploy/test_standalone_image_refs.sh)
runs in CI and fails the build when an asset stops agreeing with that file,
when a digest pin is dropped, or when the retired `v2-latest` tag reappears. It
already covers the Podman asset paths, so drift detection switches on by itself
when those assets land.

### Choosing a tag

- **Production:** `stable`, or pin a digest for full reproducibility.
- **Following mainline:** `v4-latest`.
- **Reproducing an incident:** the `<git-short-sha>` tag from the workflow run.

### Verify and pin a digest

```bash
docker buildx imagetools inspect ghcr.io/hivecommons/hive:stable
docker pull ghcr.io/hivecommons/hive@sha256:<digest>
```

In Compose, replace the tag with the digest form:

```yaml
services:
  hive:
    image: ghcr.io/hivecommons/hive@sha256:<digest>
```

To relate an image to source, compare the `<git-short-sha>` tag published by the same workflow with commits on `v4`, or inspect the Docker workflow run for the commit SHA that produced the digest.

## Scheduled-upgrade progress and history

A hub-instructed upgrade is a short state machine, and both dashboards expose it as data so a window that does not roll is never silent ([#14856](https://github.com/hivecommons/hive/issues/14856)). The hub derives it per hive on `/api/saas/my-hives` (`upgradeState`, `upgradeHistory`), and the spoke derives the same vocabulary locally on `/api/status.upgradeProgress` from its drain latch, self-upgrade marker, last landed outcome and the hub's heartbeat policy (`upgrade_policy.next_window_at`, `upgrade_policy.upgrade_history`).

| State | Meaning | Extra fields |
| --- | --- | --- |
| `scheduled` | Nothing in flight. | `nextWindowAt` — the hive's own daily/weekly slot (always, even on `stable`, where `nextUpdateAt` is the channel's promotion ETA instead). |
| `draining` | The spoke received the instruction and is waiting for kicked agent turns to end. | `drainDeadline`, `drainRemainingS`, `agentsInTurn[]` (`agent`, `elapsedS`), `agentsTotal`; `detail` reads `2/5 agents in turn: reviewer 38m, scanner 12m — proceeds in 40m`. |
| `pulling` | Instruction delivered; the spoke acknowledged it, or (older build) may be draining without reporting it. | `reason` qualifies a silent spoke or a Deployment-patch fallback. |
| `rolling` | No heartbeat since the instruction — the pod is restarting onto the target. | |
| `verifying` | The spoke is back and reports the target commit; the hub has not yet closed the attempt. | |
| `done` | The most recent attempt landed (shown for 24 h, then `scheduled`). | `endedAt` |
| `missed` | The attempt or window did not land. | `reason`: drain lapsed, hub restarted, spoke unreachable, image pull failed, abandoned after N attempts, or the schedule gate's hold reason while the window is still open. |

**Drain window.** When the instruction arrives mid-turn the spoke holds the restart for up to `hub.upgrade_drain_timeout_s` (default 45 min) and reports what it is waiting on every heartbeat (`upgrade_drain.turns`). The hub's orphan sweep leaves a draining spoke alone for that window plus a 20-minute grace; the countdown both dashboards show is anchored on the last heartbeat that carried the report.

**History.** The hub persists the last `HIVE_HUB_UPGRADE_HISTORY_MAX` (default 14) attempts and held windows per hive under `/data/saas/upgrade-history/<hive>.json`, so they survive a hub restart. Each row carries `target`, `from`, `trigger` (`scheduled` / `instant` / `manual` / `observed` / `window`), `startedAt`, `endedAt`, `outcome` (`done` / `missed`) and `reason`. A daily window the gate held records exactly one `window` row per calendar day (ET), updated in place if the hold reason changes; a window that fired records the attempt instead. Deleting a hive drops its history.

**Where it shows.** On the hub, the hive card's Version cell gains a pill for the `draining`, `pulling`, `rolling`, `verifying` and `missed` states — `Draining · 14m left`, with the agents still in turn (`2/5 agents in turn: reviewer 38m, scanner 12m`) or a missed window's reason in the pill's hover title so the Version column stays narrow — and a `history` badge whose tooltip lists the recent attempts (it turns red and counts the misses when any are present); a quiet `scheduled` row renders as before. On the spoke, `/api/version` carries the same `upgradeProgress` and `upgradeHistory`, and the operator dashboard's version menu shows a **Progress** row with the pill and an **Upgrade history** list; the header chip adds a `Draining 14m` / `Missed window` pill while either state holds.

**Upgrade now (interrupt turns).** While a hive is `draining` or `missed`, its owner sees an *Upgrade now (interrupt turns)* action on the hub card. It is the ordinary upgrade request — `POST /api/saas/hives/{id}/upgrade?interrupt=1`, same owner/admin check as every other hub mutation — with an interrupt flag the hub audit-logs (`audit: hosted hive upgrade requested … interrupt_turns=true`), records on the hive timeline, and hands to the spoke on the next heartbeat (`upgrade_interrupt: true` beside `upgrade_to`). The spoke then skips the drain and restarts at once; the agents listed as in-turn are cut and their owners must re-run those turns. The request is held in hub memory for 2 hours and cleared when the upgrade closes, so a stale interrupt never cuts a later upgrade's turns; a hub restart drops it (the spoke falls back to draining). While it is armed the card shows `interrupt requested` instead of the action.

## Governor cadence and budget

- Agent cadences are evaluated from persisted state: the last-kick map lives in `/data/hive-state.json` and is honored across pod restarts — a Deployment roll does **not** re-kick every cadenced agent at boot ([#3817](https://github.com/hivecommons/hive/pull/3817)). A fresh install (no persisted state) still kicks every cadenced agent on the first eval. There is no global default interval; a zero/absent interval means the agent is never cadence-kicked.
- Continuous mode is a generic per-mode cadence value for any configured agent: set a mode cell to `continuous`, optionally with `continuous_cooldown` (default `60s`) and `continuous_budget_pct` (default `80`); `scanner` is only a common first user, not a special case. The legacy `agents.<name>.continuous: true` bool remains a shorthand for every non-QUIET mode. Continuous uses the agent manager's existing turn-end signal — the CLI is back at its input prompt and no work marker is active — to schedule `nextKick = ended_at + cooldown`; the run loop wakes on that time (at most one early eval per 30s, backing off 1m→15m while the eval cycle keeps skipping a due agent for a dashboard pause, first-scan deferral, provider-budget gate, or upgrade drain) instead of waiting for the next cadence eval tick, and status exposes `lastKickEnd` / `idleSince` for the gap. Enabling continuous on an idle agent, including entering a mode whose cadence is `continuous`, schedules its first continuous kick after one cooldown; entering a mode where the agent is not continuous clears pending continuous kicks and status reports `continuousBlocked: "not_in_mode"`. It supersedes interval cadence while active, but only after an observed session end or idle enablement. It never types into a busy session and still respects `enabled: false`, operator/fleet-breaker pause, governor-mode `pause`/`off` (including QUIET), on-demand, non-kick channels, provider rate-limit/quota backoff, budget/provider holds, and upgrade/restart holds. The continuous budget guard stops re-kicks once the current token-budget window reaches `continuous_budget_pct` of `governor.budget.total_tokens`; normal cadence resumes until the window falls below the threshold or resets. Failed deliveries back off exponentially (capped) and `/api/status` exposes `continuous`, `continuousModes`, `continuousBlocked`, any `continuousBackoff`, and per-agent `continuousKicks` / `continuousTokens` counters. If a kicked turn never reports an end (missed pane turn-end signal), `continuous_turn_ceiling` (default `45m`, `-1` disables) presumes it over, logs `continuous turn never reported an end`, and re-arms after the cooldown rather than idling until a pod restart; a non-zero `presumedTurnEnds` in status means turn-end detection is missing for that agent. Operators set per-mode `Continuous` values in the agent settings **Cadences** form; the Governor cadence table is read-only and shows an `∞ continuous` chip in each continuous mode cell.
- Manual dashboard/API kicks that rely on Hive's generated work list wait for the first governor scan after boot. Until that scan populates the scheduler snapshot, `POST /api/kick/{agent}` returns `202` with `status: "deferred"` and Hive delivers one deduplicated kick for that agent as soon as the first scan completes.
- Kick-visibility conditions surfaced on the dashboard include `copilot-question-form`, which means the Copilot CLI asked an unattached human for clarification; Hive dismisses that form with Escape and retries delivery instead of dropping the kick.
- The governor token budget uses a rolling window of `governor.budget.period_days` (default 7 days), with a soft warning at `governor.budget.critical_pct` (default 90%). When spend reaches the limit, kicks are suppressed for all agents except those explicitly budget-exempt.
- Bob coin budgets are configured under the same governor budget window. The coin conversion comes from your Bob account/team; the values below are illustrative only (they are not Hive's built-in defaults and not public Bob pricing guidance) — always set them to the conversion your Bob team gives you:

```yaml
governor:
  budget:
    usd: 25.00                    # optional period cap in Bob-equivalent USD
    coins:
      bob:
        tokens_per_coin: 1000000  # illustrative only; use the conversion your Bob team gives you
        usd_per_coin: 1.00        # illustrative only; use the conversion your Bob team gives you
        label: "BC"
        budget: 50                # optional period cap in Bob coins
```

  `HIVE_BOB_TOKENS_PER_COIN`, `HIVE_BOB_USD_PER_COIN`, `HIVE_BOB_COIN_BUDGET`, and `HIVE_BOB_USD_BUDGET` override those values at runtime. `budget.usd` and `budget.coins.bob.budget` are first-class caps: set either or both. When both are positive, the governor trips on whichever cap is exhausted first and surfaces the exhausted unit. The Cost panel shows Bob coins alongside the configured USD equivalent so operators can enter and compare either unit. Coin conversion values are account-specific; always confirm the conversion with your Bob team rather than relying on Hive's built-in fallback values.

### Kick size and the list caps

Every kick is typed into the agent's terminal after a `/clear`, so the whole prompt is paid for — in model input tokens and in delivery time (about 400 runes/s, so roughly 2.5 s per KiB) — on every cadence tick. `governor.kick_limits.max_issues` / `max_prs` are the only knobs that change that size without editing a policy, which makes it tempting to lower them. The numbers below ([#12133](https://github.com/hivecommons/hive/issues/12133)) show why the shipped defaults stay at `100` / `50`.

**Where the bytes go.** Measured with `go test ./pkg/scheduler -run TestKickSizeMeasurement -v` (an ACMM L6 `scanner` in `ISSUES_PRS_MERGE` on the shipped `scanner-automerge.md`, three authorised repos, 70-character titles, merge-eligible and CI-failing lists scaled with the backlog — 100 and 20 at the 302-PR row):

| Open issues / PRs | Caps | Kick size | Of which lists |
|---|---|---|---|
| 0 / 0 | 100 / 50 | 32.9 KiB | — (policy, seams, overlays, repos) |
| 10 / 5 | 100 / 50 | 35.8 KiB | 2.9 KiB |
| 25 / 10 | 100 / 50 | 38.8 KiB | 5.9 KiB |
| 40 / 20 | 100 / 50 | 43.1 KiB | 10.2 KiB |
| 60 / 30 | 100 / 50 | 47.7 KiB | 14.8 KiB |
| 100 / 50 | 100 / 50 | 56.8 KiB | 23.9 KiB |
| 302 / 302 (the [#7368](https://github.com/hivecommons/hive/issues/7368) backlog) | 100 / 50 | 63.4 KiB | 30.5 KiB (issues 11.5, PRs 6.9, merge-eligible 4.8, CI-failing ≈ 7) |
| 302 / 302 | 40 / 20 | 47.0 KiB | 14.1 KiB |

The fixed cost — the policy text and the mandatory seams — is 32.9 KiB: 52 % of the worst-case kick and ~85 % of a typical one. Lowering the caps to `40` / `20` saves nothing until a repo set has more than 40 open issues or 20 open PRs, and at most ~16 KiB (≈ 40 s of typing) at the 302-PR extreme. It would also make the merge-eligible list drain a 100-PR automerge queue in five kicks instead of two, which at a 10-minute cadence is the starvation the caps were sized against ([`docs/scale-envelope.md`](scale-envelope.md)). The lever that actually moves kick size is the policy itself, not the list caps.

**Watch it rather than guess.** Each rendered kick logs one line:

```
level=INFO msg="kick rendered" agent=scanner repo="" kick_bytes=58123 issues_total=137 issues_listed=100 prs_total=61 prs_listed=50 max_issues=100 max_prs=50
```

`kick_bytes` is the prompt as typed (canary preamble included). When `issues_listed` / `prs_listed` sit well below the caps on every kick, lowering them changes nothing; when `*_total` is persistently far above `*_listed`, the backlog — not the cap — is the problem, and the overflow marker in the prompt already tells the agent the list is partial. Per-agent kicks share the same global caps; set them under **Settings → Repos → Kick prompt list caps** or in `governor.kick_limits`, with `0` meaning unlimited.

### What consumes tokens while agents are paused

Pausing agents (including the navbar fleet breaker) does not take the Hive instance down: the dashboard, governor scans, GitHub polling, token scanners, repo-cost collectors, telemetry/heartbeats, and provider-headroom probes can keep running. Those components should not make model calls by themselves. Model usage is expected from agent sessions reached through governor/manual/CEL/resume kicks; those kick paths share the same pause and budget gates. Knowledge priming reads configured knowledge stores and attaches facts to kicks; it does not independently call the model while every agent is paused. Provider-budget probes are the exception by design: after a provider spend-limit rebuff, Hive may release one kick after `governor.provider_budget.probe_interval_s` to test recovery.
- The Governor dashboard **PRs by model** panel includes rework evidence for 7d/30d/all windows: first-pass merge rate, average/worst review rounds, fix attempts, follow-up commits after first review, human change requests, median time to merge, and a top-10 **Most reworked PRs** list. The data is served by `/api/governor/pr-models` from the same cached PR snapshot as the model outcome counts.
- The **provider** spending limit is a separate signal from the token budget above ([#4294](https://github.com/hivecommons/hive/issues/4294)): the token budget counts what the hive spends, while this is the inference gateway refusing to spend more money — a LiteLLM key past its daily dollar cap, a project out of quota, an account out of credit. It is detected from the gateway's own error body (never from a bare 429, which stays on the ordinary retry path), raises an error-level dashboard alert naming the limit that was hit, and withholds every agent kick while it is in force. It does **not** pause agents: pause state stays a human decision.
### GitHub API budget governor

Spoke GitHub App installations share one core REST bucket across the scan loop and all agent-requested writes. Hive now derives an API budget mode from the observed core rate-limit window:

| Mode | Condition | Default behavior |
| --- | --- | --- |
| `normal` | `remaining >= github.api_reserve` | Full eval fan-out. Optional sweeps run every `governor.optional_sweep_every_n_cycles` cycles (default `1`). |
| `conserve` | `remaining < github.api_reserve` (default `800`) | Critical paths and unstick steps (escalation, SHA hold, supersession sweep, shared-CI rerun) continue; optional advisory/recommendation/duplicate/review-thread/collector enrichment sweeps are skipped. Eval interval is multiplied by `governor.conserve_interval_multiplier` (default `2`). |
| `critical` | `remaining < github.api_critical` (default `250`) | Same shedding as conserve plus the unstick steps, with the eval interval stretched by `4x`. |

Mode exits use hysteresis: Hive leaves conserve/critical only after quota rises above the threshold plus 100 calls or after the reset time passes. The effective interval is capped by `governor.eval_interval_max_s` (default `1800`). `/api/status` and `/api/gh-rate-limits` expose `api_budget` with the mode, remaining/limit/reset, mode `since`, `skipped_steps`, `unstick_steps` and `enrichment_steps`; the dashboard shows an API budget pill beside the GitHub rate-limit display.

- Recovery from a provider spending limit is automatic, via a probe. Withholding kicks also withholds the inference calls that would reveal the provider is serving again, so the hive suppresses only while the last refusal is recent and then lets a single kick through to test the gateway; the probe re-arms suppression the moment it is released, so at most one probe run flies per interval. A still-clipped key refuses the probe and suppression resumes for another interval; once the provider's window resets the probe succeeds, normal kicking resumes with no operator action, and a one-time recovery notification is sent (the entering notification is likewise sent once per clip, not once per cycle). Tune with `governor.provider_budget.probe_interval_s` (default 1800 — 30 minutes):

```yaml
governor:
  provider_budget:
    probe_interval_s: 1800
```

  Lower it to resume sooner after a reset at the cost of a rebuffed run per probe; raise it to waste less while noticing later.

## Merge-hotspot advisor (`governor.hotspot_*`)

The hive already predicts PR file overlap for the merge train. The advisor reuses that evidence — the train's per-PR file lists and the conflicts it hands to the rebase resolver, plus the rebase follow-through's conflict records — to find files that concurrent PRs keep colliding on, and files one modularize issue per hotspot for the ci-maintainer lane. It never lists PRs or files from GitHub itself; its only reads are the hotspot file's current content (size and top-level symbols) and the state of issues it filed earlier.

- A file is a **hotspot** when, over the rolling window, at least `hotspot_min_prs` distinct PRs touched it and at least `hotspot_min_overlaps` pairs of them were open at the same time. Score = concurrent pairs × a size weight (1 / 1.25 / 1.5 / 2 at 800 / 1500 / 4000 lines) + 2 × attributed conflicts. Hunk-level "same region" weighting is not available from the train's records and is skipped.
- **Don't splinter.** A split is only recommended for code when the file has at least `hotspot_min_lines` lines **and** two or more top-level symbol groups (Go: funcs and types via `go/ast`, methods grouped under their receiver; JS/TS/Python: top-level declarations; Markdown: `##` headings). The proposal targets at most 4 files of at least 150 lines, no behaviour change, exported API unchanged, tests move with the code.
- **By kind.** Docs (`*.md`, `*.rst`, `*.txt`) are reported but only recommended for a by-section split above 1500 lines (the `docs/api-reference/<area>.md` pattern). Generated files (`// Code generated … DO NOT EDIT`, `*.pb.go`, the dashboard SPA under `pkg/dashboard/static/` → `static-src/`) point at their source — unless that source tree is already split into more than 4 files, in which case they are report-only (regeneration makes the output touch every source change). Lock and data files are report-only.
- **One issue per file.** Title `[ci-maintainer] refactor: <path> is a merge hotspot — split into <N> units` (the tag names the same agent as the label, as every hive-filed issue title does), labels `agent/ci-maintainer` + `kind/cleanup`, origin `governor`, body marker `<!-- hive:hotspot file=<path> -->`, evidence table, the top-level units, a concrete split proposal and the don't-splinter rule. While that issue is open no second one is filed; after it closes the file waits `hotspot_refile_days` before it can be filed again. **Capped.** At most `hotspot_max_open` modularize issues are open per repo at once (an issue that already has a PR still counts) and at most `hotspot_max_per_pass` are filed per advisor pass; the highest-ranked uncapped hotspot is filed when a slot frees, and the advisor logs one INFO line per pass when the cap holds it back. Issues go through the writeback gate as `issue_file` (a paused repo or `writeback.per_repo[<repo>].mode: off` stops them) and are skipped when the GitHub budget governor is skipping optional sweeps.
- **Where to look.** `GET /api/hotspots` (or `hivectl governor hotspots [--repo owner/name]`) lists every hotspot with its recommendation and issue; `/api/status` carries `hotspots: {count, actionable, filed}`. State lives in `/data/hotspots.json`; the analysis runs at most hourly in the eval cycle, the intake on every merge-train sweep. A PR the train has not seen for two hours has its interval closed at its last sighting.

| Key | Default | Meaning |
|---|---|---|
| `governor.hotspot_enabled` | `true` | Set `false` to keep the report and `/api/hotspots` but never file an issue. |
| `governor.hotspot_window_days` | `7` | Rolling window of merged and open PRs considered. |
| `governor.hotspot_min_prs` | `6` | Distinct PRs touching a file in the window before it is reported. |
| `governor.hotspot_min_overlaps` | `3` | Concurrent PR pairs on the file before it is reported. |
| `governor.hotspot_min_lines` | `800` | Smallest code file a split is recommended for. |
| `governor.hotspot_refile_days` | `30` | Wait after a modularize issue closes before the same file may be filed again. |
| `governor.hotspot_max_open` | `2` | Most modularize issues open at once per repo; no new one is filed until one closes. |
| `governor.hotspot_max_per_pass` | `1` | Most modularize issues filed in one advisor pass. |

## Delivery SLA

The **Throughput** section of the dashboard carries a **Delivery SLA** block
(below the merged/opened metric tiles; moved there from the Overview) that
answers "how long does it take this hive to turn an issue into a merged PR?"
without spending any GitHub API quota. Everything is derived from the
lifecycle timeline the hive already keeps ([#11841](https://github.com/hivecommons/hive/issues/11841)
follow-on): the issue's `created_at` retained when it is enumerated, the
`classified` stage, the `started` entry written with the `stage/030-pr-in-progress`
label (formerly `hive/in-progress`), the `pr_opened` entry recorded when a hive PR that `Fixes`/`Closes` the
issue is opened, and the merge observed by the PR watcher, the close-on-merge
ledger or the audit stream, fanned out to every issue the PR linked.

Per repo and fleet-wide, five spans are aggregated over rolling 7-day and
30-day windows as `{p50_h, p95_h, n}` (nearest-rank percentiles, hours):

| Span | From → to |
|---|---|
| `open_to_triage` | issue created → classified |
| `triage_to_start` | classified → `stage/030-pr-in-progress` (`started`) |
| `start_to_pr` | `started` → linked PR opened |
| `pr_to_merge` | linked PR opened → merged |
| `open_to_merge` | issue created → merged |

A journey enters a window by the timestamp of the span's *end* event, so a
PR merged today counts in today's `pr_to_merge` even if it was opened last
month.

In the dashboard the three headline tiles (start → PR, PR → merge,
open → merge) sit under the Throughput merged/opened metrics and follow its
repository filter; they show the 7-day percentiles for every period pill
except `all`, which switches to the 30-day window. **Repos ▾** expands a
per-repo table and the stale list, and the collapsed Throughput summary line
appends the open → merge p50 and the stale in-progress count.

**History.** Daily buckets (`day`, p50 of `start_to_pr`, `pr_to_merge` and
`open_to_merge`, plus `n_started`, `n_pr`, `n_merged`) for the last 30 days
are persisted to `/data/sla-history.json` (written atomically, pruned to
30 days, throttled to one write a minute and flushed on shutdown). Restarting the hive or evicting old
journeys from the timeline never shrinks a stored day, so the Throughput
sparklines always have 30 days behind them. A corrupt file is moved aside
as `sla-history.json.corrupt` and history restarts from the live timeline.

**Stale in-progress.** Issues that have carried `stage/030-pr-in-progress` (formerly `hive/in-progress`) for longer
than the agent claim TTL (`claims.agent_ttl_s`, default 2h) with no linked
PR are listed as `stale_in_progress` with their start time, age and agent.
The badge in the Delivery SLA header counts them and links to each issue; it is the first
place to look when an agent claimed work and went quiet.

**Targets.** Each span's p50 is coloured green at or under its target,
yellow up to 1.5× the target and red beyond. Targets are hours in
`hive.yaml` and can also be set from **Settings → Thresholds → Delivery SLA
targets** (`PUT /api/config/sla`), applying on the next status poll:

```yaml
sla:
  targets:
    start_to_pr_h: 4      # started → linked PR opened
    pr_to_merge_h: 24     # PR opened → merged
    open_to_merge_h: 72   # issue created → merged
```

`0` or an omitted key means the default. Values above one year are
rejected.

**Reading the panel.** The three tiles show the current-window p50, `n`,
p95 and target with a 30-day sparkline of daily p50s; `7d`/`30d` toggles
the window. **Repos ▾** expands a per-repo table (start→PR p50/p95,
PR→merge p50/p95, merged in 7d, 30-day open→merge sparkline) and the
stale-issue list. The same numbers are in `GET /api/status` under `sla`
(fleet) and `repos[].sla`; see [api-reference.md](api-reference.md).

## Fleet breaker

The dashboard top bar includes a fleet breaker: a circular pause/play control plus a status pill. The `GET /api/breaker` state is readable by authenticated viewers, but engaging or releasing the breaker is owner-only (`POST /api/breaker/engage`, `POST /api/breaker/release`).

Engage pauses every agent that is currently running and is not `on_demand`. Agents already paused before engage are skipped and keep their original pause reason. Release resumes only the exact agents the breaker paused and still owns (`PausedTrigger == fleet-breaker`); if an operator manually re-pauses an agent while the breaker is engaged, release leaves it paused. The breaker state is persisted so a crash/restart can still release the same captured set.

Fixes #2953.

## Self-reported issues

When a hive notices an unusual error in itself — a restart storm, repeated
kick failures, a recovered panic, degraded model discovery, inference
auth failures or advisory post failures — it files an issue on
`hivecommons/hive` titled `[selfreport] <class>: <short message>`, or adds
itself to an open issue that already describes the problem (fingerprint
match, `hive/selfreport` + class match, or keyword match on a human-filed
issue). Joining posts one "Also seen on **<hive>**" comment per 24h and
bumps the `Affected hives (N)` block in the body, where each hive has its own
line — `- <hive> (instance <id>) — owner(s): @login — seen N, last <time>` —
naming the GitHub login(s) of the hive owner (`dashboard.authorized_users`
owner-role entries plus the device-flow operator) so a maintainer can see who
is affected. Each report also names the repositories the hive watches
(`project.repos`) and their owners, and the body block keeps an
`Affected repos (N)` / `Affected repo owners (N)` union across reporting
hives (hivecommons/hive#12338). Every selfreport carries
`origin/selfreport` (the provenance axis every hive-created issue records,
see [labels-and-control-signals.md](labels-and-control-signals/reference.md#origin-sources))
next to the deprecated `hive/selfreport`.

```yaml
governor:
  fleet_report:
    enabled: true            # unset = on for hub-managed hives, off (dry-run) for self-hosted
    repo: hivecommons/hive
    max_per_day: 5
    include_repos: true      # false: never name project.repos / repo owners (set this if any watched repo is private)
    include_owners: true     # false: render "(anonymous)" instead of the hive owner's GitHub login(s)
```

Rate limits: `max_per_day` new issues per hive per 24h, at least 1h between
new filings of different classes, no re-filing of a fingerprint closed
upstream while the hive keeps observing the condition (7-day cooldown from
the last observation), deferred while the GitHub API budget is not normal. Issues
are never auto-closed; the hive posts one "cleared" comment when the
condition stops. Hub-managed hives that cannot write to the report repo relay
through the hub (`POST /api/selfreport/ingest`, heartbeat bearer). The
dashboard shows "Self-reports: N open (last: <class> → #NNN)" in the Fleet
Report panel and the toggle/repo/cap under Settings → Governor →
"Self-Reported Issues". Full details: [fleet-report.md](fleet-report.md).

## Agent CLI session recycling

Each agent's CLI (Copilot, Claude, …) is a long-lived Node/V8 process whose
heap grows with every turn and never shrinks while idle. The health tick
samples the resident memory of the whole process tree under each agent's
tmux pane and recycles a session that is over budget — but only while the
agent is idle at its input prompt with no kick pending, so a turn is never
interrupted. A recycle is an ordinary restart (`last_restart_reason: session
recycle`): claims, queues and the kick log are preserved exactly as on any
other restart, and the governor re-kicks on the next cadence. After a recycle
the same agent is left alone for ten minutes even if it is over budget again.

```yaml
agents:
  session_recycle:        # fleet default; on unless enabled: false
    max_rss_mb: 1536      # recycle an idle session whose process tree exceeds this (MiB)
    max_age: 6h           # …or whose CLI session is older than this
    min_idle: 2m          # pane + transcript must be silent this long first
  scanner:
    session_recycle:      # optional per-agent override; absent fields inherit the fleet value
      max_rss_mb: 2048
```

The sampled value is shown as **Session RSS** (with the session age and a
`↻N` recycle count) in the agent detail panel. The log line is
`agent session recycled` with `rss_mb`, `age` and `reason`.

## GitHub API quota

Agents reach `api.github.com` through the Hive GitHub proxy, which injects the
agent-scoped App installation token and records charged requests. The App
installation quota is shared with the daemon's scan, merge, and maintenance
loops, so the proxy enforces two safeguards:

- `agents.github_api_hourly_cap` (default `300`) limits each agent to that many
  charged GitHub API responses in a sliding one-hour window. A charged response
  is any proxied response other than `304 Not Modified`; the proxy's own token
  minting is not agent-attributed and is not counted. Set the value to `0` to
  disable the fleet default, or set `agents.<name>.github_api_hourly_cap` to
  override one agent (`0` disables that agent's cap).
- `github.agent_reserve_floor` (default `400`) protects the shared core bucket.
  When the latest rate-limit reading shows `core.remaining` below the floor,
  agent read requests receive the same proxy `429` even if the agent has not
  reached its personal cap. This keeps daemon merge/scan work from being
  starved by agent polling. Only readings from the hive's own bucket update
  that snapshot: responses whose `X-RateLimit-Limit` is 60 (GitHub's per-IP
  unauthenticated bucket, e.g. an agent's `gh` call without a token) or whose
  limit differs from the daemon's are ignored, so they cannot trip the floor
  or the API budget mode.

The proxy returns JSON shaped for the GitHub CLI:

```json
{"message":"hive: agent GitHub API hourly cap reached (N/cap); stop polling and continue with local work","documentation_url":"…docs/operator-reference.md#github-api-quota"}
```

It also sets `Retry-After` to when the one-hour window (or observed reset) can
make progress, logs once at 80% of each agent window, and sends one
stop-polling nudge per agent window. Hive-authored write relays are not
agent-attributed and are not capped by this path.

Open-PR scans use `github.graphql_pr_batch` by default to move mergeability,
review-decision, linked-issue and CI-rollup reads into GitHub's separate
GraphQL bucket. `/api/gh-rate-limits` reports these calls as caller
`hive:pr_batch` on endpoint `/graphql` and exposes `graphql_pr_batch` counters
for repositories, PRs, pages, REST fallbacks, errors, and the last GraphQL
query cost. Per-PR REST reads still happen when the batch is disabled, a repo's
GraphQL query fails, or GitHub returns `mergeable: UNKNOWN` for that PR.

### Webhook-driven PR cache invalidation

See [GitHub webhooks: cadence, patch and reconcile](webhooks.md) for the
cadence keys, the fallback to polling when deliveries degrade, and the
accuracy guarantees of the ForgeState contract.

A spoke accepts GitHub App webhooks on the public `POST /api/webhook/github`,
either delivered directly by GitHub or relayed by the hub (the hub forwards the
original signed body to the hive that manages the repository). The receiver
fails closed: it rejects every delivery until `GITHUB_WEBHOOK_SECRET` is set to
the App's webhook secret, and it verifies `X-Hub-Signature-256` over the raw
body exactly as the hub does. Deliveries for repositories the hive does not
manage are ignored.

#### What the operator configures

1. **Secret.** Set `GITHUB_WEBHOOK_SECRET` in the spoke's environment (a
   Secret mount on Kubernetes; never in `hive.yaml`) to the App's webhook
   secret. On a hosted spoke this is the same value as the hub's
   `webhook-secret.key`. Without it the receiver answers 503 and the loop
   stays on the polling cadence.
2. **GitHub App webhook.** Point the App's webhook URL at the hub
   (`POST /api/github/webhook`) for hosted spokes, or at the spoke's own
   `/api/webhook/github` for a stand-alone hive (a repository or organization
   webhook with the same secret works too). Subscribe the App to these events:
   `pull_request`, `pull_request_review`, `pull_request_review_comment`,
   `issue_comment`, `issues`, `check_suite`, `check_run`, `status`, `push`,
   and, with `governor.work_source.type: github_projects`, `projects_v2_item`
   (organization-scoped; see [Work sources](work-sources.md#type-github_projects--github-projects-projects-v2)).
3. **Hub relay.** The hub verifies the HMAC and relays the body byte-for-byte,
   with the original signature and `X-GitHub-Delivery`, to the spoke whose
   reported dashboard URL manages the repository. Nothing to configure on the
   spoke beyond the shared secret.
4. **Pacing (optional).** `governor.eval_interval_webhook_s` (default `900`)
   is the base tick while webhooks are healthy; `governor.webhook_wake_debounce_s`
   (default `10`) and `governor.webhook_wake_min_gap_s` (default `30`) pace the
   early cycles described below. `review.event_driven` additionally lets
   `pull_request` deliveries dispatch reviews early.

Verify: `/api/status.webhooks.events_1h` climbs after a push or comment,
`/api/status.evalMode` reads `webhook`, and the spoke log shows
`github webhook wake: running an early eval cycle` with the delivery's reason.

Through the hub, every enrolled hive that tracks the delivery's repository
and has a dashboard URL receives the relay, so multiple hives (for example
v5 and v6 spokes) tracking one repo all stay on webhook mode.

#### What a delivery does

- `pull_request`, `pull_request_review`, `pull_request_review_comment`,
  `check_suite`, `check_run`, `status`, `issue_comment` (on PRs only) and
  `push` invalidate the cached PR detail and the GraphQL batch
  check-run/review entries for the affected PR and mark it dirty. Check and
  status events are matched by PR number and head SHA (fork PRs arrive
  without a PR list); `push` invalidates every open PR whose base or head
  branch is the pushed branch. `issues` `labeled`/`unlabeled`/`reopened`
  mark the issue for the next un-park sweep.
- `pull_request` `opened`/`synchronize`/`reopened` also hands the PR to the
  [sentinel](#suspicious-activity-alerts-sentinel), which evaluates it now
  instead of waiting for its reconcile cadence
  ([#13379](https://github.com/hivecommons/hive/issues/13379)).
- `projects_v2_item` (`created`/`edited`/`deleted`/`reordered`/`archived`/
  `restored`/`converted`) patches the GitHub Projects work-source cache: the
  delivery names the item and its project, the hive re-reads that one item
  (one GraphQL call) and upserts it, or drops it on `deleted`/archived. The
  payload carries no repository, so the spoke receiver routes it ahead of the
  repository-keyed events and the response carries `projects_patch`. The hub
  relay forwards repository-keyed deliveries only, so a hosted spoke using a
  Projects board needs the organization webhook pointed at its own
  `/api/webhook/github` for this event.
- **Every invalidating delivery wakes the governor loop.** The wake is
  debounced for `governor.webhook_wake_debounce_s` so a push and its check
  fan-out coalesce into one cycle, and webhook-triggered cycles are at least
  `governor.webhook_wake_min_gap_s` apart so a delivery storm stretches the
  batch instead of spinning cycles. **The wake cycle does not re-list
  repositories.** It plans from the governor's cached actionable snapshot
  (the one the last cadence tick enumerated, patched by the targeted
  re-reads described below), so a wake costs the targeted re-read plus the
  dirty-PR detail refreshes and nothing else: repositories with no dirty PR
  reuse their previous GraphQL PR batch (`graphql_pr_batch.webhook_skips`),
  clean PRs are served from the PR detail cache past
  `github.pr_detail_ttl_s`, and only the dirty PRs are re-read. The log line
  is `github webhook wake: running an early eval cycle` (`reason`,
  `deliveries`, `coalesced`, `source=cached`), followed by
  `eval cycle planning from cached snapshot`.
- Webhooks are **healthy** while the last delivery for a managed repository is
  newer than `2 × governor.eval_interval_s`. The first healthy window puts
  the loop in **webhook mode**: the governor's base eval interval rises to
  `governor.eval_interval_webhook_s` (default `900`, capped by
  `governor.eval_interval_max_s`) — safe because the wakes above carry the
  latency — and `/api/status.evalMode` reads `webhook`,
  `/api/status.webhooks.mode` reads `webhook`. The API budget stretch still
  applies; the larger interval wins.
- **Fallback to polling** ([webhooks.md](webhooks.md#fallback-when-webhooks-degrade)).
  Webhook mode degrades when the window lapses *while GitHub was active*
  (a cadence pass listed an item updated after the newest delivery) or when
  deliveries reach the receiver but fail verification; a quiet repository
  with no deliveries and no activity keeps the stretched tick. The loop
  re-checks as the window closes (not just at the next stretched tick),
  resets its ticker to `governor.eval_interval_s`, logs one INFO
  `webhooks degraded; polling cadence restored` (with `reason`), and
  `evalMode` reads `poll`, `webhooks.mode` reads `polling` with `since` and
  `mode_reason`. The way back has hysteresis: deliveries must keep arriving
  for `2` consecutive healthy windows (`webhooks.healthy_windows` counts
  them) before one INFO `webhooks healthy; webhook cadence engaged` restores
  the webhook cadence.
- `pull_request` `opened`, `reopened`, `synchronize`, `ready_for_review` and
  `review_requested` deliveries also queue an early review dispatch when
  `review.event_driven` is on; see
  [Event-driven dispatch](review-swarm.md#event-driven-dispatch).
- **A failed check reacts within seconds, not at the next sweep.** A
  `check_run` (`completed`, conclusion `failure`/`timed_out`), `check_suite`
  (`failure`/`timed_out`) or `status` (`failure`/`error`) delivery is
  resolved to the open PR(s) at that head from the PR detail cache — one
  cached lookup, no extra `GET /pulls` fan-out; a head no cached PR matches
  is counted under `/api/status.ci_failure_signals.unmatched` and left to
  the sweep's dirty mark. Each match becomes a **CI failure signal**
  `{repo, number, head, check, details_url, at}`, deduplicated per head +
  check and kept for two hours. A **hive-authored** PR is attributed to the
  agent whose relay opened it (`agent_pr_created` in the audit log; scanner
  when unattributed) and that agent is kicked at once with its
  FIX-BEFORE-NEW section naming the check and the log URL — the same
  section every regular kick carries, into which the signal is also folded
  until `ci-failing.json` catches up, so nothing is lost if the kick is
  gated. A **contributor** PR nudges the automerge lanes for a targeted pass
  over that PR now (the contributor lane / `stage/072` path), with or
  without the ForgeState cache. Guards, in this order: `governor.ci_failure_webhook_kick`
  (default `true`; `false` records signals only), the repo pause
  (`project.paused_repos`), the agent's config/runtime pause (the fleet
  breaker pauses every agent), governor kick eligibility, and one
  webhook-triggered kick or nudge per PR head per
  `governor.ci_failure_kick_cooldown_s` (default `600`). The activity log
  records `ci_failure_kick` — "CI failed on #N (check) — kicked agent X" /
  "nudged contributor lane". **Agents never watch or poll CI themselves**:
  detection is out-of-band via this webhook path, or the sweep as fallback.
- **A recorded review verdict also wakes the governor loop.** When the
  relay records a verdict bound to a review dispatch, the head that wrote it
  is free and the PR may be ready for its next head or its decision kick, so
  the loop runs a cached-snapshot cycle now instead of waiting for an
  unrelated delivery or `governor.eval_interval_webhook_s`. Advisory verdicts
  do not wake it. The first verdict wakes at once; later ones inside
  `governor.webhook_wake_min_gap_s` fold into one wake at the end of the gap.
  `governor.webhook_wake_debounce_s` does not apply to this wake (its
  debounce is fixed at 0, so a verdict is never held back), and the spoke logs `review verdict wake: running an early eval cycle from
  the cached snapshot`
  ([#14705](https://github.com/hivecommons/hive/issues/14705)).
- Every status publish (cadence or wake) invalidates the `/api/stage-tracker`
  cache and sends a `stage-tracker` event on `/api/events`, so the dashboard's
  activity monitor refetches at once instead of waiting out
  `dashboard.stage_tracker.cache_ttl_s` plus its poll. Invalidation only
  marks the cached body stale: the publish, and every webhook below, returns
  at once even while a tracker rebuild is running. The next poll starts
  **one** rebuild outside any lock; polls that land during it get the
  previous body with `source: stale` and `max-age=0` rather than queueing,
  so a slow build (one sentinel evidence read per open PR, on NFS in hosted
  hives) can never back up webhook handlers, the governor's publish, or an
  unbounded line of dashboard requests (hivecommons/hive#14681).
- **The activity monitor row moves without waiting for a cycle.** A delivery
  that names a PR or issue this hive tracks (`pull_request*`,
  `check_suite`/`check_run`, `status`, `push` to a PR branch, `issue_comment`,
  `issues` — any action) also drops the `/api/stage-tracker` cache at once
  and queues a **targeted re-read of just that item**: one `GET` of the PR
  or issue, debounced by `dashboard.stage_tracker.webhook_refresh_debounce_ms`
  (default `2000`) and coalesced per item, so a check fan-out of twenty
  `check_run` deliveries costs one request. The re-read patches that row of
  the status snapshot in place — labels, title, draft flag, last activity;
  a closed or merged item leaves — then drops the tracker cache again and
  sends the `stage-tracker` SSE nudge. The same re-read is handed to the
  governor, which upserts the item into its cached actionable snapshot —
  a new issue or PR appears, a closed or merged one leaves, a label change
  moves it between the actionable / held / sidelined buckets — so the wake
  cycle that follows plans against the patched item. No repository is
  re-enumerated. The read goes through the paced REST transport and is
  skipped outright while the API budget is `critical`; it refreshes the PR
  detail cache, so the following cycle does not pay for it again. The
  re-read is also remembered for
  `dashboard.stage_tracker.webhook_refresh_replay_window_s` (default
  `1800`) and re-applied to every status snapshot published meanwhile: an
  eval cycle that enumerated the item *before* the close still publishes
  from that enumeration minutes later, and without the replay its publish
  put the closed row back until the next full enumeration.
  `dashboard.stage_tracker.webhook_refresh: false` turns the re-read off
  (deliveries then still invalidate caches and wake the loop, and wake
  cycles fall back to a full enumeration because nothing else patches the
  snapshot). Independently of the re-read, the one payload fact the tracker
  trusts without a confirming read is a closed or merged `state` on the
  issue or pull request the delivery embeds: on *any* action (`labeled`,
  `unlabeled`, `created`, not only `closed`) that row leaves at once, so a
  refused or failed re-read cannot leave a closed item listed open. As a
  backstop for deliveries that never arrived, on each status publish it
  also lists each repo's items closed since its last look every
  `dashboard.stage_tracker.closed_sweep_interval_s` (default `300`; one
  list call per repo; negative disables) and evicts any still shown.

#### Polling cadence vs webhook patch vs reconcile

The full model — the three writers, the accuracy guarantees of #13373, every
cadence key with its default, and the webhook-outage fallback — is
[webhooks.md](webhooks.md). In brief: inbound webhooks cost nothing against
the GitHub rate limit; outbound API calls do. The governor therefore keeps three distinct paths and never lets a
delivery trigger a full re-list:

| Path | Trigger | GitHub cost | What it does |
| --- | --- | --- | --- |
| **Reconcile** | cadence tick — `governor.eval_interval_webhook_s` (default `900`) while webhooks are healthy, `governor.eval_interval_s` otherwise | full: lists issues and PRs for every tracked repo | Authoritative. Rebuilds the actionable snapshot from scratch and **overwrites** every webhook patch. If the pass fails outright (every repo refused: rate limit, 429, 5xx) the previous snapshot — patches included — stays published, marked stale for every repo (`stale_repos`, repo card `countsStale`), and the next tick retries; a repo refused on its own is served from its last-good enumeration the same way. |
| **Webhook patch** | one delivery naming an issue/PR | one `GET` per item, debounced and coalesced | Upserts, moves or removes that single item in both the activity-monitor row and the governor's cached snapshot. Counted in `/api/status.webhookPatches`. |
| **Cached cycle** | webhook wake, review wake, continuous-agent wake | dirty-PR detail refreshes only | Runs the governor's decision pass (kicks, automerge, stage moves) from the patched snapshot without listing. `source=cached` in the wake log; counted in `/api/status.cachedCycles`. Before the first reconcile has produced a snapshot a wake enumerates instead. |

Cadence ticks reconcile; webhooks keep the picture current in between. The
priority order is accuracy of issue/PR state, then latency, then API thrift,
which fixes four rules for the patch path:

- **The payload is a hint, never state.** A delivery only names the item;
  the GET decides what the row says. A `closed` delivery whose re-read finds
  the item open leaves it open.
- **Upserts are monotonic.** A re-read whose `updated_at` is older than the
  copy already held (two deliveries' GETs landing out of order) is dropped.
  Each applied read is recorded in the snapshot's `item_patches` (fetch time,
  `updated_at`, head SHA, `X-GitHub-Delivery` id) for audit.
- **Closed/merged removes at once; reopened re-adds** — both after the
  confirming GET.
- **Irreversible actions are freshness-bounded.** Before an issue close or a
  PR merge planned from the snapshot (stage sync, close-on-merge, automerge
  sweep), the governor refuses outright while that repo's share is stale and
  otherwise re-reads the item once if its copy is older than 60 s
  (`DecisionFreshnessBound`), proceeding only if it is still open. Refused
  actions log `irreversible action deferred` and retry next pass.

Startup runs a full enumeration before the first decision pass; a cold cache
whose GitHub share failed is published stale, never as "0 items".
`/api/status.staleSince` (and per-repo `countsStaleSince`) carries the
stale-as-of time and the Overview shows a `stale · as of HH:MM` badge.
Raising `eval_interval_webhook_s` lowers list-call volume further at the cost
of a longer window before a missed delivery is corrected by reconciliation.

##### Reading from the ForgeState cache (`forgestate.enabled: true`)

With the per-repo state cache on (`forgestate:` block in `hive.yaml`,
default off; [#13373](https://github.com/hivecommons/hive/issues/13373)) the
governor work source stops listing GitHub on the cadence tick too
([#13375](https://github.com/hivecommons/hive/issues/13375)). The cache's own
reconcile loop (`forgestate.reconcile_interval_s`, default `900`, shortened
to one minute while a repo is stale or its drift is high) is the **only**
full list; every governor pass — cadence tick, webhook wake, review wake —
takes each repo's open issues and PRs from the cached snapshot and classifies
them exactly as before. What changes:

- **Startup gate.** No decision pass runs before every repo has completed
  its first successful reconcile. The loop logs `forgestate: waiting for the
  first reconcile before the first decision pass` once a minute while it
  waits and `first reconcile complete` when it is released. A repo whose
  cache is not ready in a later pass is served from its last-good
  enumeration and marked stale, like a transient GitHub refusal.
- **Stale is inherited.** A repo whose last reconcile failed keeps its
  last-good items and appears in `stale_repos` with `stale_since` set to the
  last good reconcile, so the same close/merge guard applies.
- **Freshness bound before a kick.** Every GitHub ref in an assembled kick
  goes through the same 60 s bound as close and merge: a copy younger than
  the bound is kicked as-is, an older one is confirmed with one targeted
  `GET` (dropped from the kick if it closed or merged meanwhile), and a ref
  in a stale repo is withheld (`kick ref withheld: snapshot not fresh enough
  to act on`). A kick whose refs all fail is not sent.
- **Single-item reads feed the cache.** The webhook patch `GET` and the
  freshness confirmation `GET` are observed into the cache (as a webhook
  patch carrying the `X-GitHub-Delivery` id, or as a read-through), and the
  hive's own label add/remove, issue close and PR merge are written through
  immediately, so the next pass sees them without waiting for a webhook.
- **`item_patches` covers every cached item.** With the cache, the pass's
  `generated_at` says nothing about an item's age, so the fetch time of
  every item served from the cache is recorded there; the closed attributed
  PR history scan is reused for one reconcile interval rather than re-run
  on every wake.
- **Automerge lanes read the same cache.** The self-authored, trusted-author
  and contributor-lane sweeps take their open-PR set from the cached
  snapshot and run event-driven with a 5-minute floor pass; see
  `hive-merge.md` → "Event-driven sweeps on the ForgeState cache"
  ([#13377](https://github.com/hivecommons/hive/issues/13377)).

Repos added to the config after boot list live until the next restart
rebuilds the store with them. Enabling the cache changes no classification:
only the two list calls per repo move behind it.

##### Rate-limit pacing priority

After a GitHub rate-limit refusal (a secondary-limit 403 or a primary-limit
reset) the shared transport enters a slow-start caution window and paces
requests about 2s apart. Pacing is prioritised so periodic housekeeping cannot
starve the governor:

- the reconcile pass (`EnumerateActionable`) runs **high** priority and is
  bounded only by its own deadline;
- ordinary requests may queue at most 30s ahead;
- housekeeping (self-authored and queued automerge sweeps, the review-request
  watcher, advisory digest lookups, the dashboard metrics and fleet-stats
  collectors) runs **low** priority: a request that would queue more than 5s
  ahead is refused immediately (`github pacing: request shed`) instead of
  taking a slot, and a tick whose first slot is already further out than the
  tick could wait is skipped outright (`low-priority work shed`, logged at
  most once a minute per caller) and retried on its next tick;
- the self-authored automerge sweep backs its tick off to at least 60s while
  the window is engaged and resumes its adaptive interval when it ends;
- GitHub App installation-token mints (and the other App-JWT endpoints) do
  not go through the pacer at all: they are a separate rate-limit bucket, and
  a mint that queued behind paced traffic failed every request that needed
  the token.

##### Event-driven readers (ForgeState phase 2d)

- **The review-request watcher runs on events.** The relay that submits
  `hive-review` requests (`review_request_watcher.go`) no longer polls its
  drop box every 10 s. A request file drop wakes it through a directory
  notification, and `issue_comment`, `pull_request_review`,
  `pull_request_review_comment` and `issues` `labeled`/`unlabeled`
  deliveries wake it for a pass (`/api/status.webhooks` deliveries report
  `woke_review_watcher`). While webhooks are healthy the unprompted scan is a
  five-minute reconcile (`ReviewRequestWebhookFallbackInterval`); when
  webhooks go stale, or the drop box cannot be watched, the watcher returns
  to the legacy 10 s poll. A `--revise` request for a repo outside
  `review.revise_repos` is a policy denial (`.denied`, one WARN), not a
  retried failure.
- **The advisory issue is resolved from the ForgeState cache.** When a
  `forgestate` store is bound to the GitHub client (`forgestate.enabled`,
  hivecommons/hive#13373), `EnsureAdvisoryIssue` answers from the cache: the
  memoized advisory issue is read through the cache within its freshness
  bound (zero GitHub calls on a healthy cycle), a stale or missing cached
  copy costs one targeted `GET` of that issue, and a closed advisory issue
  is reopened in place and written through. The search → label list →
  title-scan path runs only when the cache cannot answer (repo not yet
  reconciled, or no labelled advisory issue to confirm).

`/api/status` and `/api/gh-rate-limits` expose
`webhooks: {healthy, last_event_at, events_1h, invalidations_1h,
targeted_refreshes_1h, tracker_invalidations_1h, mode, since, mode_reason,
healthy_windows}` (the middle two count the single-item re-reads and the
tracker cache drops webhooks caused; `mode` is `webhook` | `polling` and
`since` when it began — see [webhooks.md](webhooks.md));
`/api/status` adds `evalMode` (`webhook` | `poll`), `evalIntervalS`,
`lastWakeReason`, `lastWakeAt` and `webhookWakes`;
`graphql_pr_batch.webhook_skips` counts batch queries skipped because nothing
changed.

```yaml
governor:
  eval_interval_s: 300
  eval_interval_webhook_s: 900
  webhook_wake_debounce_s: 10
  webhook_wake_min_gap_s: 30
dashboard:
  stage_tracker:
    webhook_refresh: true          # re-read the PR/issue a delivery names
    webhook_refresh_debounce_ms: 2000
    webhook_refresh_replay_window_s: 1800   # re-apply re-reads to snapshots published meanwhile
    closed_sweep_interval_s: 300   # list items closed since the last look and evict rows still shown; -1 disables
    waiting_push_stale_h: 4        # hours without a push on a CI / Review row before the pill's age turns muted-amber
    toast_info_s: 15               # how long the tracker's result / waiting-on-you toasts stay (3-120 s); the merge banner stays twice as long
    toast_error_s: 30              # how long a failed row action's toast stays (3-120 s); editable from the Activity Monitor's ⚙ → Toasts & timing
    merged_hold_s: 8               # ceiling on the "Merging · merge landed" bridge; celebration fires on the merged fact (min 3)
```

## `HIVE_GITHUB_TOKEN` permissions

`github.token: ${HIVE_GITHUB_TOKEN}` creates the main GitHub client when a GitHub
App is not configured. The code reads issues, PRs, commits, contents, checks,
commit statuses, contributors, and search results; it can also create/update
issues and comments, add/remove/create labels, open PRs, approve PRs for the
merge queue, and squash-merge queued PRs.

Minimum practical PAT permissions for full PAT-authenticated operation:

- **Classic PAT:** `repo` for private repositories (`public_repo` is enough only
  when every managed repo is public and no private org data is needed).
- **Fine-grained PAT:** select the managed repositories and grant:
  - Contents: read/write (branch pushes by agents and content reads)
  - Pull requests: read/write (list, open, approve, merge)
  - Issues: read/write (list, comments, labels, advisory issue)
  - Checks: read-only and Commit statuses: read-only (CI/merge gating)
  - Administration: read-only (branch-protection verification before merge requests)
  - Metadata: read-only (required by GitHub)

If you use a GitHub App, configure equivalent repository permissions on the App
installation instead of broadening the PAT. A `repo-moved` GitHub App verdict
means the App/key/installation are healthy, but the hive still names the old
repository owner after an org transfer; update the configured project org rather
than trying to add the moved repository back to the old installation.

### GitHub App permissions the hive needs, and what each one unblocks

The App's repository permissions decide which steps the hive can take itself.
A step the App cannot take is not a question for a person to *decide*; it is
a **capability gap** the hive records as a structured blocker
(`<!-- hive:blocker kind=capability:… -->`, see
[labels-and-control-signals.md](labels-and-control-signals/glossary-and-operations.md#blockers-hiveblocker-marker)),
parks the item on, and surfaces at ⚙ → Security as *GitHub App permissions:
missing `<permission>` · N items blocked* ([#15099](https://github.com/hivecommons/hive/issues/15099)).

| Permission | Level | Without it the hive cannot… | Blocker kind |
|---|---|---|---|
| Contents | read/write | push branches, read files | `capability:repo-write` |
| Pull requests | read/write | open, approve, merge PRs | — (nothing starts) |
| Issues | read/write | comment, label, file issues | — (nothing starts) |
| Workflows | write | push any change under `.github/workflows/*` (GitHub refuses the push with *refusing to allow a GitHub App to create or update workflow*) | `capability:workflows-permission` |
| Actions | write | approve workflow runs from forks (the 085 *Awaiting human: approve workflow runs* pill) | — (`hive:unauthorized`) |
| Checks / Commit statuses | read | gate merges on CI | — |
| Administration | read | verify branch protection before a merge request | `capability:branch-protection` when a rule the App cannot satisfy stands |
| Metadata | read | anything (required by GitHub) | — |

Granting a permission on an existing installation needs the installation
owner to accept the new permission request in the App settings page the
dashboard links to (`githubAppInstallURL`). The hive re-checks on its own on
the next stage pass; *Re-check permissions* on a blocked row
(`POST /api/repos/{owner}/{repo}/items/{number}/blocker/recheck`) probes the
installation now and releases every item blocked on that permission when it is
there. `GET /api/status` carries the same picture as
`capabilities: {missing: ["workflows"], blocked_items: N, by_permission: {workflows: N}}`.

#### Onboarding errors point to the Getting Started guide

Every setup problem the dashboard's **🧭 Guide** (top navbar; also *Getting
Started* in the sidebar) has a step for is classified server-side and rendered
with a tip — *New hive? Open the 🧭 Guide → step ‘Install the Forge App
(GitHub App)’ for the install URL and permissions checklist* — whose link opens
the guide at that step. The classification rides the status payload as
`onboardingIssues: [{kind, guideStep, message}]`, on system alerts as
`onboarding: true` + `guideStep`, and on the embedded `deepHealth` checks
(`github_auth`, `repo_target`) as `onboarding`/`guideStep`. Kinds:
`github-app-not-installed`, `github-app-wrong-installation`,
`github-app-permissions`, `github-app-credentials`, `github-base-url`
(→ step `github-app`), `repo-target`, `no-repos` (→ `repos`), `no-agents`
(→ `add-agent`), `proxy-token` (→ `welcome`). Error text is unchanged; the tip
is appended. While any onboarding issue is active the Guide chip stays visible
even if the checklist was dismissed earlier. The guide's GitHub App step shows
the concrete install URL for the configured forge host (`githubAppInstallURL`,
GHES-aware; on an enterprise host with no `github_app_slug` it says what to
configure instead of linking to github.com). The guide, its entry points and
the ⚙️ Governor Config gear require a non-`read` role from `GET /api/role` —
a read-only viewer sees none of them and must be granted access via Manage
Access.

## Runtime diagnostics (`debug`, `GOMEMLIMIT`, `mem:` log line)

Added for [#14681](https://github.com/hivecommons/hive/issues/14681) (hive
process RSS growing hundreds of MiB/min with no pprof to name the culprit).

| Name | Source | Default | Purpose |
|---|---|---|---|
| `debug.pprof_addr` | `hive.yaml` | `127.0.0.1:6060` | `net/http/pprof` listener. **Loopback only**: a non-loopback host (`0.0.0.0`, pod IP) is rejected at startup with a warning and the default is used instead. `off` disables the listener. Profiles are under `/debug/pprof/`; `/debug/runtime` prints the same counters as the `mem:` log line. |
| `GOMEMLIMIT` | env | 85% of the cgroup memory limit | Go soft memory limit. When the env var is unset the hive reads `/sys/fs/cgroup/memory.max` (v2) or `memory/memory.limit_in_bytes` (v1) and sets `MemoryLimitPercent` (85%) of it via `debug.SetMemoryLimit`; an unlimited cgroup leaves the runtime default. Set the env var to override. Logged at startup as `runtime: memory limit` with `source=env|cgroup|none`. |
| `mem: runtime snapshot` | log (INFO, every 60 s) | always on | `heap_inuse`, `heap_live`, `heap_objects`, `total_sys`, `next_gc`, `gc_cycles`, `goroutines`, `threads` (OS threads from `/proc/self/status`), `gomemlimit`. Read from `runtime/metrics`, so no stop-the-world. |

The same snapshot is embedded in `GET /api/health` as `runtime` so the
dashboard health probe and `kubectl exec … curl` see goroutine count and heap
without the pprof port. Taking profiles on a pod:

```bash
kubectl -n <ns> port-forward deploy/hive 6060:6060 &
go tool pprof -top  http://127.0.0.1:6060/debug/pprof/heap          # inuse_space
curl -s 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1' | head  # parked goroutines
```

## `cmd/hive` flags and environment

### Startup

| Name | Source | Purpose |
|---|---|---|
| `--config` | flag | Path to `hive.yaml`; default `/etc/hive/hive.yaml` unless `HIVE_CONFIG` is set. |
| `HIVE_CONFIG` | env | Sets the **default** of `--config`. An explicit `--config` outranks it — and the image ships one (`CMD ["--config", "/etc/hive/hive.yaml"]`), so `entrypoint.sh` appends `--config "$HIVE_CONFIG"` to the launch argv when the variable is set. Without that append the variable is inert in the container ([#4973](https://github.com/hivecommons/hive/issues/4973)). |
| `HIVE_MODE=hub` | env | Starts the hub server instead of a spoke dashboard. |
| `HIVE_HUB_PORT` | env | Hub listen port in hub mode; default `3001`. |
| `HIVE_SINGLETON_LOCK` | env | Internal/escape hatch for the process singleton lock path; value `off` disables the guard. |

### GitHub and dashboard auth

| Name | Purpose |
|---|---|
| `HIVE_GITHUB_TOKEN` | Main PAT fallback when `github.token` is empty; also used for token identity/fleet stats fallback. Wrong-scope tokens fail with generic 403s at request time — see [github-app-setup.md](github-app-setup.md#personal-access-token-pat-scopes) for the required scopes per ACMM tier. |
| `GH_APP_KEY_FILE` | GitHub App private-key path fallback when `github.key_file` is empty. |
| `HIVE_DASHBOARD_TOKEN` | Shared dashboard/API token fallback for `dashboard.auth_token`. Any non-empty string is accepted with no strength check — generate one with `openssl rand -hex 32`; see [env-vars.md](env-vars.md#generating-and-rotating-hive_dashboard_token). |

### Hosted-spoke and fleet metadata

| Name | Purpose |
|---|---|
| `HIVE_HUB_URL` | Hub URL override for heartbeat/spoke registration. |
| `HIVE_CLUSTER_ID` | Hosted cluster ID override. |
| `HIVE_ID` | Hive ID override. |
| `HIVE_LEVEL` | ACMM level bootstrap/override used by hosted flows. |
| `HIVE_COVERAGE_BADGE_URL` | Source of the coverage percentage on the ci-maintainer card: an `http(s)` badge URL (shields JSON or SVG) or `repo://<ref>/<path>` read from the primary repo via the App client. See [env-vars.md](env-vars.md). |

### Dashboard metrics

See [Monitoring a hive with Prometheus and Grafana](monitoring.md) for setup, metric families and scraping.

| Name | Purpose |
|---|---|
| `HIVE_METRICS_ENABLED` | Registers Prometheus `/metrics` when set to `1`, `true`, `yes`, or `on`; off by default (route not registered — scrapes 404) because it exposes estimated cost data. |
| `HIVE_METRICS_TOKEN` | **Required whenever metrics are enabled** ([#3804](https://github.com/hivecommons/hive/pull/3804)): scrapers must send `Authorization: Bearer <token>` (configure Prometheus `bearer_token`). Enabled-but-tokenless serves 403 with an error naming both variables, and the hive logs a startup warning; the cost/agent series are never served unauthenticated. See [ServiceMonitor setup](monitoring.md#adding-the-monitoring-component-to-an-overlay) and [alert runbooks](monitoring.md#alerts-and-runbooks) for configuring scraper access and on-call alerts. |

> **Note on `tokens_24h`:** despite its name, the per-spoke heartbeat field
> `tokens_24h` (stored on the hub as `totalTokens24h`) is a **cumulative total**,
> not a rolling 24-hour window. Read it as lifetime token consumption for the
> spoke. See [token-tracking.md](token-tracking.md) for the full heartbeat and
> `/api/saas/usage` rollup details.

### Knowledge: Context7 content caps

| Name | Default | Purpose |
|---|---|---|
| `HIVE_CONTEXT7_MAX_SNIPPETS` | `40` | Snippets kept from one Context7 llms.txt fetch (document import or `context7` connector). |
| `HIVE_CONTEXT7_MAX_SNIPPET_BYTES` | `4096` | A single snippet larger than this is dropped whole. |
| `HIVE_CONTEXT7_MAX_TOTAL_BYTES` | `16384` | Total content kept per fetch, marker included; values under `1024` are raised to `1024`. |

Snippets are dropped whole (never clipped) by relevance to the topic, the
stored content ends with a `… truncated: N snippets / M bytes omitted (size
cap)` line, and the fetch logs the library. Unparseable or non-positive values
fall back to the default. The kick primer's own byte cap is the yaml key
`knowledge.primer.max_bytes` (default `4096`); see
[knowledge-curator.md](knowledge-curator.md#kick-primer-size-cap-knowledgeprimermax_bytes).

### Inference endpoint fallbacks

| Name | Default | Purpose |
|---|---|---|
| `HIVE_VLLM_ENDPOINT` | unset | Comma-separated vLLM endpoint list; set only to a cluster-reachable endpoint. Unset disables the built-in vLLM gateway route. |
| `HIVE_LLMD_ENDPOINT` | `http://hive-llm-d-epp.hive-inference.svc.cluster.local:8000` | Comma-separated llm-d endpoint list. |

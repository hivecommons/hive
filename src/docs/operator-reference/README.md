# Hive operator reference

[Contents](../operator-reference.md)

## Agent Go toolchain shim

Container images install `bin/go-wrapper.sh` as `go` in the runtime Go toolchain
and keep the real compiler beside it as `go-real`. When `HIVE_AGENT` or
`HIVE_AGENT_ID` is set, the shim blocks `go test`, `go vet`, and `go tool vet`
inside the hive pod and points agents to CI instead, because those commands can
read or mutate live `/data` state and have killed agent sessions. Human
operators can debug inside the pod by setting `HIVE_ALLOW_LOCAL_GO_TEST=1`; all
other Go subcommands pass through unchanged.

## Abandoned contributor PRs

An abandoned PR is a contributor PR the hive adopts to finish, either **in
place** on the contributor's branch or by **supersession** into a hive PR.
Operator **Adopt now** / `adoption/supersede` takeovers count with reason
`operator_takeover`; automatic adoptions carry `offline` or `unresponsive`.
A relay disconnect or failed task alone is not a PR adoption.

The adoption recorder deduplicates by repository and original PR: adopting the
same PR again does not increment the count. If an in-place adoption is later
superseded, its existing record gains `adopted_into_pr` rather than counting
again. A zero `adopted_into_pr` means adoption in place. Records also carry the
original `repo`, `pr`, `reason`, and timestamp `at`.

The dashboard **Contributors** tile shows **Abandoned N** in muted red only for
nonzero counts. Hover for the five most recent adopted PRs, newest first, with
the superseding PR (or “in-place”), reason, and timestamp.
`/api/status.contributorPool` exposes the historical `abandoned` total and
`abandoned_by_login` counts (lowercase login keys), including offline and
revoked profiles. `/api/contributors` exposes the underlying per-profile
`abandoned` and `abandoned_prs` data. These counters are independent of
Completed/Failed task counts and do not change contributor trust tiers.

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
  enough; defaults fill in the rest — see [agent-configuration.md](../agent-configuration.md)).

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
| `notifications` | ntfy, Slack, and Discord webhooks. | All optional; see [notifications.md](../notifications.md). |
| `notifications.discord.rfc_channel_id` | empty (off). | Discord channel that receives RFC lifecycle notices (RFC issue opened, closed, or taken off hold) through `notifications.discord.bot_token`. Env override `HIVE_DISCORD_RFC_CHANNEL_ID`; see [RFC lifecycle notices](../notifications.md#rfc-lifecycle-notices-discord-bot). |
| `dashboard` | Web UI port, snapshots, auth token, frame allowlist, authorized users. | `auth_token` can come from `HIVE_DASHBOARD_TOKEN`. |
| `agent_sandbox` | Podman-rootless sandbox launcher for hub/pod agents (`pkg/sandbox`). | Opt-in and **two-gate**: this block's own `enabled: true` sandboxes nothing by itself — each agent also needs `sandbox.enabled: true` under `agents.<name>`. The dashboard's Security tab writes only this global flag, so enabling it there alone can leave every agent unconfined — but the tab now shows this: `security.sandboxWarnings` in `GET /api/config/governor` carries `config.AgentSandboxGateWarnings`'s diagnosis (also logged at WARN at boot/reload), rendered both in the page's coherence-warnings box and inline under the toggle, naming the still-unconfined agents and the fixing key (#4918). See [sandbox-isolation.md](../sandbox-isolation.md) and the [getting-started confinement section](../getting-started.md#where-agents-actually-run-read-this-before-l3). |
| `data` | Metrics, logs, session, and agent overlay directories. | Defaults are `/data/...` in containers. |
| `knowledge` | Wiki layers, vaults, git/document sources, connectors (`knowledge.connectors`, see [knowledge-connectors.md](../knowledge-connectors.md)), primer, curator, bead synthesizer. | Disabled unless `enabled: true`. |
| `hub` | Hub/spoke hosted-hive metadata. | Usually provisioner-owned. |
| `hive_id` | Stable spoke identifier. | Usually provisioner-owned. |
| `acmm_level` | Current ACMM pack level. | May be set by hub/dashboard. |
| `quality` | Optional quality-lane capabilities. | `formal: true` enables agent-authored Spin models only at ACMM L5/L6; see [Formal verification](../formal-verification.md). |
| `runs` | Long-running run settings: `runs.external.flue`, the report-only external-execution binding pilot, and `runs.external.omp`, the OMP workbench host behind the same adapter. | Both default OFF and independent. `enabled: true` with no `mode` runs in `shadow` (persists admissions, observes, never starts external work); `mode: report-only` dispatches. Flue dispatches to a runtime at `endpoint` pinned to `workflow_version` and needs a build with the `extwork_flue` tag; OMP offers work over the contributor relay to a workbench peer declaring `ext-exec/omp` whose declared version matches `workflow_version`, and needs the `extwork_omp` tag. Neither host receives a repository credential, dashboard token, or publication tool; OMP (tier T3) is additionally refused any write-capable stage. See [External workflow admission](../design/external-workflow-admission.md). |
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
| `issues.rfc_existing_check.enabled` | `false`. | Triage existing-coverage check (#13575): new open issues labelled `rfc`, `enhancement` or `kind/feature` get one comment that lists the existing settings that may already cover the request. Each setting links to Settings with the search set to its key. The comment carries `<!-- hive:rfc-existing-coverage -->` and is never posted twice. The check never labels or closes issues. Tuning keys are `post_when_nothing_found`, `labels`, `max_matches` and `max_age`; see [Triage: does this RFC already exist?](../settings.md#triage-does-this-rfc-already-exist). |
| `runs.triage.prior_art.enabled` | `true`. | Prior-art gate (#13575): before any agent claims a feature / RFC / epic issue, the scheduler searches knowledge beads, the Settings index, merged PRs (12 months), closed issues, docs and code for the issue's key terms. A human-filed issue whose best finding reaches `runs.triage.prior_art.threshold` (default `70`) is parked at `stage/017-possible-existing` with one `<!-- hive:prior-art-gate -->` comment quoting up to `max_findings` (default `5`) findings and the two replies (`/hive decision proceed — <gap>` / `/hive decision exists`); a clanker-filed one is parked the same way and closed `already-exists` after `bot_close_ttl` (default `72h`) with no reply; a hive-filed one is never parked (findings note only). `kinds` (default `feature, rfc, epic, enhancement`) picks the gated kinds; `max_parks_per_cycle` (default `5`) bounds one cycle. See [Prior-art gate](../spektacular.md#prior-art-gate). |
| `contribute.help_links` | Defaults to Hive contributor docs, the Hive issue tracker, and the Twitch live stream. | Up to 5 `{label, url}` links shown on `/contribute` Onboarding, Operations, and printed once by relays on connect. URLs must be absolute `http://` or `https://`; labels are plain text and length-capped. Prefer the Hive Commons Discord redirect (`https://hivecommons.dev/discord`) over channel URLs for contributors who may not already be server members. |
| `contribute.max_parallel_fixes` | `2`. | Ceiling on how many stage-072 `fix-pr` sub-sessions one contributor's relay may run beside its primary session ([#12436](https://github.com/hivecommons/hive/issues/12436)). The relay advertises its own count (`max_parallel_fixes`, from `HIVE_MAX_PARALLEL_FIXES`); the hub clamps it to this. A relay that advertises no `parallel_sessions` always has capacity 1 (preempt mode). Fixes beyond capacity queue server-side and are served before new work. Settings → Hub → Fix budget. |
| `contribute.min_focus_window` | `30m`. | Preempt mode only: after a preempted primary task resumes, the hub does not preempt that contributor again until it has run uninterrupted for this long. Settings → Hub → Fix budget. |
| `variables` | Trusted variable resolver definitions. | Env-only substitution works without this block. |
| `classification` | Go-consumed subset of `hive-project.yaml`'s `classification:` block — today only `review_bots`, the external review-bot logins whose inline threads on hive-authored PRs the hive addresses and resolves itself. | Off unless `review_bots.logins` names a bot. The same key in `hive-project.yaml` is read when `hive.yaml` has none. See [review-bot-threads.md](../review-bot-threads.md). |
| `claims` | The worker-claim ledger ([#8380](https://github.com/hivecommons/hive/issues/8380)): who is on an issue right now, ranked human > agent > contributor > external, so a person can take an issue over from a hive agent or a relay contributor and the displaced holder is told to stop. | Default ON. `enabled: false` turns recording, enforcement and `/api/claims` off. `human_ttl_s` 4h, `agent_ttl_s` 2h, `contributor_ttl_s` 30m, `max_ttl_s` 24h; the owner can also set these four from **Governor Config → Features → Work intake & issue lifecycle → Issue claims** (a value of 0 or *Reset to default* uses the default; human, agent and contributor may not exceed the max), and a saved change applies without a restart to claims made or renewed afterwards while existing claims keep their expiry ([#10981](https://github.com/hivecommons/hive/issues/10981)); `comment: false` / `label: false` suppress the GitHub issue comment / `stage/020-claimed` label mirror (`label: false` also suppresses the `stage/030-pr-in-progress` start label an agent claim mirrors — formerly `claimed` / `hive/in-progress`, [#11841](https://github.com/hivecommons/hive/issues/11841)). A governor kick only *lists* the issues it names (30-minute hold, no comment, not a claim); the agent claim, with its comment, is recorded on the agent's first start signal on an issue — its own `hive-open-issue claim` request, a `hive-open-pr` request naming it, or a comment with an explicit start statement (`<!-- hive:start -->`, "starting now", an opening "Starting …" / "Implementing …"; see the [recognized forms](../hive-open-issue.md#comment)); a report-only comment (triage, verification, gate check, status), an explicit deferral ([#12387](https://github.com/hivecommons/hive/issues/12387)) or escalation, and any `label` request record no claim. `never_claim_labels` (default `[epic]`, case-insensitive; `[]` turns it off) names umbrella/tracking labels no automated producer may claim and no claim ever stages `stage/020`/`stage/030`; the same admission gate also refuses issues parked on a person (`stage/045-needs-decision`, `needs-spec`, …), `needs-human` and held issues. `escalate_after_claims` (default `2`, negative turns it off) counts those claims only: once one agent's last that-many claims on a free issue were followed by no linked PR, referencing commit, label or assignee change and no close/reopen, the issue is withheld from every kick, labelled `needs-human` and given one comment listing the claims, instead of being claimed again. A `hive/likely-done` issue an agent already started on after the merged PR (its verification) is withheld from kicks until a person comments, labels, assigns, closes or reopens it, so it is verified once rather than every cycle ([#10527](https://github.com/hivecommons/hive/issues/10527)). `reclaim_cooldown_s` (default `21600` = 6h; negative turns it off) keeps a relay contributor session off an issue it released without shipping — task failed, abandoned, or its lease revoked — while other contributors may retry at once after the short global failure cooldown; before it, a relay whose environment could not run a task took the same issue back every ten minutes, flapping its claim ([#13093](https://github.com/hivecommons/hive/issues/13093)). A relay's *disconnect* release (after the [#7838](https://github.com/hivecommons/hive/issues/7838) grace window), a revoked lease and a lease that aged out unresumed each release the issue claim on GitHub too — release marker posted, claim labels removed — so hive agents may claim the issue on the next cycle; `contributor_flap_threshold` (default `3`; negative turns it off) disconnect releases of one issue inside `contributor_flap_window_s` (default `7200` = 2h) put the issue in *contributor cool-off* for `contributor_flap_cooloff_s` (default `21600` = 6h): withheld from every contributor, immediately eligible for hive agents, work detail `contributor cool-off: N drops in W`; and `contributor_starvation_s` (default `14400` = 4h; negative turns it off) is how long contributor claims may keep hive agents off an issue with no PR before the claim becomes advisory and a hive agent may claim it anyway ([#13579](https://github.com/hivecommons/hive/issues/13579)). An issue labelled `blocked` (the review backlog's parking for a finding whose file exists only on an unmerged PR, or `hive-open-issue comment --blocked-on-pr`) is refused by the same admission gate until the tracked PR merges or closes. Driven from `hivectl claim` — see [hivectl.md](../hivectl.md#claim--unclaim--claims--issue-claims). |
| `removed_agents` | Persistent tombstones for deleted agents. | Dashboard/overlay-owned; do not seed casually. |
| `good_first_issue` | Hive-curated `good first issue` advertising: a 15-minute sweep scores open, unclaimed issues and gives the few that qualify GitHub's `good first issue` + `help wanted` labels and one welcome comment pointing at this hive's contributor relay. Not a hold — a hive agent starting the issue removes the label. | Default ON; `enabled: false` opts out. Silent until `dashboard.public_url` (or a hub-assigned dashboard URL) exists. See [Hive-curated good first issues](../good-first-issue.md). |
| `decisions.delegate` | "Let hive decide": the activity monitor's Decide… › *Let hive decide* action has the hive decide an issue or PR parked at *Needs human decision* itself, as the owner's delegate under the owner-delegate policy, recording it as a human decision would be (`/hive decision`, park lifted, queue approval on a PR) with an audit marker. | Default ON (`enabled: true`); `enabled: false` hides the action hive-wide; `repos.<owner/repo>.enabled` overrides per repository. `llm.enabled` (default OFF) consults the owner-delegate decider model for items the rules leave at the reversible default, on the governor reviewer endpoint; `llm.model` overrides the model. `effect_timeout` (default `30m`) is how long a recorded decision may go without its expected effect before a repeat click answers "decided, but it hasn't taken effect" with the plain reason and the next actions, and the hive posts one follow-up comment. Rails and policy in [Dashboard sections → Let hive decide](../dashboard-sections.md#let-hive-decide). |
| `chat.grounded_answers` | Hive Chat grounded answers: a chat message that is not a bare command (`/agents stuck`, `/prs waiting on review`, "which agent completed last?") is answered by one read-only model turn over a capped snapshot of the hive state the dashboard already holds (agents, open PRs, actionable issues, decisions, governor, alerts, version), citing the items used or saying it does not have the data. Chat never takes actions on this path; every attempt is audited as `chat_grounded_answer`. | Default ON (`enabled: true`); `enabled: false` makes scoped questions answer with the command's usage instead. `model` overrides the governor reviewer model for the turn. See [Hive Chat](../dashboard.md#hive-chat). |
| `governor.first_timer_welcome` | First-time contributor welcome: when a human whose GitHub `author_association` is `FIRST_TIME_CONTRIBUTOR`/`FIRST_TIMER` opens a PR, post one comment (per author per repo, marker `<!-- hive:first-timer-welcome -->`) that thanks them and invites them to run their own hive — profile, leaderboard, trust-tier progression, offline review-fix/rebase handling. Bots, `author/clanker`/`author/hive` PRs, drafts and known relay contributors are skipped. | Default ON; `enabled: false` opts out; `template_path` overrides the text. Silent without a public dashboard URL. See [Welcoming first-time contributors](../good-first-issue.md#welcoming-first-time-contributors). |
| `sla` | Delivery SLA targets (`sla.targets.start_to_pr_h`, `pr_to_merge_h`, `open_to_merge_h`, hours; defaults 4 / 24 / 72) the dashboard's Throughput section colours its Delivery SLA spans against. | Metrics are derived from the lifecycle timeline; no extra GitHub calls. Also editable from **Settings → Thresholds**. See [Delivery SLA](sweeps-and-operations.md#delivery-sla). |
| `sentinel` | Suspicious-activity alerts: flags open PRs (any author) that touch sensitive files or look like security overrides, privilege escalation, or codebase damage. | Default ON; observe-and-alert only. See [Suspicious-activity alerts](sweeps-and-operations.md#suspicious-activity-alerts-sentinel). |

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
| `governor.logging.kick_log_source` | `scrollback`. Also `transcript` or `auto`; unknown values mean `scrollback`. `HIVE_KICK_LOG_SOURCE` (when set to a valid value) wins over this key. | What per-kick logs (archives and the live full log) are built from: the tmux scrollback, the backend CLI's own transcript (Copilot CLI, Claude Code), or the transcript when a reader exists else the scrollback. Editable under **Settings → Logging → Kick log source**; applies to the next kick without a restart, and each log's header `source:` line (and the kick history page) shows which was used. See [agent-logging.md](../agent-logging.md#per-kick-log-history). |
| `governor.labels.automerge` | Defaults to `lgtm`. | Label applied when a merger/owner queues a PR for Hive auto-merge-on-green. With Prow's commit binding (prow-github-actions v3), Hive binds `lgtm` by also setting the `prow/lgtm` commit status (needs the App's `statuses: write` permission); alternatively choose a distinct label such as `hive/auto-merge`. Distinct from the [App self-merge sweep](#app-self-merge-sweep-auto_merge), which needs no label and no human queuer. |
| `project.repo_policies[].auto_merge` | effective `false` below L6; unset = `true` at L6 | Per-repo off switch. Switching to L6 turns this on for every active repo; owners can toggle repos afterward. `false` lets Hive open PRs for that repo but blocks all Hive merge paths (`hive-merge`, App self-authored sweep, and proxy-visible direct REST/GraphQL merge attempts). The dashboard repo-card switch persists this key and takes effect without restart. |
| `project.repo_policies[].label_driven` | Off (unset) for every repo. | Opt-in for repos whose maintainers accept and park issues with labels and their own bots (for example `needs-triage` → `triage/accepted`). On such a repo the un-park sweep posts no "What to reply" notice and never removes `needs-human`, `needs-decision` or `needs-direction` on a `/hive` comment alone; instead a trusted triager applying an approval label releases the issue and counts as human sign-off — see [Label-driven approval](#label-driven-approval). Editable from the dashboard: Settings → Repos → per-repo "Label-driven approvals (Prow-style)" (write or owner role). Details in [maintainer-commands.md](../maintainer-commands.md#label-driven-repositories). |
| `project.repo_policies[].label_driven_approval_labels` | Unset: inherits `project.label_driven_approval_labels`. | Per-repo approval-label list; only read when `label_driven` is set. Editable from the dashboard sub-row of the repo's label-driven toggle. |
| `project.repo_policies[].companion_repos` | Unset (no companions). | List of sibling repositories (`name` in the project org, or `org/name`) that this repo's tooling expects checked out beside it under the agent workspace root — for example a docs/website repo a spec tool registers at `<workspace>/<org>/<sibling>/…`. Contributor task prompts and resident-agent kicks carry a shallow-clone plan for each companion at `<workspace root>/<org>/<sibling>` (`$HIVE_WORKSPACE_DIR` for contributor agents, `$HOME/workspace` for resident agents), and agents are told that a tool reporting one of those paths missing means *clone it*, never park the issue. Self-references and malformed entries are dropped. See [running-at-level-6.md](../running-at-level-6.md#needless-parks). |
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
| `evidence.enabled` | On by default. | The review relay writes one [review evidence bundle](../review-evidence.md) per PR head to `/data/evidence/<owner>/<repo>/<number>/<head>.json` when it records a verdict or posts a review; the sentinel sweep adds its findings to the flagged head's bundle; and every merge path adds the head's CI check-run summary, the human approvals and label changes, and the merge event. Owners and mergers read bundles through `GET /api/review/evidence` and `GET /api/review/evidence/list` (see the [API reference](../api-reference.md)). Set `false` to stop writing new bundles; existing ones are left in place. |
| `evidence.signing_key_file` | Unset (bundles unsigned). | Path to an Ed25519 private key (32-byte seed or 64-byte key, hex or base64). When set and readable, bundles are signed. Dashboard/API readers require a valid signature under its public half and fail closed when the configured key is unavailable; unsigned mode verifies only the bundle content hash. Writers still record `"signed": false` when the key is empty, absent or unusable and log a warning for an unusable key, so an evidence write failure never blocks review or merge work. Keep the key off shared hosts. |
| `evidence.complexity.enabled` | On by default (when `evidence.enabled` is on). | Once a month, for each managed repo, the hive downloads the default-branch tarball through the GitHub App and writes one Go cyclomatic-complexity snapshot to `/data/evidence/<owner>/<repo>/complexity/<YYYY-MM>.json` (see [Monthly complexity snapshot](../review-evidence.md#monthly-complexity-snapshot-report-only)). Report-only: it never affects merge eligibility. Cheap by design — one archive download and an AST pass per repo per month, no module downloads and no compile. An existing month is never rewritten; the hive re-checks hourly and retries a failed repo on the next check. The last 24 months are kept per repo. Set `false` to stop writing snapshots; existing ones stay. |
| `evidence.complexity.static_analysis` | `false`. | Also run `go vet` and (when installed) `staticcheck` on the snapshot checkout and record finding counts per tool. Off by default because it compiles each repo and downloads its modules, which is not cheap on a spoke with many or large repos. |
| `review.all_authors` | Deprecated no-op; accepted and logged once. | Every open PR is reviewed regardless of author — hive agent, human, clanker, dependabot or any other bot — whenever review is on (`require_approval` and `fan_out`); there is no author-class opt-out below that switch ([#12974](https://github.com/hivecommons/hive/issues/12974)). The knob used to be required for human PRs. It never lets an agent push to those PRs (see the next row). Remove it from `hive.yaml`. |
| `review.fix_human_prs` | Retained for compatibility; treated as push-disabled. | No longer lets review-fix kicks push commits onto PRs whose login author is not a hive agent/App, even when the PR is hive-attributed ([#13124](https://github.com/hivecommons/hive/issues/13124)); the one exception is a PR the stage reconciler adopted in place (`stage/074-adopted-by-hive`). A `changes_requested` verdict on such a PR stops at the published review, with the proposed fix as a suggestion or patch block in the comment, and the refusal is audited as `review_fix_withheld` with the setting's actual value. When the setting is on, the review-thread reconciler still answers review-bot threads on those PRs, by suggestion only, never by checkout or push ([#12897](https://github.com/hivecommons/hive/issues/12897)). Existing configs may still store `true` for UI/audit continuity, but DCO squash attribution means Hive cannot add a compliant follow-up commit without forging the PR author's sign-off. See [review-swarm.md](../review-swarm.md#who-may-be-pushed-to-all_authors-and-the-retired-fix_human_prs-push). |
| `review.severity.block_at` | Unset; owner-only. | Lowest finding priority that blocks merge: `P1` (ship fast, P0–P1 block), `P2` (strict, P0–P2 block) or `P3` (everything blocks). Also drives `classification.review_bots.min_priority` when that key is not set in `hive.yaml` or `hive-project.yaml`. Any other value fails load naming the field. Features -> Review gate -> Severity & backlog. See [review-swarm.md](../review-swarm.md#blocking-line-reviewseverity-reviewbacklog). |
| `review.severity.comment_below` / `backlog_below` | `true` / `true`; owner-only. | Whether findings below the blocking line are posted as non-blocking comments and filed to `review.backlog`. |
| `review.backlog.destination` | `github_issue`; owner-only. | `github_issue`, `github_project`, `linear` or `jira`. The last three need the matching `governor.work_source.type` (`github_projects`, `linear`, `jira`); on a mismatch the hive falls back to `github_issue`. Destination fields: `project_column_id` (required for `github_project`), `linear_state`, `jira_status`. |
| `review.backlog.labels` / `max_per_pr_per_day` | `[agent/reviewer]` / `10`; owner-only. `from-review`, `origin/review`, and `origin/reviewer` are accepted and mapped to `agent/reviewer`. | Labels on every backlog item and the per-PR-per-day cap (negative fails load; 0 = default). Reviewer is an agent lane, so `agent/reviewer` is the routing/ownership/provenance label. Non-lane hive issue filers continue to use `origin/selfreport`, `origin/governor`, `origin/dashboard`, `origin/forge`, or `origin/convergence`; the one-time startup migration (`/data/origin-label-migration-v4.done`) removes redundant lane-origin labels and backfills only non-lane origins. |
| `review.reviewers.autoscale.*` | `enabled: false`; when on: `min: 1`, `max: 4` (≤ the replicas cap 5), `target_backlog_per_reviewer: 2`, `scale_up_cycles: 2`, `scale_down_cycles: 5`, `cooldown: 5m`, `cpu_per_reviewer: 1.0`, `mem_per_reviewer_mb: 2048`, `agent` unset (first `role: reviewer` agent, else `reviewer_agents[0]`); owner-only. | Sizes the reviewer lane to its backlog ([#14685](https://github.com/hivecommons/hive/issues/14685)): adds a `reviewer-N` replica after `scale_up_cycles` cycles with more PRs waiting for a free reviewer than `target_backlog_per_reviewer` per reviewer (or a wait older than the review-pending stage dwell), drains the top replica after `scale_down_cycles` idle cycles and retires it only when its turn ends. Scale-ups are refused by named guards (`at-max`, `cooldown`, `saturated` kicks, `provider-budget`, `provider-headroom`, `max-parallel-reviews`, cgroup `cpu` / `memory`). Decisions log as `reviewer autoscale: 2→3 reason=…`; state is in `/api/status.reviewerAutoscale`. See [review-swarm.md](../review-swarm.md#reviewer-autoscale-reviewreviewersautoscale). |
| `review.contributor_prs.base_sync` | Off by default; owner-only. | Lets PR follow-up use GitHub's update-branch API on hive-authored lane PRs whose head is in a contributor fork, whose `mergeable_state` is `behind`/`dirty`, and whose author enabled "allow edits by maintainers". `behind` PRs get a GitHub-authored merge commit; `dirty`/422 falls back to the `hive-base-moved` repair note and kick. This requires an agent/proxy mode with `ISSUES_PRS_MERGE` because `PUT /pulls/{n}/update-branch` is merge-scoped. It does not rewrite history and does not add a human/bot-authored DCO commit; the repository's "Sign-off survives the squash" check examines the PR's own commits, not GitHub's update-branch merge commit. |
| `governor.advisory.post_retry_backoff_s` | `3600` seconds. `HIVE_ADVISORY_POST_RETRY_BACKOFF_S` overrides the default when the key is unset; a negative value disables the pause. | How long the advisory digest write is paused after GitHub refuses it with `403 Resource not accessible by integration`. That refusal is structural (foreign-authored comment, repo dropped from the installation, or repo transferred to another owner), so per-cycle retries only burn API quota. The failed edit first falls back to creating a fresh App-authored comment; if the create is refused too, the error is recorded for the hub health row — naming the new `owner/repo` when the repository has moved — and attempts pause for this window. See [advisory.md](../advisory.md#when-the-write-is-refused). |
| `governor.merge_train_stall_warn_s` | `3600` seconds; `0` disables | How long the head of a merge train may sit in one stage before the Activity Monitor reads the train **stalled** — the queued chip turns to "Merge queue · stalled — waiting on #15253 (waiting for CI) · 3h", the Merge queue popover marks the train, and the activity log records one `merge train stalled` line per train per hour naming the head, its state and how long. A soaking head is on its clock, not stalled, until that clock itself overruns the window ([#15395](https://github.com/hivecommons/hive/issues/15395)). |
| `governor.trajectory.enabled` | Defaults to enabled. | The lane no-ops until a reviewer endpoint and model resolve from `governor.trajectory` or `governor.litellm`. |
| `governor.kick_limits.max_issues` / `max_prs` | `100` / `50` | Caps on every issue list and every PR list (actionable, stale drafts, merge-eligible, CI-failing) rendered into a kick prompt; a cut list ends with an explicit "… and N more" line. An absent key or a negative value means the default; an explicit `0` opts out of the cap entirely (`KickListUnlimited` — every item is listed), and anything above `500` is pinned to `500`. Editable without touching this file under **Settings → Repos → Kick prompt list caps**. Truncation drops the *newest, lowest-priority* items: issues are listed oldest first, and PRs in review-priority order (fixes, then refactors/docs, then tests, oldest first within each class), so lowering a cap never hides the most urgent work. Raise `max_prs` only if your agents actually work more than 50 PRs per turn; the tail of a long list costs tokens and delivery time on every kick without changing behaviour (#7368). |
| `project.agents_md_max_bytes` | `8192`; values under `2048` are raised to `2048`. | Caps the repo `AGENTS.md` block prepended to a kick. Whole sections (split at `#`/`##` headings; each resolved skill is its own unit) are kept or dropped, never clipped mid-rule; when any are dropped the kick carries a one-line `⚠️ AGENTS.md exceeded N bytes; … — read AGENTS.md …` warning naming them and the scheduler logs it. See [agents-md.md](../agents-md.md#size-cap-whole-sections-visible-warning). |
| `project.style` | unset (built-in text applies) | Hive-wide **house style**: a short how-to-change rule list prepended to every kick on every backend, directly under the header. A managed repo's root `HIVE_STYLE.md` (read from `project.checkouts_dir`) overrides it; precedence repo > hive > built-in. Inline text wins over `project.style_file`. See [house-style.md](../house-style.md). |
| `project.style_file` | unset | Path on the hive host holding the hive-wide house style; used when `project.style` is empty. |
| `project.style_max_bytes` | `4096`; values under `512` are raised to `512`. | Caps each house-style candidate. An oversize candidate is skipped whole (not clipped) and the next source applies; the skip is audited as `house_style_skip`. The repo file is also run through ioscan at the issue-body policy and refused whole on a block. |
| `dashboard.snapshot_frame_ancestors` | Empty list means CSP `frame-ancestors 'none'`. | Entries must be exact `https://` origins; paths, wildcards, credentials, query, and fragments are rejected. |
| `dashboard.authorized_users` | Empty means no per-user direct-route allowlist. | Entries can be `user` or `user:role`; roles are `read`, `read-write`, `merger`, `owner`. |
| `dashboard.public_url` | Empty means OAuth redirect URIs (Linear agent install, OpenRouter funding) fall back to `hub.dashboard_url`, then the request's forwarded/`Host` origin. | Set to this dashboard's externally reachable origin (`https://hive.example.com`, no path/query) on a standalone hive whose callback is published on a different hostname or whose ingress rewrites `Host`. Must be an absolute `http(s)://` origin; a trailing slash is trimmed and anything else fails config load. See [linear-agent.md](../linear-agent.md#setup). |
| `dashboard.experiment_lab` | Off by default. | Hides the Experiment Lab (formerly Strategy Lab) while that surface is being reworked. Set `dashboard.experiment_lab: true` to show the Experiment Lab section, sidebar nav entry, and Nous status controls. |
| `dashboard.strategy_lab` | Off by default. **Deprecated** alias of `dashboard.experiment_lab`, kept for one release (#11695). | `true` still enables the Experiment Lab and logs a one-line deprecation at config load; the value is folded into `dashboard.experiment_lab` so the next config save writes the documented key. |
| `dashboard.matrix` | Off by default (`enabled: false`, `repos: []`, `exclude_paths: []`). **Experimental.** | Opt-in Activity Monitor Matrix display block (#15862); nothing reads it yet. `repos` is an explicit exact `owner/name` allowlist (no wildcards, URLs or bare names) and an empty list captures nothing, so `enabled: true` alone stays inactive. `exclude_paths` adds non-empty path globs to the built-in deny-list. There is no `authors` or `private_repos` key: authors are fixed and private repos are always excluded. |
| `hub.contribute_announcement` | Empty/off by default. | Operator notice for contributors. Set from Governor → Hub or `/contribute` Management (owner/read-write): `text` is server-sanitised plain text capped at 500 characters and rendered only through text-safe sinks (empty clears), `level` is `info`/`warning`, `expires_at` is optional RFC3339, and the server rotates `id` whenever text changes so dismissed Operations/Profile banners reappear. Active notices show on `/contribute` Operations/Profile, above the Onboarding command block, and in connected relay terminals. |
| `hub.contribute_repo_filters` | Empty map means repos inherit the hive-wide contributor title, author, and label filters exactly as before. | Optional full `owner/repo` keys that narrow one repository's `/contribute` queue without widening the hive. Hive-wide skip labels and hive-wide filters always run first; a repo filter can only add deny matches or require an allow-mode title/author/label match for that repo. Edit from `/contribute` → Management → Repos for Contribute → Filters or Governor → Hub. Operations lists configured overrides and withheld diagnostics name repo matches, e.g. `Repo filter label "2-discussing" denied for projectbluefin/common`. |
| `hub.contribute_wall_enabled` | Off by default. | Opts the hive into the public contributor wall on `/contribute` Operations. The Management tab and Governor Hub settings mirror this switch. When off, the panel is hidden and write endpoints refuse posts. |
| `hub.contribute_wall_retention_days` | `0` means the dashboard default (90 days). | Bounds wall posts stored on the hub data volume next to contributor profiles. Older posts are pruned on load/write; use a positive number to change the retention window. |
| `hub.nps_enabled` | Unset means on for hosted spokes (`hub.hive_type: hosted`) and off for every other install. `HIVE_NPS_ENABLED` overrides it. | Enables the dashboard NPS feedback prompt, whose responses are forwarded to the hub over the spoke's authenticated hub link, without the user's identity, and are visible only to hub admins. A hive with no hub link sends nothing unless a relay is configured (below). See [nps.md](../nps.md). |
| `hub.nps_relay_url` | Empty (relay disabled). `HIVE_NPS_RELAY_URL` overrides it. | Base URL of the NPS relay. A standalone spoke (NPS enabled, no hub link) self-registers a generated key there and posts signed responses to it (no token to configure); a hub pulls them from it. `https` only, except `http` to a loopback host. See [nps.md](../nps.md#standalone-hives-the-nps-relay). |
| `hub.nps_relay_pull_secret` | Empty. `HIVE_NPS_RELAY_PULL_SECRET` overrides it. | Secret, hub only. The credential the hub uses to pull and ack relay entries. Prefer the env var. |
| `hub.snapshot_url` | Empty (unset): no snapshot URL is advertised in the hub heartbeat. | Public URL of this hive's snapshot page, sent to the hub in the heartbeat (normally written by hosted-hive provisioning). Must be an absolute `http(s)://` URL with an optional path; credentials are rejected and a trailing slash is trimmed. A non-`http(s)` scheme, missing host, or credentials fails config load and is a `400` from `PUT /api/config/governor/hub` (nothing from that save is applied). Query strings and fragments are also a `400` from the dashboard, but at config load a legacy value carrying one is preserved verbatim (no trailing-slash trimming) with a startup warning so existing hives keep booting; remove the suffix before editing the field through the dashboard. |
| `knowledge.connectors[]` | Empty (no connectors). | Scheduled sync of external knowledge into vault facts; types `git`, `document`, `github-wiki`, `confluence`, `notion`. Credentials only via `auth.env` / `auth.file` (inline secrets fail validation). Facts land in `/data/knowledge/connectors/<layer>`; status at `GET /api/knowledge/connectors`, owner-only `POST /api/knowledge/connectors/{name}/sync`. See [knowledge-connectors.md](../knowledge-connectors.md). |
| `knowledge.connectors[].scope` (`confluence`) | `deployment` auto-detects Cloud for `*.atlassian.net`; `max_pages` `2000`; `full_sync_every` `10`. | `base_url` plus `spaces` and/or `root_page_ids`; Cloud needs `email` (Basic auth with the API token), Data Center uses the token as a bearer PAT. Give the account read-only space permission. |
| `knowledge.connectors[].scope` (`notion`) | All pages shared with the integration; `max_pages` `2000`; `full_sync_every` `10`. | Optional `root_page_ids` / `database_ids`; `include_archived` fetches bodies of archived pages. Pages must be shared with the internal integration (••• → Connections). Requests are spaced 350 ms apart. |
| `knowledge.publish` | Unset (`connector` empty): publishing off. | One-way mirror of curator-promoted facts into one connector target: `connector` (a `knowledge.connectors` name whose type can publish), `layers` (`project`/`org`/`community`; personal is never published), `root`, optional `include_types`, `dry_run`, `propose_via`. Facts are read from each layer's `knowledge.layers[].path`. Started at boot (a bad connector logs `knowledge publish mirror disabled` and leaves publishing off); state in `/data/knowledge/connector-state/_publish`. Status in the `publish` object of `GET /api/knowledge/connectors`; owner-only `POST /api/knowledge/publish/sync`. See [knowledge-connectors.md](../knowledge-connectors.md#publish-mirror). |
| `knowledge.bead_synthesizer.publish` | Off (`enabled: false`). | Publishes synthesized facts into a repository through one PR on `hive/bead-synth-knowledge`: `enabled`, `repo` (empty = `project.primary_repo`), `path` (default `docs/knowledge/`; `.hive/knowledge/` is the alternative; relative, no `..`), `base_branch` (empty = repo default branch), `mode` (`pr` default or `commit`), `cadence` (`per_run` default or `daily`). ACMM L<=4 hives publish via PR only; `commit` is warned about and still opens a PR. See [knowledge-curator.md](../knowledge-curator.md#repository-publish-knowledgebead_synthesizerpublish). |
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
See [hive-open-issue.md](../hive-open-issue.md#retries-and-what-happens-when-a-request-cant-be-fulfilled).

For runtime precedence and provenance, see [config-layering.md](../config-layering.md).

## App self-merge sweep (`auto_merge`)

Three different mechanisms merge PRs automatically, and they share the word
"automerge" without sharing much configuration:

- **The human queue** — a merger/owner applies the `governor.labels.automerge`
  label (default `lgtm`) and Hive squash-merges the PR once CI is green. A
  human decision starts it. See
  [contributor-trust-and-roles.md](../contributor-trust-and-roles.md).
- **The App self-merge sweep** (`SweepSelfAuthoredAutoMerges`,
  `src/pkg/github/automerge/automerge_sweep.go`) — a background loop that merges the
  App's **own** open PRs with no human queue-approval at all. This is what the
  top-level `auto_merge:` block controls.
- **The merge-request watcher** (`hive-merge`) — agents request merges through
  a result-file protocol and the watcher verifies CI evidence itself before
  merging. It reads `auto_merge.no_ci_ok` for repos with no CI by design; the
  older `allow_unprotected_base` key is now accepted but ignored. See
  [hive-merge.md](../hive-merge.md).

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
| `auto_merge.allow_unprotected_base` | deprecated no-op | Accepted for compatibility only. [`hive-merge`](../hive-merge.md) no longer refuses solely because a base branch has no GitHub branch protection; it may merge into any branch the App can write after positive CI evidence. |
| `auto_merge.no_ci_ok` | unset (refuse) | **Merge-request watcher key, not a sweep key.** Per-repo opt-in that downgrades only the "unverified" CI verdict (zero statuses, check runs, and workflow runs) to green, for adopted repos with no CI by design. Red and pending verdicts are never downgraded (#6281). See [hive-merge.md](../hive-merge.md). |
| `auto_merge.human_merge_paths` | unset | Per-repo map of `owner/repo` to glob patterns (same syntax as `intent.guardrail_path_patterns`) for paths a person must merge, e.g. `Danathar/goodreads-mcp: [".claude/settings.json", ".claude/hooks/**"]`. Every App merge path honors it: the automerge sweep lanes (App self-merge, trusted-author, label-queued) and the agent merge-request relay refuse a PR that touches a listed path, add `hold`, and post one `<!-- hive-human-merge-path -->` comment naming the path(s); a person must merge it. If the changed-file list cannot be fetched completely for a configured repo, the merge is withheld (#11038, #11039). |
| `auto_merge.human_merge_title_prefixes` | `["⚠️", "⚠"]` | PR-title prefixes that mean "breaking change — owner merges" (U+26A0 with or without the VS16 selector by default, matched after trimming leading whitespace). One shared predicate, `automerge.RequiresHumanMerge(pr)`, runs where each lane computes eligibility — App self-merge, label-queued, trusted-author and contributor lanes, plus the refetch before a merge — so a matching PR is never merged by hive and never fronts a merge train, however green and however approved. The PR parks at stage 090 *Ready for human merge* with the `human merge` chip and reason *breaking change — owner merges*; the owner merges it with **Approve & merge as maintainer** or on GitHub. Hive posts one writeback-gated comment saying it will wait for a human. Set to `[]` to turn the title rule off. Added after a ⚠️-titled PR was auto-merged by the trusted-author lane while its owner was still deciding whether to ship it. |
| `auto_merge.human_merge_labels` | `["needs-owner-merge"]` | Labels (case-insensitive, trimmed) that carry the same "a person merges this" meaning as the title prefixes above; either match is enough. Set to `[]` to turn the label rule off. |
| `auto_merge.require_current_base` | unset | Repo list (`owner/repo`) with an explicit **on** override for the current-base merge gate (Settings → Repos → "Merge only on current base"). On a gated repo the App self-merge, trusted-author and contributor lanes only merge a head that contains the current base tip. A green head tested against an older base is updated with GitHub update-branch (a merge commit; no author commit is rewritten) and merges once CI is green on the new head, so a semantic conflict with work merged since turns that PR red instead of breaking the base ([#13634](https://github.com/hivecommons/hive/issues/13634), [#15335](https://github.com/hivecommons/hive/issues/15335)). Costs one compare call per merge candidate. Listing a repo here is also **strict**: a behind head the App cannot update (fork without maintainer edits, branch protection or a missing `contents: write`) is parked (`fork-not-editable` / `update-branch`) rather than merged as tested. Wins over `skip_current_base` and over the default. |
| `auto_merge.require_current_base_default` | `false` | Hive-wide default of the current-base gate for repos in neither `require_current_base` nor `skip_current_base` (Settings → Features → Auto-merge → "Merge only on current base"). **Off** by default: the merge train already update-branches overlapping siblings, so the gate is being soaked per repo (hive's own hive first, via `require_current_base`) before any hive turns it on everywhere. When `true`, a behind head the App cannot update merges as it was tested, with one WARN per PR (`automerge current-base gate: … merging as tested`), so fork-heavy repos are not parked wholesale. |
| `auto_merge.skip_current_base` | unset | Repo list (`owner/repo`) with an explicit **off** override for the current-base gate (Settings → Repos → "Merge only on current base" unchecked): a green head there merges even if the base has moved, whatever `require_current_base_default` says. An explicit `require_current_base` entry wins over this list. |
| `auto_merge.merge_forward_trusted_human` | unset (off) | Repo list (`owner/repo`) where a `role/trusted` + `author/human` PR on a same-repo branch that GitHub reports as conflicting (`dirty`) gets the base merged into its head as one App-authored, signed-off merge commit via the GitHub merges API (no human commit is rewritten), then merges once CI is green on the new head. A real conflict pushes nothing and posts one `<!-- hive:merge-forward-conflict head=… -->` note naming the files both sides changed. Fork heads are never written. Attempted once per head (#13636). |
| `auto_merge.merge_train.enabled` | `true` | Conflict-aware merge train in every automerge lane (hive, trusted-author, contributor) ([#15117](https://github.com/hivecommons/hive/issues/15117)). Each sweep groups the open approved / mergeable-pending PRs of one repo+base by overlapping changed files (`GET /pulls/{n}/files`, cached per head SHA) and lets ONE front per group merge — ordered readiness first: a head the lane last saw at the merge gate (green, approved, soaking or waiting on a sweep) leads, one it has not evaluated since its push comes next, one still at CI / Review trails, then clean-mergeable before behind, then oldest; recomputed every pass, so a member that turns green moves up and one that turns red moves back — while the others wait at stage/080 with lane reason `merge-train:queued-behind#<front>:<pos>/<size>:…` (tracker pill "In merge queue · 3 of 4 · ~4m", never a human stage, never `needs-rebase` as a human flag). The moment the front merges, every sibling whose branch is merely `behind` gets `PUT /pulls/{n}/update-branch`; a `dirty` one is handed to the fixer's rebase-resolve episode (`requested_by: merge-train`, the shared two-attempt guard of [#15104](https://github.com/hivecommons/hive/pull/15104); the fixer router still requires a head the hive can push to, so a fork without maintainer edits stays on the plain `conflicting` path for a person). A head the lane last saw blocked on its own (red required check, terminal hold, permanent merge refusal) never fronts, and a group whose best member is blocked imposes no ordering (fail-open) — nobody waits behind a PR that is not moving. One INFO line `merge train` per order change, not per cycle. The queue carries one PR per issue: when two open PRs declare the same `Fixes #N`, the sweep keeps one (green first, then the earlier-opened) and holds the other with lane reason `merge-train:duplicate-of#<winner>:<issue>/0:0/0:` (tracker pill "Duplicate of #15402 · needs a pick"; *Let hive decide* supersedes, [#15395](https://github.com/hivecommons/hive/issues/15395)). That pick sees one lane; when the two claimants sit in different lanes (the hive's own PR and a trusted human's PR for one issue) every lane holds its candidate with lane reason `merge-train:claim-conflict#<other>:<issue>/0:0/0:` and the log line `same-issue claim conflict undecided: #A vs #B`, so neither is admitted until a decision is recorded on one head or the other PR closes — the PR parks at 085, the chevron reads "held · duplicate of #B · needs decision", and the row's options name the concrete PRs; the hive recommends and never picks ([#15810](https://github.com/hivecommons/hive/issues/15810)). `false` restores independent per-PR evaluation. A PR GitHub reports conflicting on its own, or one at stage/072 returned-to-contributor (label or stage record), is never a member nor counted in the train's size and draws no train idle comment; a conflicting PR rides only while the train itself is repairing it (its rebase requested behind a merged front, loop guard not spent) ([#15395](https://github.com/hivecommons/hive/issues/15395)). |
| `auto_merge.merge_train.max_rebase_attempts` | `2` | Loop guard: how many rebase-resolve requests the train makes for one PR behind one front. Past it the PR moves to stage/085 with the pill "Needs human decision · rebase failed twice" and the decision option "Rebase, then merge as maintainer". |
| `auto_merge.native_merge_queue.enabled` | `false` | Opt-in: once the lane's gates pass, enqueue the PR into GitHub's native merge queue (App auth only) instead of merging directly, and withdraw only Hive's own entries when the PR loses eligibility ([#15198](https://github.com/hivecommons/hive/issues/15198)). Off keeps the pre-queue behaviour everywhere, including the `merge-queue-required` park. See [native-merge-queue.md](../native-merge-queue.md). |
| `auto_merge.native_merge_queue.repos` | unset (every watched repo once enabled) | `owner/repo` allow-list for the hand-off, matched case-insensitively. |
| `auto_merge.native_merge_queue.skip_current_base_gate` | `false` | Explicitly lets a queue-enabled base bypass the `auto_merge.require_current_base` gate and the `behind` update-branch, because the merge group re-tests the merged result and a pushed head drops the PR from the queue. Off keeps both gates in force on queue bases. |
| `auto_merge.native_merge_queue.probe_ttl` | `30m` | How long one base branch's `isMergeQueueEnabled` GraphQL answer is cached before re-reading. |
| `auto_merge.hive_superseding.auto_merge` | `true` | Owner rule: a PR the hive App itself opened to supersede another PR (`Supersedes #N` in its body — takeover, close-first adoption, re-author) auto-merges with no operator decision. A reviewer `requires_human`, security-sensitive included, is routed to the fixer as `changes_requested` instead of parking at `stage/085`; the head merges through the hive-authored lane once CI and review are clean. The only stops are an unacknowledged sentinel finding, a hold, exhausted fix attempts (CI still red / base still conflicting), or a conflict the hive already failed to rebase twice — each named in the 085 caption as `hive-superseder-hard-stop:<reason>`. `false` restores the old park. See [hive-merge.md](../hive-merge.md#hive-authored-superseding-prs-auto-merge). |
| `auto_merge.squash_only` | unset (no repo) | Repo list (`owner/repo`) whose branch policy forbids merge commits. Elsewhere, a `role/trusted` PR whose head mixes the contributor's commits with App-linked, App-signed ones (the hive's own in-place adoption fixes), every commit self-signed, is labelled `merge-method/merge` and merged with a merge commit instead of parking at `stage/085` as `adoption-stalled:hive-signoff-attribution`; a listed repo keeps squash-only policy and re-homes an in-place adoption to an App-authored superseder instead (#13975). |
| `auto_merge.credit_issue_authors` | `true` | Append `Co-authored-by: <login> <id+login@users.noreply.github.com>` for the human authors of the issues a PR closes (`Fixes|Closes|Resolves #N`) to the squash (or merge) commit message on every hive-performed merge, so reporters land on the contributor graph when an external clanker fixed their issue. Bots, the PR author and logins already credited on the branch are skipped; the logins are recorded as `issue_authors=` on the merge audit entry and in review evidence. One cached `GET /issues/{n}` per newly referenced issue. `false` leaves GitHub's default squash message untouched (#14581). |
| `auto_merge.bot_takeover.enabled` | `true` | When `github-actions[bot]` is in `auto_merge.trusted_bot_authors`, re-author its same-repo PRs whose required `pull_request` checks can never run (the bot opened them with a workflow `GITHUB_TOKEN`). After the head is older than both `min_head_age` and 30 minutes with no `pull_request` run, the App pushes the same change as one signed-off commit to `hive/takeover-<n>`, opens a PR with the original title/body plus `Supersedes #<n>`, comments `Superseded by #N` on the bot PR and closes it. One attempt per bot head SHA. See [hive-merge.md](../hive-merge.md#github-actionsbot-pr-takeover). |
| `auto_merge.trusted_authors.enabled` | `false` | Enables the opt-in human-authored PR tier: Hive may merge a green PR only when the author already holds the required hive role and, by default, GitHub push/maintain/admin permission on that repo. |
| `auto_merge.trusted_authors.repos` | empty = all watched repos | Optional repo allow-list for the trusted-author tier. The dashboard renders this as a watched-repo multi-select; selecting nothing preserves the all-repos default. |
| `auto_merge.trusted_authors.require_role` | `merger` | Minimum `dashboard.authorized_users` role the PR author must hold (`merger` or `owner`). |
| `auto_merge.trusted_authors.require_github_permission` | `true` | Also require GitHub to report author write access; API errors fail closed. Fork PRs still need this check even if the setting is false. |
| `auto_merge.trusted_authors.exclude_labels` | `hold`, `do-not-merge`, `needs-human` | Labels that keep a PR out of trusted-author auto-merge. |
| `auto_merge.contributor_lane.enabled` | `true` | Enables the contributor-lane tier: Hive merges a green, soaked, reviewer-approved PR whose author holds a `require_tiers` tier in the contributor registry (`registry_tiers_sufficient`), or whose linked issue carries this hive's App-authored `hive:claim kind=contributor` marker naming the PR author with a `tier=` in `require_tiers` and `verified=true`. Grants unattended merge only to contributors an operator granted such a tier and whose GitHub login was proven; set `false` to make every contributor PR wait for a human `/lgtm` + `/approve`. See [hive-merge.md](../hive-merge.md#contributor-lane-auto-merge). |
| `auto_merge.contributor_lane.soak_minutes` | `30` | How long the current head must stay green before the tier merges it; a new push restarts the soak. `0`/unset means the default (30); the shortest real soak is `1`. Also editable from the Activity Monitor's ⚙ → Toasts & timing ("Pre-merge soak"), which writes through `PUT /api/config/auto-merge`; the lane re-reads it on every pass, so a running soak follows the new window. |
| `auto_merge.contributor_lane.require_reviewer_verdict` | `true` | Require an approving aggregate reviewer verdict for the current head. Independently of this key, a hive with no reviewer agent never merges through the tier (`contributor-lane:no-reviewer`). |
| `auto_merge.contributor_lane.require_tiers` | `trusted`, `merger` | Contributor trust tiers whose verified claims the tier may merge. Only operator-granted tiers (`trusted`, `merger`, `advisor`) are accepted; `newcomer` and `contributor` are rejected at validation. |
| `auto_merge.contributor_lane.registry_tiers_sufficient` | `true` | Admit a PR whose author's contributor-registry profile carries a `require_tiers` tier without a linked issue or `hive:claim` marker (logged as `contributor-lane:registry-trusted`). All other gates still apply. `false` requires the claim marker for every contributor. |
| `auto_merge.contributor_lane.auto_promotion_scope` | `hive` | Where the clean-merged-PR ratchet (`auto_merge.contributor_tier.auto_promote_after_merged`) counts an author's merges: `hive` counts across every repository the hive manages (one `search/issues` query finds the repos holding the author's merges; the PR's own repo is always walked first), `repo` counts only the PR's repository. Revert and sentinel-alert evidence blocks promotion in either scope. |
| `auto_merge.contributor_tier.auto_promote_after_merged` | `5` | Clean merged PRs (no revert, no sentinel label, not undone by a later merged revert) after which the contributor lane treats an author as `trusted` without an operator grant, unless the registry marks them `revoked`. Negative disables the ratchet. |
| `auto_merge.contributor_lane.approve_held_runs_for_registry_trusted` | `true` | When the registry path admitted the PR author and the lane stops at `required-check-missing:<ctx>`, approve the head's `action_required` workflow runs once (`POST /repos/{owner}/{repo}/actions/runs/{id}/approve`, fork heads included), post one `hive:contributor-held-runs-approved` audit comment and log `contributor-lane approved held runs`. Never acts for claim-marker, auto-promoted or association admissions, sentinel/exclude-labelled PRs, or diffs touching `.github/workflows/**`. Needs the App's **Actions: Read and write** permission. See [hive-merge.md](../hive-merge.md#contributor-lane-auto-merge). |
| `auto_merge.contributor_lane.owner_approval_releases_workflow_fence` | `true` | A hive owner / trusted merger's explicit approval on the **current** head — a GitHub `APPROVED` review on that commit, or a dashboard `/hive decision approve` bound to it — releases the contributor lane's `contributor-lane:workflow-path` fence, so the lane merges a PR editing `.github/workflows/**` / `.github/actions/**` itself once CI, soak and the other gates are clear. Logs `workflow-path fence released by owner approval`, leaves one `hive:workflow-approval` audit comment per head; approval on an older head does not count. An unmarked approval binds to the PR `updated_at` pinned on the hive's first sweep of the head, not to the push, so one typed between the push and that sweep is judged stale and must be typed again ([#14754](https://github.com/hivecommons/hive/issues/14754)). Needs the App's **Workflows: Read and write**; without it the merge is refused, the PR is marked `hive:unauthorized` (`merge-workflow-path`) and parks at 090 for a person's click. `false` keeps every workflow-path PR a person's merge ([#14664](https://github.com/hivecommons/hive/issues/14664)). See [hive-merge.md](../hive-merge.md#contributor-lane-auto-merge). |
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
matches the [ACMM policy matrix](../acmm-policy-matrix.md): below L6 all agent
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

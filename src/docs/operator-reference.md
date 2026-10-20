# Hive operator reference

This page is a concise operator reference for fields and runtime knobs that are
easy to miss in `hive.yaml.example`. It was checked against `pkg/config/`
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

## Configuration blocks

Top-level YAML keys accepted by `config.Config`:

| Block | Purpose | Notes |
|---|---|---|
| `project` | Managed GitHub org, repos, primary repo, AI author. | Required for normal operation. |
| `project.agent_issue_decision_policy` | `resolve`. | `resolve` keeps Hive-authored issues from parking at `stage/045-needs-decision` for non-risk engineering questions (sequencing, naming, numbering, config shape, scope, approach A vs B); agents record `Decision taken by hive: …` and the relay downgrades to `stage/010-actionable`. The gate applies to the `issue`, `comment --needs-decision` and `label` paths alike; a typed `--step-kind` owner step always parks. `escalate` parks every Hive-authored `--needs-decision` request. Risk parks still require `Risk: <category>`. |
| `policies` | Optional git/local policy prompt source. | Hot-reloaded when configured. |
| `agents` | Agent definitions and behavior metadata. | Per-agent overlays may also live under `data.agents_dir`. |
| `governor` | Cadences, labels, sensing, health, budgets, inference gateways, trajectory review. | See notable fields below. |
| `github` | PAT or GitHub App credentials and forge URLs. | Use one auth method. |
| `classifier` | Keyword or optional Jev smart classifier for lane routing, haiku/sonnet/opus tier selection, and triage. | Defaults to keyword-only with no network calls. See [Jev smart classifier](jev-smart-classifier.md). |
| `notifications` | ntfy, Slack, and Discord webhooks. | All optional; see [notifications.md](notifications.md). |
| `dashboard` | Web UI port, snapshots, auth token, frame allowlist, authorized users. | `auth_token` can come from `HIVE_DASHBOARD_TOKEN`. |
| `agent_sandbox` | Podman-rootless sandbox launcher for hub/pod agents (`pkg/sandbox`). | Opt-in and **two-gate**: this block's own `enabled: true` sandboxes nothing by itself — each agent also needs `sandbox.enabled: true` under `agents.<name>`. The dashboard's Security tab writes only this global flag, so enabling it there alone can leave every agent unconfined — but the tab now shows this: `security.sandboxWarnings` in `GET /api/config/governor` carries `config.AgentSandboxGateWarnings`'s diagnosis (also logged at WARN at boot/reload), rendered both in the page's coherence-warnings box and inline under the toggle, naming the still-unconfined agents and the fixing key (#4918). See [sandbox-isolation.md](sandbox-isolation.md) and the [getting-started confinement section](getting-started.md#where-agents-actually-run-read-this-before-l3). |
| `data` | Metrics, logs, session, agent overlay and Nous (`data.nous_dir`) directories. | Defaults are `/data/...` in containers. |
| `nous` | Nous (Experiment Lab) runtime knobs: gate poll interval, dispatcher, dependency fetch allowlist. | Durable Nous state is under `data.nous_dir`, not here. |
| `knowledge` | Wiki layers, vaults, git/document sources, connectors (`knowledge.connectors`, see [knowledge-connectors.md](knowledge-connectors.md)), primer, curator, bead synthesizer. | Disabled unless `enabled: true`. |
| `hub` | Hub/spoke hosted-hive metadata. | Usually provisioner-owned. |
| `hive_id` | Stable spoke identifier. | Usually provisioner-owned. |
| `acmm_level` | Current ACMM pack level. | May be set by hub/dashboard. |
| `quality` | Optional quality-lane capabilities. | `formal: true` enables agent-authored Spin models only at ACMM L5/L6; see [Formal verification](formal-verification.md). |
| `runs` | Long-running run settings: `runs.external.flue`, the report-only external-execution binding pilot, and `runs.external.omp`, the OMP workbench host behind the same adapter. | Both default OFF and independent. `enabled: true` with no `mode` runs in `shadow` (persists admissions, observes, never starts external work); `mode: report-only` dispatches. Flue dispatches to a runtime at `endpoint` pinned to `workflow_version` and needs a build with the `extwork_flue` tag; OMP offers work over the contributor relay to a workbench peer declaring `ext-exec/omp` whose declared version matches `workflow_version`, and needs the `extwork_omp` tag. Neither host receives a repository credential, dashboard token, or publication tool; OMP (tier T3) is additionally refused any write-capable stage. See [External workflow admission](design/external-workflow-admission.md). |
| `auto_merge` | The App self-merge sweep: whether and how the Forge App merges its **own** CI-green PRs. | Default ON, but inert below `acmm_level: 6`. See [App self-merge sweep](#app-self-merge-sweep-auto_merge) below. |
| `issues.close_on_merge` | `true`. | When a hive-authored or trusted-author PR merges and closing keywords or Hive claim metadata show it fixed an open issue, Hive comments `Fixed by #<pr> (merged <sha>). Closing. Reply /reopen if the problem persists.` and closes the issue as `completed`. Issues labelled `needs-reporter-confirmation`, `epic`, or `needs-human` stay open: Hive comments that the fix merged, adds `hive/likely-done`, and waits for `/fixed` or more instructions. Both comments carry `<!-- hive-close-on-merge -->`, Hive's already-handled marker: an issue with that comment is never closed on merge again, so `/reopen` after a Hive close sticks. An issue reopened after the PR merged stays open even when GitHub's closing keyword, not Hive, closed it: before closing, Hive reads the issue timeline since that PR's merge, and a `reopened` event by anyone but Hive (including `github-actions[bot]` from `/reopen`) skips the close and posts one comment carrying `<!-- hive-reopened-after-merge pr=<pr> -->`, so later sweeps skip that PR without rereading the timeline. The marker is per PR: a different PR that merges after the reopen can still close the issue. If the timeline cannot be read, Hive leaves the issue open and retries on the next sweep. |
| Issue↔PR link safeguard | Always on with issue stage and claim-label sweeps. | GitHub shows its issue-list PR icon only when a default-branch PR body contains a closing keyword. When Hive sees a Hive-authored default-branch PR covering an issue and the PR body does not say the issue stays open, Hive appends `Fixes #N` to that PR body. Existing Hive-authored default-branch PRs that used `Refs #N` but are recorded by Hive stage/claim metadata as the implementation PR are retrofitted once with the same closer and `<!-- hive:issue-link-fixed -->`; human-authored PR bodies are never edited. If the body says `Part of #N` or otherwise leaves the issue open, Hive does not add a closer; it posts one stage-entry comment on the issue with the PR link and explains that GitHub will not show the icon without a closing keyword. Non-default-branch PRs get the same issue comment. |
| `issues.close_on_merge_backfill_interval` | `1h`. | How often Hive scans open issues carrying Hive PR-linkage signals (`stage/040-covered-by-pr` or the retired `🐝 covered by PR`, `hive/likely-done`, claim/covered comments, or timeline PR references) and applies the same close-on-merge policy to PRs that merged before the current process saw them. |
| `issues.close_on_merge_interval` | `10m`. | How often the close-on-merge **ledger sweep** walks every merged-PR claim in the duplicate-PR claim ledger. Passes in between (every governor eval cycle and manual rescan) only look at claims whose PR merged since the last full walk, so a fresh merge still closes its issue on the next pass without re-reading the whole ledger ([#11778](https://github.com/hivecommons/hive/issues/11778)). Both the ledger sweep and the backfill pause while the GitHub API budget mode is not `normal`; Hive logs one line on each pause and resume. Env override: `HIVE_CLOSE_ON_MERGE_INTERVAL` (Go duration). |
| `issues.close_on_merge_recheck` | `24h`. | How long a merged-PR claim keeps its settled close-on-merge verdict before the ledger sweep re-verifies it against the API. Once a sweep reaches a final verdict for a claim — issue closed, already closed, Hive marker present, awaiting confirmation, untrusted author, reopened by a person, or PR not merged — it stamps `close_on_merge_settled_at` on the claim and later sweeps skip the claim with no API call until this window elapses. Transient failures (issue, timeline or comment reads/writes that errored) never settle and retry on the next sweep as before. The marker follows the claim while the same PR claims the issue and resets when a different PR takes it over. Env override: `HIVE_CLOSE_ON_MERGE_RECHECK` (Go duration). |
| `issues.reporter_confirmation` | `false`. | Legacy opt-in switch for hives that want every human-filed bug-family issue to wait for reporter verification before close. Leave false for the default close-on-merge policy; add `hive: needs-confirmation` to an individual issue when only that issue needs the gate. `hive: reporter-confirmed` and `hive: close-on-merge` remain accepted legacy bypasses. |
| `issues.sha_hold_trusted_label` | `needs-sha`. | Soft label the SHA-hold sweep applies to a `kind/bug` without a commit SHA when the reporter's author association is in `project.issue_filter.reporter_trust.trusted_associations` (default OWNER/MEMBER/COLLABORATOR), instead of the hard `hold` (#12002). The notice is the same; the issue stays actionable. A SHA-hold `hold` already on such an issue is swapped for this label. Set to `hold` to restore the hard hold for every reporter. |
| `issues.lift_hive_hard_suppress` | `true`. | Runs the hard-suppress reconcile (#12002): a `hold` or `blocked` label on an open issue is lifted when Hive provably applied it (newest label event is a `labeled` by the App bot; for `hold`, paired with the `<!-- hive:sha-hold -->` notice) and a PR cross-referencing the issue merged after it. Hive posts one note carrying `<!-- hive:hard-suppress-lifted -->`. Human-applied labels are never lifted. Skipped with the other unstick steps when the API budget is critical. |
| `issues.hard_suppress_reconcile_interval` | `30m`. | Minimum time between hard-suppress reconcile passes. |
| `contribute.help_links` | Defaults to Hive contributor docs and the Hive issue tracker. | Up to 5 `{label, url}` links shown on `/contribute` Onboarding, Operations, and printed once by relays on connect. URLs must be absolute `http://` or `https://`; labels are plain text and length-capped. Prefer the Hive Commons Discord redirect (`https://hivecommons.dev/discord`) over channel URLs for contributors who may not already be server members. |
| `contribute.max_parallel_fixes` | `2`. | Ceiling on how many stage-072 `fix-pr` sub-sessions one contributor's relay may run beside its primary session ([#12436](https://github.com/hivecommons/hive/issues/12436)). The relay advertises its own count (`max_parallel_fixes`, from `HIVE_MAX_PARALLEL_FIXES`); the hub clamps it to this. A relay that advertises no `parallel_sessions` always has capacity 1 (preempt mode). Fixes beyond capacity queue server-side and are served before new work. Settings → Hub → Fix budget. |
| `contribute.min_focus_window` | `30m`. | Preempt mode only: after a preempted primary task resumes, the hub does not preempt that contributor again until it has run uninterrupted for this long. Settings → Hub → Fix budget. |
| `variables` | Trusted variable resolver definitions. | Env-only substitution works without this block. |
| `classification` | Go-consumed subset of `hive-project.yaml`'s `classification:` block — today only `review_bots`, the external review-bot logins whose inline threads on hive-authored PRs the hive addresses and resolves itself. | Off unless `review_bots.logins` names a bot. The same key in `hive-project.yaml` is read when `hive.yaml` has none. See [review-bot-threads.md](review-bot-threads.md). |
| `claims` | The worker-claim ledger ([#8380](https://github.com/hivecommons/hive/issues/8380)): who is on an issue right now, ranked human > agent > contributor > external, so a person can take an issue over from a hive agent or a relay contributor and the displaced holder is told to stop. | Default ON. `enabled: false` turns recording, enforcement and `/api/claims` off. `human_ttl_s` 4h, `agent_ttl_s` 2h, `contributor_ttl_s` 30m, `max_ttl_s` 24h; the owner can also set these four from **Governor Config → Features → Work intake & issue lifecycle → Issue claims** (a value of 0 or *Reset to default* uses the default; human, agent and contributor may not exceed the max), and a saved change applies without a restart to claims made or renewed afterwards while existing claims keep their expiry ([#10981](https://github.com/hivecommons/hive/issues/10981)); `comment: false` / `label: false` suppress the GitHub issue comment / `stage/020-claimed` label mirror (`label: false` also suppresses the `stage/030-pr-in-progress` start label an agent claim mirrors — formerly `claimed` / `hive/in-progress`, [#11841](https://github.com/hivecommons/hive/issues/11841)). A governor kick only *lists* the issues it names (30-minute hold, no comment, not a claim); the agent claim, with its comment, is recorded on the agent's first start signal on an issue — its own `hive-open-issue claim` request, a `hive-open-pr` request naming it, or a comment with an explicit start statement (`<!-- hive:start -->`, "starting now", an opening "Starting …" / "Implementing …"; see the [recognized forms](hive-open-issue.md#comment)); a report-only comment (triage, verification, gate check, status), an explicit deferral ([#12387](https://github.com/hivecommons/hive/issues/12387)) or escalation, and any `label` request record no claim. `never_claim_labels` (default `[epic]`, case-insensitive; `[]` turns it off) names umbrella/tracking labels no automated producer may claim and no claim ever stages `stage/020`/`stage/030`; the same admission gate also refuses issues parked on a person (`stage/045-needs-decision`, `needs-spec`, …), `needs-human` and held issues. `escalate_after_claims` (default `2`, negative turns it off) counts those claims only: once one agent's last that-many claims on a free issue were followed by no linked PR, referencing commit, label or assignee change and no close/reopen, the issue is withheld from every kick, labelled `needs-human` and given one comment listing the claims, instead of being claimed again. A `hive/likely-done` issue an agent already started on after the merged PR (its verification) is withheld from kicks until a person comments, labels, assigns, closes or reopens it, so it is verified once rather than every cycle ([#10527](https://github.com/hivecommons/hive/issues/10527)). `reclaim_cooldown_s` (default `21600` = 6h; negative turns it off) keeps a relay contributor session off an issue it released without shipping — task failed, abandoned, or its lease revoked — while other contributors may retry at once after the short global failure cooldown; before it, a relay whose environment could not run a task took the same issue back every ten minutes, flapping its claim ([#13093](https://github.com/hivecommons/hive/issues/13093)). An issue labelled `blocked` (the review backlog's parking for a finding whose file exists only on an unmerged PR, or `hive-open-issue comment --blocked-on-pr`) is refused by the same admission gate until the tracked PR merges or closes. Driven from `hivectl claim` — see [hivectl.md](hivectl.md#claim--unclaim--claims--issue-claims). |
| `removed_agents` | Persistent tombstones for deleted agents. | Dashboard/overlay-owned; do not seed casually. |
| `good_first_issue` | Hive-curated `good first issue` advertising: a 15-minute sweep scores open, unclaimed issues and gives the few that qualify GitHub's `good first issue` + `help wanted` labels and one welcome comment pointing at this hive's contributor relay. Not a hold — a hive agent starting the issue removes the label. | Default ON; `enabled: false` opts out. Silent until `dashboard.public_url` (or a hub-assigned dashboard URL) exists. See [Hive-curated good first issues](good-first-issue.md). |
| `governor.first_timer_welcome` | First-time contributor welcome: when a human whose GitHub `author_association` is `FIRST_TIME_CONTRIBUTOR`/`FIRST_TIMER` opens a PR, post one comment (per author per repo, marker `<!-- hive:first-timer-welcome -->`) that thanks them and invites them to run their own hive — profile, leaderboard, trust-tier progression, offline review-fix/rebase handling. Bots, `author/clanker`/`author/hive` PRs, drafts and known relay contributors are skipped. | Default ON; `enabled: false` opts out; `template_path` overrides the text. Silent without a public dashboard URL. See [Welcoming first-time contributors](good-first-issue.md#welcoming-first-time-contributors). |
| `sla` | Delivery SLA targets (`sla.targets.start_to_pr_h`, `pr_to_merge_h`, `open_to_merge_h`, hours; defaults 4 / 24 / 72) the dashboard's Throughput section colours its Delivery SLA spans against. | Metrics are derived from the lifecycle timeline; no extra GitHub calls. Also editable from **Settings → Thresholds**. See [Delivery SLA](#delivery-sla). |
| `sentinel` | Suspicious-activity alerts: flags open PRs (any author) that touch sensitive files or look like security overrides, privilege escalation, or codebase damage. | Default ON; observe-and-alert only. See [Suspicious-activity alerts](#suspicious-activity-alerts-sentinel). |

## Notable fields

| Field | Default / behavior | Operator note |
|---|---|---|
| `data.nous_dir` | `/data/nous`. `HIVE_NOUS_DATA_DIR` overrides. | Durable root for all Nous (Experiment Lab) state: `governor/ledger.json`, `governor/principles.json`, `snapshots/`, `panel-config.json`, `gates/` (pending gate, owner decision, history). Legacy `/var/run/nous/governor` files are copied forward at boot when newer. Everything but `snapshots/` is in the owner spoke backup. See [experiment-lab.md](experiment-lab.md#state-and-storage). |
| `nous.gate_poll_seconds` | `15` seconds. `HIVE_NOUS_GATE_POLL_INTERVAL` (Go duration, e.g. `30s`) overrides. | How often the hive looks for a new `<nous_dir>/gates/pending.json` written by the experiment agent, mirrors it into `GET /api/nous/gate-pending` and pings the owner once per gate. `GET /api/nous/gate-pending` also checks on demand, so this bounds only the ping latency. See [experiment-lab.md](experiment-lab.md#gate-protocol). Editable under **Settings → Features → Nous / Experiment Lab**. |
| `nous.dispatcher` | `inline`. `HIVE_NOUS_DISPATCHER` overrides. Valid: `inline`, `sdk`. | How Nous drives model calls (RFC #11585 part 5). `inline` runs the campaign loop in-process through `bin/nous-runner.sh`; `sdk` is the legacy agent-SDK mode and stays selectable. Configs written before this key existed resolve to `inline`; a `NOUS_AGENT` already exported in the agent's own environment still wins inside the runner. An unknown yaml value fails validation; an unknown env value is ignored. See [experiment-lab.md](experiment-lab.md#experimenter-role-and-engine-contract). Editable under **Settings → Features → Nous / Experiment Lab**. |
| `nous.fetch_allowlist` | `[github.com]`. `HIVE_NOUS_FETCH_ALLOWLIST` (comma-separated) overrides. | Exact lowercase DNS hostnames `hive nous-setup` may fetch pinned campaign dependencies from (RFC #11585 part 7). No wildcards, schemes, ports or IP literals — the same shape as `runs.spektacular.recheck.egress_allowlist`; an entry of another shape fails validation. An explicitly empty list denies every fetch; absent means the default. The hive exports the resolved list to `experimenter` agents so the setup step inside the agent pane enforces it. See [experiment-lab.md](experiment-lab.md#pinned-campaign-dependencies). Editable under **Settings → Features → Nous / Experiment Lab** (saving an empty field writes the deny-all empty list). |
| `nous.artifact_repo` | Unset (mirror disabled). `repo` (`owner/name`), optional `branch` (default: the repository's default branch), `path` (default `campaigns`), `sync_interval_seconds` (default `300`, minimum `30`). `HIVE_NOUS_ARTIFACT_REPO` / `_BRANCH` / `_PATH` / `_SYNC_INTERVAL` override. | Optional GitHub artifact repository (RFC #11585 part 8). When set, the hive mirrors each finished campaign folder under `data.nous_dir` to that repository as a PR, using its own App identity (never the agent, never the owner token), with a filename denylist, secret scrubbing, size caps, exponential backoff and a skip whenever the API budget governor is not `normal`. A public destination is warned about, not blocked. Status under `GET /api/nous/status` → `artifact_repo`; owner `POST /api/nous/artifacts/sync` forces a pass. See [experiment-lab.md](experiment-lab.md#optional-github-artifact-repository). Editable under **Settings → Features → Nous / Experiment Lab**. |
| `nous.observations` | `enabled: false`, `max_per_week: 5` (minimum `1`). | Opt-in filing of hive-state detector findings through the hub self-report relay. When enabled the detectors run once per eval cycle after the snapshot is recorded; at most `max_per_week` new observations are filed per rolling 7 days (ledger `observations-budget.json` under `data.nous_dir`, survives restarts), drops over budget are logged, and nothing is filed while a self-upgrade is draining. See [experiment-lab.md](experiment-lab.md#filing-observations). Editable under **Settings → Features → Nous / Experiment Lab**. |
| `agents.<name>.engine` | `nous` for an agent whose `role` is `experimenter`; empty (and rejected if set) for every other role. Valid: `nous`. | Which experimentation engine (`pkg/nousengine`) an experimenter runs. Nothing else reads the field, so setting it on a strategist or scanner is a load-time error rather than a silent no-op. Editable in the agent dialog (General → Experiment engine). |
| `github.pr_detail_ttl_s` | `1800` seconds. `HIVE_GITHUB_PR_DETAIL_TTL` overrides for tests/experiments. | Reuses `GET /pulls/{number}` detail responses while the cheap PR list still reports the same `head.sha` and `updated_at`, and GitHub has resolved `mergeable_state`. |
| `github.graphql_pr_batch` | `true` | Uses one paginated GraphQL query per repository scan to populate the PR detail cache and CI rollup cache, replacing most per-PR `GET /pulls/{number}` and `check-runs` reads. Set `false` to return to the REST-only scan path. |
| `github.graphql_pr_batch_page_size` | `50` | Page size for the GraphQL `pullRequests(first:)` batch. Values above GitHub's `100` maximum are rejected. |
| `governor.logging.kick_log_source` | `scrollback`. Also `transcript` or `auto`; unknown values mean `scrollback`. `HIVE_KICK_LOG_SOURCE` (when set to a valid value) wins over this key. | What per-kick logs (archives and the live full log) are built from: the tmux scrollback, the backend CLI's own transcript (Copilot CLI, Claude Code), or the transcript when a reader exists else the scrollback. Editable under **Settings → Logging → Kick log source**; applies to the next kick without a restart, and each log's header `source:` line (and the kick history page) shows which was used. See [agent-logging.md](agent-logging.md#per-kick-log-history). |
| `governor.logging.read_scan_max_mb` | Unset: `64` MiB per read. | Lowers how many megabytes of log (counting decompressed bytes of compressed history) one `GET /api/logs/read` / admin MCP `log_read` call may scan; it can only lower the built-in 64 MiB bound, never raise it. A read that reaches the bound stops early, says so in `coverage` (`complete: false`, `stopped_reason`) and returns a `next_cursor` that resumes where it stopped. See [design/admin-mcp.md](design/admin-mcp.md#log-read). |
| `governor.labels.automerge` | Defaults to `lgtm`. | Label applied when a merger/owner queues a PR for Hive auto-merge-on-green. Distinct from the [App self-merge sweep](#app-self-merge-sweep-auto_merge), which needs no label and no human queuer. |
| `project.repo_policies[].auto_merge` | effective `false` below L6; unset = `true` at L6 | Per-repo off switch. Switching to L6 turns this on for every active repo; owners can toggle repos afterward. `false` lets Hive open PRs for that repo but blocks all Hive merge paths (`hive-merge`, App self-authored sweep, and proxy-visible direct REST/GraphQL merge attempts). The dashboard repo-card switch persists this key and takes effect without restart. |
| `project.repo_policies[].label_driven` | Off (unset) for every repo. | Opt-in for repos whose maintainers accept and park issues with labels and their own bots (for example `needs-triage` → `triage/accepted`). On such a repo the un-park sweep posts no "What to reply" notice and never removes `needs-human`, `needs-decision` or `needs-direction` on a `/hive` comment alone; instead a trusted triager applying an approval label releases the issue and counts as human sign-off — see [Label-driven approval](#label-driven-approval). Editable from the dashboard: Settings → Repos → per-repo "Label-driven approvals (Prow-style)" (write or owner role). Details in [maintainer-commands.md](maintainer-commands.md#label-driven-repositories). |
| `project.repo_policies[].label_driven_approval_labels` | Unset: inherits `project.label_driven_approval_labels`. | Per-repo approval-label list; only read when `label_driven` is set. Editable from the dashboard sub-row of the repo's label-driven toggle. |
| `project.repo_policies[].release_on_approve` | Off (unset) for every repo. | Opt in to the hub App creating the exact tag and GitHub Release from an eligible maintainer's `/hive approve` on a typed `kind=release` owner step. Immediately before creating it, Hive verifies target ancestry, tag absence, required checks, and any configured version source. Success posts the release URL, audits the write, and clears Hive's parking labels; refusal leaves the issue parked. Agents remain unable to run `gh release create`. |
| `project.repo_policies[].release_version_source` | Unset (version-file check disabled). | Optional repository-relative text file, for example `VERSION`, read at the proposed release target. Its trimmed value must match the proposed tag; a single leading `v` is ignored on either value. Invalid paths, unreadable content, and mismatches refuse the release while leaving the issue parked. Only used with `release_on_approve: true`. |
| `project.repo_policies[].companion_repos` | Unset (no companions). | List of sibling repositories (`name` in the project org, or `org/name`) that this repo's tooling expects checked out beside it under the agent workspace root — for example a docs/website repo a spec tool registers at `<workspace>/<org>/<sibling>/…`. Contributor task prompts and resident-agent kicks carry a shallow-clone plan for each companion at `<workspace root>/<org>/<sibling>` (`$HIVE_WORKSPACE_DIR` for contributor agents, `$HOME/workspace` for resident agents), and agents are told that a tool reporting one of those paths missing means *clone it*, never park the issue. Self-references and malformed entries are dropped. See [running-at-level-6.md](running-at-level-6.md#needless-parks). |
| `project.label_driven_approval_labels` | `["triage/accepted"]` | Labels whose trusted addition approves an issue on every label-driven repo without its own list. Ignored by repos without `label_driven: true`. Dashboard: Settings → Repos → "Default approval labels". |
| `project.label_driven_trust_org_members` | `true` | Whether a public member of the repo's owning org who applies an approval label is trusted (Prow's own rule). Dashboard: Settings → Repos → "trust public org members". |
| `project.label_driven_trusted_bots` | `["kubestellar-prow[bot]"]` | Bot logins whose label additions count as a trusted triager (Prow command bots relaying `/triage accepted`). `github-actions[bot]` is not in the default because it is the shared identity of every workflow in the repo, including ones triggered by untrusted events; operators whose Prow emulation runs as `github-actions[bot]` must add it explicitly per deployment. Dashboard: Settings → Repos → "Trusted bots". |
| `auto_merge.allow_unprotected_base` | Deprecated no-op. | Accepted so older configs keep loading. `hive-merge` now merges into any protected or unprotected branch the App can write, subject to the CI-evidence gate and GitHub's own merge rules. |
| `auto_merge.no_ci_ok` | Empty by default. | Explicit repo list whose zero-CI merge-request verdict may pass; failing or pending CI evidence is still enforced. |
| `auto_merge.human_merge_paths` | Unset (off). | Per-repo map of `owner/repo` to glob patterns (same syntax as `intent.guardrail_path_patterns`) for paths a person must merge. Applies to every merge lane (App self-merge, trusted-author, label-queued, merge-request relay) whatever the intent tier; a matching PR is held with one comment, and an incomplete changed-file list fails closed. Details in the sweep key table below. |
| `evidence.enabled` | On by default. | The review relay writes one [review evidence bundle](review-evidence.md) per PR head to `/data/evidence/<owner>/<repo>/<number>/<head>.json` when it records a verdict or posts a review; the sentinel sweep adds its findings to the flagged head's bundle; and every merge path adds the head's CI check-run summary, the human approvals and label changes, and the merge event. Owners and mergers read bundles through `GET /api/review/evidence` and `GET /api/review/evidence/list` (see the [API reference](api-reference.md)). Set `false` to stop writing new bundles; existing ones are left in place. |
| `evidence.signing_key_file` | Unset (bundles unsigned). | Path to an Ed25519 private key (32-byte seed or 64-byte key, hex or base64). When set and readable, bundles are signed; when empty, absent or unusable they are written with `"signed": false` and a warning is logged for an unusable key. Keep the key off shared hosts. |
| `evidence.complexity.enabled` | On by default (when `evidence.enabled` is on). | Once a month, for each managed repo, the hive downloads the default-branch tarball through the GitHub App and writes one Go cyclomatic-complexity snapshot to `/data/evidence/<owner>/<repo>/complexity/<YYYY-MM>.json` (see [Monthly complexity snapshot](review-evidence.md#monthly-complexity-snapshot-report-only)). Report-only: it never affects merge eligibility. Cheap by design — one archive download and an AST pass per repo per month, no module downloads and no compile. An existing month is never rewritten; the hive re-checks hourly and retries a failed repo on the next check. The last 24 months are kept per repo. Set `false` to stop writing snapshots; existing ones stay. |
| `evidence.complexity.static_analysis` | `false`. | Also run `go vet` and (when installed) `staticcheck` on the snapshot checkout and record finding counts per tool. Off by default because it compiles each repo and downloads its modules, which is not cheap on a spoke with many or large repos. |
| `review.all_authors` | Deprecated no-op; accepted and logged once. | Every open PR is reviewed regardless of author — hive agent, human, clanker, dependabot or any other bot — whenever review is on (`require_approval` and `fan_out`); there is no author-class opt-out below that switch ([#12974](https://github.com/hivecommons/hive/issues/12974)). The knob used to be required for human PRs. It never lets an agent push to those PRs (see the next row). Remove it from `hive.yaml`. |
| `review.fix_human_prs` | Retained for compatibility; treated as push-disabled. | No longer lets review-fix kicks push commits onto PRs whose login author is not a bot-shaped (`[bot]`) login, even when the PR is hive-attributed ([#13124](https://github.com/hivecommons/hive/issues/13124)); the one person-login exception is a contributor PR the stage reconciler adopted in place (`stage/074-adopted-by-hive`). A `changes_requested` verdict on such a PR stops at the published review, with the proposed fix as a suggestion or patch block in the comment, and the refusal is audited as `review_fix_withheld` with the setting's actual value. When the setting is on, the review-thread reconciler still answers review-bot threads on those PRs, by suggestion only, never by checkout or push ([#12897](https://github.com/hivecommons/hive/issues/12897)). Existing configs may still store `true` for UI/audit continuity, but DCO squash attribution means Hive cannot add a compliant follow-up commit without forging the PR author's sign-off. See [review-swarm.md](review-swarm.md#who-may-be-pushed-to-all_authors-and-the-retired-fix_human_prs-push). |
| `review.severity.block_at` | Unset; owner-only. | Lowest finding priority that blocks merge: `P1` (ship fast, P0–P1 block), `P2` (strict, P0–P2 block) or `P3` (everything blocks). Also drives `classification.review_bots.min_priority` when that key is not set in `hive.yaml` or `hive-project.yaml`. Any other value fails load naming the field. Features -> Review gate -> Severity & backlog. See [review-swarm.md](review-swarm.md#blocking-line-reviewseverity-reviewbacklog). |
| `review.severity.comment_below` / `backlog_below` | `true` / `true`; owner-only. | Whether findings below the blocking line are posted as non-blocking comments and filed to `review.backlog`. |
| `review.backlog.destination` | `github_issue`; owner-only. | `github_issue`, `github_project`, `linear` or `jira`. The last three need the matching `governor.work_source.type` (`github_projects`, `linear`, `jira`); on a mismatch the hive falls back to `github_issue`. Destination fields: `project_column_id` (required for `github_project`), `linear_state`, `jira_status`. |
| `review.backlog.labels` / `max_per_pr_per_day` | `[agent/reviewer]` / `10`; owner-only. `from-review`, `origin/review`, and `origin/reviewer` are accepted and mapped to `agent/reviewer`. | Labels on every backlog item and the per-PR-per-day cap (negative fails load; 0 = default). Reviewer is an agent lane, so `agent/reviewer` is the routing/ownership/provenance label. Non-lane hive issue filers continue to use `origin/selfreport`, `origin/governor`, `origin/dashboard`, `origin/forge`, or `origin/convergence`; the one-time startup migration (`/data/origin-label-migration-v4.done`) removes redundant lane-origin labels and backfills only non-lane origins. |
| `review.contributor_prs.base_sync` | Off by default; owner-only. | Lets PR follow-up use GitHub's update-branch API on hive-authored lane PRs whose head is in a contributor fork, whose `mergeable_state` is `behind`/`dirty`, and whose author enabled "allow edits by maintainers". `behind` PRs get a GitHub-authored merge commit; `dirty`/422 falls back to the `hive-base-moved` repair note and kick. This requires an agent/proxy mode with `ISSUES_PRS_MERGE` because `PUT /pulls/{n}/update-branch` is merge-scoped. It does not rewrite history and does not add a human/bot-authored DCO commit; the repository's "Sign-off survives the squash" check examines the PR's own commits, not GitHub's update-branch merge commit. |
| `governor.advisory.post_retry_backoff_s` | `3600` seconds. `HIVE_ADVISORY_POST_RETRY_BACKOFF_S` overrides the default when the key is unset; a negative value disables the pause. | How long the advisory digest write is paused after GitHub refuses it with `403 Resource not accessible by integration`. That refusal is structural (foreign-authored comment, repo dropped from the installation, or repo transferred to another owner), so per-cycle retries only burn API quota. The failed edit first falls back to creating a fresh App-authored comment; if the create is refused too, the error is recorded for the hub health row — naming the new `owner/repo` when the repository has moved — and attempts pause for this window. See [advisory.md](advisory.md#when-the-write-is-refused). |
| `governor.trajectory.enabled` | Defaults to enabled. | The lane no-ops until a reviewer endpoint and model resolve from `governor.trajectory` or `governor.litellm`. |
| `governor.kick_limits.max_issues` / `max_prs` | `100` / `50` | Caps on every issue list and every PR list (actionable, stale drafts, merge-eligible, CI-failing) rendered into a kick prompt; a cut list ends with an explicit "… and N more" line. An absent key or a negative value means the default; an explicit `0` opts out of the cap entirely (`KickListUnlimited` — every item is listed), and anything above `500` is pinned to `500`. Editable without touching this file under **Settings → Repos → Kick prompt list caps**. Truncation drops the *newest, lowest-priority* items: issues are listed oldest first, and PRs in review-priority order (fixes, then refactors/docs, then tests, oldest first within each class), so lowering a cap never hides the most urgent work. Raise `max_prs` only if your agents actually work more than 50 PRs per turn; the tail of a long list costs tokens and delivery time on every kick without changing behaviour (#7368). |
| `project.agents_md_max_bytes` | `8192`; values under `2048` are raised to `2048`. | Caps the repo `AGENTS.md` block prepended to a kick. Whole sections (split at `#`/`##` headings; each resolved skill is its own unit) are kept or dropped, never clipped mid-rule; when any are dropped the kick carries a one-line `⚠️ AGENTS.md exceeded N bytes; … — read AGENTS.md …` warning naming them and the scheduler logs it. See [agents-md.md](agents-md.md#size-cap-whole-sections-visible-warning). |
| `dashboard.snapshot_frame_ancestors` | Empty list means CSP `frame-ancestors 'none'`. | Entries must be exact `https://` origins; paths, wildcards, credentials, query, and fragments are rejected. |
| `dashboard.authorized_users` | Empty means no per-user direct-route allowlist. | Entries can be `user` or `user:role`; roles are `read`, `read-write`, `merger`, `owner`. |
| `notifications.discord.allowed_users` / `notifications.slack.allowed_users` | Empty means chat commands are disabled for that surface. | Entries can be `id` or `id:role`; roles are `read`, `read-write`, `merger`, `owner`. For backwards compatibility, entry #0 without a suffix is `owner`; later bare entries are `read`. IDs match the way `dashboard.authorized_users` does — case-insensitively, with a `github:` prefix optional on either side — and the first entry for an ID wins. |
| `github.mentions.*` | Disabled unless `github.mentions.enabled: true`. | Polls GitHub issue/PR comments for `@<app bot>` summons. Summoners must hold `read-write` (or `min_role`) in `dashboard.authorized_users` or be listed in `summoners`; agents opt in with both `kick` and `mention` channels plus `converse: true` so #5591's dormant-channel guard still fails fast. |
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
| `hub.nps_timing` | Unset (or `0` per field) keeps the default: `min_sessions: 2`, `second_session_engagement_seconds: 300`, `returning_engagement_seconds: 60`, `reprompt_days: 30`, `dismiss_retry_days: 7`, `max_dismissals: 3`. | Overrides the NPS prompt's eligibility timing. Out-of-range values (negative, or above 100 sessions / 86400 seconds / 365 days / 100 dismissals) fail validation. See [nps.md](nps.md#tuning-the-timing). |
| `hub.nps_detractor_issues` | `enabled: false` | With `enabled: true` and `repo: owner/name`, a user who scores the hive 1 can tick a consent box to have this hive's App open a public issue from their feedback (at least 20 characters, mentions neutralized, no user identity, 1 per user and 3 per hive per 24h). `enabled: true` without a valid repo fails validation. See [nps.md](nps.md#public-issue-for-detractors-optional-off-by-default). |
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
- `pr_self_authorization` counts that label as human sign-off, the same way it
  counts `approved-direction`.
- `/hive approve`, `/hive decision` and `/hive done` on such a repo may also be issued by
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

For runtime precedence and provenance, see [config-layering.md](config-layering.md).

## Dashboard Runs section

The dashboard's **Runs** section shows the active long-running run projection
from `GET /api/runs` and the `/api/status` SSE stream. Each card names the run,
current `spec → plan → implement` stage, who or what it is waiting on, how long
it has been in that wait state, and any plan/receipt artifacts the run exposed.
Owner users see inline **Approve** and **Reject** buttons for runs waiting on a
human plan gate; non-owner users see the same state without mutation controls.

Click **detail** on a run to fetch `GET /api/runs/{key}` and inspect the full
stage/artifact chain plus matching audit or approval entries. Agent cards also
show the current run held by that agent, and the Governor/Health surfaces call
out runs blocked on a human or stalled beyond the default 60-minute wait
threshold. If an older spoke omits the `runs` field entirely, the dashboard
renders `runs: unknown` instead of substituting zero.

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
- `workflow-approval-required:<ctx>` (and its `missing-workflow` form),
  `required-check-never-started:<ctx>` and `required-check-never-ran:<ctx>`
  are logged once per PR head as `automerge hold`; later ticks only count the
  PR in the skip stats instead of emitting `automerge skip` every tick. On a
  dependency-bot PR (Renovate, Dependabot, mergeraptor) the same first report
  also hands the PR to a person: one comment naming the check and the action
  (approve the held runs and add the `hive-ok-to-test` workflow, re-push or
  close and reopen, or merge by hand) plus `needs-human`, so it shows as your
  turn. A plain `workflow-approval-required:<ctx>` is not handed off while
  the repository workflow may still approve the runs; only the
  `missing-workflow` form after the grace is. The label comes off when the
  head changes or the PR closes.
- Dependency-bot PRs stay out of the fix loop. The governor's dependency-bot
  sweep hands them to a person the same way when a required check stays red on
  the same head for 4 hours (the comment asks for a re-run instead when every
  failing run carries a `hive-ci-transient` triage marker), or when the PR conflicts with its base for 24
  hours (at once for a Renovate PR whose config rebases never). It skips PRs
  already labelled `needs-human`, `hold` or `do-not-merge`, comments once per
  head and reason, and removes only the `needs-human` label it added once CI
  is green, the conflict is gone or the PR closes (#11558, #12379).
- fork heads, sentinel-alert PRs, and human-authored lanes are never touched; a
  first-time contributor's held run is still a person's decision.

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
| `auto_merge.bot_takeover.enabled` | `true` | When `github-actions[bot]` is in `auto_merge.trusted_bot_authors`, re-author its same-repo PRs whose required `pull_request` checks can never run (the bot opened them with a workflow `GITHUB_TOKEN`). After the head is older than both `min_head_age` and 30 minutes with no `pull_request` run, the App pushes the same change as one signed-off commit to `hive/takeover-<n>`, opens a PR with the original title/body plus `Supersedes #<n>`, comments `Superseded by #N` on the bot PR and closes it. One attempt per bot head SHA. See [hive-merge.md](hive-merge.md#github-actionsbot-pr-takeover). |
| `auto_merge.trusted_authors.enabled` | `false` | Enables the opt-in human-authored PR tier: Hive may merge a green PR only when the author already holds the required hive role and, by default, GitHub push/maintain/admin permission on that repo. |
| `auto_merge.trusted_authors.repos` | empty = all watched repos | Optional repo allow-list for the trusted-author tier. The dashboard renders this as a watched-repo multi-select; selecting nothing preserves the all-repos default. |
| `auto_merge.trusted_authors.require_role` | `merger` | Minimum `dashboard.authorized_users` role the PR author must hold (`merger` or `owner`). |
| `auto_merge.trusted_authors.require_github_permission` | `true` | Also require GitHub to report author write access; API errors fail closed. Fork PRs still need this check even if the setting is false. |
| `auto_merge.trusted_authors.exclude_labels` | `hold`, `do-not-merge`, `needs-human` | Labels that keep a PR out of trusted-author auto-merge. |
| `auto_merge.contributor_lane.enabled` | `true` | Enables the contributor-lane tier: Hive merges a green, soaked, reviewer-approved PR whose author holds a `require_tiers` tier in the contributor registry (`registry_tiers_sufficient`), or whose linked issue carries this hive's App-authored `hive:claim kind=contributor` marker naming the PR author with a `tier=` in `require_tiers` and `verified=true`. Grants unattended merge only to contributors an operator granted such a tier and whose GitHub login was proven; set `false` to make every contributor PR wait for a human `/lgtm` + `/approve`. See [hive-merge.md](hive-merge.md#contributor-lane-auto-merge). |
| `auto_merge.contributor_lane.soak_minutes` | `30` | How long the current head must stay green before the tier merges it; a new push restarts the soak. `0`/unset means the default (30); the shortest real soak is `1`. |
| `auto_merge.contributor_lane.require_reviewer_verdict` | `true` | Require an approving aggregate reviewer verdict for the current head. Independently of this key, a hive with no reviewer agent never merges through the tier (`contributor-lane:no-reviewer`). |
| `auto_merge.contributor_lane.require_tiers` | `trusted`, `merger` | Contributor trust tiers whose verified claims the tier may merge. Only operator-granted tiers (`trusted`, `merger`, `advisor`) are accepted; `newcomer` and `contributor` are rejected at validation. |
| `auto_merge.contributor_lane.registry_tiers_sufficient` | `true` | Admit a PR whose author's contributor-registry profile carries a `require_tiers` tier without a linked issue or `hive:claim` marker (logged as `contributor-lane:registry-trusted`). All other gates still apply. `false` requires the claim marker for every contributor. |
| `auto_merge.contributor_lane.repos` | empty = all watched repos | Optional repo allow-list for the contributor-lane tier (bare name or `owner/repo`). |
| `auto_merge.contributor_lane.exclude_labels` | `hold`, `do-not-merge`, `needs-human`, `blocked`, `needs-rebase` | Labels that keep a PR out of contributor-lane auto-merge (`do-not-merge/*` always excluded). |

Owners can edit `auto_merge.trusted_authors.*` and `auto_merge.contributor_lane.*`
from **Settings → Features → Auto-merge**. The dashboard writes the same
owner-only `/api/config/auto-merge` overlay as the other auto-merge controls, so
hosted spoke changes survive restarts without editing `hive.yaml`.

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
Completed failures are only deferred to the merge endpoint when the required
set is genuinely unknown; a known-empty set means GitHub enforces nothing, so
any completed non-meta, non-ignorable CI failure blocks locally with
`check-failure` / `status-failure` (#11489).

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

## Per-repo merge strategy (`merge_strategy`)

`merge_strategy` decides **how** Hive merges into a repository, never
**whether** it does: `auto_merge`, the ACMM gate, holds, pauses and every
merge path's own checks apply exactly as before. It is set per repository,
next to the per-repo `auto_merge` switch:

```yaml
project:
  repo_policies:
    - repo: atomic-image-builder
      merge_strategy: hive-serialized   # or: direct (the default when unset)
```

| Strategy | Behaviour |
|---|---|
| `direct` (default) | Each merge path (the self-merge sweep, the `lgtm` queue, the [`hive-merge`](hive-merge.md) relay) merges an eligible pull request on its own, exactly as Hive always has. Deciding a repository is `direct` needs no GitHub call. |
| `hive-serialized` | The **serialized lane**: per target branch at most one pull request — the one at the front — is brought up to date, has its required checks run on its exact head, is re-checked live and is merged with its head pinned. Every other eligible pull request waits in order with the reason `deferred: not at the front of the lane`. Meant for personal-account repositories, where GitHub's merge queue is not available. |

An unrecognised value is a configuration error for that repository: Hive does
not merge there and does not fall back to `direct`. Anyone who may switch a
repository's auto-merge off (owner or repo write) may switch it to
`hive-serialized`; only an owner may switch it back to `direct`. The dashboard
repository card shows the strategy and offers the change to permitted users;
the same API is `GET`/`POST /api/repos/merge-strategy` (see
[api-reference.md](api-reference.md)).

`merge_strategy` is unrelated to `knowledge.primer.merge_strategy`, which
controls how knowledge primers are merged. The two only share a name.

### Front timeout (`merge_front_timeout`)

The serialized lane's front timeout is 60 minutes unless configured. Set it
hive-wide under `project`, and override it per repository next to
`merge_strategy`; the value is a Go duration (`90m`, `2h`). An invalid or
non-positive value is a configuration error.

```yaml
project:
  merge_front_timeout: 90m            # hive-wide; unset means 60m
  repo_policies:
    - repo: exult
      merge_strategy: hive-serialized
      merge_front_timeout: 3h         # this repository only
```

When the front's timeout runs out while every required check on the head it
is evaluating is still queued or in progress (none failed, none missing), the
lane is waiting on CI capacity rather than on the pull request: rotating the
front would only start another CI run for the next pull request. The lane then
extends the deadline by one more timeout and records a `front-extend` decision
(`front timeout extended by 1h0m0s (1 of 3): required checks still queued or
in progress; last known state: …`). It does so at most three times, so a front
waits at most 4x the timeout after its last branch update; after that it
leaves with the usual `front timeout after …` reason.

### What the serialized lane guarantees

For every `hive-serialized` repository:

1. At most one pull request per target branch is in final validation, across
   all merge paths and across restarts (the front record is durable under
   `/data/mergelane/`; a restart repeats an interrupted validation in full).
2. No pull request merges unless its head contained the target branch tip the
   final re-check read.
3. No pull request merges unless every required check finished successfully on
   the exact head that merges. Missing, queued or running never counts as
   passing; an unknown or empty required-check set means no merge.

**Administration: Read-only is optional for `hive-serialized`.** Without it
the App cannot read classic branch protection (GitHub answers 403); the lane
then uses the required checks the branch's rulesets list, and GitHub still
enforces any classic rule itself at merge time. The lane panel and the ACMM
merge-queue row show the reason when the required-check set is unknown, for
example a 403 on a branch whose rulesets require no checks. Without Administration,
classic-protection `require_code_owner_reviews` is likewise invisible (the
403 reads as "not required"), so only ruleset code-owner rules are detected.
4. The merge call pins the validated head.
5. Immediately before the merge Hive re-reads the head, the branch tip,
   open/draft state, the target branch, mergeability, the required-check set
   and its results, hold/do-not-merge/exempt/pause labels, the strategy,
   auto-merge permission and the calling path's own authorization.
6. Any change, error or ambiguous answer means no merge (fail closed).
7. One validation never covers two merges.
8. No existing gate on any merge path is weakened or skipped, and setting the
   strategy never turns merging on.
9. Repositories left on `direct` behave exactly as before.
10. Hive never rebases, force-pushes or otherwise rewrites a pull request
    branch; it only merges the target branch into it, pinned to the head it
    evaluated.

A front pull request whose required check fails on the head the lane is
evaluating leaves the front at once with that reason (for example
`required check "test" finished as failure on head 4d9ac7c`), and the next
one starts. This holds even when a merge path (the self-merge sweep or the
`lgtm` queue sweep) rejects the front before the lane, for example as not
mergeable once GitHub reports it `blocked`: the path still hands its current
front to the lane, which re-evaluates it without merging and lets it leave
with its own reason. The front timeout (`merge_front_timeout`, 60 minutes by
default, restarted on every branch update, and extended up to 4x while the
required checks are only queued or in progress) is the fallback for a front
whose checks never finish; its reason says what the lane last knew, for
example that the head after the branch update was never evaluated, or the
last recorded waiting reason.

A front that GitHub still reports as `blocked` after its head contains the
current target tip and all required checks pass also leaves immediately. The
lane re-reads the PR after observing passing checks, so a block in the earlier
snapshot alone cannot evict the front. If the final live re-check still finds
the block, the lane records that no remaining lane action can satisfy it and promotes
the next pull request. Pending, missing or unreadable required checks still
wait; an eligible pull request that returns joins the back of the lane.

### The remaining window, and GitHub's up-to-date rule

No GitHub merge API can be made conditional on the target branch's tip; they
pin only the head. Between Hive's final re-read of the tip and the merge call
there is therefore a window of about one API call. Without GitHub's own
"Require branches to be up to date before merging" rule on the branch, a commit
landing in that instant can slip through (guarantee 2 then fails). Hive does
not hide this: after every merge it checks that the merge landed directly on
the validated tip and, if it did not, raises an alert naming the merged pull
request and the unexpected commit (and that commit's pull request, when it has
one). The alert is logged at error level (`merge lane: merge did not land on
the validated tip`) and recorded as an `alert` lane decision.

**Recommendation:** turn GitHub's "Require branches to be up to date before
merging" rule on for every target branch of a `hive-serialized` repository
(Settings → Rules → Rulesets, "Require status checks to pass" with "Require
branches to be up to date"). GitHub then closes the window on the server. It
costs one CI run per merge, because the lane only ever updates the pull request
at the front.

### Seeing what the lane is doing

On a `hive-serialized` repository card the dashboard shows a serialized lane
panel per target branch: the pull request at the front and its stage
(updating, waiting for checks, final re-check, merging), the waiting pull
requests in order, the last reason a pull request left the front, every
recorded no-merge or deferral reason, and the guarantee in force:

| Label | Meaning |
|---|---|
| server-enforced | A ruleset on the branch requires up-to-date branches; GitHub closes the window. |
| Hive-checked | Rulesets require checks but not up-to-date branches; only Hive's own check and the post-merge alert apply. |
| unknown | No ruleset speaks to it. Classic branch protection carries the flag, but reading it needs the Administration permission the App does not hold. It also appears when Hive could not read the branch rules at that moment (for example while rate limited); the reason is shown and no up-to-date-rule recommendation is shown in that case. |

For Hive-checked and unknown lanes the panel recommends turning the up-to-date
rule on. The same state is available to scripts, the admin MCP server and
`hivectl` through `GET /api/repos/merge-lane?repo=<repo>`; a `direct`
repository returns only its strategy and has no lane panel.

### Fork pull requests

The lane brings a pull request up to date only by merging the target branch
into its branch, pinned to the evaluated head; it never rebases or
force-pushes, and never rewrites a contributor's commits. A fork pull request
that already contains the current tip with passing required checks needs no
update and merges normally. Fork CI waiting for maintainer approval
(`action_required`) counts as not passed. When Hive cannot update a fork's
branch (GitHub refuses the update, for example because "Allow edits by
maintainers" is off), the pull request does not merge from the front; it
leaves the front with the recorded reason once the front timeout runs out,
the next pull request starts, and it stays eligible to merge at a later turn
once its author has brought it up to date.

### Branches with GitHub's merge queue

A target branch that has GitHub's merge queue is left alone: the serialized
lane does not run for it and Hive makes no direct merge into it. When Hive
cannot establish whether the branch has GitHub's merge queue, it does not merge
(fail closed). The lane panel lists the refusal among its reasons.

### Downgrading

A Hive without the serialized lane ignores the `merge_strategy` key
(configuration decoding ignores unknown keys), so after a downgrade every
opted-in repository silently goes back to `direct` merging. Older versions
cannot be changed to warn about this; check your repositories' strategy before
downgrading.
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
  touch was also touched by the merged PR; below L6 they get `needs-human`.
  Once the notice is on the PR, the sweep re-applies `needs-human` only when
  the notice changes (a different merged PR or reason); clearing the label
  for an unchanged notice sticks.
- **Human-authored PRs** get the notice and are surfaced to operators in the
  dashboard Review Queue and inbox (below); they are never closed automatically
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
  Queue with a link to the merged PR, and the inbox recommends **CLOSE**
  (reason code `superseded_recommended_close`). When auto-close is active the
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
the hub reports no presence). `hold`/`do-not-merge` blocks adoption. When the
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
lane. If the hive cannot push in place, it
pushes `hive/adopt-<n>` at the contributor head, opens a superseding App PR
that preserves the author's commits, moves the original to 074 and closes it
once the superseder merges. Whether that rework reaches each agent is visible per row
on Operations → Connected contributor agents: the relay's parallel capacity
(`capacity N · spawn|preempt`, or *not advertised*), one line per in-flight
item tagged **NEW** / **REWORK** with its stage label, and a red *N PRs
awaiting rework (not yet dispatched)* line when the contributor has
`stage/072` PRs the hub has not handed them (`parallel_capacity`,
`in_flight[]`, `fix_queue_depth`, `pending_rework` on `/api/contribute/fleet`;
see [contributor-relay.md](contributor-relay.md#what-the-operator-sees-operations--connected-contributor-agents)). The retired `🐝 covered by PR`,
`claimed` and `hive/in-progress` labels are removed from open items on the
first cycle after upgrade. Full table, state diagram and migration note:
[labels-and-control-signals.md](labels-and-control-signals.md#stage-labels-stagennn--the-pipeline-state-machine).

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
| `shadow` (default) | engine derives alongside the sweep; the sweep's derivation is written; every disagreement is counted and logged once per `(kind, legacy, engine)` |
| `enforce` | engine's derivation is written; out-of-order transitions are rejected and counted as anomalies |
| `off` | rollback — the sweep's derivation is written verbatim, the engine is inert |

Runbook:

1. Run `shadow` for 48h. Watch `/api/status.stageEngine`
   (`mode`, `derivations`, `mismatches`, `anomalies`,
   `lastMismatchExamples[≤5]`), the strip at the top of the activity
   tracker (`engine shadow · N mismatches · N anomalies`) and
   `hive_stage_shadow_mismatch_total{kind,legacy,engine}` on `/metrics`.
2. Investigate every distinct mismatch pair; an item where the engine is
   right is a legacy fix, an item where legacy is right is a deriver fix.
   The gate is 48h with zero unexplained mismatches.
3. Set `enforce`. Watch `hive_stage_transition_anomaly_total{kind,from,to}`
   and `hive_stage_duplicate_pills_healed_total{kind}`; anomalies should
   stay at the baseline shadow showed.
4. Rollback at any step: set `off` — the sweeps write exactly what they
   derived before the engine existed. `shadow` is a safe intermediate.

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

## Suspicious-activity alerts (`sentinel`)

The sentinel sweep runs every ~15 minutes alongside the other run-loop sweeps
(`pkg/sentinel`, `pkg/github.SweepSentinel`). It inspects every open PR in the
watched repos — human, bot, or agent — and, when a PR matches one of the
behaviors below, posts one `<!-- hive-sentinel -->` comment per head SHA
listing the matched rules and paths. For untrusted authors, it also adds the
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
see [labels-and-control-signals.md](labels-and-control-signals.md#origin-sources))
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
within 7 days, deferred while the GitHub API budget is not normal. Issues
are never auto-closed; the hive posts one "cleared" comment when the
condition stops. Hub-managed hives that cannot write to the report repo relay
through the hub (`POST /api/selfreport/ingest`, heartbeat bearer). The
dashboard shows "Self-reports: N open (last: <class> → #NNN)" in the Fleet
Report panel and the toggle/repo/cap under Settings → Governor →
"Self-Reported Issues". Full details: [fleet-report.md](fleet-report.md).

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
   `issue_comment`, `issues`, `check_suite`, `check_run`, `status`, `push`.
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
- **Every invalidating delivery wakes the governor loop.** The wake is
  debounced for `governor.webhook_wake_debounce_s` so a push and its check
  fan-out coalesce into one cycle, and webhook-triggered cycles are at least
  `governor.webhook_wake_min_gap_s` apart so a delivery storm stretches the
  batch instead of spinning cycles. The wake cycle is a mini-cycle in cost:
  repositories with no dirty PR reuse their previous GraphQL PR batch
  (`graphql_pr_batch.webhook_skips`), clean PRs are served from the PR detail
  cache past `github.pr_detail_ttl_s`, and only the dirty PRs are re-read.
  The log line is `github webhook wake: running an early eval cycle`
  (`reason`, `deliveries`, `coalesced`).
- Webhooks are **healthy** while the last delivery for a managed repository is
  newer than `2 × governor.eval_interval_s`. While healthy the governor's base
  eval interval rises to `governor.eval_interval_webhook_s` (default `900`,
  capped by `governor.eval_interval_max_s`) — safe because the wakes above
  carry the latency — and `/api/status.evalMode` reads `webhook`. The API
  budget stretch still applies; the larger interval wins.
- When deliveries stop, webhooks go stale after `2 × governor.eval_interval_s`.
  The loop re-checks health as that window closes (not just at the next
  stretched tick), resets its ticker to the configured interval, logs one
  WARN per healthy → stale transition
  (`github webhooks went stale; eval mode poll`) and one INFO per stale →
  healthy transition (`github webhooks healthy; eval mode webhook`), and
  `evalMode` reads `poll`.
- `pull_request` `opened`, `reopened`, `synchronize`, `ready_for_review` and
  `review_requested` deliveries also queue an early review dispatch when
  `review.event_driven` is on; see
  [Event-driven dispatch](review-swarm.md#event-driven-dispatch).
- Every status publish (cadence or wake) invalidates the `/api/stage-tracker`
  cache and sends a `stage-tracker` event on `/api/events`, so the dashboard's
  activity monitor refetches at once instead of waiting out
  `dashboard.stage_tracker.cache_ttl_s` plus its poll.

`/api/status` and `/api/gh-rate-limits` expose
`webhooks: {healthy, last_event_at, events_1h, invalidations_1h}`;
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

| Name | Purpose |
|---|---|
| `HIVE_METRICS_ENABLED` | Registers Prometheus `/metrics` when set to `1`, `true`, `yes`, or `on`; off by default (route not registered — scrapes 404) because it exposes estimated cost data. |
| `HIVE_METRICS_TOKEN` | **Required whenever metrics are enabled** ([#3804](https://github.com/hivecommons/hive/pull/3804)): scrapers must send `Authorization: Bearer <token>` (configure Prometheus `bearer_token`). Enabled-but-tokenless serves 403 with an error naming both variables, and the hive logs a startup warning; the cost/agent series are never served unauthenticated. |

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

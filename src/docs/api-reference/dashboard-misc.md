# Dashboard REST API reference — Dashboard miscellaneous

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


`GET /api/stage-tracker` adds independent PR `ci_activity`, `review_activity`
and `code_activity` observations, also copied to `linked_prs[]`. Each is
`{head, state, caption, actor?, source, observed_at?, since?, blocker?, url?, active?}`.
These are display evidence, **not merge authorization**. An absent timestamp
means unknown freshness; a different head invalidates the observation.
CI is classified only against the resolved required set from the existing
check snapshot. Review assignment comes from dispatch, current work additionally
requires its actual worker, and approval/findings require verified current-head
review evidence. Authors and claimants are not inferred reviewers.
`display_column` may be `30` (source repair or explicit owner/adoption wait)
or `50` (CI and re-review), without changing native stage identities, counts,
filters or routing. Consumers must not turn either display field into a gate.
The monitor renders CI and Review concurrently on one shared PR row; see
[Reading the tracks](../streaming-the-activity-monitor.md#reading-the-tracks).

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/audit` | Read-write role | Audit Log — `{"entries": [...]}` envelope, newest first, capped at 200; response shape and the serve-time `user_name` field in [audit-log.md](../audit-log.md#get-apiaudit) | `pkg/dashboard/api.go:62` |
| `GET` | `/api/watchdog/activity` | Read-write role | Watchdog activity readout for the Health tab (#7254): `watchdog-*` audit actions over `?days=` (default 30, clamped to 90) — total / taken / observed / `byAction`, a zero-filled per-day `daily` histogram, per-agent `agents` liveness, and the Observe → Heal `promotion` hint; see [agent-watchdog.md](../agent-watchdog.md#watchdog-activity-strip) | `pkg/dashboard/api.go:500` |
| `POST` | `/api/presence` | Dashboard auth/session | Presence | `pkg/dashboard/api.go:65` |
| `POST` | `/api/upgrade/announce` | Owner role | Announce a pod roll before it happens ([#15788](https://github.com/hivecommons/hive/issues/15788)): body `{"target": "<sha|tag>", "source": "hub"|"operator", "reason": "..."}` (`version` is an alias of `target`; all fields optional). Marks `/api/status`/`/api/version` `upgradeProgress` as `state: pulling, phase: announced` with the target and publishes an SSE `upgrading` event so every open dashboard shows the upgrade animation; a SIGTERM afterwards (or with no announce at all) publishes `phase: restarting` from the same record. The hub sends the same announce through the heartbeat response (`upgrade_announce`) for hub-managed hives; an announce with no kill expires after 15 minutes. Returns `{ok, upgradeProgress}` | `pkg/dashboard/api.go:68` |
| `GET` | `/api/prompt-history` | Dashboard auth/session | Prompt History | `pkg/dashboard/api.go:66` |
| `POST` | `/api/self-upgrade` | Owner only | Self Upgrade | `pkg/dashboard/api.go:67` |
| `GET` | `/api/backup/status` | Owner only | Backup Status — `available:false` with a reason when no encryption key is configured | `pkg/dashboard/api.go:72` |
| `POST` | `/api/backup` | Owner only | Backup Download — `412` when no encryption key is configured (never an unencrypted archive) | `pkg/dashboard/api.go:73` |
| `POST` | `/api/banner-dismissed` | Dashboard auth/session | Banner Dismissed | `pkg/dashboard/api.go:74` |
| `GET` | `/api/history` | Dashboard auth/session | History | `pkg/dashboard/api.go:85` |
| `GET` | `/api/timeline` | Dashboard auth/session | Timeline | `pkg/dashboard/api.go:87` |
| `GET` | `/api/lifecycle-timeline` | Dashboard auth/session | Issue→PR lifecycle journeys plus derived stage timeline. Each journey keeps the dense `stages` map and adds a backwards-compatible `details` object for expandable rows: chronological stages with durations, current-stage age, agent, classification attrs, issue/PR links, blocked reason, and kick/run identifiers when recorded. | `pkg/dashboard/api.go:88` |
| `GET` | `/api/pr-throughput` | Dashboard auth/session | Throughput: pull/merge requests and issues across tracked forges. Counts hive-created PRs/issues, hive/observed reviews, observed PR merges/closures, and issue closes over `?hours=N` (default 24, capped at audit retention; `hours=0` = all time from durable counters in `/data/pr-throughput-counters.json`) with optional exact case-insensitive `?repo=owner/name` and `?role=created\|reviewed\|merged` for the actor trend. The response keeps legacy `opened`/`merged`/`closed`, `merged_by_path`, `buckets`, velocity metrics, previous-window trends, top agents/repos/reasons, and `recorded_since`, and adds `by_actor` (`pr`/`issue` × `created`/`reviewed`/`merged`/`closed` × `hive`/`human`/`other`) plus `series` points (`t`, `hive`, `human`, `other`) for the selected role. | `pkg/dashboard/api.go:89` |


| `GET` | `/api/convergence/soak` | Owner only | Convergence Soak Status | `pkg/dashboard/api.go:261` |
| `POST` | `/api/plan/from-issue` | Dashboard auth/session | Plan From Issue | `pkg/dashboard/api.go:452` |
| `GET` | `/api/plan/{epicID}` | Dashboard auth/session | Plan Tree | `pkg/dashboard/api.go:455` |
| `POST` | `/api/plan/{epicID}/approve` | Owner only | Plan Approve | `pkg/dashboard/api.go:456` |
| `POST` | `/api/plan/{epicID}/reject` | Owner only | Plan Reject | `pkg/dashboard/api.go:458` |
| `POST` | `/api/plan/{epicID}/child/{childID}` | Owner only | Plan Child Action | `pkg/dashboard/api.go:459` |
| `POST` | `/api/linear/agent/install` | Owner only | Linear Agent Install (OAuth start) | `pkg/dashboard/api_linear_agent.go:215` |
| `GET` | `/api/linear/agent/status` | Owner only | Linear Agent Status | `pkg/dashboard/api_linear_agent.go:216` |
| `POST` | `/api/linear/agent/disconnect` | Owner only | Linear Agent Disconnect | `pkg/dashboard/api_linear_agent.go:217` |
| `GET` | `/api/upstream-watch` | Owner only | Upstream-watch divergence: per watched fork the upstream followed, the watermark and last-run time, surfaced/ported/dismissed/skipped counts, and the recent upstream refs with their upstream URL, diff URL, fork issue number and state. Read-only — it reads the config and the watch state file, never GitHub. Also carries `interval`, `project_repos` and each repo's configured entry (`config`) for the editor. | `pkg/dashboard/api.go:303` |
| `PUT` | `/api/config/upstream-watch` | Owner only | Upstream Watch Config Update: edit `upstream_watch` from the Features tab. Body `{enabled?, interval?, repos?: {<repo>: {upstream?, sources?, pr_labels?, max_issues_per_run?, label?} \| null}}` — absent keys are unchanged, `null` removes a repo, keys are stored as the bare repo name. Validated with the loader's rules before the config changes (400 on failure), then persisted and audited as `config_upstream_watch`. | `pkg/dashboard/api.go:304` |

| `GET` | `/api/hotspots` | Dashboard auth/session | Merge-hotspot advisor report (`?repo=owner/name` optional): files repeatedly touched by concurrent PRs over the last `governor.hotspot_window_days`, each with `prs` (distinct PRs), `pr_numbers`, `overlaps` (PR pairs open at the same time), `conflicts` (rebase conflicts blamed on the file), `lines`, `kind` (`code`/`doc`/`lock`/`generated`/`data`), `score`, `symbol_groups`, `recommendation` (`split-code`/`split-doc`/`split-source`/`report-only`/`none`), `reason`, `generated_from`, `source_files` (files under `generated_from`, when known), `issue_number`/`issue_url` of the modularize issue filed, `last_updated`; top-level `count`/`actionable`/`filed`. Derived from the merge train's own PR file lists and the rebase follow-through's conflict records — never lists PRs or files from GitHub. See [operator-reference.md § Merge-hotspot advisor](../operator-reference.md#merge-hotspot-advisor-governorhotspot_). | `pkg/dashboard/api.go:537` |
| `GET` | `/api/widget` | Dashboard auth/session | Widget | `pkg/dashboard/api.go:117` |
| `GET` | `/api/pane/{agent}` | Dashboard auth/session | Pane | `pkg/dashboard/api.go:122` |
| `GET` | `/api/role` | Dashboard auth/session | Role | `pkg/dashboard/api.go:136` |
| `POST` | `/api/reset-restarts/{agent}` | Owner only | Reset Restarts | `pkg/dashboard/api.go:158` |
| `GET` | `/api/budget-ignore` | Dashboard auth/session | Budget Ignore Get | `pkg/dashboard/api.go:176` |
| `POST` | `/api/budget-ignore` | Owner only | Budget Ignore Set | `pkg/dashboard/api.go:177` |
| `GET` | `/api/summaries` | Dashboard auth/session | Summaries | `pkg/dashboard/api.go:189` |
| `GET` | `/api/stage-tracker` | Dashboard auth/session | Stage Tracker: every open issue and pull request of the watched repos for the Hive activity monitor — actionable, held, claimed, in-progress, covered-by-PR, filtered (`needs-*`), draft and exempt-labelled items alike, each with `stage`, `stage_since`, `labels`, `created_at`, `author_kind`, `modifiers`, `claimant: {kind: hive-agent|clanker|person, login, agent, hive, since, source: claim-ledger|claim-comment|assignee|kick-history|author}` naming who holds a claimed / in-progress issue or a PR under review or rework (absent when nobody is named; mirrored onto `linked_prs[]`), (PRs) `sentinel_alert` / `sentinel_findings` / `sentinel_reason` when the sentinel flagged the PR, `decision_needed` on a row parked on a person (085 / 045 / 015, the needs-human pill; mirrored onto the issue's `linked_prs[]` entry): `{question, asked_by, since, blocker, blocker_reason, options: [{action, label, enabled, reason_if_disabled}]}` — the reviewer's own decision question when the verdict record has one, the blocker in plain words, and the applicable actions under the row context menu's action ids (`decide:delegate` first), (PRs) `lineage` on the hive's copy of an adopted PR — `{kind: adopted|superseded|rebased-by-hive, from_number, from_url, from_author, from_author_kind, to_number, to_url, at, reason}`, derived from the stage store's adoption record or the `hive/adopt-<n>` head branch, mirrored onto the issue's `linked_prs[]` entry; absent on every other row — and (issues) `linked_prs`; items with no stage yet report stage `0` "Open"; `needs_human_labels` is the "a person must act" label set behind the monitor's Needs human facet (the same list as `/api/status.labelPresets[needs-human]`); `recently_merged` lists the PRs (`repo`, `number`, `merged_at`) whose `pr_merged` audit entry is under 30 minutes old, so a client diffing two payloads can tell a merge from a close; a PR in a conflict-aware merge train carries `merge_train` `{id, state, front, position, size, attempt, max_attempts, overlap_files, ahead, length, head_number, head_state, head_reason, head_since, eta_s, eta_at, eta_basis, late, stalled, stalled_s}` — `head_state` one of `needs-human` / `ci` / `soaking` / `ready` / `merging` / `fixing` / `unknown` with `head_reason` in plain words; `eta_s` / `eta_at` the estimated wait for this member's own merge attempt (own remaining soak + each PR ahead's remaining soak or one sweep + the next sweep tick; absent while the head waits on a person or CI), `eta_at` anchored to the member's first estimate at its position so a countdown survives polls, `eta_basis` its short reading ("2 ahead · head soaking 12m · next sweep in 40s"), `late` once now has overrun the first estimate twice over, `stalled` / `stalled_s` once the head has sat longer than `governor.merge_train_stall_warn_s` ([#15395](https://github.com/hivecommons/hive/issues/15395)); a PR the queue holds because another open PR declares the same `Fixes #N` carries no `merge_train` and a `merge_gate` of `{state: "duplicate-of:<winner>", duplicate_of, duplicate_issue}` plus the `needs-decision` modifier (the queue keeps one PR per issue: green first, then the earlier-opened); a PR GitHub reports conflicting on its own, or one under repair (stage/070, 071, 072, 074, 085, or a red required check), carries no `merge_train` and is not counted in any member's `size` / `length` — a repaired member the view dropped carries `left_train: {why: fixing|needs-author|returned-to-contributor|adopted-by-hive|needs-decision|checks-failing, note}` instead (mirrored onto the issue's `linked_prs[]` entry), and a train left with one member reports no `merge_train` at all; a PR short of the merge queue (below stage/080) the planner ordered rides no train either — it carries `overlap: {behind, with, files, note}` ("overlaps #15637 (sidebar.html, index.html) — will queue behind it") and no `merge_train`, and counts in no member's `size` — the state slugs (`front`, `queued-behind`) are payload values only, the SPA renders ordinal places ("Merge queued · 1st of 2", with the train's tag when more than one train is on the page). Served from a server-side cache rebuilt at most every `dashboard.stage_tracker.cache_ttl_s` (default `governor.eval_interval_s`) from the spoke's existing snapshot — a request never calls GitHub. Carries `generated_at`, `source` (`cache` or `fresh`), `cache_ttl_s`, an `ETag` and `Cache-Control: private, max-age=<remaining>`; `If-None-Match` yields 304. Each hit is recorded under REST caller `dashboard:stage-tracker` so `/api/gh-rate-limits` shows it charged nothing. | `pkg/dashboard/api.go:190` |
| `GET` | `/api/matrix/lines` | Dashboard auth/session | Matrix Lines: serve bounded backlog of pushed lines behind dashboard.matrix with repo allowlist filtering. `max` counts only lines the repo allowlist admits, so a page is never emptied by filtered-out lines. A client paging with `max` should use the last returned line's `seq` as its next `since`, not the top-level `seq`, which is the source's latest sequence number and would skip lines still past the page. | `pkg/dashboard/api.go:191` |
| `POST` | `/api/prs/{owner}/{repo}/{number}/queue-automerge` | Merger/owner role | Queue PRAuto Merge | `pkg/dashboard/api.go:192` |
| `GET` | `/api/inference/models/{backend}` | Dashboard auth/session | Inference Models | `pkg/dashboard/api.go:369` |
| `GET` | `/api/hive-id` | Dashboard auth/session | Hive IDGet | `pkg/dashboard/api.go:431` |
| `PUT` | `/api/hive-id` | Owner only | Hive IDSet | `pkg/dashboard/api.go:432` |
| `POST` | `/api/chat` | Dashboard auth/session | Chat | `pkg/dashboard/api.go:461` |
| `GET` | `/api/auth/token` | Public | Auth Token | `pkg/dashboard/api.go:495` |
| `GET` | `/api/v1/` | GitHub token | Contributor v1 API dispatcher | `pkg/dashboard/api_contribute.go:350` |
| `POST` | `/api/v1/` | GitHub token | Contributor v1 API dispatcher | `pkg/dashboard/api_contribute.go:351` |
| `GET` | `/api/docs` | Dashboard auth/session | APIDocs | `pkg/dashboard/api_contribute.go:352` |
| `GET` | `/leaderboard` | Public | Leaderboard Page | `pkg/dashboard/api_contribute.go:354` |
| `GET` | `/api/leaderboard` | Public | Leaderboard API | `pkg/dashboard/api_contribute.go:355` |
| `GET` | `/api/leaderboard/teams` | Public | Team Leaderboards | `pkg/dashboard/api_contribute.go:356` |
| `GET` | `/api/leaderboard/style` | Public | Leaderboard Style | `pkg/dashboard/api_contribute.go:357` |
| `GET` | `/api/leaderboard/battle-log` | Public | Scrubbed public Battle Log activity feed for leaderboard widgets. | `pkg/dashboard/battle_log.go:108` |
| `GET` | `/api/leaderboard/hive-of-week` | Public | Featured weekly project metadata, configured render URLs (empty when none), gource source URL, and embed snippets. | `pkg/dashboard/battle_log.go:109` |
| `GET` | `/api/leaderboard/gource-log` | Public | Gource custom log source for a project/week, derived from public hive event data. | `pkg/dashboard/battle_log.go:110` |
| `POST` | `/api/leaderboard/hive-of-week/archive` | Owner only | Archive the selected project's weekly gource source log into knowledge; stores source only, never rendered video. | `pkg/dashboard/battle_log.go:111` |
| `GET` | `/api/leaderboard/contributor/{username}` | Public | Contributor Profile | `pkg/dashboard/api_contribute.go:364` |
| `GET` | `/api/hives` | Dashboard auth/session | Hives List | `pkg/dashboard/api_contribute.go:375` |
| `POST` | `/api/hives/register` | Dashboard auth/session | Hives Register | `pkg/dashboard/api_contribute.go:376` |
| `POST` | `/api/hives/{id}/heartbeat` | Dashboard auth/session | Hives Heartbeat | `pkg/dashboard/api_contribute.go:377` |
| `DELETE` | `/api/hives/{id}` | Owner only | Hives Delete | `pkg/dashboard/api_contribute.go:378` |
| `POST` | `/api/hives/onboard` | Dashboard auth/session | Hives Onboard | `pkg/dashboard/api_contribute.go:379` |
| `GET` | `/sso` | Public | SSO | `pkg/dashboard/server.go:1670` |
### Activity observation provenance

The CI snapshot retains real check/node IDs, head, App ID and transition times.
Its `required_checks` entries retain forge policy's context/App binding:
with `app_known: true`, `app_id: null` or `-1` is an observed any-App rule, not
permission to replace a pinned producer. `required_source` distinguishes actual protection/branch/rules
reads from config fallback; failed actual policy reads do not make the activity
track's `required_known` true. Context-name-only fallback cannot establish an
App binding and remains unknown, rather than treating missing detail as an
explicit any-App rule. Unordered or producer-unknown duplicates remain
ambiguous, never synthetic positional IDs.

For REST check-run snapshots, an absent required name also checks the actual
head's legacy commit statuses before claiming it is missing. Explicit any-App
rules collect statuses even when a check run exists: a check cannot hide a
same-name failing/pending status, or turn a failed status read green.
`statuses_known`
and `statuses_observed_at` distinguish a complete status read from denied,
partial or wrong-head reads; those remain unknown, not missing or passing.
Status observations retain `kind: "commit_status"` and actual IDs/URLs/times.
An unbound status cannot establish a pinned App's success. Canonical protection
facts keep their existing name-set contract; display ambiguity does not become
a new merge/DCO authorization rule.

Real check IDs order retries consistently, including queued runs with no start
time. Start time never changes the ordering rule depending on its comparison
partner. Conflicting equal-identity snapshots remain ambiguous for display;
canonical selection remains one observation per producer and does not let
missing IDs erase an observed blocker.
REST check-run and status pages must finish successfully; malformed or partial
collections cannot establish absence or passing CI.

Source repair activity is derived from the assigned actor's fresh
`codeWork` runtime observation plus current-round PR scope, not administrative
PR writes. Push and same-head retry observations do not inherit an old fixer,
owner or worker observation timestamp. Review completion uses the dispatch
ledger's own agent/review-head/decision receipt and aggregate participation;
another agent's same-perspective bundle verdict cannot settle that assignment.

### Published-checkpoint regions

`GET /api/matrix/lines?format=regions&since=<seq>&max=<lines>` selects whole
changed regions from the existing authorized sentinel observer. Authentication
and dashboard read permission are required, including in kiosk mode; responses
use `Cache-Control: no-store`. Disabled capture returns 404. An active cold
source returns 200 with an empty `regions` array and null `last_ingest_at`.

The response contains continuation `seq`, source high-water mark `latest`, policy
`epoch`, source label `Pushed PR`, stage `pushed-checkpoint`, `last_ingest_at`,
`next_ingest_in_s`, and `regions`. Each region has its own sequence, repo, actual
PR number and `pr_url`, head, path, stage, observed `at`, and ordered `lines`
(`op` is `+` or `-`, `line` is untrimmed text). Follow response `seq`, not
`latest`, until caught up. `max` is a line budget (1–500, default 200); the first
eligible region is always returned whole, even if larger than the budget.
Code buffers are bounded and memory-only, so this is not an audit history.
Clear retained browser code on epoch changes and failed/disabled/unauthorized
pulls. An empty response says nothing about work outside the admitted source.

The internal `/api/matrix/lines` and `dashboard.matrix.*` names are deprecated
but remain supported for compatibility. The sole code display is now **Code activity**
at `?kiosk=1&view=reader`; the Matrix renderers and their `mx.*`
settings are retired. Activation remains explicitly opt-in and default-off;
only pushed, published PR checkpoints are admitted, never workspace or pre-push
content. The cutover grants no anonymous access or broadcast authorization.

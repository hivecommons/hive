# Dashboard REST API reference — Dashboard miscellaneous

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/audit` | Read-write role | Audit Log — `{"entries": [...]}` envelope, newest first, capped at 200; response shape and the serve-time `user_name` field in [audit-log.md](../audit-log.md#get-apiaudit) | `pkg/dashboard/api.go:59` |
| `GET` | `/api/watchdog/activity` | Read-write role | Watchdog activity readout for the Health tab (#7254): `watchdog-*` audit actions over `?days=` (default 30, clamped to 90) — total / taken / observed / `byAction`, a zero-filled per-day `daily` histogram, per-agent `agents` liveness, and the Observe → Heal `promotion` hint; see [agent-watchdog.md](../agent-watchdog.md#watchdog-activity-strip) | `pkg/dashboard/api.go:494` |
| `POST` | `/api/presence` | Dashboard auth/session | Presence | `pkg/dashboard/api.go:62` |
| `GET` | `/api/prompt-history` | Dashboard auth/session | Prompt History | `pkg/dashboard/api.go:63` |
| `POST` | `/api/self-upgrade` | Owner only | Self Upgrade | `pkg/dashboard/api.go:64` |
| `GET` | `/api/backup/status` | Owner only | Backup Status — `available:false` with a reason when no encryption key is configured | `pkg/dashboard/api.go:68` |
| `POST` | `/api/backup` | Owner only | Backup Download — `412` when no encryption key is configured (never an unencrypted archive) | `pkg/dashboard/api.go:69` |
| `POST` | `/api/banner-dismissed` | Dashboard auth/session | Banner Dismissed | `pkg/dashboard/api.go:70` |
| `GET` | `/api/history` | Dashboard auth/session | History | `pkg/dashboard/api.go:81` |
| `GET` | `/api/timeline` | Dashboard auth/session | Timeline | `pkg/dashboard/api.go:83` |
| `GET` | `/api/lifecycle-timeline` | Dashboard auth/session | Issue→PR lifecycle journeys plus derived stage timeline. Each journey keeps the dense `stages` map and adds a backwards-compatible `details` object for expandable rows: chronological stages with durations, current-stage age, agent, classification attrs, issue/PR links, blocked reason, and kick/run identifiers when recorded. | `pkg/dashboard/api.go:84` |
| `GET` | `/api/pr-throughput` | Dashboard auth/session | Throughput: pull/merge requests and issues across tracked forges. Counts hive-created PRs/issues, hive/observed reviews, observed PR merges/closures, and issue closes over `?hours=N` (default 24, capped at audit retention; `hours=0` = all time from durable counters in `/data/pr-throughput-counters.json`) with optional exact case-insensitive `?repo=owner/name` and `?role=created\|reviewed\|merged` for the actor trend. The response keeps legacy `opened`/`merged`/`closed`, `merged_by_path`, `buckets`, velocity metrics, previous-window trends, top agents/repos/reasons, and `recorded_since`, and adds `by_actor` (`pr`/`issue` × `created`/`reviewed`/`merged`/`closed` × `hive`/`human`/`other`) plus `series` points (`t`, `hive`, `human`, `other`) for the selected role. | `pkg/dashboard/api.go:85` |


| `GET` | `/api/convergence/soak` | Owner only | Convergence Soak Status | `pkg/dashboard/api.go:255` |
| `POST` | `/api/plan/from-issue` | Dashboard auth/session | Plan From Issue | `pkg/dashboard/api.go:446` |
| `GET` | `/api/plan/{epicID}` | Dashboard auth/session | Plan Tree | `pkg/dashboard/api.go:449` |
| `POST` | `/api/plan/{epicID}/approve` | Owner only | Plan Approve | `pkg/dashboard/api.go:450` |
| `POST` | `/api/plan/{epicID}/reject` | Owner only | Plan Reject | `pkg/dashboard/api.go:452` |
| `POST` | `/api/plan/{epicID}/child/{childID}` | Owner only | Plan Child Action | `pkg/dashboard/api.go:453` |
| `POST` | `/api/linear/agent/install` | Owner only | Linear Agent Install (OAuth start) | `pkg/dashboard/api_linear_agent.go:215` |
| `GET` | `/api/linear/agent/status` | Owner only | Linear Agent Status | `pkg/dashboard/api_linear_agent.go:216` |
| `POST` | `/api/linear/agent/disconnect` | Owner only | Linear Agent Disconnect | `pkg/dashboard/api_linear_agent.go:217` |
| `GET` | `/api/upstream-watch` | Owner only | Upstream-watch divergence: per watched fork the upstream followed, the watermark and last-run time, surfaced/ported/dismissed/skipped counts, and the recent upstream refs with their upstream URL, diff URL, fork issue number and state. Read-only — it reads the config and the watch state file, never GitHub. Also carries `interval`, `project_repos` and each repo's configured entry (`config`) for the editor. | `pkg/dashboard/api.go:297` |
| `PUT` | `/api/config/upstream-watch` | Owner only | Upstream Watch Config Update: edit `upstream_watch` from the Features tab. Body `{enabled?, interval?, repos?: {<repo>: {upstream?, sources?, pr_labels?, max_issues_per_run?, label?} \| null}}` — absent keys are unchanged, `null` removes a repo, keys are stored as the bare repo name. Validated with the loader's rules before the config changes (400 on failure), then persisted and audited as `config_upstream_watch`. | `pkg/dashboard/api.go:298` |

| `GET` | `/api/widget` | Dashboard auth/session | Widget | `pkg/dashboard/api.go:113` |
| `GET` | `/api/pane/{agent}` | Dashboard auth/session | Pane | `pkg/dashboard/api.go:118` |
| `GET` | `/api/role` | Dashboard auth/session | Role | `pkg/dashboard/api.go:132` |
| `POST` | `/api/reset-restarts/{agent}` | Owner only | Reset Restarts | `pkg/dashboard/api.go:154` |
| `GET` | `/api/budget-ignore` | Dashboard auth/session | Budget Ignore Get | `pkg/dashboard/api.go:172` |
| `POST` | `/api/budget-ignore` | Owner only | Budget Ignore Set | `pkg/dashboard/api.go:173` |
| `GET` | `/api/summaries` | Dashboard auth/session | Summaries | `pkg/dashboard/api.go:185` |
| `GET` | `/api/stage-tracker` | Dashboard auth/session | Stage Tracker: every open issue and pull request of the watched repos for the Hive activity monitor — actionable, held, claimed, in-progress, covered-by-PR, filtered (`needs-*`), draft and exempt-labelled items alike, each with `stage`, `stage_since`, `labels`, `created_at`, `author_kind`, `modifiers`, (PRs) `sentinel_alert` / `sentinel_findings` / `sentinel_reason` when the sentinel flagged the PR, `decision_needed` on a row parked on a person (085 / 045 / 015, the needs-human pill; mirrored onto the issue's `linked_prs[]` entry): `{question, asked_by, since, blocker, blocker_reason, options: [{action, label, enabled, reason_if_disabled}]}` — the reviewer's own decision question when the verdict record has one, the blocker in plain words, and the applicable actions under the row context menu's action ids (`decide:delegate` first), (PRs) `lineage` on the hive's copy of an adopted PR — `{kind: adopted|superseded|rebased-by-hive, from_number, from_url, from_author, from_author_kind, to_number, to_url, at, reason}`, derived from the stage store's adoption record or the `hive/adopt-<n>` head branch, mirrored onto the issue's `linked_prs[]` entry; absent on every other row — and (issues) `linked_prs`; items with no stage yet report stage `0` "Open"; `needs_human_labels` is the "a person must act" label set behind the monitor's Needs human facet (the same list as `/api/status.labelPresets[needs-human]`); `recently_merged` lists the PRs (`repo`, `number`, `merged_at`) whose `pr_merged` audit entry is under 30 minutes old, so a client diffing two payloads can tell a merge from a close. Served from a server-side cache rebuilt at most every `dashboard.stage_tracker.cache_ttl_s` (default `governor.eval_interval_s`) from the spoke's existing snapshot — a request never calls GitHub. Carries `generated_at`, `source` (`cache` or `fresh`), `cache_ttl_s`, an `ETag` and `Cache-Control: private, max-age=<remaining>`; `If-None-Match` yields 304. Each hit is recorded under REST caller `dashboard:stage-tracker` so `/api/gh-rate-limits` shows it charged nothing. | `pkg/dashboard/api.go:186` |
| `POST` | `/api/prs/{owner}/{repo}/{number}/queue-automerge` | Merger/owner role | Queue PRAuto Merge | `pkg/dashboard/api.go:187` |
| `GET` | `/api/inference/models/{backend}` | Dashboard auth/session | Inference Models | `pkg/dashboard/api.go:363` |
| `GET` | `/api/hive-id` | Dashboard auth/session | Hive IDGet | `pkg/dashboard/api.go:425` |
| `PUT` | `/api/hive-id` | Owner only | Hive IDSet | `pkg/dashboard/api.go:426` |
| `POST` | `/api/chat` | Dashboard auth/session | Chat | `pkg/dashboard/api.go:455` |
| `GET` | `/api/auth/token` | Public | Auth Token | `pkg/dashboard/api.go:489` |
| `GET` | `/api/v1/` | GitHub token | Contributor v1 API dispatcher | `pkg/dashboard/api_contribute.go:248` |
| `POST` | `/api/v1/` | GitHub token | Contributor v1 API dispatcher | `pkg/dashboard/api_contribute.go:249` |
| `GET` | `/api/docs` | Dashboard auth/session | APIDocs | `pkg/dashboard/api_contribute.go:250` |
| `GET` | `/leaderboard` | Public | Leaderboard Page | `pkg/dashboard/api_contribute.go:252` |
| `GET` | `/api/leaderboard` | Public | Leaderboard API | `pkg/dashboard/api_contribute.go:253` |
| `GET` | `/api/leaderboard/teams` | Public | Team Leaderboards | `pkg/dashboard/api_contribute.go:254` |
| `GET` | `/api/leaderboard/style` | Public | Leaderboard Style | `pkg/dashboard/api_contribute.go:255` |
| `GET` | `/api/leaderboard/battle-log` | Public | Scrubbed public Battle Log activity feed for leaderboard widgets. | `pkg/dashboard/battle_log.go:108` |
| `GET` | `/api/leaderboard/hive-of-week` | Public | Featured weekly project metadata, configured render URLs (empty when none), gource source URL, and embed snippets. | `pkg/dashboard/battle_log.go:109` |
| `GET` | `/api/leaderboard/gource-log` | Public | Gource custom log source for a project/week, derived from public hive event data. | `pkg/dashboard/battle_log.go:110` |
| `POST` | `/api/leaderboard/hive-of-week/archive` | Owner only | Archive the selected project's weekly gource source log into knowledge; stores source only, never rendered video. | `pkg/dashboard/battle_log.go:111` |
| `GET` | `/api/leaderboard/contributor/{username}` | Public | Contributor Profile | `pkg/dashboard/api_contribute.go:262` |
| `GET` | `/api/hives` | Dashboard auth/session | Hives List | `pkg/dashboard/api_contribute.go:273` |
| `POST` | `/api/hives/register` | Dashboard auth/session | Hives Register | `pkg/dashboard/api_contribute.go:274` |
| `POST` | `/api/hives/{id}/heartbeat` | Dashboard auth/session | Hives Heartbeat | `pkg/dashboard/api_contribute.go:275` |
| `DELETE` | `/api/hives/{id}` | Owner only | Hives Delete | `pkg/dashboard/api_contribute.go:276` |
| `POST` | `/api/hives/onboard` | Dashboard auth/session | Hives Onboard | `pkg/dashboard/api_contribute.go:277` |
| `GET` | `/sso` | Public | SSO | `pkg/dashboard/server.go:1620` |

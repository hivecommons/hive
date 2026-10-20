# Review evidence bundle

A **review evidence bundle** is one self-contained JSON document per pull request head commit. It records what Hive reviewed, under which policy, what the reviewers found, what CI reported, who acted on the PR, and how it was merged. A bundle is **evidence, not certification**: it lets your own control owners decide whether Hive's automated review satisfies your change-management policy. See [SOC 2 control mapping](soc2-control-mapping.md) for how to use it, and [general technical review](general-technical-review.md) for what Hive does and does not claim.

> **Status.** Every field of the schema is populated today: the review relay writes bundles (verdicts, posted reviews and the policy snapshot), the sentinel sweep adds its findings, and every merge path adds the CI check-run summary, human actions and the merge event and posts the evidence pointer comment (see [Where it lives](#where-it-lives-and-retention)). Bundles are hashed, optionally signed (`pkg/evidence`), and can be read through `GET /api/review/evidence`, the dashboard Evidence action and `hivectl review evidence` (see [Downloading](#downloading)). Tracked in [epic #11058](https://github.com/hivecommons/hive/issues/11058).

## What a bundle is

- One bundle per `owner/repo#number@headSHA` (its `id`).
- Append-only per head SHA. A new push does not edit the existing bundle: it starts a **new** bundle for the new head whose `previous_bundle_id` points at the old one, so the chain of heads for a PR stays traceable.
- Carries a content `hash` and, when a signing key is configured, a detached Ed25519 `signature`, so it can be checked offline.

## Schema v1

The machine-readable schema is [`pkg/evidence/schema/v1.json`](../pkg/evidence/schema/v1.json); the Go types live in `pkg/evidence`. Timestamps are RFC 3339.

### Top level

| Field | Meaning |
|---|---|
| `schema_version` | Always `v1` for this schema. |
| `id` | `owner/repo#number@headSHA`. |
| `repo`, `number` | The repository (`owner/name`) and PR number. |
| `author` | `login` and `kind` (`human`, `agent` or `bot`). An `agent` author also carries `agent`, `backend` and `model`: the launch metadata the hive recorded when that agent opened the PR (the same values as the PR's attribution trailer); a field the hive did not know is omitted ([#12065](https://github.com/hivecommons/hive/issues/12065)). |
| `base_sha`, `head_sha` | The commits the PR compares. The bundle is about `head_sha`. |
| `previous_bundle_id` | The bundle for the previous head of this PR, if any. |
| `policy` | Snapshot of the policy in force (below). |
| `verdicts` | Every reviewer verdict (below). |
| `posted_reviews` | Reviews Hive posted on the PR: `url`, `head`, `at`. |
| `ci` | CI for the head: `status` (the merge gate's verdict, `success`, `failure` or `pending`; absent on bundles written before it was captured), `checks[]` (`name`, `conclusion`, `url`) and `captured_at`. |
| `sentinel` | Why the sentinel sweep flagged the PR, if it did: `rule`, `summary`, `paths`. |
| `human_actions` | Approvals, label changes, hold lifts: `actor`, `kind`, `detail`, `at`. |
| `merge` | Present once merged: `actor`, `method`, `at`, `sha`, and `opened_at` (when the PR was opened on GitHub, so `at` − `opened_at` is its time to merge; absent on bundles recorded before it was captured — such a merge is left out of time-to-merge, never counted as zero). Absent before merge. |
| `revert_of`, `followup_of` | Bundle ids of earlier merged PRs this PR reverts or follows up with a fix (see [Change-failure links](#change-failure-links)). |
| `reverted_by`, `followed_up_by` | Bundle ids of later PRs that reverted or followed up this one. |
| `issue_reopens` | Issues this PR closed that a person reopened within 14 days of the merge: `issue`, `repo` (only when it differs from the PR's), `at`, `source` (`claim-ledger`). A change-failure signal for the Outcome card ([#13241](https://github.com/hivecommons/hive/issues/13241)). |
| `shadow` | Optional. What shadow-mode gates would have done, recorded only: `review_gate` (`would_have_held`, plus `lane` — `self-merge`, `trusted-bot` or `trusted-author` when the [self-merge review gate](hive-merge.md#self-merge-review-gate) recorded it on that sweep lane, absent for the merge-eligible lane) and `intent` (`would_have_blocked`), each with `reason`, `tier`, `confidence` and `evaluated_at`. Absent unless a shadow gate ran; it never affects a merge ([#12103](https://github.com/hivecommons/hive/issues/12103)). |
| `backfilled`, `backfill` | Optional. Present only on bundles reconstructed from forge history by `hivectl evidence backfill` rather than recorded live: `backfilled: true` and a `backfill` block with `source`, `at`, `state` (`open`, `closed`, `merged`), `closed_at`, `author_kind_rule` and the pre-registration exclusion `flags` (`placeholder_hive`, `hive_on_hive`, `under_min_merged`). Such a bundle has no `verdicts`, `ci`, `sentinel` or `shadow`; see [Backfilled bundles](#backfilled-bundles) ([#12371](https://github.com/hivecommons/hive/issues/12371)). |
| `outcome` | Optional. What the forge said became of a PR this hive reviewed but did not merge, stamped by the governor's review-outcome ledger (see [Where outcomes come from](#where-outcomes-come-from)): `source` (`review-outcome-ledger`), `state` (`merged` or `closed`), `at`, and `merged_by` / `merged_at` or `closed_at`. A `merged` outcome comes with a `merge` block whose `actor` is the merger GitHub reported and whose `method` is `unrecorded`. |
| `created_at`, `updated_at` | When the bundle was first written and last completed. |
| `signed` | `true` only when a valid signing key produced `signature`. |
| `hash` | Hex SHA-256 of the canonical encoding (see below). |
| `signature` | Base64 Ed25519 signature over the `hash` string. Absent when `signed` is `false`. |

### `policy`

`acmm_level`, `perspectives`, `require_approval`, `min_priority`, `human_merge_paths`, and `sentinel_config_hash` (a hash of the sentinel configuration, so a later config change is detectable without copying the config).

### `verdicts[]`

`perspective`, `model`, `backend`, `verdict`, `confidence` (0 to 1), `recorded_at`, and `findings[]` of `path`, `line`, `severity`, `summary`.

## Canonical form, hash and signature

1. Marshal the bundle, drop the `hash` and `signature` fields, then re-encode with object keys sorted lexically, no insignificant whitespace, and no HTML escaping. This is the canonical JSON. Omitted optional fields stay omitted; do not add `null` or empty arrays.
2. `hash` is the lowercase hex SHA-256 of those bytes. The `signed` flag is inside the hashed content.
3. `signature` is the base64 Ed25519 signature of the ASCII `hash` string.

Any change to any covered field changes the hash and fails verification.

## Where it lives and retention

Bundles are written under the hive data directory, not `/var/run`, so they survive restarts:

```
/data/evidence/<owner>/<repo>/<number>/<head_sha>.json
```

The review relay writes them whenever it records a validated verdict (with or without posting a comment) or posts a review:

- The first write for a head fetches the PR once for `author` and `base_sha`, snapshots `policy` (ACMM level, `review.require_approval`, the repo's `auto_merge.human_merge_paths`, the perspective set, and a hash of the `sentinel` block), and sets `previous_bundle_id` to the newest bundle for an earlier head of the same PR.
- Later writes append to `verdicts` and `posted_reviews`. A verdict identical to one already recorded (ignoring `recorded_at`), or a review URL already listed, is not added again, and a pass that adds nothing leaves the file untouched, so re-running the relay never duplicates entries. A changed verdict from the same perspective is kept beside the earlier one.
- Every write recomputes `hash`, and signs when `evidence.signing_key_file` is set. Files are written then renamed, so a reader never sees a partial bundle. An existing bundle that cannot be parsed is left untouched rather than replaced.
- When the sentinel sweep flags a PR, its findings (`rule`, `summary`, sorted `paths`) are added to `sentinel` in the bundle for the flagged head. If the relay has not written that head yet, the sweep starts the bundle from the PR it already listed. A finding already recorded is not added again.
- Each time Hive evaluates a head's merge eligibility, it stores the CI gate's verdict as `ci.status` with the latest check runs and `captured_at`. If review has not started the head's bundle yet, the CI snapshot starts it from the PR metadata already fetched for the eligibility check. The bundle is rewritten only when the verdict or a check name or conclusion changed (check URLs alone do not count), so an unchanged verdict does not churn the hash; a merged bundle is never touched. When the PR merges, before the merge event is sealed in, Hive refreshes `ci` from GitHub, keeping the eligibility-time `status`: the latest check run per check name for the bundle's head, with its conclusion (or its status, such as `in_progress`, if it had not finished) and URL. It also captures `human_actions` from the PR's reviews and issue events: `approval` (an approving review of this head, or any approval since the bundle was created; `detail` is the review URL), `label_added`, `label_removed`, and `hold_lift` (a hold label removed); `detail` is the label name. Label changes count only from the bundle's `created_at`. Bot and App accounts, including Hive's own, are left out; their writes are in the hive audit log. Each capture is best-effort: if GitHub cannot be read, a warning is logged, that field stays empty, and the merge is still recorded. A replayed merge changes nothing.
- When the merged PR's title or body references an earlier PR of the same repo, the two bundles are linked; see [Change-failure links](#change-failure-links).
- A finding reported against the PR as a whole, with no file, is recorded with `path` `(pr)`. A verdict's `confidence` is that perspective's own 0 to 5 review confidence score divided by 5.
- `evidence.enabled: false` stops new writes. Failures are logged and never fail or retry the review itself.

Nothing prunes bundles automatically; they stay until an operator removes them. Reads follow the same retention contract as `/api/runs/audit`: a bundle Hive can prove existed (its PR directory is still there, or a newer bundle's `previous_bundle_id` names it) is reported with an explicit `expired` marker, never as silent absence.

### Change-failure links

A proxy for the change failure rate, not a verdict on any change ([#12067](https://github.com/hivecommons/hive/issues/12067)). When a PR merges, Hive scans its title and body for a keyword followed by `#N` or `owner/repo#N` (optionally `PR #N` / `pull request #N`) in the same repository:

- **Revert:** `revert`, `reverts`, `reverting`, `revert of` (GitHub's Revert button writes `Reverts owner/repo#N`).
- **Follow-up fix:** `fix`, `fixes`, `fixed`, `follow-up to|of|for` (also `followup`, `follow up`), `regression from|in|of`.

A PR named both ways counts as a revert. A referenced number is linked only when its newest merged bundle records a merge in the 14 days before this one, so issue numbers and PRs Hive never reviewed link nothing. The merging bundle gets `revert_of` / `followup_of` (bundle ids) before it is sealed; then each linked bundle gets `reverted_by` / `followed_up_by` naming it and is re-sealed (and re-signed), so its `hash` differs from the one in its own merge comment. Commit messages are not scanned. A replayed merge adds nothing, and failures are logged and never affect the merge.

**Reverts the hive did not merge** ([#13241](https://github.com/hivecommons/hive/issues/13241)): a merge the hive only observes — the `pull_request` `closed` webhook with `merged: true`, the PR state fetched for the review-outcome ledger, or the merged-PR backfill listing — is also checked for revert text, from the title and body already in hand and with no further GitHub call. `Revert "…"` titles (including GitHub's generated `Revert "Original title (#N)"`), `Reverts #N` references and git's `This reverts commit <sha>` line (7+ hex characters, matched against each candidate bundle's merge commit and head SHA) resolve to merged bundles in the same 14-day window and receive a `reverted_by` back-link naming the reverting PR as `owner/repo#N@<head>` (or `@merged` when the head is unknown). A revert whose text names neither a PR nor a commit is missed; the card says so.

**Issue re-opens:** when the duplicate-PR claim ledger voids a merged claim because a person reopened the issue it closed (the `issues` `reopened` webhook, or an enumeration listing the issue with `state_reason: reopened`), it hands the claim to `Client.RecordReviewEvidenceIssueReopen`, which stamps the closing PR's merged bundle with an `issue_reopens` entry if the reopen falls within 14 days of the merge, then re-seals it. Each issue is recorded once per bundle. Only issues linked by a closing keyword are tracked — the ledger never saw an issue closed by hand.

### Shadow gates and cross-family review in the study

The two opt-in experimental arms of the study (RFC [#12062](https://github.com/hivecommons/hive/issues/12062)) read these records: shadow-gate observations (`shadow`) and per-verdict reviewer model/backend. The run plan — consenting hives, 30-day periods, exact `review.shadow` / `intent.shadow` / `review_models.exclude_author_family` diffs, rollback — is [`research/ablation-plan-2026-10.md`](research/ablation-plan-2026-10.md). Auto-merge is unchanged everywhere in it.

### Backfilled bundles

`hivectl evidence backfill --repo <owner/repo> --since <date> [--until <date>] [--root <dir>] [--dry-run]` reconstructs bundles for PRs that predate Hive's recorder, so the study in [`research/preregistration-2026-10.md`](research/preregistration-2026-10.md) has a baseline ([#12371](https://github.com/hivecommons/hive/issues/12371); run plan in [`research/backfill.md`](research/backfill.md)). It writes what GitHub history still knows and nothing else:

- `author.login` and `author.kind` by a documented heuristic, recorded in `backfill.author_kind_rule`: Hive's `— hive:` attribution trailer in the body (`attribution_trailer`, carrying `agent`/`backend`/`model`), a login passed with `--agent-login` (`agent_login`), Hive's 🐝 identity footer (`identity_footer`), a GitHub `Bot` account or `[bot]` suffix (`bot_account`), else `default_human`.
- `merge` (`actor` from the merger GitHub reports, else `unrecorded`; `method` always `unrecorded`), or `backfill.state: closed` with `closed_at`, or `open`.
- `created_at` is the PR's creation time, so backfilled bundles fall into the right historical window; `updated_at` and `backfill.at` are when the backfill ran.
- `revert_of` / `followup_of` and the reverse links, under exactly the [change-failure link rules](#change-failure-links) (`pkg/evidence.ChangeRefs`, 14-day window), against bundles built in the same run and bundles already on disk.

`verdicts`, `ci` (left with a zero `captured_at`), `sentinel` and `shadow` are **absent, never fabricated**; `policy.acmm_level` is 0 unless `--acmm-level` stamps a level known to be in force for the window. Every bundle carries `backfilled: true` so each chart and export can exclude or annotate it. Population exclusions from the pre-registration are applied as `backfill.flags`, never by dropping rows: `hive_on_hive` is added for `hivecommons/hive`, `under_min_merged` when fewer than `--min-merged` (default 10) PRs merged in the window, and `placeholder_hive` by `--flag`. Existing bundles for a head are skipped, so re-running is safe; the run pauses on GitHub rate limits and `--dry-run` prints counts only. Like everything in the evidence store, the backfill is record-only: nothing reads it to decide a merge.

### Where outcomes come from

A PR's outcome (`pkg/evidence.PR.OutcomeKnown`) has three sources, folded per PR by `evidence.FoldPRs`. **Live merge:** the hive's own merge paths (relay, sweep, queue) complete the merged head's bundle with `merge` via `RecordPRMergedAudit`, and a bundle started after GitHub already reports a merge records it on creation. **Review-outcome ledger:** every governor cycle folds the open-PR list into `/data/review-outcomes.json` (`pkg/review/outcomes.go`) and resolves PRs that left the list with one `GetPRState` each (capped at 40 per cycle); the same pass now stamps the PR's newest bundle with `outcome` — `merged` by someone other than this hive (a human by hand, prow/tide, another bot) plus an `unrecorded` `merge` naming the merger, or `closed` without merge — so no extra GitHub call is spent (`Client.RecordReviewEvidenceOutcome`). A bundle that already states an outcome is never touched, and a PR still open is not stamped. At read time, `GET /api/evidence/summary` also applies the ledger's rows to PRs whose records state no outcome (`evidence.ApplyOutcomeHints`): a PR that merged or closed before the ledger could write to its bundle, and a PR the ledger still sees open, both count as known. **Backfill:** `hivectl evidence backfill` reconstructs `backfill.state` from forge history for PRs that predate the recorder. The summary reports `merged_elsewhere` per author kind (a subset of `merged`), `missing.prs_outcome_from_ledger` (PRs whose outcome is known only from the ledger) and `missing.prs_outcome_unknown` (PRs with none of the three: typically opened and reviewed minutes before the ledger's first look, or in a repository the governor does not enumerate).

## Downloading

- **API:** `GET /api/review/evidence?repo=<owner/repo>&number=<n>[&head=<sha>][&format=json|zip]` for owner/merger roles. The default is the bundle for the PR's latest head; `head` takes a full SHA or a unique prefix of at least 7 characters (409 if it matches more than one). `format=json` returns the bundle bytes exactly as stored, so its hash and signature verify offline; add `download=1` for an attachment. `format=zip` returns `bundle.json`, `verdicts.json` (the matching verdict reports), `review-links.json` (the PR's review-links slice) and `manifest.json` (SHA-256 of each file, plus the signing public key when the bundle is signed and the key is readable). A missing bundle is a 404 whose body has `status: "expired"` and `expired: true` when retention removed it, otherwise `status: "not_found"`. `GET /api/review/evidence/list?repo=&number=` lists every retained bundle id and head for the PR.
- **Aggregate:** `GET /api/evidence/summary` reports read-only aggregates over stored bundles, including per-tier proxy `write_counters` (`{attempted, blocked, blocked_by_reason}`, omitted when the counter file is absent) per-tier `intent_counters` (below), and `complexity_by_repo` (the newest 12 [monthly complexity snapshots](#monthly-complexity-snapshot-report-only) per repo, omitted when none exist); see [API reference](api-reference.md).
- **Window:** the summary takes `hours=` (default 24, `0` = all time, capped at 90 days) or a custom range `from=`/`to=` as RFC3339 or `YYYY-MM-DD` (a date-only `to` is inclusive; `to` defaults to now). `to` before `from`, a span over 3 × 366 days, or `to` without `from` is a `400`. A custom range echoes `from`/`to` in the response and keeps PRs opened in `[from, to)`. **A PR is placed in a window by its open time, never by when a bundle was written:** every bundle of the same PR is folded into one row — the newest record supplies verdicts, CI, sentinel and shadow observations, the earliest `created_at` (or `merge.opened_at`) is the open time, and the merge event, the change-failure links (`revert_of`, `followup_of`, `reverted_by`, `followed_up_by`) and a backfilled `backfill.state: closed` are taken from whichever record knows them. A live bundle is created when review starts and gains a merge event when the hive itself merged, or — when the bundle is started after the PR has already merged (automerge ran before the first review wrote one, or someone else merged) — carries the merge GitHub reports at creation (`method: unrecorded`, since the API says who and when, not how); a merge by another actor after the bundle exists is still unobserved until a backfill; a backfilled bundle ([#12371](https://github.com/hivecommons/hive/issues/12371)) is stamped with the PR's open time and the forge's outcome. Without the fold, a hive that reviews but does not merge (Bluefin) showed every reviewed PR as open and every approval as a false approve, and a review sweep over old PRs pulled them into the 24h window. The fold is `evidence.FoldPRs` in `pkg/evidence`; the [hub heartbeat evidence block](fleet-health.md#evidence-summary-heartbeat-12114) and [`hivectl evidence export`](#exporting) use the same one, so the three agree per PR ([#12516](https://github.com/hivecommons/hive/issues/12516)). The response also carries `available_windows` (`{"24", "168", "720", "all"}` → PR count under the same `repo`/`author_kind` filter) so the card can say "window too short — switch to 7d or 30d" instead of a bare "Not enough data". The Evidence card offers **24h · 7d · 30d · custom** pills (custom accepts backfilled history, up to 3 × 366 days) and remembers the choice per browser (`hive.evidence.window`); its header reads "Evidence · last 30d" or "Evidence · Oct 1 – Oct 15".
- **Hub revert rates by author kind:** heartbeat evidence includes `by_author_kind`, a fixed `human` / `agent` / `bot` map of merged PR counts (`n`) and linked revert counts (`bad`) over the newest bundle per PR in the 90-day creation window. Follow-up links do not count as reverts. The owner-only hub evidence drill-down pools these counts into `revert_rate_by_author_kind`, displaying the sample size and 95% Wilson interval (null rate below 10 merges). Older spokes without the map contribute no author-kind samples. Revert links are best-effort, within 14 days of merge; recent merges may not yet have a full observation window. These rates are descriptive, not merge gates. Each hive row's `counts.by_author_kind` carries the same buckets so a hive or ACMM-level selection re-pools them in the browser.
- **Outcome by author kind (panel 1):** `outcomes_by_author_kind` in the summary counts, over the folded record per PR in the window, `n`, `merged`, `closed`, `open`, `unknown`, `reverted_14d` (merged and then linked by `reverted_by` within 14 days), `issue_reopened_14d` (merged and then an issue it closed was reopened within 14 days, from `issue_reopens`), `change_failures` (the two added; a PR both reverted and re-opened counts twice) and `change_failure_rate` (change failures ÷ merged, `null` under 1 merge) for `human`, `agent` and `bot` authors ([#13241](https://github.com/hivecommons/hive/issues/13241)). The Evidence sub-panel draws it as the **Outcome by author kind** panel: one grouped bar per kind for merged / closed / undone (reverted or re-opened) plus a hollow bar for outcome unknown, with `n=`, the merged rate and the split of undone in the tooltip. **Denominator policy:** merged rate = merged ÷ PRs with a known outcome; change-failure rate = (reverted + re-opened) ÷ merged. This is the change-failure proxy for "a PR being accepted is not the same as the project being better off": it asks whether agent-written work gets undone more often than human work. Both signals are detected from data already fetched (see [Change-failure links](#change-failure-links)); reverts are only linked since 2026-10-15 and re-opens only since 2026-10-20. `merged_elsewhere` is the subset of `merged` that someone other than this hive performed, learned from the review-outcome ledger; `closed` and `open` come from a backfilled record of the PR or from the ledger (see [Where outcomes come from](#where-outcomes-come-from)). A PR with no merge event, no backfilled record and no ledger row has **no recorded outcome** — it is `unknown` (`pkg/evidence.PR.OutcomeKnown`), counted and shown, and excluded from every rate rather than counted as open or as not merged. The summary's `missing.prs_outcome_unknown` gives the window total and `missing.prs_outcome_from_ledger` how many the ledger resolved. A revert only counts once the reverting PR is linked. Reporting only ([#12364](https://github.com/hivecommons/hive/issues/12364)).
- **Maintainability trend (panel 7):** the Evidence sub-panel draws `complexity_by_repo` as three per-repo lanes on separate scales — median Go cyclomatic complexity, functions over the threshold, and warnings by tool (dashed, one dash pattern per tool) — with the first and last month on the axis. Snapshots whose month falls in the selected window are kept, boundary months included, and `n=` is the number of repos with at least two snapshots in that window; a repo with one snapshot is drawn as points. Missing months and missing tool measurements stay gaps, never zero. Vertical dotted **ACMM markers** come from `acmm_changes`: for each repo and level, the month of the earliest stored bundle whose `policy.acmm_level` is that level (1 … `acmmadvisor.MaxLevel`). Unlike the other aggregates they are computed over **all** stored bundles for the repo — not the latest bundle per PR, not the `author_kind` filter, not the `hours`/`from`/`to` window — because policy maturity is a repo property, and the chart clips them to the months it shows. A marker is the first observation, not the moment the setting changed, and a return to an earlier level is not shown. The panel is deliberately **not graded**: its explainer ends in a **Not graded** badge (grade key `ungraded`, "report-only; no threshold to grade against") rather than a Strong/Fair/Weak/Not enough data rating, because complexity rising or falling is an observation with no causal claim about agents, so there is no threshold to hold it to. It shows an empty state while no repo has a monthly snapshot. Reporting only ([#12369](https://github.com/hivecommons/hive/issues/12369)).
- **Reviewer calibration and accuracy (panels 2–3):** `reviewer_calibration` buckets each verdict by its stated confidence per perspective with `n`, `merged`, `blocks` and `unknown` (verdicts whose PR has no recorded outcome). **Denominator policy:** merged rate = merged ÷ (`n` − `unknown`); block rate = blocks ÷ `n` (a block is known at verdict time). The card draws a bucket with fewer than `EV_CALIB_MIN_BUCKET_N` (5) known-outcome verdicts hollow and leaves it out of the joined line, because one or two verdicts invert a line without meaning anything. `reviewer_accuracy_by_model` carries `approvals`, `approved_not_merged`, `approved_unknown`, `blocks`, `merged_after_block` and `blocked_unknown` per model family × author kind, with blocks split by the bundle's `policy.require_approval` into `enforced_blocks` (`merged_after_enforced_block`, `enforced_blocked_unknown`) and `advisory_blocks` (`dissent_before_merge`, `advisory_blocked_unknown`) — no row is dropped and the unsplit totals stay ([#12595](https://github.com/hivecommons/hive/issues/12595)): false-approve rate = approvals on PRs **known** not to have merged (closed without merge, or a backfilled record still open) ÷ approvals on PRs with a known outcome; false-block rate = **enforced** blocks on PRs that merged ÷ enforced blocks on PRs with a known outcome; an advisory block on a PR that merged is dissent before merge, shown as a count and a share of advisory blocks and never as an error rate (when every block in the window was advisory the card reads "Perspective verdicts were advisory on this hive (review.require_approval off); n blocks preceded a merge."); the unknown counts are shown beside each rate. Nothing is dropped silently and nothing unknown is counted as a failure. One bias is stated on the card because it cannot be removed by policy: on a live-only hive the only way an outcome becomes known is a merge this hive performed (or, since the merge-at-start fix, a merge GitHub already reported when the bundle began), so until a backfill covers the window "merged ÷ known" is an upper bound and "false-approve ÷ known" a lower bound; the unknown count is the honest measure of what is unresolved.
- **Boundary enforcement (panel 4):** the headline is **blocked ÷ every agent GitHub write** the hive can see in the window, from `write_activity` in the summary: `total_writes` = `mediated.n` (hive-mediated writes the audit log records — agent comments, issues, PRs, reviews, labels, issue closes, branch pushes, review requests) + `direct.attempted` (writes agents sent straight to GitHub through the proxy, summed from the proxy's weekly counter buckets that overlap the window, per tier under `direct.by_tier`); `blocked` = proxy refusals (`direct.blocked`, split by `direct.blocked_by_reason`) + audited refusals of mediated writes (`mediated.refused`). The panel prints "*B* of *T* agent writes blocked · *x*%" with both absolute counts, one row per intent tier (0 `ADVISORY` … 3 `ISSUES_PRS_MERGE`) showing blocked ÷ direct writes for that tier, one row for hive-mediated writes, the top block reasons in the legend, and `sentinel_hits_per_week` as a sparkline; `n=` is `total_writes`. **Denominator policy:** every agent write counts once; would-block counts from intent verification (`intent_counters`) are shown separately in the explainer and never added to blocked. The proxy's `write-counters.json` and the intent `intent-counters.json` now live under `/data/hive-metrics/` (the PVC) so they survive container restarts — before this they sat in container-local `/var/run/hive-metrics` and were lost on every restart, which is why a hive could show "16 attempted" after thousands of writes; a file left at the old path is folded in once on startup. The counter file records `recorded_since` (the Monday of its earliest week) and `direct.since` / `direct.partial_coverage` / `direct.recorded` tell the card when a window reaches back before direct-write counting began: the card then says "direct-write counters recorded since <date>" and the total for earlier weeks holds hive-mediated writes only — stated, never back-filled. Writes that bypass both the hive and the proxy are invisible. `write_counters` (all-time totals per tier) is still emitted for compatibility. Reporting only ([#12368](https://github.com/hivecommons/hive/issues/12368)).
- **Intent counters:** every intent evaluation — the merge-eligible lane's `writeIntentVerdicts` pass and the App self-merge sweep's gate, whether or not `intent.enforce` or `intent.shadow` is on — increments an in-memory counter keyed by tier (`tier0`..`tier3`) and writes one audit-log line, action `intent_evaluated`, detail `lane=<merge-eligible|self-merge> tier=<tierN> would_block=<bool> enforce=<bool> [reason=<code>]` ([#12362](https://github.com/hivecommons/hive/issues/12362)). `would_block` is what `BlocksMerge(true)` reports, so with enforcement off it is the shadow answer; reason codes are `tier1_needs_issue`, `tier2_needs_plan`, `tier3_needs_approval`, `alignment_misaligned`, `evidence_unavailable` or `unknown`. The counters flush once a minute (and on shutdown) into `/data/hive-metrics/intent-counters.json`, beside the proxy's `write-counters.json` and in the same shape: cumulative `totals` per tier (`attempted` = evaluations, `blocked` = would-block, `blocked_by_reason`) and `weeks` keyed by the UTC Monday of each ISO week. The summary API exposes `totals` as `intent_counters` (`{attempted, blocked}` per tier), omitted when the file is absent. Record-only: the counters are never read back into eligibility.
- **Shadow gates (panel 8):** `shadow_gate` in the summary counts, over the latest bundle per PR in the window, the PRs with any shadow observation (`n`), `would_have_held` (review gate) and `would_have_blocked` (intent), and for each gate (`review_gate`, `intent`) splits the evaluated PRs into `held` and `passed`, each with how they actually turned out: `merged`, `open`, `reverted` (merged and later linked by `reverted_by`), `closed` (from a backfilled record of the same PR; a live-only bundle does not record a close) and `unknown` (no recorded outcome; excluded from the false-alarm rate, shown as a count). `review_gate_by_lane` splits the review gate's counts into `merge_eligible` and `self_merge` (the self-merge ReviewGate, [#12592](https://github.com/hivecommons/hive/issues/12592)); `review_gate` stays the sum of both. The Throughput section's Evidence sub-panel draws it as the **Shadow gates** panel (one review-gate row per lane once the self-merge lane has observations), with a zero state while shadow mode is off. Reporting only; nothing reads it to decide a merge ([#12068](https://github.com/hivecommons/hive/issues/12068)).
- **Claim vs verdict (panel 5):** `claim_vs_verdict` in the summary groups the latest bundle per agent-authored PR (`author.kind == "agent"`) by the ISO week it was created and counts `claimed` (every such PR: opening or marking it ready is the agent saying "done"), `ci_not_green`, `verdict_not_approve` (any perspective verdict other than `approve`) and `both`, plus `n`, `ci_pending` and `ci_captured`, the CI column's denominator. CI state is classified by `evidenceCIState`: the bundle's `ci.status` (`success`/`failure`/`pending`) wins when set; otherwise any check with a failing conclusion (`failure`, `cancelled`, `timed_out`, `action_required`, `startup_failure`, `stale`) is not green, checks that all finished `success`/`skipped`/`neutral` are green (the same reading the merge gate applies — a skipped job is not a red one), and anything still `queued`/`in_progress` is **pending**. **Denominator policy:** CI not green rate = not green ÷ (`ci_captured` − `ci_pending`); a record with no captured checks is disclosed under `missing.records_empty_ci`, one still running under `missing.records_ci_pending`, and neither is counted as agreement or as failure. Before this classification every `skipped` check read as "not green", which flagged almost every captured record on hives that skip path-filtered jobs. The Evidence sub-panel draws it as the **Claim vs verdict** panel with grouped weekly bars. Reporting only; nothing reads it to decide a merge ([#12365](https://github.com/hivecommons/hive/issues/12365)). **Read as the CI gate — integrity + convergence** (`ci_recovery` in the summary; the panel is titled *CI gate*): this hive hands off to CI by policy — agents open the PR as soon as they believe it is done and do not wait for CI; a red check holds the PR until the agent pushes a fix (`auto_merge.self_authored` on, `auto_merge.required_checks` if set; there is no knob that makes agents wait for CI, and the summary echoes the policy under `ci_recovery.policy`). CI red at claim is therefore **by design: disclosed, not graded**. Two grades are: **Grade A, gate integrity (safety)** — `gate` covers every PR opened in the window that merged, any author, by the check state of the head that merged. The merged head is the backfilled record's `head_sha` (GitHub's view) when one exists, else the newest live head — a review-time record often predates the final push, so the newest reviewed head is frequently *not* the merged head. The state is GitHub's check-run conclusions on that head from the commit-trail rollup where the collector has reached the PR (authoritative), else the newest live snapshot for that head. **What counts as red is what the hive's own merge gate counts:** `PRCIRecoveryTrail` resolves the PR's base branch's required set *as it is now* (branch protection via `RequiredStatusCheckContexts`, else the configured `auto_merge.required_checks`; `required_known`, `required_checks` on the trail) and judges each head on those contexts only — green when every required context succeeded (a required DCO context is satisfied once any DCO context did), red when one failed (`required_red`), pending when one is still running or has never reported (`missing_required`). A failed check *outside* the required set — a docs link check, a labeller, a cancelled optional build — never blocked the merge and is **not** a breach; it is carried as `non_required_red` on the head and disclosed. Without a resolvable required set the head is judged on every gating check (the pre-existing reading) and counted under `required_unknown`. The trail also records `merged_by`. `merged_n`, `with_checks`, `green` (`green_from_bundles` of them rest on a snapshot — a re-run cannot turn a green head red, so they count; `non_required_red` of them merged with a red non-required check), `breaches` (**confirmed only**: GitHub reports a required check red on the merged head under a resolved required set; each listed in `breach_prs` with repo, number, head, author kind, `state`, `red` (the check names), `merged_at`, `merged_by`, `source` — a breach is a bypass by whoever merged, not a fault of the author, and a check made required after an older merge reads red for that merge, which is why the date is shown), `required_unknown`, `suspected` / `suspected_prs` (three readings that cannot be confirmed: a review-time snapshot that was red on the merged head — a check can be re-run to green on the same head after it; a required check GitHub shows no run for (`state` `missing_required`, `missing`) — the merge gate accepts a path-filtered skip and this panel cannot verify one; a red read under an unresolved required set — it may be an optional check. **Not** breaches; listed; the collector enriches these first), `pending` (merged while a check was still running), `unknown` (no snapshot on the merged head, no trail). **Denominator policy:** gate integrity = (`with_checks` − `breaches`) ÷ `with_checks` where `with_checks` = `green` + `breaches` + `pending`; `suspected` and `unknown` are counts outside the rate — a merged PR nobody snapshotted is not green and not a breach, and a stale red snapshot is a question for GitHub, not a verdict. Secondary **gate latency**: `median_green_to_merge_hours` over `green_to_merge_n` merged PRs, first green on the merged head → merge. **Grade B, convergence (efficiency)** — `convergence` covers agent-authored PRs with a live record (`n`): a *pass* is one CI evaluation of a distinct head SHA; `passes` buckets settled PRs by passes until the first green head: `"1"`, `"2"`, `"3"`, `"4+"`, `"never"` (closed without merge and without a green). Passes come from the GitHub commit trail where the collector has reached the PR (`from_github`: `PRCIRecoveryTrail` scans the PR's commits from the first, reading check-run conclusions through the app token, until the first green or `PRCIConvergenceCap` = 4 evaluated heads — `evaluated`, `passes_to_green`, `scan_capped`) and from the hive's own live snapshots otherwise (`from_bundles`; bundles can miss heads nobody reviewed, so this is a stand-in until the trail arrives). `settled` = Σ `passes`; `in_flight` (open, not yet green) and `unknown` (no CI captured, or merged without an observed green) are counts outside every denominator. `median_passes` is over PRs that went green, counting `4+` as 4. `by_week` repeats the read per ISO week of PR open time. **Denominator policy:** abandoned share = `never` ÷ `settled`; first-pass share = `"1"` ÷ (`settled` − `never`) — the RFC's C1 number, drawn as a neutral line, **not graded**. The collector enriches ≤ `ciRecoveryMaxPerPass` = 40 PRs per hourly pass — red-at-claim, suspected and merged-with-unknown-state PRs first, then the newest agent PRs without a trail — into the durable rollup `/data/hive-metrics/ci-recovery.json`. **Disclosed facts** (no grade): `at_claim` `{green, red, pending, none}` over agent claims (the claim is the first live agent bundle with captured checks), swarm non-approve verdicts, and the **recovery read** `red` for red-at-claim PRs: `recovered_merged`, `green_open`, `abandoned`, `pending`, `outcome_unknown`, `enriched` / `not_enriched`, `median_pushes_to_green`, `median_hours_to_green`, `same_head_recovered`; recovery rate = `recovered_merged` ÷ (`recovered_merged` + `abandoned`). **Grading constants** (`static/index.html`): Grade A — 0 breaches Excellent, breaches ÷ `with_checks` ≤ 1% Good (`EV_GATE_BREACH_GOOD`), ≤ 3% Fair (`EV_GATE_BREACH_FAIR`), above Weak. Grade B needs both conditions: median passes ≤ 1.5 and abandoned < 5% Excellent (`EV_CONV_PASSES_EXCELLENT`, `EV_CONV_ABANDON_EXCELLENT`); ≤ 2.5 and < 10% Good (`EV_CONV_PASSES_GOOD`, `EV_CONV_ABANDON_GOOD`); ≤ 4 and < 20% Fair (`EV_CONV_PASSES_FAIR`, `EV_CONV_ABANDON_FAIR`); otherwise Weak. Panel grade = the lower of A and B (`evLowerGrade`); both need `EV_GRADE_MIN_N` samples in their denominator. The weekly bars keep the claim-time facts (claimed, CI not green at claim, swarm not approve, both).
- **Work balance (panel 6):** answers "is the queue keeping up?" from GitHub truth rather than from the hive's own audit log alone. `work_balance` in the summary has one row per ISO week of the window with `opened` (issues, split `human` / `agent` / `bot`), `closed`, `prs_opened` (same split), `prs_merged`, `prs_closed` (closed without merge), `queue_issues` / `queue_prs` (open at week end), `issue_source` and `throttled`; `work_balance_sources` names where each series came from. **Sources:** issue opens and closes come from GitHub's issues API (`state=all`, `since`), every author included, pulled by the metrics collector hourly into the durable rollup `/data/hive-metrics/issue-activity.json` (42-day initial lookback, 180-day retention; `issues_github_since` is the first date it covers, `issues_fetched_at` the last refresh); author kind is `agent` when the body carries an agent attribution trailer, `bot` for GitHub `Bot` accounts, else `human`, and an audited agent-created issue overrides the kind to `agent`. Weeks whose Monday predates `issues_github_since` read the backfilled issue series at `<evidence-root>/<owner>/<repo>/issues/<YYYY-Www>.json` when its coverage contains the requested portion of that week. Event timestamps keep partial date ranges exact; zero-event files establish observed zero counts. The collector stays authoritative for weeks whose Monday is at or after `issues_github_since`, including exact equality. Missing, malformed, or insufficiently covered series files retain the audit fallback per repository (`issue_source: "audit"`, counted in `audit_only_weeks`); mixed repository rows also disclose audit coverage. Backfilled rows use `issue_source: "github"`; `issue_series_weeks` counts the repository-weeks supplied by the series. Series events carry no issue number, so the audited agent-created override above cannot be applied to series weeks: an agent-opened issue without the attribution trailer or 🐝 footer keeps the kind the writer recorded (`bot` for a `Bot` account) in series weeks while the collector reclassifies the same issue as `agent` in rollup weeks. The audit log sees only hive-mediated writes, so the human column is a floor for audit weeks. `hivectl evidence backfill-issues` writes the series (see [CLI reference](hivectl.md#evidence--backfill-bundles-from-github-history)). When the collector starts midweek, backfill through the following Monday (exclusive `--until`) so the entire boundary week has series coverage; stopping at the collector timestamp leaves that week on audit fallback. PR opens, merges and closes come from the folded evidence bundles (live and backfilled) for PRs opened in the window, so PRs this hive never saw are missing. Week-end queue depth is the governor's last state snapshot of the week (`/data/nous/snapshots`, actionable open issues / PRs — the same numbers as the dashboard queue section, held items excluded; `queue_samples`, `queue_retention_days` 30). **Denominator policy:** issue ratio = `closed` ÷ `opened` and PR ratio = `prs_merged` ÷ `prs_opened` over the whole window; nothing is dropped, PRs closed without merge are drawn as a tick but never counted as merged, and an unmatched close (an issue opened before the window) still counts as closed because the question is throughput, not per-issue lifecycle. **Grading** (`EV_THROUGHPUT_*`, `EV_QUEUE_GROWTH_MAX`): issues and PRs are graded separately and the card takes the lower — ratio ≥ 0.95 Excellent, ≥ 0.85 Good, ≥ 0.70 Fair, below Weak, and any grade is Weak when the week-end queue grew more than 10% from the previous week. Raw volume is never graded: a hive that opens 400 issues a week and closes 400 is Excellent. The Evidence sub-panel draws two charts (issues, PRs): opened stacked by kind, a closed / merged line, a closed-unmerged tick and a dashed week-end depth line on the right axis, under a headline "Queue at end of <week>: N open issues · M open PRs". `throttled` stays false because this hive reports no governor issue-creation throttle windows (`missing.throttle_windows_unavailable`). Reporting only ([#12366](https://github.com/hivecommons/hive/issues/12366)).
- **Dashboard:** owners get an **Evidence** button on Review Queue rows and Pipeline cards. It opens a modal with the bundle id, head, hash, whether it is signed, the verdict count and merge status, and **Download JSON** / **Download ZIP** buttons.
- **CLI:** `hivectl review evidence <owner/repo#n> [--head <sha>] [-o FILE] [--zip]` prints or saves the bundle, and `hivectl review evidence verify FILE [--pubkey KEY]` checks it offline (see [hivectl](hivectl.md#review--review-evidence)).
- **Export:** `hivectl evidence export [--since <RFC3339|duration>] [--format csv|jsonl] [--repo owner/name] [--out FILE] [--root DIR]` dumps one row per PR straight from the bundles on disk; see [Exporting](#exporting).
- **On the PR:** when a PR merges through any Hive merge path, the merge event is added to the head's bundle (re-signed) and Hive posts one comment:

  ```
  <!-- hive-review-evidence --> Evidence bundle `<id>` sha256:<hash> — download from the dashboard or `hivectl review evidence <owner/repo#n>`.
  ```

  The marker makes it idempotent: a PR gets one such comment, however many sweeps see the merge.

## Exporting

Operators and the study need the raw per-PR evidence off a spoke without scraping `/data/evidence`. `hivectl evidence export` reads the schema v1 bundles under `--root` (default `/data/evidence`, layout `<org>/<repo>/<pr>/<sha>.json`) from the local filesystem — no hive session, no network — and emits one row per pull request, **folding every bundle of the PR** exactly as `GET /api/evidence/summary` does (see **Window** under [Downloading](#downloading)), so exported counts reconcile with the dashboard Evidence charts ([#12367](https://github.com/hivecommons/hive/issues/12367), [#12516](https://github.com/hivecommons/hive/issues/12516), RFC [#12062](https://github.com/hivecommons/hive/issues/12062)). Identity, verdict, CI, sentinel, shadow and `created_at`/`updated_at` columns come from the newest bundle; `outcome`, `merged_at`, the four link columns and `opened_at` come from whichever bundle of the PR recorded them.

```bash
hivectl evidence export --since 168h --format csv --out evidence.csv   # last 7 days
hivectl evidence export --since 2026-10-01T00:00:00Z --repo hivecommons/hive   # JSON Lines on stdout
```

- `--since` takes an RFC3339 timestamp or a Go duration and keeps PRs whose open time (`opened_at`) is at or after it, as the summary API does; a PR opened before the cutoff stays out even when a newer head was reviewed after it.
- `--format` is `jsonl` (default, one JSON object per line) or `csv` (header row first). Lists join with `;` in CSV and each verdict is `perspective:verdict:model:backend:confidence`.
- Columns: `repo`, `pr`, `head_sha`, `bundle_id`, `author_login`, `author_kind`, `author_model`, `acmm_level`, `perspectives`, `reviewer_models` (distinct, sorted), `verdicts` (perspective, verdict, model, backend, confidence; sorted by perspective), `ci_conclusion` (`ci.status`), `ci_captured_at`, `outcome` (`merged`, `closed` when a backfilled bundle records a close without merge, else `open`), `merged_at`, `revert_of`, `reverted_by`, `followup_of`, `followed_up_by` (bundle ids), `shadow_review_would_have_held`, `shadow_intent_would_have_blocked` (empty / `null` when the gate was not evaluated), `sentinel_hits`, `signed`, `created_at`, `updated_at` (of the newest bundle), `opened_at` (PR open time).
- Rows are ordered by repo (case-insensitive) then PR number, so two runs over the same files are byte-identical. Files that do not parse or are not schema v1 are skipped and counted on stderr; nothing is fabricated.
- Reporting only: the export reads bundles and never influences a merge.

## Signing key

Set `evidence.signing_key_file` in `hive.yaml` (see the [operator reference](operator-reference.md#notable-fields)). The file holds an Ed25519 private key as a 32-byte seed or 64-byte key, hex or base64 encoded. If the path is empty or the file is absent, bundles are written **unsigned** with `"signed": false`; they still carry a `hash`, which detects accidental change but not tampering by someone who can recompute it. Keep the private key off shared hosts and publish only the public key.

## Verify offline

You need the bundle JSON and the operator's Ed25519 public key (32 bytes, hex). No Hive access is required.

1. Check `"signed": true` and that `signature` is present. If not, the bundle is unsigned and cannot be verified beyond its hash.
2. Build the canonical JSON as described above.
3. Compute SHA-256 of it and compare, as lowercase hex, with `hash`. A mismatch means the content changed.
4. Base64-decode `signature` and verify it as an Ed25519 signature of the ASCII `hash` string under the public key.

A reference check in Python (needs `pip install cryptography`):

```python
import base64, hashlib, json, sys
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

bundle = json.load(open(sys.argv[1]))
pub = Ed25519PublicKey.from_public_bytes(bytes.fromhex(sys.argv[2]))
sig = base64.b64decode(bundle.pop("signature"))
claimed = bundle.pop("hash")
canon = json.dumps(bundle, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()
assert hashlib.sha256(canon).hexdigest() == claimed, "content changed"
pub.verify(sig, claimed.encode())  # raises InvalidSignature on failure
print("ok", bundle["id"])
```

Go users can call `evidence.Verify(&b, pub)` from `pkg/evidence`. The Python sketch assumes numbers re-encode identically (integers and short decimals such as `0.9`); if a number does not round-trip, use the Go verifier.

## How to read the Evidence card

Every panel on the Throughput section's Evidence card shares one layout ([#13204](https://github.com/hivecommons/hive/issues/13204)): a header row — title · ⓘ · `n=…` · **letter grade** — then one plain-English sentence, then the chart and a compact legend. The letter grade and the sentence are computed on the server (`pkg/evidence/grade.go`) from the same `/api/evidence/summary` numbers the chart draws, and shipped as `cards[<panel>] = {grade: {letter, band, reason, enough_data, rubric}, plain_summary}`. Nothing inside a panel renders larger than the header row, and nothing in it is read by anything that decides a merge.

The ⓘ button opens a popover over the chart (never inline) with the plain-language explainer — **What is measured**, **Why it matters**, **How it is measured**, **What good looks like**, **What ours shows**, **Details** (the denominators, policy and coverage text that used to sit under the chart), **What this does not show** — followed by the grade line: the letter, the one-line reason, and the rubric it was computed by. The "ours" and "details" sections are computed from the summary for the selected window, so they change with the 24h / 7d / 30d / custom pills. The button is keyboard reachable (`aria-expanded`, `aria-controls`); Esc closes an open explainer.

### Grades and what they mean

A grade is a letter from **A+** to **C-** (A band green, B band amber, C band red) or **—** (grey, dashed) when the panel does not have enough data to be graded. Hovering the pill shows the reason ("86% of 22 PRs with a known outcome merged; 1 of 19 merged was reverted"); the popover adds the rubric. Where a rubric produces a 0–1 score the letter is: A+ ≥ 0.97, A ≥ 0.93, A- ≥ 0.90, B+ ≥ 0.87, B ≥ 0.83, B- ≥ 0.80, C+ ≥ 0.77, C ≥ 0.73, otherwise C-. Every rubric needs at least `GradeMinN` (20) samples of its denominator, else **—** with a reason that names what is missing. Rubrics are pure functions in `pkg/evidence/grade.go`, each table-tested; change the table and the code together.

| Panel | Needs | Rubric |
|---|---|---|
| Outcome by author kind | 20 PRs with a known outcome (merged, closed or open) | score = merged ÷ known × (1 − (reverted + issue re-opened within 14 d) ÷ merged, capped at 1); letter from the score table |
| Reviewer calibration | 20 verdicts with a known outcome in the 0.9–1.0 bucket | score = merged share in the top bucket, minus 0.10 when the merged share is not non-decreasing across buckets that hold ≥ 5 known outcomes |
| Reviewer accuracy × model family | the rework-based outcome ledger (`pkg/review/accuracy.go`) with ≥ 20 samples | score = 1 − max(false-approve rate, false-block rate) summed over reviewer models; **—** ("needs the reviewer outcome ledger") until the ledger exists — merge-vs-close is never used as ground truth |
| Maintainability trend | one repo with ≥ 2 monthly snapshots in the window | first→last change of median cyclomatic and of functions over threshold, summed across repos: neither worse → A- / A / A+ by how many improved (0 / 1 / 2); one worse → B (B+ if the other improved); both worse → C |
| Boundary enforcement | 20 agent writes (hive-mediated + direct through the proxy) | blocked ÷ writes: 0 blocked → A- ("boundary not yet exercised"); ≤ 1% A+, ≤ 2% A, ≤ 5% B, ≤ 10% C+, above C- |
| CI gate — integrity + convergence | 20 merged PRs with a known merge-time check state | confirmed breaches (required check red or missing on the merged head, as the merge gate judges): 0 → A+; ≤ 1% C+, ≤ 3% C, above C-. Convergence and CI red at claim are disclosed in the card and popover, not graded |
| Work balance | 20 issues opened in the window | issues opened ÷ closed: ≤ 0.8 A+, ≤ 0.9 A, ≤ 1.0 A-, ≤ 1.15 B+, ≤ 1.3 B, ≤ 1.5 B-, ≤ 1.75 C+, ≤ 2.0 C, above (or nothing closed) C- |
| Shadow gates | 20 PRs observed by the shadow gates | would-have-held ÷ observed: ≤ 5% A+, ≤ 10% A, ≤ 15% A-, ≤ 20% B+, ≤ 30% B, ≤ 40% B-, ≤ 50% C+, ≤ 65% C, above C- (nothing was actually held; the share says how often the gates would have intervened) |

The plain sentence under the header is at most 20 words and is templated from the numbers, for example "Agents: 418 merged, 1 reverted, 0 re-opened (0.2%). Humans: 346 merged, 0 undone. 3 outcomes not recorded yet." (a kind with no change failures is shortened to "0 undone" to stay within the word budget) or "All 58 merged PRs with a known state had green required checks. Agent PRs needed a median 1 CI pass." Zero-data windows get an explicit sentence ("No PRs with a recorded outcome in this window.") rather than a blank.

The legacy client-side Strong / Fair / Weak bands below are still computed inside the explainers (they feed the "What good looks like" wording) and remain the fallback grade line when a hive does not yet ship `cards`.

Terms used on first sight: a **confidence bucket** groups verdicts by the reviewer's stated confidence (0.0–0.5, 0.5–0.7, 0.7–0.9, 0.9–1.0); **calibration** asks whether that confidence matches how often the verdict turned out right; a **false approve** is an approval on a PR that did not merge; a **false block** is a block an *enforcing* gate was overridden on (the PR merged anyway while `review.require_approval` was on); a block recorded while perspective verdicts were advisory (`policy.require_approval` false) that preceded a merge is **dissent before merge** — a count and a share of advisory blocks, never an error rate ([#12595](https://github.com/hivecommons/hive/issues/12595)).

Grades are **Strong**, **Fair**, **Weak** or **Not enough data** — Work balance uses the four-band **Excellent** / **Good** / **Fair** / **Weak** scale the throughput ratio calls for. They describe the data in the window and are never read by anything that decides a merge. Thresholds are named constants in `index.html` and listed here; change both together.

| Panel | Graded on | Strong | Fair | Weak |
|---|---|---|---|---|
| Outcome by author kind | (reverts + issue re-opens) within 14 d ÷ merged (`EV_REVERT_*`) | ≤ 5% | ≤ 15% | above |
| Reviewer calibration | merged rate in the 0.9–1.0 bucket (`EV_CALIB_*`) | ≥ 80% | ≥ 60% | below |
| Reviewer accuracy × model family | worse of false-approve and false-block rate (`EV_ERR_*`); the false-block rate counts enforced blocks only, so advisory dissent before merge never paints the panel red | ≤ 10% | ≤ 25% | above |
| Boundary enforcement | blocked ÷ **all** agent writes in the window (hive-mediated from the audit log + direct through the proxy; `EV_BLOCKED_*`). A handful of refusals in thousands of writes is the boundary working quietly | ≤ 1% | ≤ 5% | above |
| Shadow gates | would-have-stopped PRs that merged without a revert ÷ stopped PRs that merged (`EV_SHADOW_FALSE_ALARM_*`) | ≤ 25% | ≤ 50% | above |
| CI gate — A, integrity | breaches (merged with a *required* check red or missing, as the merge gate judges; red non-required checks are disclosed, not breaches) ÷ merged PRs with a known merge-time state (`EV_GATE_BREACH_*`); unknown, suspected and pending are counts outside the rate | 0 breaches Excellent; ≤ 1% Good | ≤ 3% | above |
| CI gate — B, convergence | median CI passes to first green over agent PRs that went green AND abandoned (never green, closed unmerged) ÷ settled (`EV_CONV_PASSES_*`, `EV_CONV_ABANDON_*`); panel grade = lower of A and B. CI red at claim is disclosed, not graded: this hive hands off to CI by policy | ≤ 1.5 passes & < 5% Excellent; ≤ 2.5 & < 10% Good | ≤ 4 & < 20% | beyond |
| Work balance | issues closed ÷ opened and PRs merged ÷ opened, graded separately, card takes the lower (`EV_THROUGHPUT_EXCELLENT` 0.95 / `EV_THROUGHPUT_GOOD` 0.85 / `EV_THROUGHPUT_FAIR` 0.70); Weak regardless when week-end queue depth grew > 10% week over week (`EV_QUEUE_GROWTH_MAX`) | Excellent ≥ 95% · Good ≥ 85% | ≥ 70% | below, or queue growing |

**Maintainability trend** is not in the table on purpose: its explainer ends in a **Not graded** badge ("report-only; no threshold to grade against") rather than one of the rated grades, because complexity trends are observations with no causal claim. Not graded is distinct from Not enough data: more snapshots would not produce a grade, and its "What ours shows" section carries no early-read caveat.

A panel is **Not enough data** when the graded denominator is below `EV_GRADE_MIN_N` (20); its reason line then names the denominator ("needs at least 20 merged PRs") and, when the summary's `available_windows` shows a longer preset holding more PRs, adds "window too short: 7d 509, 30d 2553, all time 8731 PRs; switch to 7d or 30d". The card header shows the same hint when the window holds no PRs at all. Windows of `EV_SHORT_WINDOW_HOURS` (48) or less add an "early read" caveat. Every explainer states its denominator policy and repeats the honest limits: merged is not the same as correct, a PR with no recorded outcome is reported as **unknown n=X** and left out of every rate (never counted as "not merged" or as a false approve), live evidence (verdicts, CI, sentinel) has only been recorded since `EV_DATA_SINCE` (2026-10-08) reverts only since `EV_REVERTS_SINCE` (2026-10-15) and issue re-opens only since `EV_REOPENS_SINCE` (2026-10-20); backfilled records reach further back but carry only author, outcome and dates.

## What a new push does

A push changes `head_sha`, so earlier verdicts no longer describe the code. Hive starts a new bundle for the new head with `previous_bundle_id` set, and re-reviews. The old bundle is left untouched and remains verifiable.

## How to read the hub evidence view

Hub admins reach the fleet evidence view from the **Fleet quadrant** card on
the hub dashboard (click the chart or its **🔬 Evidence** button). It is
`GET /api/hub/evidence` rendered findings-first: a **What this says** block
of three to six plain sentences generated from the data, then one table per
section — each with a one-line plain finding under its title, a sticky title
and column heads — a **window** selector (24h · 7d · 30d · 90d · custom
date range — hives report fixed windows in their heartbeat, so a custom range
is served from the smallest window that covers it and the view says so), hive
and autonomy-level filters, a **📄 Report / ⬇ Markdown** pair (see
*Downloadable report* below) and an ⓘ button per section that opens a
plain-language explainer with four headings: **What is measured · Why it
matters · How it is measured · How to read our numbers**. The last one is
computed from the numbers on screen and ends with a grade.

### Findings first

Every sentence in **What this says**, and every one-line finding under a
section title, is generated from the response fields (`findings[]` on the
API; regenerated client-side by the same rules when a filter is active) and
never typed in. Each has an explicit fallback when the data is not there
("No autonomy level has 10 merged PRs yet, so revert rates cannot be
reported") so no number is ever invented. Sections no selected hive reports
are not drawn as empty tables; they are listed once under **Waiting on
hives** at the bottom, in user-facing words ("hives report this once upgraded
to the Oct 16 build or later"). The technical pointer — which heartbeat field,
which issue — lives only in that item's ⓘ explainer and in this document.
Visible modal copy never cites an issue number
(`TestEvidenceModalNoIssueNumbers`).

### Plain labels

| On screen | Means |
|---|---|
| **All selected hives** | the pooled row — every hive the filter currently selects (the whole fleet with no filter). When exactly one hive is selected its own row reads *(same hive as above)* instead of repeating the numbers. |
| **Merged PRs** | the number of pull requests GitHub says were merged in the last 90 days (the denominator for coverage). |
| **X% of N** | the rate and how many cases it is computed from, in one cell. |
| **margin lo – hi** (muted line under a rate) | the **margin (95%)**: with this many cases, the true value is very likely inside this range. Fewer cases, wider range. |
| **—** | below the **minimum sample** of 10 cases a rate prints as a dash, never 0%, because a percentage from so few cases is mostly luck; the count is still shown. |
| **Reviewer confidence** | how sure the AI reviewer said it was, as one of four ranges (below). |
| **False approve** | the reviewer approved, but the PR was never merged or was reverted. |
| **False block** | the reviewer blocked while an enforcing gate was on (`review.require_approval`), but the PR merged anyway after a human override. |
| **Dissent before merge** | the reviewer blocked while verdicts were advisory (`review.require_approval` off) and the PR merged; reported as a count and a share of advisory blocks, not as an error. |
| **Autonomy level** | the ACMM level the hive runs at, L1 (advice only) to L6 (merges on green CI without a human). |
| **Median time-to-merge (median of hive medians)** | column of the *Autonomy level × outcome* table: time from PR opened to merged. See below. |

### Median time-to-merge per autonomy level

Each spoke reports, per heartbeat window, the median time-to-merge
(`merge.at` − `merge.opened_at`, via `evidence.MedianTimeToMerge`) of the
merged PRs opened in that window that recorded `merge.opened_at`, plus that
sample size, as `time_to_merge` (`median_s`, `n`) in its heartbeat evidence
block. Medians cannot be averaged or summed, and the heartbeat carries one
median per hive rather than a distribution, so the hub reports, per ACMM
level, the **median of the per-hive medians** (`by_acmm_level[].time_to_merge`:
`median_of_hive_medians_s`, `hives`, `n`; with an even number of hives, the
mean of the two middle medians, rounded down to the second). Each hive counts
once however many PRs it merged; `n` is the total PRs behind the medians and
is shown under the value. The column header says *median of hive medians* so
it is not read as a pooled median. A level where no hive reports it — older
builds, or only PRs merged before `opened_at` was captured — shows
**not reported**, never 0, and the item stays under **Waiting on hives**.
The client re-pools a hive / level selection by the same rule
(`newEvidenceLevelTimeToMerge` in `pkg/hub/evidence.go`, mirrored by
`evidenceLevelTimeToMerge` in `dashboard.html`). Reporting only: nothing reads
it to decide a merge.

### Per-axis criteria

The **Per-axis criteria** table sits below the findings and the evidence
tables. For each selected hive it lists the four quadrant axes (Trust ·
Productivity · Satisfaction · Efficiency) and sorts each axis's criteria into
**met**, **unmet** and **not reported**. It is derived hub-side from the same
criteria the Fleet quadrant card scores (`pkg/hub/quadrant_axes.go`), so
spokes send nothing new. It is served as `hives[].criteria[]` on
`GET /api/hub/evidence`: one entry per axis (`axis`, `label`, `scored`) with
its `criteria[]` (`name`, `label`, `status`, and `display` / `score` when
measured, `reason` when not). Its rules:

- A measured criterion is **met** when its 0–100 score is at least 50, that
  is, at least halfway from its documented worst value to its documented best
  (`evidenceCriterionMetScore` in `pkg/hub/evidence_criteria.go`; the curves
  are in `pkg/hub/quadrant_curve.go`). Below 50 it is **unmet**.
- A criterion with no input is **not reported**, with the reason. It is never
  **unmet**.
- An offline hive, a hive that has not sent an evidence summary, or a hive
  with no scored axis shows every criterion as **not reported**.

The table reflects each hive's latest heartbeat, not the selected window.
Reporting only: nothing reads it to decide a merge.

### Reviewer-confidence ranges

Spokes report calibration as six equal-width heartbeat buckets
(`int(confidence × 6)`, 0–5). The spoke's own Evidence card shows four ranges.
The hub view, the findings and the report fold the six buckets into those
four ranges and label each with both the name and the numeric range:

| Hub label | Heartbeat buckets | Spoke card range |
|---|---|---|
| Not confident (0.0–0.5) | 0, 1, 2 | 0.0–0.5 |
| Somewhat confident (0.5–0.67) | 3 | 0.5–0.7 |
| Confident (0.67–0.83) | 4 | 0.7–0.9 |
| Very confident (0.83–1.0) | 5 | 0.9–1.0 |

The bucket edges are sixths, so the hub's 0.67 / 0.83 boundaries approximate
the spoke card's 0.7 / 0.9; the mapping is in `evidenceConfidenceRanges`
(`pkg/hub/evidence_findings.go`) and its JS mirror `EVIDENCE_CONFIDENCE_RANGES`.
Pooled buckets carry `bad` (wrong calls) as well as `n` so the hub can
regroup them without re-deriving counts from a rounded rate.

### Downloadable report

The **📄 Report** link in the window row opens the current view — same window,
same hive / level filter — as a standalone plain-language page in a new tab;
**⬇ Markdown** downloads the same report as a `.md` file. Both are
`GET /api/hub/evidence/report` (hub admin / owner only, like
`/api/hub/evidence`): `?hive=<id>[,<id>…]`, `?level=<n>`, the same
`hours=` / `from=` / `to=` window parameters, `format=md` (default) or
`format=html`, and `download=1` to receive it as an attachment named
`hive-evidence-<hive|fleet>-<from>-<to>.md|html`. It is rendered server-side in
Go from the same `EvidenceView` the modal uses, so every figure reconciles with
what is on screen. Sections, in order: **Summary** (the *What this says*
sentences) · **What we measured and why** · **The numbers** (each section's
table with the plain labels) · **How to read these** (margin of error, merged
≠ correct, open PRs count as not merged, recording since 2026-10-08, backfilled
bundles carry no verdicts) · **What is not reported yet and why** · **Grade per
section** with the thresholds spelled out · a footer linking back here. The
HTML variant is a single page with inline CSS and no scripts or external
assets; hive names are escaped. Golden files live in
`pkg/hub/testdata/evidence_report_*.golden`.

Grades and their thresholds (named constants in `dashboard.html` and mirrored
in `pkg/hub/evidence_report.go`; `Not enough data` whenever the graded rate is
below the minimum sample, `Not graded` where there is no "good" value):

| Section | Graded on | Strong | Fair | Constants |
|---|---|---|---|---|
| Evidence coverage | share of merged PRs (90d) with a record, higher is better | ≥ 80% | ≥ 40% | `EVIDENCE_COVERAGE_STRONG/FAIR` |
| Reviewer confidence | wrong-call rate in the most-confident measurable range, lower is better (needs ≥ 2 measurable ranges) | ≤ 10% | ≤ 25% | `EVIDENCE_ERR_STRONG/FAIR` |
| Outcome by author kind | revert rate of agent authors (human if no agents), lower is better | ≤ 5% | ≤ 15% | `EVIDENCE_REVERT_STRONG/FAIR` |
| Autonomy level × outcome | revert rate of the highest level with enough data, lower is better | ≤ 5% | ≤ 15% | `EVIDENCE_REVERT_STRONG/FAIR` |
| Boundary enforcement | — (not graded: no block rate is "good" on its own) | | | |
| Same- vs cross-family review | false-approve rate of the mix with most verdicts, lower is better | ≤ 10% | ≤ 25% | `EVIDENCE_ERR_STRONG/FAIR` |
| Per-hive evidence | share of hives reporting for the window, higher is better (no minimum sample) | ≥ 80% | ≥ 40% | `EVIDENCE_REPORTING_STRONG/FAIR` |
| Shadow gates × outcome | false-alarm rate (would-have-held PRs that merged and stuck) of the gate with most held PRs, lower is better | ≤ 25% | ≤ 50% | `EVIDENCE_SHADOW_FALSE_ALARM_STRONG/FAIR` |

Limits every explainer repeats: merged is not the same as correct; open PRs
are not failures; closed-without-merge is not recorded and counts as not
merged; reverts are only linked since 2026-10-15 (`EVIDENCE_REVERTS_SINCE`)
and evidence records only exist since 2026-10-08 (`EVIDENCE_DATA_SINCE`).
Nothing in this view or the report is read by any merge decision.

## Not covered

A bundle shows what Hive recorded. It does not prove who a reviewer's identity is, does not make the operator's policy accept automated review, and is not a compliance claim of any kind.

## Monthly complexity snapshot (report-only)

`pkg/evidence/complexity` writes one maintainability snapshot per repo per
month to `/data/evidence/<org>/<repo>/complexity/<YYYY-MM>.json`: Go
cyclomatic complexity (total, mean, median, p90, max, functions over a
threshold, top hotspots) plus static-analysis finding counts per tool. An
existing month is never overwritten. It is report-only and never blocks a
merge (RFC #12062, #12072, #12363); it feeds the dashboard maintainability
panel and the study.

- **Schedule:** the hive runs the snapshot itself (spoke side; nothing is
  added to the managed repos' CI). A background loop checks hourly which
  managed repos have no file for the current month and snapshots only those,
  so a pass in a month that is already covered is a `stat` per repo. The first
  check runs two minutes after boot. The loop stops with the process and a shutdown waits
  for an in-flight pass.
- **Source:** the repo's default branch, fetched as a GitHub tarball through
  the App (no `git` on the spoke, no clone cache). The archive is extracted
  into a temporary directory that is removed after measuring; `ref` in the
  snapshot is the default-branch name.
- **Defaults:** `evidence.complexity.enabled` is on (it needs `evidence.enabled`
  too) because a monthly archive download plus an AST pass is cheap.
  `evidence.complexity.static_analysis` is off because `go vet` / `staticcheck`
  compile the repo and download its modules; turn it on where that cost is
  acceptable. Analyzer failures are logged and the snapshot is still written
  without that tool's count.
- **Retention:** the newest 24 monthly files are kept per repo; older ones are
  removed after a new month is written. `GET /api/evidence/summary` returns
  the newest 12 per repo as `complexity_by_repo` (hotspot lists are left on
  disk). The review-bundle walkers skip the `complexity/` directory.
- **Failures:** a repo whose snapshot fails (no GitHub client yet, archive
  error, invalid repo name) is logged and retried on the next hourly check;
  other repos are unaffected.

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
| `merge` | Present once merged: `actor`, `method`, `at`, `sha`. Absent before merge. |
| `revert_of`, `followup_of` | Bundle ids of earlier merged PRs this PR reverts or follows up with a fix (see [Change-failure links](#change-failure-links)). |
| `reverted_by`, `followed_up_by` | Bundle ids of later PRs that reverted or followed up this one. |
| `shadow` | Optional. What shadow-mode gates would have done, recorded only: `review_gate` (`would_have_held`) and `intent` (`would_have_blocked`), each with `reason`, `tier`, `confidence` and `evaluated_at`. Absent unless a shadow gate ran; it never affects a merge ([#12103](https://github.com/hivecommons/hive/issues/12103)). |
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
- Each time Hive evaluates a head's merge eligibility, it stores the CI gate's verdict as `ci.status` with the latest check runs and `captured_at` on the head's existing bundle. The bundle is rewritten only when the verdict or a check name or conclusion changed (check URLs alone do not count), so an unchanged verdict does not churn the hash; a merged bundle is never touched. When the PR merges, before the merge event is sealed in, Hive refreshes `ci` from GitHub, keeping the eligibility-time `status`: the latest check run per check name for the bundle's head, with its conclusion (or its status, such as `in_progress`, if it had not finished) and URL. It also captures `human_actions` from the PR's reviews and issue events: `approval` (an approving review of this head, or any approval since the bundle was created; `detail` is the review URL), `label_added`, `label_removed`, and `hold_lift` (a hold label removed); `detail` is the label name. Label changes count only from the bundle's `created_at`. Bot and App accounts, including Hive's own, are left out; their writes are in the hive audit log. Each capture is best-effort: if GitHub cannot be read, a warning is logged, that field stays empty, and the merge is still recorded. A replayed merge changes nothing.
- When the merged PR's title or body references an earlier PR of the same repo, the two bundles are linked; see [Change-failure links](#change-failure-links).
- A finding reported against the PR as a whole, with no file, is recorded with `path` `(pr)`. A verdict's `confidence` is that perspective's own 0 to 5 review confidence score divided by 5.
- `evidence.enabled: false` stops new writes. Failures are logged and never fail or retry the review itself.

Nothing prunes bundles automatically; they stay until an operator removes them. Reads follow the same retention contract as `/api/runs/audit`: a bundle Hive can prove existed (its PR directory is still there, or a newer bundle's `previous_bundle_id` names it) is reported with an explicit `expired` marker, never as silent absence.

### Change-failure links

A proxy for the change failure rate, not a verdict on any change ([#12067](https://github.com/hivecommons/hive/issues/12067)). When a PR merges, Hive scans its title and body for a keyword followed by `#N` or `owner/repo#N` (optionally `PR #N` / `pull request #N`) in the same repository:

- **Revert:** `revert`, `reverts`, `reverting`, `revert of` (GitHub's Revert button writes `Reverts owner/repo#N`).
- **Follow-up fix:** `fix`, `fixes`, `fixed`, `follow-up to|of|for` (also `followup`, `follow up`), `regression from|in|of`.

A PR named both ways counts as a revert. A referenced number is linked only when its newest merged bundle records a merge in the 14 days before this one, so issue numbers and PRs Hive never reviewed link nothing. The merging bundle gets `revert_of` / `followup_of` (bundle ids) before it is sealed; then each linked bundle gets `reverted_by` / `followed_up_by` naming it and is re-sealed (and re-signed), so its `hash` differs from the one in its own merge comment. Commit messages are not scanned. A replayed merge adds nothing, and failures are logged and never affect the merge.

### Shadow gates and cross-family review in the study

The two opt-in experimental arms of the study (RFC [#12062](https://github.com/hivecommons/hive/issues/12062)) read these records: shadow-gate observations (`shadow`) and per-verdict reviewer model/backend. The run plan — consenting hives, 30-day periods, exact `review.shadow` / `intent.shadow` / `review_models.exclude_author_family` diffs, rollback — is [`research/ablation-plan-2026-10.md`](research/ablation-plan-2026-10.md). Auto-merge is unchanged everywhere in it.

## Downloading

- **API:** `GET /api/review/evidence?repo=<owner/repo>&number=<n>[&head=<sha>][&format=json|zip]` for owner/merger roles. The default is the bundle for the PR's latest head; `head` takes a full SHA or a unique prefix of at least 7 characters (409 if it matches more than one). `format=json` returns the bundle bytes exactly as stored, so its hash and signature verify offline; add `download=1` for an attachment. `format=zip` returns `bundle.json`, `verdicts.json` (the matching verdict reports), `review-links.json` (the PR's review-links slice) and `manifest.json` (SHA-256 of each file, plus the signing public key when the bundle is signed and the key is readable). A missing bundle is a 404 whose body has `status: "expired"` and `expired: true` when retention removed it, otherwise `status: "not_found"`. `GET /api/review/evidence/list?repo=&number=` lists every retained bundle id and head for the PR.
- **Aggregate:** `GET /api/evidence/summary` reports read-only aggregates over stored bundles, including per-tier proxy `write_counters` (`{attempted, blocked}`, omitted when the counter file is absent) and per-tier `intent_counters` (below); see [API reference](api-reference.md).
- **Outcome by author kind (panel 1):** `outcomes_by_author_kind` in the summary counts, over the latest bundle per PR in the window, `n`, `merged`, `closed`, `open` and `reverted_14d` (merged and then linked by `reverted_by` within 14 days) for `human`, `agent` and `bot` authors. The Evidence sub-panel draws it as the **Outcome by author kind** panel: one grouped bar per kind for merged / closed / reverted, with `n=` and the merged and reverted rates (open PRs excluded) in the tooltip. `closed` reads 0 until bundles record a close without merge, and a revert only counts once the reverting PR is linked. Reporting only ([#12364](https://github.com/hivecommons/hive/issues/12364)).
- **Intent counters:** every intent evaluation — the merge-eligible lane's `writeIntentVerdicts` pass and the App self-merge sweep's gate, whether or not `intent.enforce` or `intent.shadow` is on — increments an in-memory counter keyed by tier (`tier0`..`tier3`) and writes one audit-log line, action `intent_evaluated`, detail `lane=<merge-eligible|self-merge> tier=<tierN> would_block=<bool> enforce=<bool> [reason=<code>]` ([#12362](https://github.com/hivecommons/hive/issues/12362)). `would_block` is what `BlocksMerge(true)` reports, so with enforcement off it is the shadow answer; reason codes are `tier1_needs_issue`, `tier2_needs_plan`, `tier3_needs_approval`, `alignment_misaligned`, `evidence_unavailable` or `unknown`. The counters flush once a minute (and on shutdown) into `/var/run/hive-metrics/intent-counters.json`, beside the proxy's `write-counters.json` and in the same shape: cumulative `totals` per tier (`attempted` = evaluations, `blocked` = would-block, `blocked_by_reason`) and `weeks` keyed by the UTC Monday of each ISO week. The summary API exposes `totals` as `intent_counters` (`{attempted, blocked}` per tier), omitted when the file is absent. Record-only: the counters are never read back into eligibility.
- **Shadow gates (panel 8):** `shadow_gate` in the summary counts, over the latest bundle per PR in the window, the PRs with any shadow observation (`n`), `would_have_held` (review gate) and `would_have_blocked` (intent), and for each gate (`review_gate`, `intent`) splits the evaluated PRs into `held` and `passed`, each with how they actually turned out: `merged`, `open`, `reverted` (merged and later linked by `reverted_by`) and `closed` (0 until bundles record a close without merge). The Throughput section's Evidence sub-panel draws it as the **Shadow gates** panel, with a zero state while shadow mode is off. Reporting only; nothing reads it to decide a merge ([#12068](https://github.com/hivecommons/hive/issues/12068)).
- **Claim vs verdict (panel 5):** `claim_vs_verdict` in the summary groups the latest bundle per agent-authored PR (`author.kind == "agent"`) by the ISO week it was created and counts `claimed` (every such PR: opening or marking it ready is the agent saying "done"), `ci_not_green` (any captured check whose conclusion is not `success`, as of the merge-eligibility evaluation that captured it), `verdict_not_approve` (any perspective verdict other than `approve`) and `both`, plus `n` and `ci_captured`, the CI column's denominator. A record with no captured checks is disclosed under `missing.records_empty_ci` and never counted as CI agreement. The Evidence sub-panel draws it as the **Claim vs verdict** panel with grouped weekly bars. Reporting only; nothing reads it to decide a merge ([#12365](https://github.com/hivecommons/hive/issues/12365)).
- **Dashboard:** owners get an **Evidence** button on Review Queue rows and Pipeline cards. It opens a modal with the bundle id, head, hash, whether it is signed, the verdict count and merge status, and **Download JSON** / **Download ZIP** buttons.
- **CLI:** `hivectl review evidence <owner/repo#n> [--head <sha>] [-o FILE] [--zip]` prints or saves the bundle, and `hivectl review evidence verify FILE [--pubkey KEY]` checks it offline (see [hivectl](hivectl.md#review--review-evidence)).
- **Export:** `hivectl evidence export [--since <RFC3339|duration>] [--format csv|jsonl] [--repo owner/name] [--out FILE] [--root DIR]` dumps one row per PR straight from the bundles on disk; see [Exporting](#exporting).
- **On the PR:** when a PR merges through any Hive merge path, the merge event is added to the head's bundle (re-signed) and Hive posts one comment:

  ```
  <!-- hive-review-evidence --> Evidence bundle `<id>` sha256:<hash> — download from the dashboard or `hivectl review evidence <owner/repo#n>`.
  ```

  The marker makes it idempotent: a PR gets one such comment, however many sweeps see the merge.

## Exporting

Operators and the study need the raw per-PR evidence off a spoke without scraping `/data/evidence`. `hivectl evidence export` reads the schema v1 bundles under `--root` (default `/data/evidence`, layout `<org>/<repo>/<pr>/<sha>.json`) from the local filesystem — no hive session, no network — and emits one row per pull request from the **newest bundle per PR**, the same rule `GET /api/evidence/summary` uses, so exported counts reconcile with the dashboard Evidence charts ([#12367](https://github.com/hivecommons/hive/issues/12367), RFC [#12062](https://github.com/hivecommons/hive/issues/12062)).

```bash
hivectl evidence export --since 168h --format csv --out evidence.csv   # last 7 days
hivectl evidence export --since 2026-10-01T00:00:00Z --repo hivecommons/hive   # JSON Lines on stdout
```

- `--since` takes an RFC3339 timestamp or a Go duration and keeps bundles whose `created_at` is at or after it; it is applied before the newest-per-PR choice, as the summary API does.
- `--format` is `jsonl` (default, one JSON object per line) or `csv` (header row first). Lists join with `;` in CSV and each verdict is `perspective:verdict:model:backend:confidence`.
- Columns: `repo`, `pr`, `head_sha`, `bundle_id`, `author_login`, `author_kind`, `author_model`, `acmm_level`, `perspectives`, `reviewer_models` (distinct, sorted), `verdicts` (perspective, verdict, model, backend, confidence; sorted by perspective), `ci_conclusion` (`ci.status`), `ci_captured_at`, `outcome` (`merged` or `open`; v1 bundles do not record a close without merge), `merged_at`, `revert_of`, `reverted_by`, `followup_of`, `followed_up_by` (bundle ids), `shadow_review_would_have_held`, `shadow_intent_would_have_blocked` (empty / `null` when the gate was not evaluated), `sentinel_hits`, `signed`, `created_at`, `updated_at`.
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

## What a new push does

A push changes `head_sha`, so earlier verdicts no longer describe the code. Hive starts a new bundle for the new head with `previous_bundle_id` set, and re-reviews. The old bundle is left untouched and remains verifiable.

## Not covered

A bundle shows what Hive recorded. It does not prove who a reviewer's identity is, does not make the operator's policy accept automated review, and is not a compliance claim of any kind.

## Monthly complexity snapshot (report-only)

`pkg/evidence/complexity` writes one maintainability snapshot per repo per
month to `/data/evidence/<org>/<repo>/complexity/<YYYY-MM>.json`: Go
cyclomatic complexity (total, mean, max, functions over a threshold, top
hotspots) plus static-analysis finding counts per tool. An existing month is
never overwritten. It is report-only and never blocks a merge (RFC #12062,
#12072); it feeds the dashboard maintainability panel and the study.

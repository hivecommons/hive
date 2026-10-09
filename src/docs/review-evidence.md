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
| `author` | `login` and `kind` (`human`, `agent` or `bot`). |
| `base_sha`, `head_sha` | The commits the PR compares. The bundle is about `head_sha`. |
| `previous_bundle_id` | The bundle for the previous head of this PR, if any. |
| `policy` | Snapshot of the policy in force (below). |
| `verdicts` | Every reviewer verdict (below). |
| `posted_reviews` | Reviews Hive posted on the PR: `url`, `head`, `at`. |
| `ci` | Check runs for the head: `checks[]` (`name`, `conclusion`, `url`) and `captured_at`. |
| `sentinel` | Why the sentinel sweep flagged the PR, if it did: `rule`, `summary`, `paths`. |
| `human_actions` | Approvals, label changes, hold lifts: `actor`, `kind`, `detail`, `at`. |
| `merge` | Present once merged: `actor`, `method`, `at`, `sha`. Absent before merge. |
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
- When the PR merges, before the merge event is sealed in, Hive captures `ci` from GitHub: the latest check run per check name for the bundle's head, with its conclusion (or its status, such as `in_progress`, if it had not finished) and URL. It also captures `human_actions` from the PR's reviews and issue events: `approval` (an approving review of this head, or any approval since the bundle was created; `detail` is the review URL), `label_added`, `label_removed`, and `hold_lift` (a hold label removed); `detail` is the label name. Label changes count only from the bundle's `created_at`. Bot and App accounts, including Hive's own, are left out; their writes are in the hive audit log. Each capture is best-effort: if GitHub cannot be read, a warning is logged, that field stays empty, and the merge is still recorded. A replayed merge changes nothing.
- A finding reported against the PR as a whole, with no file, is recorded with `path` `(pr)`. A verdict's `confidence` is that perspective's own 0 to 5 review confidence score divided by 5.
- `evidence.enabled: false` stops new writes. Failures are logged and never fail or retry the review itself.

Nothing prunes bundles automatically; they stay until an operator removes them. Reads follow the same retention contract as `/api/runs/audit`: a bundle Hive can prove existed (its PR directory is still there, or a newer bundle's `previous_bundle_id` names it) is reported with an explicit `expired` marker, never as silent absence.

## Downloading

- **API:** `GET /api/review/evidence?repo=<owner/repo>&number=<n>[&head=<sha>][&format=json|zip]` for owner/merger roles. The default is the bundle for the PR's latest head; `head` takes a full SHA or a unique prefix of at least 7 characters (409 if it matches more than one). `format=json` returns the bundle bytes exactly as stored, so its hash and signature verify offline; add `download=1` for an attachment. `format=zip` returns `bundle.json`, `verdicts.json` (the matching verdict reports), `review-links.json` (the PR's review-links slice) and `manifest.json` (SHA-256 of each file, plus the signing public key when the bundle is signed and the key is readable). A missing bundle is a 404 whose body has `status: "expired"` and `expired: true` when retention removed it, otherwise `status: "not_found"`. `GET /api/review/evidence/list?repo=&number=` lists every retained bundle id and head for the PR.
- **Dashboard:** owners get an **Evidence** button on review queue rows and review pipeline cards. It opens a modal with the bundle id, head, hash, whether it is signed, the verdict count and merge status, and **Download JSON** / **Download ZIP** buttons.
- **CLI:** `hivectl review evidence <owner/repo#n> [--head <sha>] [-o FILE] [--zip]` prints or saves the bundle, and `hivectl review evidence verify FILE [--pubkey KEY]` checks it offline (see [hivectl](hivectl.md#review--review-evidence)).
- **On the PR:** when a PR merges through any Hive merge path, the merge event is added to the head's bundle (re-signed) and Hive posts one comment:

  ```
  <!-- hive-review-evidence --> Evidence bundle `<id>` sha256:<hash> — download from the dashboard or `hivectl review evidence <owner/repo#n>`.
  ```

  The marker makes it idempotent: a PR gets one such comment, however many sweeps see the merge.

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

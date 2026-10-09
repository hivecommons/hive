# SOC 2 control mapping for review evidence (not a compliance claim)

**Hive is not SOC 2 certified and makes no claim of SOC 2, FedRAMP, HIPAA or any other compliance or certification** (see [general technical review](general-technical-review.md)). This guide does not change that. It does not assert that using Hive satisfies any control.

## Framing

Hive produces **evidence**: a per-PR [review evidence bundle](review-evidence.md) plus existing artifacts. Whether an automated review counts as peer review, change authorisation or any other control is decided by **your organisation's own change-management policy**, your control owners and your auditor. Hive cannot decide that for you.

> **Status.** The bundle and every surface named below exist today: the relay, sentinel sweep and merge paths write it, and the API, dashboard and CLI read it ([epic #11058](https://github.com/hivecommons/hive/issues/11058)).

## Auditor questions and where the evidence is

Control names are illustrative of common change-management questions; mapping to specific criteria is for your control owners.

| Auditor question | Bundle fields and Hive artifacts | Limits |
|---|---|---|
| Was the change authorised? | `policy` (ACMM level, `require_approval`, `human_merge_paths`), `human_actions` (approvals, hold lifts), `merge.actor` and `merge.method`. | Shows what policy Hive applied and who acted, not that your organisation authorised that policy. |
| Was it reviewed by someone other than the author? | `author.login` and `author.kind` against `verdicts[].model`/`backend`/`perspective`, and `human_actions[].actor`. | Reviewer identity is a model/backend name or a GitHub login; Hive does not prove who a person is. |
| Is the review documented? | `verdicts[]` with `findings[]` (file, line, severity), `posted_reviews[]` links, `hash` and `signature`. | Verifiable as unaltered since sealing, not as correct. See the reviewer accuracy view for calibration. |
| Did tests pass before deploy? | `ci.checks[]` conclusions for `head_sha` and `ci.captured_at`, compared with `merge.at`. | Records the check runs GitHub reported, not the quality of the tests. |
| Is there segregation of duties? | `author`, `verdicts[]`, `human_actions[]` and `merge.actor` are separate recorded identities. | Whether the same person controls several identities is outside Hive's knowledge. |
| Are emergency or out-of-process changes traceable? | `human_actions[]` (hold lifts, label changes), `sentinel[]` findings, `merge` fields, and the [audit log](audit-log.md). | Changes made outside Hive and GitHub do not appear. |
| Did the code change after review? | `head_sha`, `previous_bundle_id` chain, `hash`, `signature`. | A new push creates a new bundle; see [what a new push does](review-evidence.md#what-a-new-push-does). |

Related Hive artifacts: the review verdict artifact and verdict flow in [review swarm](review-swarm.md), the [audit log](audit-log.md), and the [security model](security-model.md).

## Sample policy language

Adapt, and have your own policy owners and counsel approve, before use. This is a template, not advice.

> Code changes to `<systems in scope>` require review before merge. Review may be performed by a human reviewer, or by the automated review system `<system and version>` where all of the following hold: (1) the system's policy configuration (`<ACMM level>`, `<review perspectives>`, `<paths requiring a human>`) is approved by `<role>` and changes to it are themselves reviewed; (2) each merged change has a review evidence bundle whose hash and signature verify against the public key held by `<role>`; (3) required CI checks passed on the merged head commit; (4) changes touching `<sensitive paths>` additionally require approval by a named human; and (5) the evidence is retained for `<period>`. `<Role>` samples `<n>` bundles per `<period>` and reports exceptions.

## What Hive does not cover

- **Reviewer identity assurance.** A bundle names a model, backend or GitHub login. It does not verify that a human reviewer is who they claim.
- **Reviewer eligibility.** Geographic, citizenship or contractual eligibility of reviewers is not modelled.
- **Legal responsibility.** Who is accountable for a decision made by an agent is a matter for your organisation and counsel.
- **Policy authorisation.** Your policy must explicitly authorise automated review; Hive cannot supply that.
- **Evidence integrity beyond the key.** An unsigned bundle, or one signed with a key you do not control, offers weaker assurance. Key custody, rotation and publication are yours.
- **Anything outside the PR flow**, including direct pushes, infrastructure changes and access reviews.

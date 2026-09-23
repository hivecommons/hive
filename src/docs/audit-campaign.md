# Report-only audit campaign pilot

The audit campaign pilot proves the runs three-gate model on a workload that never opens a pull request. It inspects a pinned in-repo scope, validates findings, deduplicates them, records receipts, and leaves publication off.

## Fixture

The fixture scope lives at `src/pkg/convergence/testdata/audit-scope/components.json`. Run it through the package tests:

```sh
cd src && go test ./pkg/convergence ./pkg/convergence/proof ./pkg/retro -count=1
```

The test clears `HIVE_GITHUB_TOKEN` and uses a fake GitHub client that must remain at zero calls. The pilot refuses to run if `HIVE_GITHUB_TOKEN` is present.

## State model

Campaign state is stored as ordinary beads:

- one `campaign` bead for the pinned scope;
- one `inspection` bead per component, with state `pending`, `inspected`, or `Unknown` when the mutation journal cannot prove whether a crash-window effect completed;
- one `finding` bead per candidate finding, with state `validated`, `rejected`, or `duplicate_of`;
- publication remains `none` for this slice.

No new store, CRD, DSL, or GitHub credential path is introduced.

## Shadow execution and receipts

Each component inspection uses the existing mutation executor in `shadow` mode with effect kind `hive.record-finding/v1`. The logical operation ID is derived from the campaign, component, and content hash, so rerunning unchanged content reuses the same journal entry instead of creating another finding.

Each inspected component emits a `stage-receipt/v1` `StageReceipt` with result class `completed`. The proof predicate `hive.inspection.recorded/v1` binds the inspection bead ID and receipt digest to producer `hive-audit-lane`.

## Burndown

Burndown reports completed inspection obligations, known remaining work, and unknown evidence. If any component is `Unknown`, the satisfied count is `null` with a reason; the pilot never substitutes a percentage or guessed number.

## Publication

Publication is the third gate. Verification and permission to publish are different decisions: a validated finding never implies a public issue, because hive's policy requires private security disclosure. The publisher in `src/pkg/convergence/publish` is the single trusted publication effect.

### Turning it on

Publication is off by default. It files only when every one of these holds:

```yaml
publication:
  enabled: true                       # default false
  owner: maintainer-login             # the campaign owner every publication runs under
  private_channel: "repo:org/security-intake"   # or "notify"
```

- `publication.enabled` is the operator opt-in. The Features panel exposes it (owner-only) together with the owner and private channel fields.
- The hive's ACMM level must be at least L3. Below L3 no agent holds issue-writing authority, so the publisher refuses with `ErrLevelBelowFloor` and writes a `finding_publication_refused` audit entry.
- The convergence mode must be `enforce`. In `shadow` and `off` the publisher records `withheld:mode` and performs no forge write at all; the test `TestAuditPublishDisabledAndShadowPerformNoGitHubWrites` runs the campaign against an httptest GitHub that fails on any write.
- `publication.owner` names the maintainer principal the publisher acts as. It is the actor on the outcome ledger and in the audit trail; publication stays off until it is set.

### What one publication does

Each validated finding is one `github.create-issue/v1` effect executed through `mutation.Executor` under the campaign's claim. The logical operation ID derives from the campaign key, the finding hash, and the channel, never from the holder, epoch, or attempt, so:

- rerunning the campaign replays the recorded issue instead of filing a second one;
- a crash between intent and acknowledgment leaves the operation `Unknown`; the next run reconciles it by looking the issue up by its marker before any retry;
- an open issue that already carries the marker records `existing:<n>` and files nothing.

The published body carries the evidence, the cited files, the inspection receipt digest, a link to the run, the marker `<!-- hive-finding: <hash> -->`, and a `Hive-Run: <run key>` trailer. A `hive.finding.published/v1` proof binds the issue number and finding hash.

### Private disclosure

A named classifier (`publish.ConservativeClassifier`, replaceable) reads the finding's predicate, labels, and title. Any security vocabulary marks the finding sensitive, and a sensitive finding goes only to the private channel:

| `private_channel` | Where the finding goes |
|---|---|
| `repo:owner/name` | An issue in that private repository, through the same forge seam, with the full evidence. |
| `notify` | The operator notifier, carrying only the title and finding hash. |
| unset | Refused: `ErrNoPrivateChannel`, audited as `finding_publication_refused`, nothing filed anywhere. |

The publication bead records `private:<ref>` without the finding body.

### Outcome ledger

The publisher books the campaign's predicted end state, one issue per validated public finding, one disclosure per sensitive finding, nothing for duplicates and rejections, as an `outcome.Record` (`default/<repo>@audit-<campaign>`). The prediction is created at generation 1, superseded when the finding set changes, and accepted only when the actual publications satisfy it. "Issue filed" (the mutation journal) and "repository outcome satisfied" (the outcome ledger) therefore stay different statuses; the publication bead carries no extra status field for this.

### Publication records

Finding beads carry `publication_state` with one of `published:<n>`, `existing:<n>`, `private:<ref>`, `withheld:disabled`, `withheld:mode`, `refused:level`, `refused:no-private-channel`, or `none`. The campaign's publication bead summarises the counts, for example `none=1 private=1 published=1`.

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

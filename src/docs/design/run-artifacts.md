# Run artifacts

Long-running runs keep review state in the existing plan and lease artifacts.
They do not introduce a new store, CRD, DSL, or GitHub credential path.

## Multi-repo waves

A plan may declare a repository, repository role, and wave on each child task
using bracket annotations such as `[repo:owner/repo] [role:service] [wave:1]`.
Hive stores those annotations as child metadata and adds a wave barrier: children
in the next wave depend on all children in the previous wave, so the next wave is
not offerable until the prior wave has finalized or been explicitly skipped.

The stage runner reads the Spektacular status verb, refuses fan-out when the hub
constellation overlap index reports two spokes claiming one repository, and then
creates one implementation lease per participating repository for the wave. Each
lease is keyed by the run key and wave number and remains subject to the existing
role, capability, and ioscan guard invariant.

Partial failure uses forward-fix by default. A failed repository's lease ends
without `document_status: final` and enters the normal stale/human handling path.
The run remains open with `waiting_on=human`; repositories that already merged
are not reverted. The run detail groups all PRs for a wave under one header and
offers one approve action for the plan wave rather than per-PR approvals.

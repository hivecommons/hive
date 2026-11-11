# CNCF TechDocs AI-assisted assessment — curated workflow model

`cncf-techdocs-assessment@curated-2026.11.1` is the first curated model in the hub
marketplace (https://hive.hivecommons.dev → Settings → Workflows → Marketplace on any
hive). It encodes the CNCF TechDocs team's AI-assisted documentation assessment process
so a hive bound to `cncf/techdocs` runs intake, drafting, verification, stakeholder review
and approval as a visible lifecycle, with every agent action followed by a human gate.

Source of truth (the model cites both by SHA in its `workflow.hivecommons.dev/source*`
annotations and in each element's `citations`):

- Specification: [`specs/ai-assisted-techdocs-assessment-spec.md`](https://github.com/nate-double-u/cncf-techdocs/blob/ai-assisted-assessment-spec/specs/ai-assisted-techdocs-assessment-spec.md)
  at `nate-double-u/cncf-techdocs@882be6fef28794433bc3a6a34ac1401391c1d3e4`.
- Intake template: [`.github/ISSUE_TEMPLATE/assessment-request.yml`](https://github.com/nate-double-u/cncf-techdocs/blob/ad915303f9774b9aa6b7646969897fada7975a00/.github/ISSUE_TEMPLATE/assessment-request.yml)
  at `nate-double-u/cncf-techdocs@ad915303f9774b9aa6b7646969897fada7975a00`.
- Checks and labels: the `cncf/techdocs` `.github` tree (format, link and spell-check
  workflows; `settings.yml` labels), as snapshotted in
  `pkg/workflowmodel/ingest/testdata/techdocs/`.

The model's scope is `github.com cncf/techdocs` — the upstream repository where the
process runs; the fork above is where the spec was written. Model file:
`pkg/hub/marketplace/curated/cncf-techdocs-assessment.yaml`
(`apiVersion: workflow.hivecommons.dev/v1alpha1`, `derivation.mode: rules`,
`meta.provenance: curated`). The same repository is also in the ingest corpus
(`pkg/workflowmodel/ingest/corpus/extra.json`), so an `ingested-unreviewed` version built
from the pipeline files alone sits next to this one in the catalogue.

## How the spec maps onto the model

The spec (§5 Lifecycle, §6 Phases, §12 Lifecycle bindings) describes one six-step loop —
request → accept → draft → review → stakeholder review → merge — run once per phase
(A analysis, B implementation plan, C issue backlog). The model encodes the loop once and
carries the phase as a label (`phase/assessment`, `phase/implementation`,
`phase/backlog`; `skip-implementation` makes the A merge open C). Each phase's tracking
issue and the intake issue itself travel the issue states; each AI-drafted deliverable is
a pull request that travels the PR states.

### Columns

| Column | States | Spec |
|---|---|---|
| Requested | `requested` | §5 step 1, §12 Request |
| Accepted | `accepted`, `escalated` | §5 step 2, §12 Accept; §5 failure path |
| Drafting | `draft-open` (issue), `drafting` (PR) | §5 step 3, §12 Draft |
| Review | `in-review`, `checks-failing` | §5 step 4, §12 Review |
| Stakeholders | `stakeholder-review` | §5 step 5, §12 Stakeholder review |
| Approval | `awaiting-merge` | §5 step 6, §12 Merge |
| Done (terminal) | `merged`, `closed`, `declined`, `discarded` | §12 Merge / Failure and abort |

### States and rules (first match wins)

Issues (intake and per-phase tracking issues):

| State | Stage | Rule | Owner |
|---|---|---|---|
| `declined` | terminal | closed and `triage/declined` | — |
| `closed` | terminal | closed | — |
| `escalated` | 45 needs-decision | a hold label is present | administrator |
| `draft-open` | 40 covered-by-PR | `triage/accepted` and a covering PR exists | registered agent |
| `accepted` | 20 claimed | `triage/accepted` | reviewer (technical writer) |
| `requested` | 10 actionable | `needs-triage` | reviewer |

Pull requests (deliverables carry the `Docs analysis` label; anything else abstains):

| State | Stage | Rule |
|---|---|---|
| `merged` | terminal | merged |
| `discarded` | terminal | closed unmerged |
| `in-review` | 50 review-pending | draft and `verified` |
| `drafting` | 70 fixing | draft |
| `checks-failing` | 71 needs-author | checks red |
| `awaiting-merge` | 90 ready-for-human-merge | `confirmed` |
| `stakeholder-review` | 60 reviewed | ready and `verified` |
| `in-review` | 50 review-pending | ready but not yet `verified` |

### Transitions

| From | To | Event | Gate (who) | Spec |
|---|---|---|---|---|
| requested | accepted | `accepted` | `/accept` (reviewer) | §12 Accept |
| requested | declined | `declined` | `/decline` (reviewer) | §12 Accept |
| requested | closed | `closed` | — | withdrawn |
| accepted | draft-open | `draft-opened` | — (drafter opens the PR) | §12 Draft |
| accepted | escalated | `held` | — | §5 failure path |
| accepted | closed | `closed` | — | — |
| draft-open | accepted | `draft-withdrawn` | — | restart / hand-write / stop |
| draft-open | escalated | `held` | — | §5 failure path |
| draft-open | closed | `closed` | — | phase merged or aborted |
| escalated | accepted | `released` | — (administrator) | — |
| escalated | closed | `closed` | `/abort` (administrator) | §12 Failure and abort |
| drafting | in-review | `verified` | `/verify` (reviewer; needs `verified`) | §12 Review |
| in-review | stakeholder-review | `ready` | `/ready` (reviewer; needs `verified` + green checks) | §12 Stakeholder review |
| stakeholder-review | awaiting-merge | `confirmed` | `/confirm` (stakeholder) | §12 Stakeholder review |
| stakeholder-review / awaiting-merge | checks-failing | `checks-red` | — | — |
| checks-failing | stakeholder-review | `checks-green` | — | — |
| awaiting-merge | merged | `merged` | `/approve` (approver; needs `confirmed` + green) | §5 step 6, §12 Merge |
| drafting / in-review / checks-failing / stakeholder-review / awaiting-merge | discarded | `closed` | `/discard` (reviewer) | — |

Every gate is released by a human role; the two agent profiles (`techdocs-drafter` in
`accepted`/`drafting`, `techdocs-verifier` in `drafting`) can only move work up to a gate,
never through one — the compiler's `agent-no-human-gate` refusal enforces this.

### Roles

| Role | Selector | Spec |
|---|---|---|
| requester | issue author (human) | §4 |
| reviewer | repository write permission (technical writer who `/accept`ed) | §4 |
| approver | repository write permission | §4 |
| stakeholder | set frozen at `/accept` from the intake's validated contacts | §4, §12 |
| administrator | repository admin | §4, §12 Failure and abort |
| drafter, verifier | the hive bot (non-human) | §4, §6 |

### Labels

From `settings.yml`: `Docs analysis`. Proposed for the assessment: `needs-triage`,
`triage/accepted`, `triage/declined`, `phase/assessment`, `phase/implementation`,
`phase/backlog`, `verified`, `confirmed`, `skip-implementation`.

### Checks

Required CI: format-check, link-check, spell-check (the three `cncf/techdocs` workflows,
ingested as pipelines with their source hashes). Required manual: provenance block present
and the strict check passes (spec §15).

### Intake

`assessment-request.yml` fields map to facts as recorded in the
`workflow.hivecommons.dev/intake-fields` annotation: project name, maturity and
moving-levels → assessment metadata; website/docs/repo/other-scope/out-of-scope → scope;
domains → the agent's egress allowlist; contacts → the frozen stakeholder set (validated
at `/accept`); tooling/context → drafter input; the HC-6 disclosure acknowledgement is
required.

## Not representable today

These parts of the spec are documented in the model's annotations but not enforced by
the engine:

- Close *reason* (completed vs. not planned) — no fact distinguishes them.
- Approver independence from the drafter and reviewer — no fact records who drafted or
  reviewed.
- Phase as a state dimension and the A→C skip — carried by labels; the
  `open-next-phase-tracking-issue`, `notify-contacts` and `mention-stakeholders`
  bookkeeping actions are declarative only.
- `/verify` dispatching the verifier agent and membership of the frozen stakeholder set
  — the gates exist, the side effects do not yet.
- Data-collection scripts and the strict provenance check — a manual check.

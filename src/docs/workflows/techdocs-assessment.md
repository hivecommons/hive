# CNCF TechDocs AI-assisted assessment — curated workflow model

`cncf-techdocs-assessment@curated-2026.11.2` is the current version of the first curated
model in the hub marketplace (https://hive.hivecommons.dev → Settings → Workflows →
Marketplace on any hive); `curated-2026.11.1` stays in the catalogue unchanged. It encodes the CNCF TechDocs team's AI-assisted documentation assessment process
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
process runs; the fork above is where the spec was written. Model files:
`pkg/hub/marketplace/curated/cncf-techdocs-assessment-2026.11.2.yaml` (current) and
`pkg/hub/marketplace/curated/cncf-techdocs-assessment.yaml` (`curated-2026.11.1`)
(`apiVersion: workflow.hivecommons.dev/v1alpha1`, `derivation.mode: rules`,
`meta.provenance: curated`). Curated files are never edited in place: a change is a new
file with a new `meta.version`, and the catalogue keeps every version. The same repository is also in the ingest corpus
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

The `accepted` state reuses the built-in stage number 20, so an accepted issue wears
`stage/020-claimed`, but **no agent holds a claim on it**: the state means "waiting on the
reviewer to start the drafter". The in-progress sweep leaves that label alone on a
model-bound repository and does not withhold the issue for it
([hivecommons/hive#18434](https://github.com/hivecommons/hive/issues/18434); before the fix
the label was removed and re-added every enumeration cycle).

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

#### Reference repos from the request

`curated-2026.11.2` adds a `spec.intake` block
([hivecommons/hive#18139](https://github.com/hivecommons/hive/issues/18139)):

```yaml
intake:
  referenceRepoFields:
    - "Website or documentation repository"
    - "Other repositories or sites in scope"
  outOfScopeFields:
    - "Out of scope"
```

The entries are the rendered `### <label>` headings of `assessment-request.yml` as they
appear in a request issue's body, not the form's field ids. A hive bound to this version
derives one write-tier (`issues+prs`) `project.reference_repos` entry per repository named
under a `referenceRepoFields` heading and not under an `outOfScopeFields` heading, once a
maintainer with write access to `cncf/techdocs` applies `triage/accepted` to the request.
The derived entries carry the request as provenance (`cncf/techdocs#395` style), are
rebuilt on every config load and never written to the overlay or runtime config, and
disappear when the issue closes or the label is removed. `curated-2026.11.1` is unchanged
and declares no intake fields, so a hive that stays bound to it derives nothing.

#### Operator walkthrough: the Hive Commons pilot

The first request through this path is
[cncf/techdocs#395](https://github.com/cncf/techdocs/issues/395), the Hive Commons assessment.
What the operator of the TechDocs hive (authorized repo `cncf/techdocs`) does and sees:

1. **Rebind to 11.2.** Settings → Workflows → Marketplace, bind `cncf/techdocs` to
   `cncf-techdocs-assessment@curated-2026.11.2`. This step is required: a hive still bound to
   `curated-2026.11.1` derives nothing, however many requests are accepted. No
   `reference_repos` entry is written by hand.
2. **A maintainer accepts the request.** Someone with write access to `cncf/techdocs` applies
   `triage/accepted` to #395. A label applied by a bot grants nothing.
3. **Three derived entries appear** on the next pass (boot, config reload or every
   `HIVE_REFERENCE_REPOS_DERIVE_INTERVAL`, default `10m`): `hivecommons/docs`,
   `hivecommons/hivecommons.github.io` and `hivecommons/hive`, each at the `issues+prs` tier with
   `derived_from: cncf/techdocs#395`. The kick's `📚 REFERENCE REPOS` block lists them with
   `(from cncf/techdocs#395)`. Settings → Reference repos shows them read-only with the source
   link. The `credential_state` depends on where **this hive's** GitHub App is installed:
   - **App installed on the reference repos** (the Hive Commons hive itself, whose App is on
     every hivecommons repo): `credential_state: write-tier`, and the hive may do what step 4
     lists.
   - **App installed only on `cncf/techdocs`** (a contributor's or the TechDocs team's own
     hive): the entries read `read-only` with a reason, #395 carries one
     `hive:reference-repos-read-only` comment, and the hive keeps running. The App cannot be
     installed in an organization the hive does not own, so nothing in the hive turns this into
     write access on its own. The drafter can still clone and read the repos (public repos need
     no credential) and the deliverable still lands in `cncf/techdocs`, which is all Phase A
     needs. Step 4's issue, PR and branch actions are not available there until the
     reference repos' owner grants write access.
4. **What the hive may do there (write tier only).** Clone and read the three repos, read their issues and PRs,
   open an issue, open a PR or push a topic branch, and comment on an issue or PR it opened
   itself. The assessment deliverable still lives in `cncf/techdocs`.
5. **What it may not do.** Merge, push to a default branch, label or assign other people's
   items, review, or close other people's issues. The proxy refuses each of these and logs
   `reference_repo` in the violations log. `hivecommons` repos get no queue, no ACMM evaluation
   and no work items.
6. **Closing #395 removes them.** When the request issue closes, or `triage/accepted` is
   removed, the three entries, their proxy policies and their kick lines are gone on the next
   pass. Nothing was ever written to the overlay or runtime config, so there is nothing to clean
   up. A hand-written entry for one of the repos, if the operator adds one, takes precedence and
   stays.

## Starting the drafter (Accepted → Draft)

The hive hands the phase to its agents itself. The model grants `techdocs-drafter` the
`accepted` and `drafting` states and `techdocs-verifier` the `drafting` state; the compiler
resolves where each takes over ([Agent dispatch](../design/workflow-models.md#agent-dispatch)):

| State | Profile | Starts when |
|-------|---------|-------------|
| Accepted (issue) | `techdocs-drafter` | `triage/accepted` is on the issue and no PR covers it |
| Drafting (draft PR) | `techdocs-verifier` | a person with write access comments `/verify` on the draft PR |

On each pass the hive:

1. withholds every item in those states from the generic lanes, so the scanner or a feature
   agent never picks up an accepted request before the drafter does;
2. resolves the hive agent that fills the profile (`workflow.dispatch.profiles.<profile>.agent`;
   unset: an agent named after the profile, else the first enabled `writer`, `docs` or
   `feature` agent by name);
3. for the issue, takes the agent claim on the hive's claim ledger first; a claim someone
   else holds (a person working the phase by hand) stops the hand-off;
4. kicks that agent once with the issue, the state's guidance and the deliverable the model
   requires: a draft PR in `cncf/techdocs` labeled `Docs analysis` plus the issue's phase
   label, named in the PR request (`hive-open-pr --issues N`) and referenced with `Refs #N`,
   not marked ready and without the `verified` / `confirmed` gate labels;
5. records the hand-off in `workflow-dispatch.json` under the workflow store root when the
   kick lands, so neither a later pass nor a restart repeats it. A kick that could not be
   delivered (busy, paused, failed) is retried on a later pass.

The verifier runs once per `/verify` comment: each new `/verify` from the reviewer is a new
pass, posting its report as a PR comment. A `/hive verify` command is the model-reply path
instead: it applies `verified` directly, which is how a reviewer records a deliberate skip.

To use it, the hive needs `workflow.enabled: true` and `cncf/techdocs` bound to
`cncf-techdocs-assessment`; dispatch is on by default (`workflow.dispatch.enabled`). Pin the
agents if the default pick is wrong:

```yaml
workflow:
  enabled: true
  dispatch:
    profiles:
      techdocs-drafter: {agent: writer}
      techdocs-verifier: {agent: reviewer}
```

`workflow.dispatch.profiles.<profile>.disabled: true` stops one profile's automatic
hand-off, and `workflow.dispatch.enabled: false` stops them all (owned items stay withheld
from the generic lanes either way). `prompt_template` replaces the built-in prompt; see the
design doc for its `${VAR}` placeholders.

### Starting the drafter by hand (fallback)

When no agent fills the profile, dispatch is off, or a discarded draft should be restarted (a
hand-off happens once per issue and state: retries are a person's choice, spec §5), a hive
owner starts the draft by hand:

1. Check the request is accepted: `triage/accepted` is on the issue, applied by a maintainer
   with write access to `cncf/techdocs`, and the hive's workflow column shows **Accepted**.
2. On the hive dashboard, signed in with the **owner** role (kicks with a prompt are
   owner-only), open the agent card of an agent that may work on `cncf/techdocs` and type the
   drafting instruction into its **Send a message to …** box, then **Send**. Name the issue,
   the phase and the deliverable, for example:

   ```text
   Draft the Phase A (assessment) deliverable for cncf/techdocs#395 following the CNCF
   TechDocs assessment spec (§6 Phase A, §14 Assessment drafter, §15 provenance block).
   Read the request issue for the project, scope and contacts. Work on a topic branch in
   cncf/techdocs, open a DRAFT pull request labeled `Docs analysis` and `phase/assessment`,
   name the issue in the PR request (`hive-open-pr --issues 395`) and reference it in
   the body with `Refs #395` (not a closing keyword: the intake closes at the final
   phase). Do not mark it ready and do not apply `verified`.
   ```

3. Once the draft PR is open and the PR request named the issue (a `Refs` line alone in the
   body does not cover it; a declared issue does), the next pass moves the issue to
   `draft-open` (stage 40) and the PR to `drafting`. From there every step is a human gate:
   the reviewer runs or skips the verifier (`/verify`, the `verified` label), marks the PR
   ready (`/ready`), stakeholders `/confirm`, and an independent approver merges.

## Not representable today

These parts of the spec are documented in the model's annotations but not enforced by
the engine:

- Close *reason* (completed vs. not planned) — no fact distinguishes them.
- Approver independence from the drafter and reviewer — no fact records who drafted or
  reviewed.
- Phase as a state dimension and the A→C skip — carried by labels; the
  `open-next-phase-tracking-issue`, `notify-contacts` and `mention-stakeholders`
  bookkeeping actions are declarative only.
- Membership of the frozen stakeholder set — the gate exists, the check does not yet.
- Data-collection scripts and the strict provenance check — a manual check.

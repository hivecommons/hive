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
2. resolves the hive agent that fills the profile, in the order listed under
   [Which agent fills a profile](#which-agent-fills-a-profile);
3. for the issue, takes the agent claim on the hive's claim ledger first; a claim someone
   else holds (a person working the phase by hand) stops the hand-off;
4. kicks that agent once with the issue, the state's guidance and the deliverable the model
   requires: a draft PR in `cncf/techdocs` labeled `Docs analysis` plus the issue's phase
   label, named in the PR request (`hive-open-pr --issues N`) and referenced with `Refs #N`,
   not marked ready and without the `verified` / `confirmed` gate labels;
5. records the hand-off in `workflow-dispatch.json` under the workflow store root when the
   kick lands, as pending, so neither a later pass nor a restart repeats it. A kick that
   could not be delivered (busy, paused, failed) is retried on a later pass;
6. checks on later passes that the hand-off actually started: see
   [When the agent does not start](#when-the-agent-does-not-start).

The verifier runs once per `/verify` comment: each new `/verify` from the reviewer is a new
pass, posting its report as a PR comment. A `/hive verify` command is the model-reply path
instead: it applies `verified` directly, which is how a reviewer records a deliberate skip.

### Which agent fills a profile

The model's grant describes the profile's work (its tools and outputs). It does not give a
hive agent any permission: an agent acts with the mode its own configuration gives it
(`mode:` on the agent, else the default for its name at the hive's ACMM level), and the hive's
write gates enforce that mode. So the hive only ever hands a profile to an agent that already
may do the work:

- `techdocs-drafter` opens a draft pull request and pushes. Its agent needs a mode that may
  open pull requests (`ISSUES_AND_PRS` or `ISSUES_PRS_MERGE`), in a repository whose ACMM
  level lets that agent open them;
- `techdocs-verifier` posts one comment. Its agent needs comment rights (`ISSUES_ONLY` or
  above, or `converse: true`).

The first of these that names an enabled agent with those rights is the one kicked (a replica
is never chosen: its base agent is):

1. the pin, `workflow.dispatch.profiles.<profile>.agent`. A pin that names no enabled agent,
   or an agent without the rights the profile needs, stops the hand-off and is shown as a
   problem; it never falls through to another agent;
2. an agent named after the profile (`techdocs-drafter`);
3. the first `writer`, `docs` or `feature` agent, in that role order, by name;
4. the hive's standard agent for the work, so a default fleet needs no setup: for a profile
   that opens a pull request (`techdocs-drafter`) the issue worker, the `scanner` role that
   every issue without a lane of its own goes to; for a profile that reviews
   (`techdocs-verifier`) the `reviewer`; and when that agent lacks the rights, the first
   agent by name that has them. It must be allowed to work on the bound repository (its
   `repos:` scope, when it has one, includes `cncf/techdocs`).

On a hive whose standard roster (scanner, reviewer, ci-maintainer, quality, guide,
brainstorm, supervisor) all may open pull requests, the drafter is therefore the scanner and
the verifier the reviewer. On a hive where the scanner runs `ADVISORY` and only `quality`
runs `ISSUES_AND_PRS`, the drafter is `quality`. When no agent has the rights, the profile is
unfilled and the reason says so:
`no agent on this hive may open pull requests in cncf/techdocs; agents are advisory-only`.
Raise an agent's mode (Settings → Agents, or `mode: ISSUES_AND_PRS` on the agent) to fill
it; the hive never raises a mode for a hand-off on its own.

When nothing fills a profile the hand-off cannot happen, and the item stays withheld from the
generic lanes, so the hive says so in three places:

- **Settings → Repos**: the repository's workflow row lists each profile of the bound model
  with the agent it is handed to. A profile nothing fills shows a warning naming it, and
  the picker next to it saves the pin (owner only, `PUT /api/workflows/dispatch/profiles`,
  audited `workflow_dispatch_agent`). The picker lists every agent; one whose mode does not
  let it do the profile's work is shown with its mode, cannot be picked, and gives the
  reason on hover. **Automatic** clears the pin. The next pass uses the pick; no restart is
  needed.
- **Activity Monitor**: the item's row wears a `no agent for <profile>` badge whose hover
  gives the reason.
- **Send to *profile***: the manual send answers with the same reason and where to fix it.

The hive log repeats it on each pass (`workflow dispatch: item not handed off`, with the
profile and the reason).

To use the workflow, the hive needs `workflow.enabled: true` and `cncf/techdocs` bound to
`cncf-techdocs-assessment`; dispatch is on by default (`workflow.dispatch.enabled`). Pin the
agents, here or with the picker, if the default pick is wrong:

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

### Assessment criteria and reviewer rules

The assessment rubric lives in the TechDocs repository's
[`docs/analysis/criteria.md`](https://github.com/cncf/techdocs/blob/main/docs/analysis/criteria.md)
and its assessment specification (linked above). The curated workflow model defines
lifecycle states, human gates and drafter/verifier profiles, not a second scoring
rubric. Hive selects an eligible agent (which may be `quality`, as described above)
and builds its instructions with `scheduler.BuildWorkflowDispatchPrompt` or the
profile's configured `prompt_template`.

Reviewer-stated corrections belong in the shared knowledge store as small,
source-linked entries, not only in a reply or the agent's private memory. Scope
assessment rules to the **workflow model name**, independent of its version and
of which agent fills the profile:

```yaml
title: cncf-techdocs-assessment reviewer rule for <criterion>
type: gotcha
layer: project
state: approved
tags: [repo:cncf/techdocs, techdocs, assessment, reviewer-rule]
source: https://github.com/cncf/techdocs/pull/<pr>#discussion_r<comment-id>
scope:
  lanes: [cncf-techdocs-assessment]
```

This is frontmatter for a [knowledge entry](../knowledge-entries.md); follow it
with the verified rule and its project-specific limits. Preserve the originating
comment URL/id when capturing a rule. Draft rules remain excluded until approved.
Do not use `lanes: [quality]` alone: that would teach unrelated quality work an
assessment-specific rule and miss a drafter assigned to a different agent.

Workflow hand-offs run the knowledge primer before delivery. The bound model's
unversioned name is an additional lane match and search term, alongside the base
agent lane and item labels. The usual agent knowledge scope, lifecycle filter,
relevance selection and byte cap still apply. A rule must be in a registered
vault/git source and fit the primer budget to reach the next kick. See
[registering repository knowledge](../knowledge-entries.md#how-the-directory-reaches-the-primer).

The initial reviewer rules from `cncf/techdocs#424` live in
[`techdocs-assessment-evidence.md`](../knowledge/techdocs-assessment-evidence.md)
and [`techdocs-assessment-ownership.md`](../knowledge/techdocs-assessment-ownership.md).
They cover evidence-based readiness grading on the documentation domain and the
project's authoritative maintainer roster. They supplement rather than replace
the upstream rubric. Automated review-feedback capture and reply verification
are tracked separately in hivecommons/hive#19048; this injection path also accepts
subsequent captured entries with the same scope.

### When the agent does not start

A kick that landed only means the agent was told. An agent can decline (one whose session
may not push will, and is right to) or end its turn without producing anything. The hive
does not count that as a hand-off made.

After the kick, each pass checks the item while it is still in the handed-off state. The
hand-off is marked **not started**, with the reason, when:

- the agent declined: it refused the kick, or ended its turn on a `STAND DOWN` line, on a
  question, or on a report that it produced nothing (the prompt tells an agent whose session
  cannot do the work to end on a `STAND DOWN` line with the reason, and, for a deliverable
  that is a pull request, spells out the shell commands: `git` and `hive-open-pr` are commands,
  not tools in the CLI's tool list, and a stand-down is for a command the hive refused, not for
  a tool list with no entry of that name, [#18777](https://github.com/hivecommons/hive/issues/18777));
- the agent's turn ended and, 15 minutes later, there is still no draft pull request
  covering the issue (for the verifier: no new comment on the draft);
- nothing is known about the turn (the hive restarted) and 2 hours have passed.

The hive then releases the claim that agent held and hands the item, once, to a different
agent that has the rights. If no other agent has them, or the second agent does not start
either, the item waits for an owner: use **Send to techdocs-drafter**. There is no further
automatic retry, so an agent that keeps declining cannot loop.

A hand-off that was not started is visible while the item waits:

- **Settings → Repos**: the profile's line says which item, why, and what happens next;
- **Activity Monitor**: the row wears a `not started: <profile>` badge whose hover gives the
  reason;
- the hive log: `workflow dispatch: hand-off not started`, with the agent, the reason and
  the next step.

**After an upgrade.** A hive that ran an earlier version may hold a record saying an Accepted
request was already handed off, to an agent that could never open the pull request (an
advisory scanner). On the first pass after the upgrade that record is re-evaluated: it is
marked not started, the agent's claim is released, and the request is handed to an agent
that has the rights. Nobody needs to find the item or clear anything by hand. If no agent
has the rights, the row and the workflow line say so until one does.

### Send to drafter (owner)

On the Activity Monitor, the **⋯** menu of a `cncf/techdocs` row in a dispatched state offers
**Send to techdocs-drafter** (an Accepted issue) or **Send to techdocs-verifier** (a draft PR in
Drafting). It is owner-only and runs the same hand-off on demand: it reads the item fresh,
resolves the agent (a profile set `disabled` for automatic dispatch still resolves), takes the
claim, kicks the agent and records the hand-off (`POST /api/workflows/dispatch`, audited
`workflow_dispatch`). Use it to restart after a discarded draft (a hand-off happens once per
issue and state on its own: retries are a person's choice, spec §5) or to run the verifier
without a `/verify` comment.

### Starting the drafter by hand (fallback)

When no agent fills the profile (pick one first: see
[Which agent fills a profile](#which-agent-fills-a-profile)) or the hive predates automatic
dispatch, a hive owner starts the draft by hand:

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

## Independent fact-check before a person reviews (optional)

The first review of a drafted deliverable can be done by a second agent instead of the
reviewer's own pass over the draft. The hive's opt-in fact-check pass
(`review.analysis_factcheck`, [Independent fact-check for analysis PRs](../review-swarm.md#independent-fact-check-for-analysis-prs-reviewanalysis_factcheck))
runs a different agent over a draft `Docs analysis` pull request after the drafter opens it
and before anyone marks it ready. It re-verifies each claim, score and attribution against
the cited source, checks the judgments for skew in both directions, checks that the material
rated was actually inspected, and checks people and roles against the project's maintainers
file. Its findings go back to the drafter, which revises the same branch, for a bounded
number of rounds.

It is off by default and the model does not turn it on: the qualifying agents and labels are
the operator's choice in `hive.yaml`. There is no Settings control for this block. See
[Editing `hive.yaml`](../editing-hive-yaml.md) for where to make the change and how to
apply it, including what to ask for if you only have the hosted dashboard. For a hive
bound to `cncf/techdocs`, for example:

```yaml
review:
  analysis_factcheck:
    enabled: true
    author_agents: [quality]     # the agent that fills the techdocs-drafter profile
    labels: [Docs analysis]      # the deliverable label this model uses
    reviewer_agents: [reviewer]  # must be a different agent or model from the drafter
```

How it fits the lifecycle above:

- The PR stays in `drafting` while the pass runs. The pass never applies `verified`, never
  marks the PR ready on the default `ready_policy: human`, and never merges; `/verify`,
  `/ready`, `/confirm` and `/approve` stay human gates.
- It is separate from the `techdocs-verifier` profile and the `verified` label. The pass
  gives the reviewer a PR that already carries an independent review.
- It stops as soon as a person reviews the PR, so a reviewer's own comments reach the
  drafter through the review-feedback sweep instead.

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

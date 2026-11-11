# Workflow models

**Status: phase 1 implemented** (ingest, model, renderers and the proving harness —
[#17620](https://github.com/hivecommons/hive/issues/17620)); phases 2–4 (editor, stage-engine
binding, marketplace) are design only. Lands on the `v6` branch; the v5 and v6 lifecycles are
untouched until phase 2 opts a hive in.

## What this is

A deterministic reading of how a repository already delivers software — its GitHub Actions
workflows and its Tekton / OpenShift Pipelines resources — expressed as one model
(`workflow.hivecommons.dev/v1alpha1`) that hive can render, diff, store and, later, run as a
hive's lifecycle. No LLM is involved in the ingest: two YAML definitions that mean the same thing
produce the same content hash.

The model has two layers:

- **Process graph** — the states an issue moves through (open → actionable → claimed → PR in
  progress → review ∥ CI → merge queue → merged, then optionally main CI → release → deploy →
  done), with guards on the transitions. With no pipelines bound this is exactly the hive default
  (`HiveDefault` in `pkg/workflowmodel/defaults.go`) and renders today's Activity Monitor columns.
- **Pipelines** — each workflow or Pipeline as an execution DAG (jobs / tasks / reusable
  workflows as nodes, `needs` / `runAfter` / result references as edges), with matrix,
  concurrency, environment, `finally` and `when` recorded on the nodes and the triggers that
  start it.

`Bind` joins them: a pipeline's **role** (derived from its triggers — `pull_request` /
`merge_group` ⇒ PR verify, `push` to branches ⇒ push verify, tags or `release` ⇒ release,
environments or deployment events ⇒ deploy, otherwise scheduled / manual / other) decides which
state it fills. One workflow serving several triggers binds to each role its triggers imply
(`BindingRoles`), so `on: [pull_request, push]` appears under both *CI / Review* and *Main CI*.

## Packages

| Package | Role |
|---|---|
| `pkg/workflowmodel` | types, `Validate`, `Hash`, `HiveDefault`, `Bind`, topological `Levels`, `Compile` → `Profile` |
| `pkg/workflowmodel/hivedefault` | `Generate()` the `hive-default` model from `stagelabels.Table` + `stageengine.DiagramEdges()`; `Profile()` compiled |
| `pkg/stageengine` (`workflow.go`) | `DeriveFromModel(profile, kind)`: builtin delegation or the rules evaluator; `Options.Workflow` |
| `pkg/workflowmodel/githubactions` | `.github/workflows/*.y*ml` → pipelines |
| `pkg/workflowmodel/tekton` | Pipeline / PipelineRun / Task (+ Pipelines-as-Code annotations) → pipelines |
| `pkg/workflowmodel/ingest` | `Repository(scope, files)`: route files to ingesters, compose, bind |
| `pkg/workflowmodel/render` | `Columns` (Activity Monitor), `Diff` against the default, `Mermaid` |
| `cmd/workflow-survey` | walk a corpus of repositories, write per-repo model / columns / diagram and a summary |

Everything the ingest could not represent is a **finding** on `spec.coverage` with a stable code
(`reusable-only`, `secrets-inherit`, `no-trigger`, `environment-dynamic`, `yaml-parse`, …) and a
severity, never a guess.

## Column derivation

- PR-verify pipelines fill the `ci` region; their merged topological levels become sub-stages of
  *CI / Review* (at most six, names truncated past three per level).
- Push-verify adds *Main CI*; release adds *Release*; deploy adds one *Deploy: \<env\>* column per
  literal environment (or a single *Deploy* when the environment is an expression); the chain
  ends in *Done*. *Merged* stops being the final state only when a post-merge chain exists.
- Scheduled, manual and other pipelines bind to no state and are listed, not rendered.

## Proving corpus

`pkg/workflowmodel/testdata/corpus/<owner>__<repo>/` holds real CI files copied from public
repositories (argo-cd, cilium, envoy, konflux-ci, tektoncd/pipeline, serverless-operator) with a
golden summary per repository; `go test ./pkg/workflowmodel/ingest -update` rewrites the goldens
after an intended change. The full 317-repository CNCF-landscape survey and its collector live
outside the repository; the results are on #17620.

## Lifecycle half and the compiler (RFC phase 1)

The RFC (#17620) asks for one engine that derives under any model. The decision that keeps v5/v6
byte-identical is that **`hive-default` derivation stays in Go**: `hivedefault.Generate()` builds
the model from the stage table, the engine's expected edges and the monitor's column anchors, and
declares `derivation: builtin`. `stageengine.DeriveFromModel` then delegates to `IssueDeriver` /
`PRDeriver` and only checks coverage — a stage the model has no state for abstains with
`workflow: not covered` instead of writing a label the model does not know. The parity test
(`TestDeriveFromModelHiveDefaultParity`) runs every PR stage golden and an issue sweep through both
and requires identical `Decision`s.

Custom models declare `derivation: rules`: ordered rules, each a conjunction of predicates over the
fact vocabulary in `workflowmodel.Facts` (`open`, `merged`, `draft`, `labels has stage/*`,
`author.kind is human`, `pr.checks is green`, `pr.review is approved`, `issue.covered`, …); the
first rule whose predicates all hold names the state, and an item no rule matches is not covered.
Rules reach only the facts the engine already computes; no LLM is ever in the derivation path.
`testdata/models/techdocs.json` is the hand-written reference model (cncf/techdocs engagement).

`Compile` refuses (error diagnostics, no profile): `state-no-exit`, `gate-no-owner`,
`agent-no-human-gate` (every path from an agent-granted state to a terminal outcome must cross a
transition gated by a role with `human: true`; closure edges — the item closed unmerged — are
observations and exempt), `duplicate-event-fact`, `state-no-column` / `unknown-column`,
`duplicate-stage-number`, `unknown-fact` / `bad-op`, `no-rules`, `no-version`. Elements marked
`confidence: inferred` compile with an `inferred-element` warning the editor shows.

Stage numbers in a model are Hive's label numbers (`stagelabels.Table`) for now, so a bound repo's
labels, tracker anchors and replies keep working; a state whose number does not apply to the item
kind is not covered. Models are whole (no inheritance) through phase 4.

## Known gaps (phase 1)

- Prow (`kubernetes/*`) is a third syntax not read; those repos render the default columns.
- `pull_request_target`-only bot workflows (labelers, welcome bots) count as PR verify and inflate
  the first CI level; the editor (phase 2) is where a maintainer deselects them.
- Pipelines-as-Code `on-cel-expression` triggers are recorded as `cel` events and not classified
  into roles, so a tag-push expressed in CEL does not add *Release*.
- Bare Tekton `Pipeline` library files (catalogs) have no trigger and bind nowhere (`no-trigger`).

## Store, bindings and engine wiring (RFC phase 2)

- **Store** (`pkg/workflowmodel/store`): `<DataDir>/workflows/<name>/<version>.json` plus an
  `index.json` rebuilt on write; names/versions are regex-guarded, writes are atomic.
  `hive-default` is virtual and read-only. Drafts (models that do not compile) save but
  `Profile()` returns `ErrNotCompiled`. Entries reserve `adoption`, `ratings`, `benchmarks`
  (raw JSON, carried verbatim) so the phase-7 marketplace can add them without a migration.
- **Binding** (`pkg/config/workflow_binding.go`): `project.workflow_bindings: [{repo, name,
  version}]`, mirroring `repo_policies`. `WorkflowBindingFor` is keyed like repo pauses
  (lowercased, org-qualified) and reads the live config, so a PUT takes effect without a
  restart. Binding requires the model to compile; deleting a bound model is a 409.
- **API** (`pkg/dashboard/api_workflows.go`): `GET/POST /api/workflows`, `POST
  /api/workflows/compile`, `GET/PUT /api/workflows/bindings`, `GET/PUT/DELETE
  /api/workflows/{name}/{version}`. Writes need the owner role and are audited; 503 when no
  store is configured.
- **Engine**: `stageengine.Options.WorkflowFor(owner, repo) *Profile` and `DeriveForRepo` (a
  per-profile, hash-cached deriver) route bound repositories through `DeriveFromModel`; unbound
  repositories and `workflow.enabled: false` take the builtin path unchanged. An unusable
  binding (missing or non-compiling model) warns once and falls back.
- **Tracker**: the response gains `workflows: {default: {...}, repos: {<repo>: {name, version,
  columns, numbers, error}}}` (nil when nothing is bound). The JS labels the model column,
  renders a per-row column strip by stage number, and shows an amber "Not covered" chevron for
  abstentions. Settings → Repos has a per-repo Workflow select that PUTs the binding.

## Ingest → lifecycle (RFC phase 3a)

`ingest.Repository` (phase 0) is unchanged and still produces the process half. `ingest.Lifecycle`
composes on top of it:

1. Start from `hivedefault.Generate()` — the compiled hive-default lifecycle, `derivation: builtin`.
2. Copy the ingested pipelines, sources and coverage; `meta.source` is the system(s) read,
   `meta.provenance` is `ingested-unreviewed`, `meta.version` is `1` (the store assigns later
   versions).
3. PR-verify pipelines are bound to every state in the CI / Review column, each with a citation to
   the file the pipeline came from.
4. Every post-merge state the process half produced (main CI, release, `deploy-<env>`, done)
   becomes a state in its own column after Merged, `confidence: inferred`, with the environment
   guards carried over. The chain's transitions fire on `pipeline-succeeded` =
   `pipeline.status is green`. The engine does not observe post-merge runs yet and reports
   `unknown` for that fact, so a bound ingested model derives exactly like hive-default today;
   the extra columns render, nothing lands in them until post-merge observation exists.

The compiler warns once per inferred element; a human confirming them in the editor (phase 4)
is what moves provenance to `local`/`curated`.

`POST /api/workflows/ingest` is the spoke entry point (owner role): `{repo}` fetches the default
branch through `FetchDefaultBranchTree`, `ingest.ReadTree` keeps GitHub Actions workflows, YAML
under Tekton directories and pipeline-named YAML that looks like Tekton (the survey collector's
rules), and the checkout is removed. `{files}` accepts pasted definitions. `save: true` stores the
result at the next integer version under `name` (default: the repository slug).

`cmd/workflow-survey` now compiles every corpus repository's lifecycle model (`lifecycle.json`
alongside `model.json`) and reports pass/refusal counts by diagnostic code.

## Workflows tab (RFC phase 3b)

Settings → **Workflows** is the spoke's home for models (`static-src/js/config-workflows-library.js`,
`css/workflows.css`). It has two sub-views and a detail page, all built from `GET /api/workflows`
and `GET /api/workflows/{name}/{version}`:

- **Library** — one row per `name@version`: provenance badge (`built-in` for the virtual
  hive-default and any read-only entry, else `meta.provenance`), compile badge (`compiles`,
  `compiles · N warnings`, `does not compile`, with the diagnostics as the tooltip), source,
  states / columns, and the repositories bound to it. Built-in rows sort first. Owners get
  Duplicate (saves a copy under a new name at version 1, provenance `local`) and Delete; Delete is
  withheld while anything is bound (the API returns 409 anyway) and the row says `bound` instead.
  Export shows the model JSON in-app (copy to clipboard) for everyone.
- **Detail** — facts, the diagram, diagnostics, citations and the raw model/profile JSON.
  `workflowDiagramSvg(model, profile)` is a pure function: the compiled profile's columns are lanes
  in display order, each state is a chip carrying its padded stage number, transitions are bezier
  edges (same-lane edges bow to the right, backward edges are styled `back`), inferred states are
  dashed, the initial state and terminal column are marked. Duplicate and dangling transitions are
  dropped rather than drawn.
- **Ingest** — pick a configured repository (or type `owner/repo`), optionally a name (default: the
  slug), then Preview or Ingest-and-save; both call `POST /api/workflows/ingest`. The result shows
  the compile outcome, files read, pipelines found, and opens the saved entry.

The tab never binds: binding stays on Settings → Repos so the per-repository decision sits next
to the repository's other policy. The Activity Monitor's model badge deep-links here via
`openWorkflowLibraryEntry(name, version)`. Every HTML builder is a pure function of its inputs so
`workflows_library_ui_test.go` runs them under node without a DOM. Editing is phase 4.

## Simulate (RFC phase 3c)

"Paste or pick a real issue/PR and see which state the model puts it in and why, using the same
`Facts` the engine would use" — the operator's confidence check before binding. Nothing is
written; any logged-in role may simulate.

`POST /api/workflows/{name}/{version}/simulate` takes

```json
{ "kind": "issue" | "pr", "facts": { ...SimulateFacts... }, "repo": "owner/repo", "number": 123 }
```

`facts` is a lowerCamel JSON mirror of `stageengine.Facts` (`SimulateFacts` in
`pkg/dashboard/api_workflows_simulate.go`): `labels`, `open`, `merged`, `draft`, `origin`,
`author{login,kind,selfAuthored,role}`, `park{holds,needsHuman,…}`, `issue{bucket,…}` and
`pr{checks,review,dco,conflicting,changesRequested,humanApprover,lane,…}`. Enumerations use the
engine's name helpers so a response can be pasted back as a request: `author.kind`
hive|clanker|human|unknown, `pr.checks` green|red|pending|unknown, `pr.review`
approved|changes_requested|pending|unavailable|requires_human, `pr.dco` yes|no|unknown
(`stageengine.ParseAuthorKind/ParseChecks/ParseReview/ParseDCO` are the inverses of the name
helpers). `park.holds` left null is derived from the labels with `stagelabels.Holds`.

The handler loads and compiles the stored model exactly as `GET` does, converts the facts, and
calls `stageengine.Explain(profile, kind, facts)`, which returns the same `Decision` as
`DeriveFromModel(...).Derive` plus a trace:

```json
{
  "decision": { "state": "accepted", "column": "Accepted", "stage": 20, "label": "stage/020-accepted",
                "reason": "rule accepted", "abstain": false, "covered": true, "terminal": false },
  "trace": [ { "id": "requested", "state": "requested", "evaluated": true, "matched": false,
               "predicates": [ { "fact": "label", "op": "has", "value": "techdocs/request", "holds": true, "known": true },
                               { "fact": "label", "op": "not", "value": "techdocs/accepted", "holds": false, "known": true, "actual": "techdocs/accepted" } ] },
             { "id": "accepted", "state": "accepted", "evaluated": true, "matched": true, "predicates": [ … ] },
             { "id": "pr-review", "state": "review", "kind": "pr", "evaluated": false, "matched": false, "skipped": "kind pr" },
             { "id": "later", "state": "done", "evaluated": false, "matched": false } ],
  "facts": { …normalized echo… }, "filled": [], "unknown": [], "notes": []
}
```

Trace semantics: for `derivation: rules` every rule appears in order; rules for the other item
kind are `skipped`; rules are `evaluated` until the first `matched` one, each with its predicates'
`holds`/`known` verdicts and the actual value when it differs; rules after the match are listed
with `evaluated: false`. The uncovered case has no matched rule and the decision carries
`abstain: true, covered: false, reason "workflow: not covered…"`. For `derivation: builtin`
(hive-default and ingested models) the trace is a single `{builtin: true, state, reason}` entry,
because the built-in derivers are not rule lists. `column` is looked up from the profile and
`stage` from the state's `lifecycle.number`; a built-in decision of Stage None (for example a
draft PR) returns `covered: true` with an empty `state` and the deriver's reason.

Built-in PR derivation reads lane facts the engine's lane observers normally supply. When the
request carries no `pr.lane`, `stageengine.SimulatedLane` synthesizes one from the other facts
(self-authored / trusted-author / contributor lane; reason from merged, closed, draft, conflict,
changes requested, holds, needs-human, red or pending checks, missing review, untrusted role) and
the response says so in `notes`. Give `pr.lane{lane,reason}` explicitly to override it.

Best-effort fill: `repo` + `number` without `facts` fetches the item through the hive's GitHub
client (`github.Client.WorkflowItemSnapshot`: the issue, the PR when it is one, its mergeable
state and the head commit's check summary) and fills `labels`, `open`, `merged`, `draft`,
`author.login`, `author.role` (from `role/*` labels), `park.holds`, `origin`, and for PRs
`pr.checks`, `pr.dco`, `pr.conflicting`, `pr.headSHA`, `pr.baseRef`. `filled` lists what came
from the forge and `unknown` what stayed at its default — `author.kind`, `pr.review`,
`issue.bucket` and the claim/covering-PR facts need the engine's own observers and are not
fetched; edit them in the form before running. `kind` may be omitted and is taken from the item;
a mismatch is a 400. Without a wired GitHub client the endpoint answers 422 rather than guessing.

The UI (`renderWorkflowSimulate`, `renderWorkflowSimulateResult` in
`config-workflows-library.js`) lives in a collapsed **Simulate** section of the model detail page:
a `owner/repo#123` picker with Fetch facts, an issue/PR radio, and a facts form generated from the
declarative `WF_SIM_FIELDS` table (so the builder, the form reader and
`workflows_simulate_ui_test.go` share one definition). Run posts the form, shows the decision chip
(state · column · stage · reason or **Not covered**), the trace as an ordered list with the
matched rule highlighted and each predicate as `fact op value → ✓/✗/?`, and
`wfSimMarkDiagram` tags the resulting state's chip in the diagram (`.wf-dg-state.simulated`).

## Versions, diff, confirm and label regeneration (RFC phase 4, backend)

Phase 4's definition of done is "the TechDocs model can be changed from the diagram and the change
regenerates labels and commands". The backend half gives the editor the primitives for that; the
diagram UI lands separately on top of it.

### Structural diff — `workflowmodel.Diff`

`Diff(a, b *Model) ModelDiff` is pure and deterministic: spec only (metadata never counts),
every list sorted by key, the same pair always yields the same JSON. It compares five keyed
collections plus `spec.meta`:

| Set | Key | Fields compared |
|---|---|---|
| `columns` | column id | label, hint, states, terminal |
| `states` | state id (regions flattened, with `parent`) | label, owner, type, parent, guardRef, pipelines, origin, column, number, kinds, waiting, replies, confidence |
| `transitions` | `from>to` (`from>to@on` when the pair occurs more than once) | on, guardRef, gateRef, note |
| `labels` | label name | color, description, state |
| `rules` | rule id, else `rules[i]` | kind, state, when (rendered predicates), reason |
| `meta` | — | version, author, description, license, source, provenance, derivation mode |

Each set is `{added, removed, changed:[{key, fields:[{field, from, to}]}]}`; `summary` is one
line (`+2 states, −1 transition, 3 changed`; `no changes` when `empty`). Lists are rendered as
comma-joined strings so a field change is always a flat string pair.

### Confirm inferred → local — `workflowmodel.Confirm`

`Confirm(m, by)` returns a deep copy with every `confidence: inferred` element (states at any
depth, events, gates) marked confirmed, `meta.provenance = local`, `meta.version` cleared so the
store assigns the next integer version, and `metadata.annotations["workflow.hivecommons.dev/confirmed-by"]`
set to the caller. `meta.source` is kept: the model still says where it was ingested from. The
input is never modified; `Inferred(m)` counts what is left to confirm. The store gained
`NextVersion(name)` (largest integer version + 1, never a taken one) for this.

### API

| Route | Role | Answer |
|---|---|---|
| `GET /api/workflows/{name}/versions` | any | `{versions: []Entry}` newest save first; hive-default returns its one virtual entry; 404 unknown |
| `GET /api/workflows/{name}/diff?from=V1&to=V2[&fromName=&toName=]` | any | `{from: Entry, to: Entry, diff: ModelDiff}`; 400 when `from`/`to` missing, 404 when either side is unknown. `fromName`/`toName` let a local model be diffed against `hive-default` |
| `POST /api/workflows/{name}/{version}/confirm` | owner | 201 `{entry, confirmed, from}` — the confirmed copy saved as the next version; 409 for hive-default (read-only); audited `workflow_confirm` |
| `GET /api/workflows/{name}/{version}/labels` | any | `{name, version, hash, prefix, labels: []LabelDef{name,color,description,state}, commands: [{state, verbs}], stages: [{state, number, label}]}` — the regeneration preview; 400 with diagnostics for a draft that does not compile |

`labels` is `Profile.LabelDefs(stagelabels.StageColor)`: the model's `spec.labels.entries` with
the stage colour filled in where an entry names none, the state's label as description where an
entry has none, and descriptions clamped to GitHub's limit. `commands` is `Profile.Replies` (the
human-gate verbs per state) in column order. `stages` is the built-in stage label each numbered
state maps onto today, since the engine still writes the built-in family by stage number.

### Label regeneration on bind

`automerge.Engine.ensureStageLabels` provisions the built-in family once per repository per
process. It now also asks `stageengine.Options.WorkflowFor` (the P2 lookup cmd/hive wires from
the bindings and the store) for the profile bound to the repository; for a bound, non-hive-default
profile it creates or recolours that profile's `LabelDefs` in the same pass, after the built-in
family, reusing the one label list call. The memo is per repository *and* profile hash
(`StageTracker.ensuredWorkflow`), so a rebinding, an unbind, or a new version of the bound model
changes the hash and the next pass re-lists and provisions only what is missing. No reload hook is
needed: the dashboard's binding edit mutates the live config, `WorkflowFor` reads it on every call,
and the hash comparison does the rest. An unbound repository (or one bound to hive-default) takes
exactly the pre-P4 path: one list, the built-in family, no model labels.

### Known gaps (phase 4 backend)

- Labels are only ever created or recoloured, never deleted: unbinding or removing an entry leaves
  the label in the repository (same policy as the built-in family). Removal needs an explicit
  operator action, not a sweep side effect.
- The engine derives through the built-in stage table by number; a model's own `labels.entries`
  are provisioned but not yet written onto items. Writing model labels is the engine half of
  phase 4 and follows the diagram editor.
- A model without a `labels` block regenerates nothing (TechDocs as shipped); the preview still
  shows its commands and the built-in stages its numbered states land on.

## Ingest: intake, triage, roles and branch rules (RFC phase 5)

`ingest.Lifecycle` now reads the repository's process conventions next to its pipelines and adds
an *intake half* to the draft: how work enters, who triages it, who approves a change and what the
forge requires before merge. `ReadTree` keeps the files below in addition to the CI definitions
(`ingest.IsIntakePath`); the dashboard seam (`WorkflowRepoFiles`) is unchanged. Everything this
half adds is `confidence: inferred` and cites where it came from; the compiler warns once per
element and a human confirms them in the editor exactly as for the pipeline half. Trees with none
of these inputs produce the byte-identical model as before (`TestLifecyclePipelineOnlyUnchanged`).

### Heuristics

| Input | Read as | Proposes |
|---|---|---|
| `.github/ISSUE_TEMPLATE/*.{yml,yaml}` (issue forms), `*.md` and `.github/ISSUE_TEMPLATE.md` (front matter) | `name`, `labels` (list or comma string) | One `requested-<slug>` state per distinct label set in an **Intake** column after Open; event `issue-opened-with-template:<slug>` = `labels has <bind label>` (the template's first label that is not a triage label and not already bound); transitions open→requested, requested→triage (or →actionable when no triage label exists) and requested→closed; a `labels.entries` row per template label pointing at the state. |
| `.github/ISSUE_TEMPLATE/config.yml` | `blank_issues_enabled` | Annotation `workflow.hivecommons.dev/blank-issues: disabled` when false (every issue arrives through a template). |
| Labels seen in templates, `.github/settings.yml` (probot `labels:`), `.github/labels.yml` (sequence, `labels:` wrapper or mapping), `.github/labeler.yml` (top-level keys), `CONTRIBUTING.md` (backticked labels, or hyphen/slash labels at word boundaries) | Triage vocabulary: `needs-triage`/`triage/needed`/`status/needs-triage`/…; `triage/accepted`/`triage-accepted`/`accepted`; `triage/declined`/`wontfix`/`invalid`/`not-a-bug`; conventions `good first issue`, `help wanted` | A `triage` state in a **Triage** column after Intake; events `triage-requested` (`labels has <needs>`), `triage-accepted` (`labels has <accepted>`, or `labels lacks <needs>` when there is no accepted label), `triage-declined`; gate `triage-decision` released by `approver` when one exists, else `maintainer`, with reply `/label <accepted>`; transitions open→triage, triage→actionable (gated), triage→closed; label rows `needs → triage`, `accepted → actionable`, `declined → closed`; convention labels as plain rows. |
| PR template (`.github/PULL_REQUEST_TEMPLATE.md`, `PULL_REQUEST_TEMPLATE.md`, `docs/…`, `.github/PULL_REQUEST_TEMPLATE/*.md`) | `- [ ]` / `* [ ]` / `[x]` items | `checks` entries `pr-template-<slug>` of kind `manual`, `source: path:line`. Duplicates by id are dropped. |
| `CODEOWNERS` (root, `.github/`, `docs/`) | Every owner on every rule (users and `org/team`) | Role `approver` of kind `members` (params `members`, `source: path:line,…`). |
| root `OWNERS`, `OWNERS_ALIASES` (Prow) | `approvers`, `reviewers`, `filters[".*"]`, aliases | Roles `approver` and `reviewer`; the review gate's reply is `/approve`. |
| Any `approver` role | | Gate `review` released by `approver` (condition `pr.review is approved`) attached to the hive-default edges from CI / Review into the admitted Merge-queue states (`awaiting-merge-admission`, `ready-for-human-merge`, `pre-merge-soak`, `merge-queue`), so the compiler's "gate with no owner role" refusal is satisfied by a real role. |
| Branch rules: `GET /repos/{o}/{r}/branches/{b}/protection`, falling back to `GET /repos/{o}/{r}/rules/branches/{b}` (repository and organisation rulesets); or probot `.github/settings.yml` `branches[].protection` when the API gave nothing | required status checks, required approving reviews, code-owner review, dismiss stale reviews, enforce admins, linear history, signed commits | `checks` entries `check-<slug>` of kind `ci`, `required: true`; guard `required-checks` (params `contexts`) on the same merge edges; `review.minApprovals: N`; gate `codeowner-review`; bookkeeping effects on `merged` (`enforce-admins`, `require-linear-history`, `require-signed-commits`, `dismiss-stale-reviews`) with the source in their params. |

The forge half arrives as a synthetic file at `ingest.BranchRulesPath` (`.hive/branch-rules.json`,
`ingest.BranchRules`): `cmd/hive`'s `workflowRepoFiles` fills it through `Client.FetchBranchRules`
when ingesting by `repo`, so pasted `files` and the survey corpus can carry the same reading.
`ingest.LifecycleOptions` accepts the rules explicitly, overriding the file.

### Citations

Every element cites `path:line` for file-derived facts (`Citation.Path`/`Line`; `checks[].source`,
`guards[].source`, `roles[].params.source` and `bookkeeping[].params.source` carry the same
`path:line` string) and `api:<endpoint>` — for example `api:repos/cncf/techdocs/rules/branches/main`
— for facts read from the forge. `meta.source` becomes `issue-templates` (or
`github-actions+issue-templates`) once a template was read.

### What `cncf/techdocs` proposes

With its current `main` (no issue templates yet; `.github/settings.yml` declares `needs-triage`,
`triage/accepted`, `triage/declined`, `good first issue`, `help wanted`; the org ruleset requires no
checks or approvals): columns `open · triage · actionable · claimed · pr-in-progress · ci-review ·
merge-queue · merged`, the `triage` state citing `.github/settings.yml:129/132/135`, the three
triage events, the `triage-decision` gate released by `maintainer`, and the five label rows. The
intake form on the pending `build/03-intake` branch (`assessment-request.yml`, labels `Docs analysis`
+ `needs-triage`) adds `intake[requested-documentation-assessment-request]` before Triage — the
fixture under `pkg/workflowmodel/ingest/testdata/techdocs` carries both.

### Limitations

- Heuristics prefer precision: a template without labels, or whose labels are all triage labels or
  already bound to another intake state, yields a `issue-template-no-labels` / `issue-template-merged`
  finding rather than a state. Label names are matched from a fixed table; a repository's own
  triage word (`status: new`) is not recognised.
- Only the root `OWNERS` is read (nested owners files scope by path, which the model does not
  express); CODEOWNERS owners are all `approver`, since the file has no reviewer notion.
- `codeowner-review` is informational: the engine does not observe which files a PR touches.
- The engine still derives through the built-in stage table; nothing lands in Intake or Triage
  until the label-driven engine half of phase 4 writes model labels onto items.
- Branch protection needs `administration:read`; without it the rulesets endpoint is used, which
  omits legacy protection settings such as `enforce_admins`.

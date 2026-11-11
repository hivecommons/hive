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

## Editor (RFC phase 4, UI)

The owner's brief was "the editor needs to be a good design tool too": a design surface in the
spirit of a lightweight draw.io for state machines, not a form over the JSON. Settings → Workflows
grows an **Editor** nav item (`config-workflows-editor.js`, `workflows-editor.css`, registered
right after the library module), and the library rows and detail page grow **Edit** (owner) or
**Edit a copy** (hive-default and other built-ins, which are read-only); an ingest result gains
**Open in editor** so an ingested draft can be reviewed on the canvas before it is saved.

**Layout.** Toolbar · left templates palette · centre SVG canvas · right inspector · bottom
tabbed drawer (Problems · YAML · Diff · Simulate). With no model loaded the Editor shows a start
screen: New blank model, Duplicate hive-default, Open from Library, From Ingest, Marketplace
(disabled placeholder for phase 7).

**Canvas.** Columns are vertical lanes: `+ Column` adds one, double-click the header renames it,
drag the header horizontally to reorder, drag its right edge to resize (snapped to an 8px grid,
`view.laneWidths`), terminal lanes are shaded and dashed. States are chips inside lanes: drag
within or between lanes (rewrites `columns[].states` and `lifecycle.column`), double-click to
rename the label, Delete/Backspace removes the selection and its transitions (confirmed when more
than one), marquee on empty canvas, shift-click to extend, arrow keys nudge order. Each chip has
a right-edge port; dragging it onto another chip creates a transition with `on: ''`, selects it
and focuses the inspector's `on` field. Edges are beziers (self-loops as a small loop, backward
edges dashed); clicking one selects it. Pan with space+drag, middle button or wheel; zoom with
ctrl/⌘+wheel or the toolbar ± / Fit; a minimap appears when the model outgrows the viewport.
**Auto-layout** orders lanes by BFS depth from the initial state (terminal lanes last), drops
unassigned states into the nearest lane by depth, and sorts each lane by stage number then
label. Compile diagnostics appear as badges on chips (error red, warning amber, tooltip text);
inferred states are dashed with a one-click ✓ **Confirm** that sets `confidence: local` locally,
and a toolbar **Confirm all inferred** that, for a stored ingested version, also calls
`POST /api/workflows/{name}/{version}/confirm` after saving. The initial state carries a marker.

**Pure core.** Everything that mutates or lays out the model is a pure function over plain JSON
(never mutates its input, deterministic), so the undo stack is a snapshot list and the core is
unit-tested under node (`workflows_editor_ui_test.go`): `wfEdNewModel`, `wfEdNormalize`,
`wfEdAddColumn` / `wfEdMoveColumn` / `wfEdRenameColumn` / `wfEdRemoveColumn(model, id,
moveStatesTo)`, `wfEdAddState` / `wfEdMoveState` / `wfEdRenameState` (rewrites transitions,
initial, label entries, derivation rules, lanes and agent grants) / `wfEdRemoveState`,
`wfEdAddTransition` / `wfEdRemoveTransition`, `wfEdUpdate(model, path, value)` with paths like
`spec.states[id=open].lifecycle.number` (empty value deletes), `wfEdUniqueId` (slug + `-N`),
`wfEdAutoLayout`, `wfEdLayout(model, profile, view)` → lanes/chips/edges geometry,
`wfEdLaneAt` / `wfEdSlotAt` for drops, `wfEdLocalProblems` (mirrors the compiler's refusals —
`state-no-exit`, `state-no-column`, `unknown-column`, `unknown-state`, `column-empty`,
`no-terminal-column`, `duplicate-stage-number`, `unknown-event`, `inferred-element` — for
instant feedback before the server answers), `wfEdDiff(a, b)` (the client twin of
`workflowmodel.Diff`: keyed change sets for columns, states, transitions, labels and rules plus
meta field changes and a summary), `wfEdSvg(model, layout, state)` and `wfEdMinimapSvg`.

**Runtime.** `_wfEd = {model, saved, history[], future[], selection, view{panX,panY,zoom,
laneWidths}, compile{busy,profile,diags}, drawer, sim, yaml, …}`. `wfEdCommit(next)` pushes a
deep-copy snapshot (cap 200), bumps a revision, re-renders only the affected regions and
schedules a live compile: 300 ms after the last change the model is posted to
`POST /api/workflows/compile` and the toolbar pill reads `compiles` / `N warnings` / `does not
compile`. Pointer interactions attach once per render (`wfEdAttach(root)`, idempotent) to the
canvas root; every other control is a delegated `data-action`. Cmd/Ctrl-Z, Shift-Cmd-Z and Cmd-S
work when the editor has focus. Leaving the editor with unsaved changes asks via `hiveConfirm`.

**Drawers.** *Problems* lists diagnostics (click selects the element). *YAML* is a textarea
bound two-way through the new `POST /api/workflows/yaml` (`{model}` → `{yaml}` and `{yaml}` →
`{model, error?}`; model→YAML refreshes while the tab is visible and the textarea is not focused;
Apply parses back and commits; parse errors show inline). *Diff* shows the client-side
`wfEdDiff` of the working copy against the last save as grouped lists, and, for a stored
version, the stored-versions diff (`GET …/diff?from&to`) against any earlier version. *Simulate*
uses the library's declarative `WF_SIM_FIELDS` form and posts `{model, kind, facts}` to the new
`POST /api/workflows/simulate`, which compiles the unsaved body and runs `stageengine.Explain`
(422 with diagnostics when the model does not compile); the decided state is highlighted on the
canvas (`.wf-ed-sim-hit`, others dimmed) and the reason and trace render below the form.

**Persistence.** *Save* posts the model with `meta.version: ''`, which `handleWorkflowSave` now
resolves through `store.NextVersion(name)`, then reloads the library and reopens the saved entry
in the editor. *Save as…* prompts a new name (version 1, provenance local). *Revert* restores the
last saved snapshot. hive-default opens read-only with an *Edit a copy* button.

**Export.** SVG (the canvas serialized with the CSS custom-property values resolved via
`getComputedStyle`, so it is self-contained), PNG (drawn to a canvas and shown in a dialog with
open-in-new-tab — no forced download), model JSON and model YAML, all through
`hiveExportViewer`.

**Deferred.** Editing events, gates, roles and derivation rules (they round-trip untouched and
are editable through the YAML drawer); nested regions beyond flattening; marketplace (phase 7);
keyboard-only lane reorder; collaborative/locking semantics; server-side layout persistence
(lane widths live in the session).

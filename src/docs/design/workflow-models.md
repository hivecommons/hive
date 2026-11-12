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
- **Derivation switch** — the Library header carries an owner-only toggle, *Derive stages from
  bound models* (`renderWorkflowEnabledToggle`, `PUT /api/workflows/enabled {enabled}`), so a
  hosted hive with no `hive.yaml` access can turn `workflow.enabled` on or off. Off: a binding
  only changes the Activity Monitor's columns. On: a bound repository's stages are derived by its
  model's rules and the model's labels are provisioned. Turning on asks for confirmation (it
  changes derivation for every bound repository and creates labels in them); turning off is
  immediate. The handler sets `Config.Workflow.Enabled`, saves `hive.yaml`, audits
  `workflow_enabled` / `workflow_disabled`, and — on the way on — calls
  `Dependencies.WorkflowProvisionLabels` for each binding (cmd/hive wires it to
  `automerge.Engine.EnsureStageLabels`, the sweep's memoised `ensureStageLabels` path over the
  shared stage-engine options and tracker), so the bound models' labels exist at once rather than
  on the next PR that needs a label write; a repository that fails is returned in `warnings[]`
  and the next sweep retries. No reload hook is needed: `stageEngineWorkflowFor` and the tracker
  read `cfg.Workflow.Enabled` on every call, and the hive.yaml reloader mutates the same
  `*Config` in place. The Repos binding row shows *columns only — turn on derivation in
  Workflows* when a binding exists while the flag is off.
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
and focuses the inspector's `on` field; clicking an edge selects it. Pan with space+drag, middle button or wheel; zoom with
ctrl/⌘+wheel or the toolbar ± / Fit; a minimap appears when the model outgrows the viewport.
**Auto-layout** orders lanes by BFS depth from the initial state (terminal lanes last), drops
unassigned states into the nearest lane by depth, and sorts each lane by stage number then
label. Compile diagnostics appear as badges on chips (error red, warning amber, tooltip text);
inferred states are dashed with a one-click ✓ **Confirm** that sets `confidence: local` locally,
and a toolbar **Confirm all inferred** that, for a stored ingested version, also calls
`POST /api/workflows/{name}/{version}/confirm` after saving. The initial state carries a marker.

**Edge layer: quiet by default, detail on focus.** A real model (hive-default has 22 states and
~90 transitions) turns a naive one-bezier-per-transition drawing into a knot, so the canvas and
the read-only library diagram share one pure router (`wfRouteEdges` in
`config-workflows-library.js`) and a set of rules:

- *No labels by default.* Every edge renders its event as a floating pill, but CSS shows it only
  while that edge is hovered or selected. A toolbar **Labels** toggle shows labels for the
  exceptional events only — the model's most common `on` (e.g. `stage-change`, detected by
  `wfEdgeCommonEvent`) is never worth labelling and stays quiet.
- *Orthogonal routing through gutters, bundled per lane pair.* Edges leave the source chip's
  right port, run into the gutter between the lanes, drop vertically in a channel reserved for
  that (from-lane → to-lane) pair and enter the target's left port with rounded corners
  (`wfOrthoPath`). Edges that share a lane pair share one vertical trunk, so the Actionable →
  Claimed bundle is one line that fans out at both ends. Same-lane edges hug the lane's right
  margin, stacked by distance. Edges that skip lanes run below the lanes in a corridor row;
  backward edges between adjacent lanes run down the shared gutter (both ports face it), and
  backward edges that skip lanes route around the *outside* above the lane headers, dashed and
  muted. `wfEdgeChannels` counts the corridor rows so the layout reserves that space.
- *Focus mode.* Hovering or selecting a state dims every edge not incident to it
  (`--wf-edge-dim`), lifts the incident edges into a front layer in the accent colour with
  arrowheads (`fill="context-stroke"`), and outlines the neighbouring chips; hovering an edge
  highlights it and both end chips and reveals its label. The hover handler
  (`wfEdgeFocusHover`) is DOM-only and shared by the editor canvas and the library diagram.
- *Fan-out collapse.* More than four transitions from one state into the same lane collapse to
  one trunk with a **+N** badge at the source port; hovering expands the fan, and in the editor
  clicking the badge pins it open (`view.fansOpen`).

Ports, drag-to-connect, arrowheads and the initial/terminal styling are unchanged; every colour,
opacity and size is a token so the style ratchet stays green.

**Pure core.** Everything that mutates or lays out the model is a pure function over plain JSON
(never mutates its input, deterministic), so the undo stack is a snapshot list and the core is
unit-tested under node (`workflows_editor_ui_test.go`): `wfEdNewModel`, `wfEdNormalize`,
`wfEdAddColumn` / `wfEdMoveColumn` / `wfEdRenameColumn` / `wfEdRemoveColumn(model, id,
moveStatesTo)`, `wfEdAddState` / `wfEdMoveState` / `wfEdRenameState` (rewrites transitions,
initial, label entries, derivation rules, lanes and agent grants) / `wfEdRemoveState`,
`wfEdAddTransition` / `wfEdRemoveTransition`, `wfEdUpdate(model, path, value)` with paths like
`spec.states[id=open].lifecycle.number` (empty value deletes), `wfEdUniqueId` (slug + `-N`),
`wfEdAutoLayout`, `wfEdLayout(model, profile, view)` → lanes/chips/edges/fans geometry (edges via the shared `wfRouteEdges` / `wfOrthoPath` / `wfEdgeChannels` router),
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
| `MAINTAINERS`, `MAINTAINERS.md` (root, `.github/`, `docs/`) | `@handle` and `github.com/handle` spellings (plain list, markdown table or prose; names and e-mail addresses are not identities) | The hive-default permission-based `maintainer` role becomes kind `members` listing those handles (`source: path:line`); when no `approver` role exists, the review gate is released by `maintainer`. |
| `kind/*`, `priority/*`, `type/*`, `area/*`, `severity/*`, `size/*` labels declared in `labels.yml`, `settings.yml`, `labeler.yml`, templates or backticked in `CONTRIBUTING.md` | Classification vocabulary triage applies | Plain `labels.entries` rows (no state), sorted by name, so the triage gate's vocabulary is visible in the editor. |
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

## Label GC and model commands (RFC phase 4, follow-up)

Two of the phase-4 backend gaps close here: stale model labels are now reclaimed, and a bound
model's `replies` reach the human gate as commands.

### Label garbage collection on unbind / rebind

`ensureStageLabels` already provisions a bound model's `LabelDefs` after the built-in family. It
now also remembers what it provisioned and reconciles on change
(`automerge_stage_labels_gc.go`):

- **Record.** After each provisioning pass the engine writes one JSON record per repository,
  `workflows/provisioned/<owner>__<repo>.json` under `data.workflows_dir`
  (`workflowmodel.ProvisionedLabels{repo, model name@version, hash, labels[]}`, through the
  `workflowmodel.ProvisionedStore` interface the workflow store implements). The record is the
  hive's memory of which labels *it* created or adopted by recolouring for a model; a label the
  repository already had for its own reasons is never in it.
- **Diff.** When the profile hash differs from the record's — a rebind, a new version of the bound
  model, or an unbind (lookup returns nil or hive-default) — the engine computes
  `previous − current` model labels.
- **Delete, only when safe.** A stale label is deleted only if it is in the record, is not in the
  built-in family (`stagelabels.LabelDefsFor` plus the retired spellings in `stagelabels.Renames`),
  and no open issue or PR wears it (one `label:"x" is:open` search per label; a failed search keeps
  the label and logs it). Anything kept stays in the record so the next pass retries it. Every
  deletion is audited as `workflow_label_removed` with the repository, label and the model key it
  belonged to.
- **Knob.** `workflow.label_gc` (`WorkflowConfig.LabelGC`, default `true`). `false` keeps every
  label and logs what would have gone; the record is still maintained, so turning it back on
  reclaims what accumulated. The knob is yaml-only (no Settings control; allowlisted in
  `settings_parity_allowlist.txt`).
- **Zero change for unbound repositories.** With no `ProvisionedStore` wired, or for a repository
  that was never bound, nothing is remembered and nothing is deleted; the pre-P4 path is unchanged
  and its tests are byte-identical.

### Model replies as human-gate commands

A model's `replies[state]` lists the verbs a human may answer the gate with. They now flow through
three places, all for a repository bound to a non-default model only:

- **Lookup.** `Profile.RepliesForNumber(stage)` returns the verbs for the state at a stage number;
  `Profile.ReplyForNumber(stage, verb)` resolves one to a `ReplyEffect{Verb, From, To, Label}` —
  the transition the model declares with that verb as its `on` event (if any), the target state,
  and the label `labels.entries` maps to that state (if any). `NormalizeReply` folds case, inner
  whitespace and a leading slash so "Approve Analysis", "approve  analysis" and "/approve analysis"
  are one verb.
- **Comments.** The `/hive` parser (`parseUnparkCommandFor`) tries the model's verbs first —
  multi-word, longest match wins, trailing punctuation trimmed, the rest of the line is the
  decision text — then the built-ins. With no verbs it *is* the old parser, so `/hive approve` on
  an unbound repository is exactly what it was. On a parked issue the sweep answers a model verb
  with `applyIssueModelReply`: the parking labels are cleared as for `/hive approve`
  (`approved-direction` added, `needs-human` lifted), the mapped label is applied when there is one
  (audited `hive_label_applied` with `reply=<verb>`), and one reply comment is posted that begins
  with the same `Decision:` sentence the decision history reads, then says what the model did
  ("moves the item from `a` to `b`; `label` applied" / "…the model maps no label to that state" /
  "is recorded; the model declares no transition for it"). Answered replies are idempotency
  watermarks like the built-ins. cmd/hive passes the stage engine's `WorkflowFor` to the un-park
  sweep so comments see the same binding the labeller does.
- **Dashboard.** The tracker payload's `workflows.repos[<repo>].replies` maps stage number →
  verbs (hive-default's block carries none). `stageTrackerHumanGateVerbs` appends them after the
  built-in verbs, deduped by label, as `wf:<verb>` actions; choosing one prompts for an optional
  note and POSTs `{"decision":"model-reply","reply":"<verb>","comment":…}` to
  `/api/repos/{repo}/items/{n}/decision`. The handler resolves the bound profile
  (`workflowProfileForRepo`), refuses unbound repositories and verbs the model does not offer at
  the item's state with 409, PRs with 400, and otherwise runs the same `applyIssueModelReply`
  path, audited `repo_item_decision_model_reply`.

### Known gaps (phase 4 follow-up)

- Model replies act on **parked issues** only. A verb declared for a PR state (TechDocs'
  `draft-review: approve / request changes`) is not offered by the pill or honoured in comments; PRs
  keep GitHub's native review signal. Un-parked issue states with replies are likewise not
  answered — the reply needs a gate to answer.
- The pill offers model verbs only where it already shows a human gate; a model state with replies
  but no `needs-human` / needs-decision label on the item shows nothing extra.
- Label GC trusts the provisioned record: labels created by an earlier hive build (before the
  record existed) are adopted into the record on the next provisioning pass only while the model
  still declares them; labels a model dropped *before* this change shipped are never reclaimed
  automatically.

## Ingest from cluster and from written specs (RFC phase 6)

Two more ways into a draft model, beside the repository ingest of phase 3a. Both end in the
same place — a `WorkflowModel` with `meta.provenance: ingested-unreviewed` that has been through
`Compile` — and both are owner-only, audited as `workflow_ingest`, and advertised by
`GET /api/workflows/ingest/capabilities` (`{cluster, clusterReason, contexts[], spec: {llm, model}}`)
so the Ingest view can grey out what the spoke cannot do.

### Tekton from a live cluster — `pkg/workflowmodel/ingest/cluster`

The hive has no Kubernetes client library; everything that talks to a cluster shells out to
`kubectl`, and this package does the same through a `Runner` seam (`Kubectl{Kubeconfig}` in
production, a table-driven fake in tests). `Fetch` runs
`kubectl [--context C] get <plural> -o json (-n NS | --all-namespaces) [-l SELECTOR]` for
`pipelines`, `tasks` and `pipelineruns` in `tekton.dev` and `eventlisteners`, `triggers`,
`triggertemplates` and `triggerbindings` in `triggers.tekton.dev`, keeps the newest
`RunsPerPipeline` runs per pipeline (default 3, max 20), serialises every object to YAML under
`cluster/<namespace>/<kind>/<name>.yaml` and hands that file set to the **existing** file-based
Tekton ingester. There is one Tekton parser; citations in the resulting model point at those
virtual paths.

Runs never become lifecycle elements. The last run per pipeline is reported in the response
(`runs[]: {pipeline, namespace, run, status, completed}`), recorded on the model as an
`observed` annotation (`workflow.hivecommons.dev/observed-run/<pipeline id>`) and as a
`CodeObservedRun` coverage finding — so a reviewer sees "this pipeline last Succeeded" without
the model claiming anything it did not read from a definition.

Failure modes map to explicit errors and statuses: no Tekton CRDs → `ErrNoCRD` (422), RBAC
denied → `ErrForbidden` (403), unknown context → `ErrNoContext` (404), no namespace and no
`allNamespaces` → `ErrNamespaceRequired` (400), no kubectl/kubeconfig on the spoke →
`ErrNoRunner` (422), caps exceeded → `ErrTooLarge` (413). Caps: 25 namespaces, 20 runs per
pipeline, 500 objects after run trimming, 8 MiB of YAML, 60 s per kubectl call.

```
POST /api/workflows/ingest/cluster {context?, namespace | allNamespaces, selector?, runsPerPipeline?, repository?, save?, name?}
→ {model, compiled, diagnostics, profile, files, pipelines, source: "cluster:<ctx>/<ns>", runs[], namespaces[], entry?}
```

### Written spec — `pkg/workflowmodel/ingest/spec`

Input is markdown or plain text up to 256 KiB — pasted, read from a repository's default branch
through the forge client, or fetched from an `https` URL on a public host (the contribute SSRF
guard applies). `Split` cuts it into heading-delimited sections with line ranges; a section's
reference is `spec:<heading>#L<start>-L<end>` and every citation in the output uses one.

**Stage 1 — deterministic, no LLM.** `Extract` reads numbered and bulleted step lists into
states in order; a small hint table maps words such as *triage*, *intake*, *request*, *review*,
*approve*, *merge*, *publish*, *deploy* onto column hints; *if / when / after / unless*
sentences within a step become transition guards; role nouns (*maintainer*, *reviewer*, *TOC*,
*author*, …) become roles, and a step that names a role becomes a gate released by it. The
skeleton always compiles: `Extract` is the fallback for everything below.

**Stage 2 — LLM refinement, optional.** When a model gateway is configured (the same reviewer
endpoint the owner-delegate decider and chat grounding use, through a `TurnRunner` with an
8 000-token completion budget) and the caller leaves `useLLM` on, `Ingest` prompts it with the
spec, the section references, the skeleton, a schema derived by reflection from the Go types,
the fact vocabulary and the compiler's refusal rules (`prompt.go`, `schema.go`), asking for one
Model JSON object. The answer is accepted only if it survives, in order:

1. `json.Unmarshal` with `DisallowUnknownFields` into `workflowmodel.Model`;
2. `Mark` — apiVersion/kind/name forced, `meta.version "1"`, `meta.source spec:<origin>`,
   `meta.provenance ingested-unreviewed`, **every** state, event and gate set to
   `origin: ingested` and `confidence: inferred`, every citation checked against the section
   references (unknown ones replaced by a whole-document reference and reported as
   `spec-llm-citation-replaced`), `pipelines`/`sources`/`agents` stripped;
3. `Compile`.

A failure at any step is fed back once (`MaxAttempts = 2`) as diagnostics appended to the
prompt; a second failure falls back to the skeleton and reports `spec-llm-fell-back`. When the
gateway's answer is used, the model carries the annotation
`workflow.hivecommons.dev/llm-assisted: "LLM-assisted (model <id>)"` so the editor can show it.

The boundary this keeps: **the LLM is consulted only here, only to draft, and only through the
`Completer` seam of this package**. Nothing in derivation, compile, the engine or the other
ingesters imports a gateway. Its output cannot carry `confidence: confirmed`, cannot cite
anything but the spec, cannot add pipelines, and cannot bypass the compiler; the worst it can do
is be ignored.

```
POST /api/workflows/ingest/spec {text | repo+path | url, useLLM?, repository?, save?, name?}
→ {model, compiled, diagnostics, profile, source: "spec:<origin>", llm: {used, model, attempts, accepted, error?}, sections[], entry?}
```

`useLLM` defaults to "a gateway is configured"; asking for it without one is a 422 rather than a
silent deterministic run.

### Ingest view

`static-src/js/config-workflows-ingest-sources.js` appends two collapsible panels to the Ingest
view through one guarded hook at the end of `renderWorkflowIngest`: *From a cluster* (context
from the capabilities route's list or a text field, namespace, selector, all-namespaces, the
observed-run list) and *From a written spec* (paste, URL or repository file; a *Use AI
assistance* checkbox rendered only when the spoke reports a gateway; the LLM note and section
list in the result). Both reuse the repository ingest's result block, diagnostics and diagram.

## Bundles and marketplace (RFC phase 7)

Phase 7 lets a model leave the hive it was authored on. Three pieces: a signed
**bundle** format (`pkg/workflowmodel/bundle`), an immutable **catalogue** on the
hub (`pkg/hub/marketplace*.go`), and the spoke's **import / publish / browse**
surface (`pkg/dashboard/api_workflows_marketplace.go`, Settings → Workflows →
Marketplace).

### Bundle — `pkg/workflowmodel/bundle`

```
{ apiVersion: "workflow.hivecommons.dev/v1alpha1-bundle", kind: "WorkflowBundle",
  model: <Model>, profile: <compiled Profile>, hash: <sha256>, createdAt, createdBy, tier }
```

- `Export(m, by, now)` compiles the model and refuses on compile errors; the hash is
  `CanonicalHash(m)` — sha256 of the model's canonical JSON (sorted keys, no
  insignificant whitespace, `apiVersion/kind/metadata/spec` only).
- `Encode`/`Decode` are canonical and strict (`DisallowUnknownFields`); the bundle
  bytes themselves are deterministic so a store can compare them byte-for-byte.
- `Verify(b)` recomputes the hash **and recompiles the model locally**, comparing the
  result structurally with the published profile. A mismatch is
  `ErrProfileMismatch` ("published profile does not match local compile"). Nothing
  downstream ever trusts a published profile: the receiving side always recompiles.
- Tiers are provenance for the catalogue: `built-in`, `curated`,
  `ingested-unreviewed`, `local`, `local-published`. `Export` derives the default tier
  from `spec.meta.provenance`.
- `IndexEntry`/`Index` are the shared wire types for the catalogue index (below) so the
  hub and spoke agree without the hub importing dashboard code.

### Hub catalogue — `pkg/hub/marketplace*.go`

Storage is a directory under the hub data dir:

```
/data/marketplace/
  workflows/<name>/<version>.bundle.json   immutable; refuse overwrite of name@version
  index.json                               derived; rebuilt from the bundles if missing
  refresh/<owner>__<repo>.json             last ingest run per repository
  last-refresh.json                        summary of the last completed run
```

Entries are keyed `name@version` plus hash. Publishing the identical bundle again is a
no-op (`200`); different content under an existing key is `409`. The index carries
`{name, version, hash, tier, description, source, states, columns, publishedAt,
publishedBy, sourceSHA?, newerIngestAvailable, adoption:{}, ratings:{}, benchmarks:{}}`;
the last three are reserved and empty today. `ETag` is the sha256 of the index bytes.

Routes (see [hub-server.md](../api-reference/hub-server.md)):

| Route | Who | Notes |
|---|---|---|
| `GET /api/marketplace/workflows` | public | index; `If-None-Match` → `304` |
| `GET /api/marketplace/workflows/{name}/{version}/bundle` | public | one bundle, `Cache-Control: immutable` |
| `POST /api/marketplace/workflows` | spoke bearer or hub admin | publish; hub re-verifies |
| `POST /api/marketplace/refresh[?repo=]` | spoke bearer or hub admin | on-demand ingest; spokes rate-limited (`429`) |
| `GET /api/marketplace/status[?verbose=1]` | public (`verbose`: spoke bearer or hub admin) | last refresh, counts by tier, failing repos with their errors; `verbose` adds pending repos |

**Who may publish what.** A spoke authenticates with its heartbeat bearer bound to the
`X-Hive-ID` header (the same per-hive key the heartbeat and NPS lanes verify) and may
assert `local`/`local-published` (normalised to `local-published`) or
`ingested-unreviewed`. `curated` and `built-in` require a hub-admin session with
same-origin CSRF evidence. The hub never stores a bundle it did not itself `Verify`.

**Curated seeding.** Reviewed models ship with the hub binary under
`pkg/hub/marketplace/curated/*.yaml` (`go:embed`; loader `curated.Load`). On startup —
before any ingest, and without a GitHub token — and again at the start of every refresh
run, `seedCuratedModels` decodes each file strictly (unknown fields refused), checks the
curated invariants (`spec.meta.provenance: curated`, a `spec.meta.version`, zero
`inferred` elements), compiles it through `bundle.Export`, sets `Tier=curated` and
`Publish`es it when that `name@version` is absent. A file that fails any step is logged
(`marketplace: curated model refused` / `does not compile` / `not published`) and
skipped; the rest of the catalogue still seeds and the error appears in the run's
`errors` as `curated: …`. Seeded keys are reported in `lastRefresh.curated`, not
`published`. Entries are immutable: re-running is a no-op, and editing a file without
bumping its version is a `409` conflict at seed time, so **a new version is a new file**
(`cncf-techdocs-assessment.yaml` carries `curated-2026.11.1`; the next revision is a
second file with `curated-2026.12.1` or similar — the catalogue keeps both and the spoke
pins what it imported). To add a curated model: write the YAML (start from
`ingest.Lifecycle` + `spec.Ingest` output, confirm every inferred element, cite the
source document in `workflow.hivecommons.dev/source*` annotations and the element
`citations`), drop it in that directory, and add rules-evaluator cases to
`curated_test.go` — `TestLoadEmbedded` already refuses anything that does not compile
clean. The first curated model is the CNCF TechDocs assessment,
[docs/workflows/techdocs-assessment.md](../workflows/techdocs-assessment.md).

**Refresh.** The corpus is the CNCF survey list, now embedded as
`pkg/workflowmodel/ingest/corpus` (`corpus.json`, 262 projects / 299 repositories;
`cmd/workflow-survey` falls back to it when no `projects.json` is given) plus the
operator-added non-landscape list `extra.json` in the same package (`{"name",
"category", "repos", "why"}` per entry; `cncf/techdocs` is there so its
`ingested-unreviewed` version exists next to the curated one) plus
`HIVE_HUB_MARKETPLACE_EXTRA_REPOS`. For each repository the hub fetches the default-branch
tarball through its GitHub identity and streams it (`ingest.ReadTarball`): only the paths
`ingest.ReadTree` would select are kept (per file ≤ 1 MiB, ≤ 32 MiB in total), everything
else is discarded as it streams, so a monorepo's source size never matters. It then
records `{format, sourceSHA, filesHash, files, checkedAt}`. Unchanged hash → no-op. Changed →
`ingest.Lifecycle` → `bundle.Export` → publish as `ingested-YYYYMMDD.N`, tier
`ingested-unreviewed`, with the source SHA in an annotation. Curated versions are never
touched; `marketplaceNormaliseIndex` flags a curated entry `newerIngestAvailable` when a
later ingest of the same name exists. The crawl is sequential in sorted repository order
with a short pause between repositories, logs progress every 25, honours
`X-RateLimit-Remaining`/`Reset` and sleeps on a `403`/`429` with `Retry-After` (secondary
limits). It runs nightly at `HIVE_HUB_MARKETPLACE_REFRESH_SCHEDULE` (HH:MM UTC, default
`03:00`, empty disables) and on demand. `HIVE_HUB_MARKETPLACE_DISABLED` turns the routes off
(`404`).

**Seeding is resumable.** Each repository's record is persisted as the crawl goes, so a
run interrupted by a hub restart (releases roll several times a day) loses nothing. At
start, after a short delay and when a GitHub token is present, the hub ingests every corpus
repository that is *due*: no record, a record older than the nightly interval (24 h) or in
an older `format`, or an error older than one hour. A hub that restarts repeatedly
therefore only re-crawls what is missing instead of hammering GitHub, and a catalogue that
seeded one entry before a roll fills in on the next boot. Bumping
`marketplaceRecordFormat` forces a full re-ingest when the ingest itself changes (format 2:
tarball extraction recognises git's `pax_global_header`, without which the wrapped
directory was not detected and almost every repository read as having no pipeline files).
The last run's summary is persisted (`last-refresh.json`) so `status.lastRefresh` survives a
restart.

**Refresh auth.** `POST /api/marketplace/refresh` takes the same credentials as publish: a
hub-admin session (same-origin CSRF evidence) or a registered spoke's heartbeat bearer +
`X-Hive-ID`, so a hive owner's **Refresh catalogue** button works through the spoke proxy.
Spokes are rate-limited across all hives — one full run per hour, ten `?repo=` runs per
minute — and get `429` with `Retry-After`; admins are not limited. A run already in flight
is `409`. The spoke relays the hub's `401/403` as `403`, keeps `429`/`409`, and maps other
failures to `502`, always with the hub's message in the body.

### Spoke — import, publish, browse

Config (`hive.yaml`):

```yaml
workflow:
  marketplace:
    enabled: true          # default true; false hides the tab and 404s the routes
    url: ""                # default: hub.url
    refresh_interval: 15m  # how long the spoke trusts its cached index before revalidating
```

Routes (`pkg/dashboard/api_workflows_marketplace.go`):

- `GET /api/workflows/{name}/{version}/bundle` — export a stored model as a bundle (any role).
- `POST /api/workflows/import` (owner) — body is a bundle; `Verify` → recompile → `store.Save`
  pinned to the bundle's version when free, else the next free version; `201 {entry,
  verified:true}`; `422` when verification fails. Audit `workflow_import`.
- `GET /api/marketplace/workflows` — proxy to the hub with `If-None-Match`; response
  `{index, stale, etag, fetchedAt, error?}`. Hub unreachable → `502` with the cached index
  and `stale:true` so the UI can still render.
- `POST /api/marketplace/workflows/{name}/{version}/import` (owner) — fetch the bundle from
  the hub and import as above. Audit `workflow_import`.
- `POST /api/marketplace/workflows/publish {name, version}` (owner) — export and POST to
  the hub with the spoke's heartbeat credential; `201 {published, hash, tier}`. Audit
  `workflow_publish`.
- `POST /api/marketplace/refresh[?repo=]` (owner) — forwards to the hub with the spoke's
  heartbeat credential; the hub's `429` (rate limit, with `Retry-After`) and `409` (run in
  progress) pass through with its message, `401/403` → `403`. Audit
  `workflow_marketplace_refresh`.

Imported models are pinned `name@version`; a newer catalogue version is a new library entry
and **bindings never move automatically** — rebinding stays an explicit act on Settings →
Repos. Built-in names cannot be imported over.

UI: Settings → Workflows → **Marketplace** (`config-workflows-marketplace.js`) lists the
catalogue with tier badges, search, a tier filter, the `newer ingest available` flag,
reserved Adoption/Rating columns (rendered `—`), **Import** (→ verified, compiled, library
entry opened), **Reload**, and for owners **Refresh catalogue**; library rows and the detail
view gain **Publish** (confirmed via `hiveConfirm`). A stale banner shows when the proxy
served cached data.

### Known gaps (phase 7)

- `adoption`, `ratings`, `benchmarks` are reserved and empty; nothing collects them yet.
- Hub-side marketplace settings are environment variables, not hub YAML.
- The hub logs publishes/refreshes through its structured logger; it has no audit store.
- Ingest versions are per-day sequence numbers, not semantic; curation (promoting an ingest
  to `curated`) is a manual admin publish of a reviewed model or a new file under
  `pkg/hub/marketplace/curated/`.

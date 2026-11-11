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

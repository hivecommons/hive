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

# Nous → Spek: the winning experiment as a `spek-new` source

**Status: proposed — design only, no Hive change.** A straw man for the
conversation with the Nous team that
[#17020](https://github.com/hivecommons/hive/issues/17020) asks for. It records
what each side already produces and reads, where those don't meet, and the
questions the Nous team has to answer before anyone writes code. Nothing here
is current behaviour beyond the "What exists today" section.

## Goal

`spek-new` already starts a spec from existing material and records where it
came from in `sources[]`. The missing input is Nous: the experiment variant
(code + config) that measured best should become a source, so the spec is
written from evidence of what worked. The spec then leaves through the existing
publish hand-off (RFC + children → Hive, see
[Spektacular: Publish hand-off](../spektacular.md#publish-hand-off)).

Prior art: [#13579](https://github.com/hivecommons/hive/issues/13579) (Experiment
Lab → Spektacular, closed) asked for the same hand-off as two dashboard buttons;
[spektacular#109](https://github.com/hivecommons/spektacular/issues/109) lists
Nous → Spek as out of scope for publishing.

## What exists today

### Spek: what a source is

`spektacular spec new --data '{"name": …, "sources": [{"uri": "<stable link>"}]}'`
is the whole input contract. A source is **one URI**; the CLI stamps its
retrieval date, and the `spek-new` skill has the agent read the material behind
the link and seed every section it can. The skill recognises an issue, epic,
design document, file, web page or pasted text. It has no `--from nous:<id>`
form and no notion of a Nous experiment.

### Nous: what a campaign leaves on disk

The experiment loop is the external Nous framework
([AI-native-Systems-Research/agentic-strategy-evolution](https://github.com/AI-native-Systems-Research/agentic-strategy-evolution)),
installed at the commit pinned in `bin/nous-install.sh`. At that pin a
campaign's work directory already holds most of what a "winning experiment"
needs to say:

| File | What it says about the winner |
|---|---|
| `best_found.json` | `top_k[]` ranked by composite score: `candidate_id`, `score`, per-metric `components`, `iteration`, `arm_id`. `top_k[0]` is the best candidate so far. |
| `meta_findings.json` (campaign done) | `deployment_recommendation`: `verdict` (`deploy` / `deploy_with_caveats` / `fall_back_to_baseline`), `top_candidate_id`, `score`, `citations[]`, `caveats[]`; plus `campaign_design_lessons`, `target_system_asks`, `nous_asks`, each citing a moment in the campaign. |
| `runs/iter-<n>/findings.json` | per-iteration prediction-vs-outcome: `bundle_ref`, `arms`, `experiment_valid`, `discrepancy_analysis`. |
| the iteration's bundle | each arm's hypothesis and optional `code_changes[]` of `{file, intent, rationale}` — **intent in plain English, not a patch**. |
| `ledger.json` | one row per iteration: `iteration`, `candidate_id`, `h_main_result` (`CONFIRMED` / `REFUTED` / `PARTIALLY_CONFIRMED`), ablation/control/robustness results, `principles_extracted`, optional `domain_metrics`. |
| `principles.json` | principles with `statement`, `confidence`, `regime`, `evidence`, … |

So "the winning experiment" is not one artifact yet: it is
`deployment_recommendation.top_candidate_id`, resolved against `best_found.json`
for its score, against the findings and bundle for its hypothesis and code
intent, and against the ledger for what it confirmed.

### Hive: what it reads from Nous

- **Two files, once at boot.** `loadNousStateFromPaths` (`cmd/hive/main.go`)
  reads `ledger.json` (`iterations[]`) and `principles.json` from
  `/var/run/nous/governor/`; a new row shows up after a restart. It reads
  nothing else from the work directory — not `best_found.json`, not
  `meta_findings.json`. See
  [Experiment Lab: The experiment loop](../experiment-lab.md#the-experiment-loop).
- **Ledger rows pass through untouched.** `GET /api/nous/ledger` (dashboard
  auth) returns each row as the loop wrote it, minus Hive-self rows
  (`nousRepoOnlyLedger`). Extra keys the loop writes are already served.
- **The Results view expects a different vocabulary.** `nousExperimentRow`
  (`pkg/dashboard/api_nous_progress.go`) looks for `id`/`experiment_id`/`name`,
  `title`, `hypothesis`, `repo`, `status`/`state`/`type`, `verdict`, `delta`
  and `pr_url`/`issue_url`/`pr_urls`/`issue_urls`, and `nousExperimentProgress`
  skips every row without a `type`. A row in the framework's own ledger schema
  has none of these keys, so it never appears as a judged result: the two
  ledgers share a file name, not a shape.
- **Principles link to an experiment by `category`.** `nousOutputs` attaches a
  principle to a row when `NousPrinciple.Source` equals the row id, and
  `loadNousStateFromPaths` fills `Source` from the principle's `category`.
- **Campaigns are Hive's own record.** `pkg/nous` keeps the operator's
  research question, hypothesis and scope in `/data/nous/campaigns.json`, and
  freezes `outcome.results` when a campaign is archived. Hive does not know
  which framework campaign or run answered which Hive campaign.

## Gaps

1. **No single artifact.** The winner is spread over four files; nothing
   assembles it.
2. **No stable URI.** Spek takes a link. The work directory lives in the hive
   pod (`/var/run/nous/<scope>/`), which a standalone Spek cannot reach, and the
   dashboard API needs a session.
3. **No real change.** `code_changes` are intents. The actual diff exists only
   if an output mode opened a PR (recorded as `pr_url` on a Hive-shaped row).
4. **No link from a Hive campaign to the framework run** that produced the
   winner, so "the experiment for this question" cannot be looked up.

## Straw man: the winning-experiment artifact

One document per winning candidate, readable by a person and by the
`spek-new` agent, each field landing in a known spec section:

| Field | Source today | Seeds spec section |
|---|---|---|
| id `<run_id>/<candidate_id>` | `meta_findings.run_id`, `top_candidate_id` | provenance |
| question, hypothesis, scope repos | Hive campaign; arm hypothesis in the bundle | Overview / Problem |
| verdict, score, baseline score, metric components | `deployment_recommendation`, `best_found.json` top_k[0], `pre_work.json` | Success metrics |
| what changed: file + intent + rationale, and the PR/diff link when one exists | bundle `code_changes`, row `pr_url` | Requirements; plan seed |
| caveats, refuted ablations, `experiment_valid: false` iterations | `deployment_recommendation.caveats`, ledger, findings | Constraints / risks |
| principles extracted | ledger `principles_extracted` → `principles.json` | Context |
| citations | `deployment_recommendation.citations` | evidence links |

`fall_back_to_baseline` is not a winner: the artifact is only produced for
`deploy` and `deploy_with_caveats`.

## How `spek-new` could get it

Spek's contract is a URI, so the question is where that URI points.

| Option | Changes | Works standalone? | Cost |
|---|---|---|---|
| **A. Nous publishes the artifact as a GitHub issue** (or the PR its output mode already opens) and `spek-new` starts from that link | Nous framework / runner only | yes | none in Hive or Spek; GitHub stays the bus, as in the publish hand-off |
| **B. Hive serves it** (e.g. `GET /api/nous/experiments/{id}`) and `spek-new` gets the dashboard URL | Hive (new endpoint, read of `meta_findings.json` / `best_found.json`) | no — needs a dashboard session | new auth path for a non-browser client |
| **C. `nous:<id>` scheme** that the `spek-new` skill resolves by reading the work directory | Spek skill + resolver | only co-located with the loop | couples Spek to the framework's file layout |

This page proposes **A** as the first step: it needs no change in Hive or Spek,
and the same link works for Spek running in-hive or standalone. `spek-new
--from nous:<id>` (C) can be added later as sugar that resolves to the
published link. B is only worth building if the artifact must stay private to
the hive.

## Open questions for the Nous team

1. Is `deployment_recommendation.top_candidate_id` the right definition of
   "winning", or should a person confirm it first (a Findings Review gate
   decision)?
2. Should the framework emit the assembled artifact itself (a
   `winning_candidate.json` next to `meta_findings.json`), or should the runner
   assemble it?
3. Can a winning arm carry a real patch or a branch, not only `code_changes`
   intents?
4. Where does the published artifact live: the target repo, a Nous results
   repo, or the hive's work source?
5. What id joins a Hive campaign to a framework run (`run_id`?), so the
   dashboard can link the experiment card to its spec?
6. Should the Hive-shaped ledger keys (`id`, `type`, `verdict`, `pr_url`) be
   written by the framework, by `bin/nous-sync.py`, or replaced by Hive reading
   the framework's schema?

## What Hive would change, once agreed

Nothing for option A. Separately from this hand-off, the vocabulary mismatch
under "What exists today" (and question 6) keeps rows in the framework's own
ledger schema out of the Experiment Lab Results view today.

# Weekly competitor report grounding

The source is `.github/workflows/weekly-competitor-report.md`. Its lock file is
generated, not edited. Use the compiler version pinned in
`.github/workflows/gh-aw-compile.yml` (currently v1.0.1):

```sh
gh aw compile weekly-competitor-report
```

The compiler migration is tracked in #16226 and shipped in #16227. This report
does not independently upgrade compiler, runtime, actions, or container pins.

## Preparation

Before inference, `.github/scripts/competitor_report.py prepare` reads Git
objects at the actual checkout SHA, verifies `GITHUB_SHA`, and records the
repository API's actual default branch separately. A manually dispatched
revision can differ from the default branch tip. A sparse working tree does
not prevent reading fetched Git objects.

The bounded source closure contains upstream Crux configuration, forge
interfaces and adapters, governor construction/enumeration callers, integration
and operational-limit docs, license text, README, and the latest three dated
outcome benchmarks. It is a list of source locations, not frozen product facts.
If a required path moves or budgets are exceeded, preparation fails explicitly.
`report-source.md` provides line-numbered documentation and bounded Go excerpts;
`report-source.json` contains the selected full files and a path inventory.
Neither is a permanent capability registry or a credential store.

Preparation includes all-state issue/PR topic searches and earlier forge design
decisions #6170/#6177. These and all source/web contents are data, never agent
instructions. The existing network allowlist, secrets and engine policy remain
unchanged.

## Publication gate

The report uses identical visible text for GitHub and Discord. The GitHub body
also ends with a fenced JSON provenance ledger. HTML comments cannot carry this
ledger because gh-aw removes hidden comments. Exact source quotes travel as
base64 UTF-8 data so URL, mention and template sanitization cannot alter quoted
bytes; no decoded content is executed.

The trusted safe-output pre-step checks out `github.sha` with credentials
disabled and rebuilds evidence independently of the model's mutable workspace.
It validates both deliveries before the built-in comment/issue handlers run.
Discord explicitly requires `safe_outputs.result == 'success'`; merely adding
`needs` is insufficient because gh-aw's generated custom-job condition uses
`!cancelled()`. The compiler infers **contents: read** in `safe_outputs` and its
conclusion job from the new checkout; no new write permission is granted.

Checks include revision/path/line validity, exact quotations, dated metric
provenance with visible cohort/window/caveat phrases, primary-news attestations,
uncertainty labels, matching report text, and repeated bounded all-state work
searches for each recommendation. Existing proposed paths and matching work
block "new" tasks; follow-ups/deferred items must cite actual matching work.
Optional issues remain off by default and must exactly reproduce a grounded
report item. Missing, invalid or incomplete output fails the run, rather than
publishing an empty success. Reports stay on tracking issue #14613; if it is
unavailable, publication fails rather than creating a misclassified archive.

These are **mechanical provenance checks, not semantic proof**. A correct
quotation can still be misinterpreted. The researcher must read implementation
and operational limitations together, derive current worker engines upstream
in `src/crux`, and confirm primary news. Search matches are bounded text
deduplication, not proof that semantically equivalent work does not exist.
Unconfirmed news cannot support automated work. There is deliberately no
unauthenticated HEAD requirement for every external URL.

## Regression verdict

The existing **gh-aw on-ramp** CI job runs offline fixtures and two canonical
compiles with a clean generated diff. Fixtures cover the reported multi-forge
distinction, default-v5 versus dispatched-SHA handling, stale `blob/main` and
moving-branch links, frozen prompt assertions, missing paths/quotes, license
detection ambiguity, historical cost attribution, existing/deferred work,
unsupported news, issue opt-in, empty reports, and both publication gate edges.
Repository policy reserves executable tests for CI; do not run the suite locally.

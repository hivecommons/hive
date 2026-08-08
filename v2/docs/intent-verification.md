# Intent verification

Intent verification adds a deterministic authorization layer between “CI is green” and “Hive may merge this PR”. It classifies agent-authored PRs into change tiers, gathers authorization evidence, and checks whether the actual diff still matches the stated/authorized intent.

## Change tiers

| Tier | Change shape | Authorization rule |
| --- | --- | --- |
| Tier 0 | Additive docs-only or test-only changes (`*.md`, test directories, `*_test.go`, `*.test.*`, `*.spec.*`) | Pre-authorized |
| Tier 1 | Bugfix/chore/default code changes, including test weakening | Requires a linked issue |
| Tier 2 | Feature/enhancement changes | Requires an approved plan or maintainer approval |
| Tier 3 | Strategic or guardrail-critical changes | Requires maintainer approval |

Test-only changes are Tier 0 only when additive. Deleted test files or deleted lines in test files are treated as test weakening and escalated to Tier 1.

Tier 3 applies when a PR touches guardrail-critical paths such as GitHub workflows, the `gh` wrapper, proxy rules, `policies/`, `hive.yaml`, `OWNERS`, or `CODEOWNERS`. These patterns have built-in defaults and can be overridden in configuration.

## Evidence rules

- **Linked issue**: a PR body closing reference such as `Fixes #123`, `Closes owner/repo#123`, or a bead with an external issue reference.
- **Approved plan**: a bead lineage carrying `plan_status=approved`, matching the planning-intelligence plan-review record.
- **Human approval**: an explicit `APPROVED` pull-request review from a maintainer association (`OWNER`, `MEMBER`, or `COLLABORATOR`).

Authorization evaluates the tier and evidence into an `authorized` boolean plus a human-readable reason. Non-agent PRs are reported but are not gated by this phase.

## Intent alignment

After authorization, Hive builds a bounded `AlignmentContext` for each agent-authored PR:

- authorized intent text from linked issue titles/bodies;
- matching bead titles/notes, including approved plan epics and child beads when present;
- the PR title/body; and
- a capped diff summary containing changed filenames plus additions/deletions.

The model prompt context is intentionally small (`MaxAlignmentFiles` and `MaxAlignmentBytes` in `pkg/intent`) so the merge-gate path stays predictable. Deterministic enforcement still evaluates the complete changed-file list; if GitHub reports more changed files than Hive can retrieve, evidence fetch fails closed instead of silently passing a partial diff.

Deterministic checks always run:

- **Scope drift** flags changed files whose path segments/domains are not referenced by the authorized intent or PR description. For example, a docs-only intent that changes `pkg/proxy/` is reported as misaligned.
- **Tier-0 escape** flags a Tier-0-classified PR when the diff includes anything outside configured docs/test paths. This protects against classifier drift.

Findings are appended under the optional `alignment` field in each `intent-verdicts.json` verdict. The field is omitted for old/legacy verdicts, so existing readers remain compatible.

### Optional model review

Set `intent.alignment_model` to enable a second-model alignment reviewer. Hive reuses the governor reviewer endpoint/key resolution and sends the bounded alignment context to an OpenAI-compatible `/v1/chat/completions` model. The model returns `aligned`, `misaligned`, or `unclear` with a one-sentence rationale.

Model review fails open: endpoint, transport, or parse errors are recorded in the verdict but never block merge eligibility by themselves.

## Report-only vs enforce

Intent verification is report-only by default. With no `intent:` block, Hive writes `/var/run/hive-metrics/intent-verdicts.json` and logs denials, but `merge-eligible.json` is unchanged.

To enforce intent authorization in the merge gate:

```yaml
intent:
  enforce: true
```

When enforcement is enabled, unauthorized agent-authored PRs are excluded from `/var/run/hive-metrics/merge-eligible.json`, so the merge relay denies them through the existing merge-eligible binding.

A `misaligned` deterministic or model alignment verdict is treated the same as unauthorized when `intent.enforce: true`: the PR is excluded from `merge-eligible.json`. In report-only mode, Hive only records the verdict and creates an advisory bead describing the drift.

Optional pattern overrides:

```yaml
intent:
  alignment_model: "openai/gpt-4o-mini" # optional; empty/omitted disables model review
  test_path_patterns: ["**/*_test.go", "tests/**"]
  docs_path_patterns: ["**/*.md"]
  guardrail_path_patterns: [".github/workflows/**", "policies/**", "hive.yaml", "OWNERS", "CODEOWNERS"]
  feature_signals: ["feat", "feature", "enhancement"]
```

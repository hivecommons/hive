# Intent verification

Intent verification adds a deterministic authorization layer between “CI is green” and “Hive may merge this PR”. Phase 1 classifies agent-authored PRs into change tiers, gathers authorization evidence, and reports a verdict for the merge gate. The trajectory-based intent-alignment diff review is deferred to a follow-up.

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

## Report-only vs enforce

Intent verification is report-only by default. With no `intent:` block, Hive writes `/var/run/hive-metrics/intent-verdicts.json` and logs denials, but `merge-eligible.json` is unchanged.

To enforce intent authorization in the merge gate:

```yaml
intent:
  enforce: true
```

When enforcement is enabled, unauthorized agent-authored PRs are excluded from `/var/run/hive-metrics/merge-eligible.json`, so the merge relay denies them through the existing merge-eligible binding.

Optional pattern overrides:

```yaml
intent:
  test_path_patterns: ["**/*_test.go", "tests/**"]
  docs_path_patterns: ["**/*.md"]
  guardrail_path_patterns: [".github/workflows/**", "policies/**", "hive.yaml", "OWNERS", "CODEOWNERS"]
  feature_signals: ["feat", "feature", "enhancement"]
```

## Deferred alignment check

This phase does not compare the final diff against the issue, bead, or approved plan text. That trajectory-integrated intent-alignment check is intentionally deferred to a follow-up so Phase 1 remains deterministic and merge-gate focused.

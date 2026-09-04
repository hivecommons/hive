package dashboard

// ACMMCriterion defines a single ACMM evaluation criterion that can be
// checked against a repository via file-existence detection.
type ACMMCriterion struct {
	ID       string
	Source   string // "acmm" or "aef"
	Level    int    // 0, 2, 3, 4, 5, 6
	Category string
	Name     string
	Patterns []string // file paths to check (pass if ANY exists)
}

// acmmLevelNames maps numeric ACMM levels to human-readable names. Levels
// 1-6 mirror the canonical pack names in src/pkg/config/packs/level-*.yaml.
// Level 0 is not a level of its own: it means the repo has not yet met the
// prerequisites for L1.
var acmmLevelNames = map[int]string{
	0: "Prerequisites not met",
	1: "Inception",
	2: "Advisory",
	3: "Quality-Gated",
	4: "Security-Aware",
	5: "Semi-Autonomous",
	6: "Fully Autonomous",
}

// universalCriteria contains the 43 repo-universal ACMM criteria ported
// from the console scan API (acmm:* and aef:* sources). CNCF-specific
// criteria (fullsend:*, claude-reflect:*) are intentionally excluded so
// that hive spokes running against private/internal repos get meaningful
// evaluations.
var universalCriteria = []ACMMCriterion{
	// ── L0 Prerequisites (8) ──
	{ID: "acmm:prereq-test-suite", Source: "acmm", Level: 0, Category: "prerequisite", Name: "Test suite",
		Patterns: []string{"vitest.config.ts", "vitest.config.js", "jest.config.js", "jest.config.ts", "go.mod", "pytest.ini", "pyproject.toml", "test/", "tests/", "__tests__/", "spec/"}},
	{ID: "acmm:prereq-e2e", Source: "acmm", Level: 0, Category: "prerequisite", Name: "E2E tests",
		Patterns: []string{"playwright.config.ts", "playwright.config.js", "cypress.config.ts", "e2e/", "web/e2e/", "web/playwright.config.ts", "web/playwright.config.js", "test/e2e/", "tests/e2e/"}},
	{ID: "acmm:prereq-cicd", Source: "acmm", Level: 0, Category: "prerequisite", Name: "CI/CD pipeline",
		Patterns: []string{".github/workflows/", ".gitlab-ci.yml", "Jenkinsfile", ".circleci/"}},
	{ID: "acmm:prereq-pr-template", Source: "acmm", Level: 0, Category: "prerequisite", Name: "PR template",
		Patterns: []string{".github/pull_request_template.md", ".github/PULL_REQUEST_TEMPLATE.md"}},
	{ID: "acmm:prereq-issue-template", Source: "acmm", Level: 0, Category: "prerequisite", Name: "Issue templates",
		Patterns: []string{".github/ISSUE_TEMPLATE/", ".github/issue_template.md"}},
	{ID: "acmm:prereq-contrib-guide", Source: "acmm", Level: 0, Category: "prerequisite", Name: "Contributing guide",
		Patterns: []string{"CONTRIBUTING.md", "docs/contributing.md", ".github/CONTRIBUTING.md"}},
	// Patterns must span the languages hive evaluates, not just JS/Python/Go:
	// this is an L0 prerequisite, so a language whose linter is missing here
	// pins the repo below Level 0 permanently and re-files the gap issue on
	// every run. Keep this list to explicit formatter/linter config files;
	// broad project manifests would make the criterion match repos with no
	// code-style configuration at all.
	{ID: "acmm:prereq-code-style", Source: "acmm", Level: 0, Category: "prerequisite", Name: "Code style config",
		Patterns: []string{".eslintrc", ".eslintrc.json", ".eslintrc.js", "eslint.config.js", "eslint.config.mjs", ".prettierrc", ".prettierrc.json", "prettier.config.js", "ruff.toml", ".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json", "biome.json", ".shellcheckrc", ".editorconfig", ".clang-format", "checkstyle.xml", "config/checkstyle/checkstyle.xml", ".rubocop.yml", ".rubocop.yaml", "rustfmt.toml", ".rustfmt.toml", "clippy.toml", ".clippy.toml", ".swiftlint.yml", ".stylelintrc", ".stylelintrc.json", ".stylelintrc.yaml", ".stylelintrc.yml"}},
	{ID: "acmm:prereq-coverage-gate", Source: "acmm", Level: 0, Category: "prerequisite", Name: "Coverage gate",
		Patterns: []string{"codecov.yml", ".codecov.yml", ".github/workflows/coverage-gate.yml", "coverage.yml", ".coverage-thresholds.json"}},

	// ── L2 Advisory (8) ──
	{ID: "acmm:claude-md", Source: "acmm", Level: 2, Category: "feedback-loop", Name: "CLAUDE.md instruction file",
		Patterns: []string{"CLAUDE.md", ".claude/CLAUDE.md"}},
	{ID: "acmm:copilot-instructions", Source: "acmm", Level: 2, Category: "feedback-loop", Name: "Copilot instructions",
		Patterns: []string{".github/copilot-instructions.md"}},
	{ID: "acmm:agents-md", Source: "acmm", Level: 2, Category: "feedback-loop", Name: "AGENTS.md",
		Patterns: []string{"AGENTS.md"}},
	{ID: "acmm:cursor-rules", Source: "acmm", Level: 2, Category: "feedback-loop", Name: "Cursor rules",
		Patterns: []string{".cursorrules", ".cursor/rules"}},
	{ID: "acmm:prompts-catalog", Source: "acmm", Level: 2, Category: "feedback-loop", Name: "Prompts catalog",
		Patterns: []string{"prompts/", ".prompts/", "docs/prompts/", ".github/prompts/", ".github/agents/"}},
	{ID: "acmm:editor-config", Source: "acmm", Level: 2, Category: "feedback-loop", Name: "EditorConfig",
		Patterns: []string{".editorconfig"}},
	{ID: "acmm:simple-skills", Source: "acmm", Level: 2, Category: "feedback-loop", Name: "Simple skills",
		Patterns: []string{".claude/skills/", ".claude/commands/", "skills/"}},
	{ID: "acmm:correction-capture", Source: "acmm", Level: 2, Category: "learning", Name: "Correction capture",
		Patterns: []string{".claude/memory/", ".memory/", "corrections.jsonl"}},

	// ── L3 Quality-Gated (8) ──
	{ID: "acmm:pr-acceptance-metric", Source: "acmm", Level: 3, Category: "feedback-loop", Name: "PR acceptance metric",
		Patterns: []string{"scripts/build-accm-history.mjs", ".github/workflows/accm-history-update.yml", "scripts/pr-metrics.mjs", ".github/workflows/pr-metrics.yml", "docs/metrics.md"}},
	{ID: "acmm:pr-review-rubric", Source: "acmm", Level: 3, Category: "feedback-loop", Name: "PR review rubric",
		Patterns: []string{".github/workflows/review.yml", "docs/review-rubric.md", ".github/review-checklist.md", ".github/prompts/review.md", "docs/qa/"}},
	{ID: "acmm:quality-dashboard", Source: "acmm", Level: 3, Category: "observability", Name: "Quality dashboard",
		Patterns: []string{"public/analytics.js", "web/public/analytics.js", "web/src/components/analytics/", "docs/quality.md", ".github/workflows/quality-report.yml", "docs/AI-QUALITY-ASSURANCE.md"}},
	{ID: "acmm:ci-matrix", Source: "acmm", Level: 3, Category: "feedback-loop", Name: "CI test matrix",
		Patterns: []string{".github/workflows/ci.yml", ".github/workflows/test.yml", ".github/workflows/build.yml"}},
	{ID: "acmm:layered-safety", Source: "acmm", Level: 3, Category: "governance", Name: "Layered safety model",
		Patterns: []string{".claude/settings.json", ".claude/settings.local.json"}},
	{ID: "acmm:mechanical-enforcement", Source: "acmm", Level: 3, Category: "governance", Name: "Mechanical enforcement",
		Patterns: []string{".claude/settings.json"}},
	{ID: "acmm:session-summary", Source: "acmm", Level: 3, Category: "learning", Name: "Session summary artifact",
		Patterns: []string{".claude/session-summary.md", ".claude/checkpoint.md"}},
	{ID: "acmm:structural-gates", Source: "acmm", Level: 3, Category: "traceability", Name: "Structural gates",
		Patterns: []string{".claude/settings.json"}},

	// ── L4 Security-Aware (9) ──
	{ID: "acmm:auto-qa-tuning", Source: "acmm", Level: 4, Category: "self-tuning", Name: "Auto-QA self-tuning",
		Patterns: []string{".github/auto-qa-tuning.json", ".github/workflows/auto-qa.yml", "scripts/auto-qa-tuner.mjs"}},
	{ID: "acmm:nightly-compliance", Source: "acmm", Level: 4, Category: "feedback-loop", Name: "Nightly compliance",
		Patterns: []string{".github/workflows/nightly-compliance.yml", ".github/workflows/nightly.yml", ".github/workflows/nightly-test.yml", ".github/workflows/nightly-test-suite.yml"}},
	{ID: "acmm:copilot-review-apply", Source: "acmm", Level: 4, Category: "feedback-loop", Name: "Automated review application",
		Patterns: []string{".github/workflows/copilot-review-apply.yml", ".github/workflows/apply-copilot.yml", ".github/workflows/ai-fix.yml", ".github/workflows/auto-review.yml"}},
	{ID: "acmm:auto-label", Source: "acmm", Level: 4, Category: "feedback-loop", Name: "Automated issue labeling",
		Patterns: []string{".github/labeler.yml", ".github/workflows/labeler.yml", ".github/workflows/triage.yml"}},
	{ID: "acmm:ai-fix-workflow", Source: "acmm", Level: 4, Category: "feedback-loop", Name: "AI-fix-requested workflow",
		Patterns: []string{".github/workflows/ai-fix.yml", ".github/workflows/ai-fix-requested.yml", ".github/workflows/claude.yml"}},
	{ID: "acmm:tier-classifier", Source: "acmm", Level: 4, Category: "governance", Name: "Change classification",
		Patterns: []string{".github/workflows/tier-classifier.yml", "docs/risk-tiers.md", ".github/risk-tiers.yml", ".github/workflows/pr-size.yml"}},
	{ID: "acmm:security-ai-md", Source: "acmm", Level: 4, Category: "governance", Name: "AI security policy",
		Patterns: []string{"docs/security/SECURITY-AI.md", "SECURITY-AI.md", "docs/SECURITY-AI.md"}},
	{ID: "acmm:session-continuity", Source: "acmm", Level: 4, Category: "learning", Name: "Session continuity",
		Patterns: []string{".claude/checkpoint.md", ".claude/session-summary.md"}},
	{ID: "acmm:cross-session-knowledge", Source: "acmm", Level: 4, Category: "learning", Name: "Cross-session knowledge",
		Patterns: []string{"knowledge.jsonl", ".knowledge/", "docs/reflections/"}},

	// ── L5 Semi-Autonomous (5) ──
	{ID: "acmm:github-actions-ai", Source: "acmm", Level: 5, Category: "feedback-loop", Name: "GitHub Actions AI integration",
		Patterns: []string{".github/workflows/claude.yml", ".github/workflows/claude-code-review.yml"}},
	{ID: "acmm:auto-qa-self-tuning", Source: "acmm", Level: 5, Category: "self-tuning", Name: "Auto-QA with self-tuning",
		Patterns: []string{".github/workflows/auto-qa.yml", ".github/auto-qa-tuning.json"}},
	{ID: "acmm:public-metrics", Source: "acmm", Level: 5, Category: "observability", Name: "Public metrics",
		Patterns: []string{"web/netlify/functions/analytics-accm.mts", "web/public/analytics.js", "docs/metrics/"}},
	{ID: "acmm:policy-as-code", Source: "acmm", Level: 5, Category: "governance", Name: "Policy-as-code",
		Patterns: []string{"policies/", ".github/policies/", "kyverno/", "conftest.yaml", "opa/"}},
	{ID: "acmm:reflection-log", Source: "acmm", Level: 5, Category: "feedback-loop", Name: "Reflection log",
		Patterns: []string{".claude/reflections/", "memory/", ".memory/", "docs/reflections/", "REFLECTIONS.md"}},

	// ── L6 Fully Autonomous (6 acmm) ──
	{ID: "acmm:auto-issue-gen", Source: "acmm", Level: 6, Category: "autonomy", Name: "Auto issue generation",
		Patterns: []string{".github/workflows/auto-issues.yml", ".github/workflows/auto-issue.yml", ".github/workflows/issue-gen.yml", ".github/workflows/auto-generate-issues.yml", "scripts/generate-issues.mjs"}},
	{ID: "acmm:multi-agent-orchestration", Source: "acmm", Level: 6, Category: "autonomy", Name: "Multi-agent orchestration",
		Patterns: []string{".github/workflows/dispatcher.yml", ".github/workflows/orchestrate.yml", "scripts/orchestrate.mjs", "docs/multi-agent.md", ".claude/dispatcher/", "orchestrator/"}},
	{ID: "acmm:strategic-dashboard", Source: "acmm", Level: 6, Category: "observability", Name: "Strategic dashboard",
		Patterns: []string{"web/src/components/acmm/", "docs/strategy.md", ".github/workflows/strategy-report.yml", "docs/autonomous-work-log.md"}},
	{ID: "acmm:merge-queue", Source: "acmm", Level: 6, Category: "autonomy", Name: "Merge queue",
		Patterns: []string{".github/workflows/merge-queue.yml", ".github/merge-queue.yml", ".prow.yaml", "tide.yaml"}},
	{ID: "acmm:risk-assessment-config", Source: "acmm", Level: 6, Category: "governance", Name: "Risk assessment config",
		Patterns: []string{"risk-config.json", ".claude/risk-config.json", ".github/risk-assessment.yml"}},
	{ID: "acmm:observability-runbook", Source: "acmm", Level: 6, Category: "governance", Name: "Observability runbook",
		Patterns: []string{"docs/ai-ops-runbook.md", "docs/runbook/", "RUNBOOK.md"}},

	// ── L6 Agentic Engineering Framework (6 aef) ──
	{ID: "aef:task-traceability", Source: "aef", Level: 6, Category: "governance", Name: "Task traceability",
		Patterns: []string{".agent/tasks/", "docs/agent-tasks/", ".github/agent-log/", "agent-tasks.md"}},
	{ID: "aef:structural-gates", Source: "aef", Level: 6, Category: "governance", Name: "Structural gates",
		Patterns: []string{"CODEOWNERS", ".github/CODEOWNERS", ".agent/boundaries.yml", "docs/agent-boundaries.md"}},
	{ID: "aef:session-continuity", Source: "aef", Level: 6, Category: "governance", Name: "Session continuity",
		Patterns: []string{"CLAUDE.md", "AGENTS.md", ".cursorrules", ".github/copilot-instructions.md", "docs/agent-context.md"}},
	{ID: "aef:audit-trail", Source: "aef", Level: 6, Category: "governance", Name: "Audit trail workflow",
		Patterns: []string{".github/workflows/ai-audit.yml", ".github/workflows/agent-audit.yml", "scripts/ai-audit-report.mjs"}},
	{ID: "aef:cross-tool-config", Source: "aef", Level: 6, Category: "governance", Name: "Cross-tool agent config",
		Patterns: []string{"AGENTS.md", "docs/ai-contributors.md", ".github/ai-config.yml"}},
	{ID: "aef:change-classification", Source: "aef", Level: 6, Category: "governance", Name: "Change classification",
		Patterns: []string{"docs/change-classification.md", ".github/change-tiers.yml", "docs/risk-tiers.md"}},
}

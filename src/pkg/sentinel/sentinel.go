// Package sentinel flags pull requests that look like attempts to override
// security controls, escalate privileges, or damage a codebase — regardless
// of who authored them. It is deliberately a pure evaluator: callers hand it
// the PR's author, title and changed files (with unified-diff patches) and
// receive a list of findings. Applying the alert label, posting the comment
// and auditing live in pkg/github (SweepSentinel), and the operator-facing
// configuration lives in pkg/config (SentinelConfig).
//
// Findings are heuristics, not verdicts. The alert label exists so a human
// looks before a merge happens; it never blocks on its own.
package sentinel

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/intent"
)

// Rule names identify which behavior produced a finding. They are stable
// strings: operators toggle them by name in config and they appear in the
// alert comment and audit log.
const (
	RuleSensitivePath        = "sensitive_path"
	RuleOwnerSelfNomination  = "owner_self_nomination"
	RulePermissionEscalation = "permission_escalation"
	RuleSecretExposure       = "secret_exposure"
	RuleCIGateWeakening      = "ci_gate_weakening"
	RuleTestRemoval          = "test_removal"
	RuleSecurityPolicyEdit   = "security_policy_edit"
	RuleRemoteCodeExecution  = "remote_code_execution"
)

// AllRules lists every rule in display order.
var AllRules = []string{
	RuleSensitivePath,
	RuleOwnerSelfNomination,
	RulePermissionEscalation,
	RuleSecretExposure,
	RuleCIGateWeakening,
	RuleTestRemoval,
	RuleSecurityPolicyEdit,
	RuleRemoteCodeExecution,
}

// RuleDescriptions explains each rule in one line for the dashboard and the
// alert comment.
var RuleDescriptions = map[string]string{
	RuleSensitivePath:        "Touches a sensitive path (OWNERS, workflows, policies, hive config, security docs, …)",
	RuleOwnerSelfNomination:  "Adds the PR author's own login to OWNERS / CODEOWNERS / MAINTAINERS",
	RulePermissionEscalation: "Widens GitHub Actions permissions or switches to pull_request_target",
	RuleSecretExposure:       "Workflow change that prints, encodes or ships secrets off-host",
	RuleCIGateWeakening:      "Disables or softens CI gates (continue-on-error, || true, removed checks)",
	RuleTestRemoval:          "Deletes test files or guts test coverage",
	RuleSecurityPolicyEdit:   "Edits SECURITY.md, dependabot/CodeQL config, rulesets or git hooks",
	RuleRemoteCodeExecution:  "Adds curl|sh, eval of encoded payloads, reverse shells or embedded private keys",
}

// DefaultSensitivePaths is the starter list of paths whose modification
// always warrants a human look. It unions intent's guardrail paths with the
// governance and supply-chain files most often targeted by a hostile change.
var DefaultSensitivePaths = []string{
	// Governance / trust
	"OWNERS", "**/OWNERS", "OWNERS_ALIASES", "**/OWNERS_ALIASES",
	"CODEOWNERS", "**/CODEOWNERS", "MAINTAINERS", "**/MAINTAINERS",
	"GOVERNANCE.md", "SECURITY.md", "**/SECURITY.md",
	// CI / automation
	".github/workflows/**", "**/.github/workflows/**",
	".github/actions/**", "**/action.yml", "**/action.yaml",
	".github/dependabot.yml", ".github/codeql/**", ".github/rulesets/**",
	".github/settings.yml", ".github/CODEOWNERS",
	".gitlab-ci.yml", ".circleci/**", "Jenkinsfile", ".travis.yml",
	"Makefile", "Justfile",
	// Hooks and local enforcement
	".pre-commit-config.yaml", "githooks/**", ".husky/**",
	// Hive's own policy and runtime config
	"policies/**", "**/policies/**", "hive.yaml", "**/hive.yaml",
	"hive.yaml.dashboard", "**/hive.yaml.dashboard",
	"**/gh-wrapper*", "**/gh_wrapper*", "**/proxy/rules*",
	// Build, release and supply chain
	"Dockerfile", "**/Dockerfile*", "install.sh", "**/install.sh",
	"go.mod", "**/go.mod", "package.json", "**/package.json",
	"**/package-lock.json", "**/yarn.lock", "**/pnpm-lock.yaml",
	"**/requirements*.txt", "**/pyproject.toml", "**/Cargo.toml",
	".goreleaser*", "**/release*.yml", "**/release*.yaml",
	// Secrets-adjacent
	"**/.env*", "**/*.pem", "**/*.key", "**/id_rsa*", "**/secrets*",
	// Deployment
	"deploy/**", "**/deploy/**", "**/*.tf", "**/helm/**", "**/charts/**",
	"**/k8s/**", "**/kubernetes/**",
}

// ownersFileNames are the governance files owner_self_nomination inspects.
var ownersFileNames = map[string]bool{
	"OWNERS": true, "OWNERS_ALIASES": true, "CODEOWNERS": true, "MAINTAINERS": true, "MAINTAINERS.md": true,
}

var securityPolicyPaths = []string{
	"SECURITY.md", "**/SECURITY.md", "security-insights.yml", "**/security-insights.yml",
	".github/dependabot.yml", ".github/dependabot.yaml", ".github/codeql/**", "**/codeql*.yml",
	".github/rulesets/**", ".github/settings.yml", ".github/branch-protection*",
	".pre-commit-config.yaml", "githooks/**", ".husky/**", "**/.husky/**",
	"**/SECURITY_CONTACTS", "**/.snyk", "**/.trivyignore", "**/.gitleaks*", "**/.semgrep*",
}

var ciPaths = []string{
	".github/workflows/**", "**/.github/workflows/**", ".github/actions/**",
	".gitlab-ci.yml", ".circleci/**", "Jenkinsfile", ".travis.yml",
	"Makefile", "**/Makefile", "Justfile", "**/Justfile",
	"**/*.mk", "**/scripts/check-*", "**/scripts/*gate*", "**/scripts/*ratchet*",
}

var workflowPaths = []string{".github/workflows/**", "**/.github/workflows/**", ".github/actions/**", "**/action.yml", "**/action.yaml"}

var testPaths = []string{
	"**/*_test.go", "**/*.test.*", "**/*.spec.*", "**/test/**", "**/tests/**",
	"test/**", "tests/**", "**/__tests__/**", "**/testdata/**",
}

var (
	permissionEscalationRE = regexp.MustCompile(`(?i)^\s*(permissions:\s*write-all|(contents|actions|packages|administration|id-token|pull-requests|security-events|statuses|checks|deployments|pages|repository-projects|organization-administration):\s*write|on:\s*\[?\s*pull_request_target|pull_request_target:)`)
	secretRefRE            = regexp.MustCompile(`(?i)(\$\{\{\s*secrets\.|toJSON\s*\(\s*secrets\s*\)|\bsecrets\.[A-Za-z_]+)`)
	secretSinkRE           = regexp.MustCompile(`(?i)\b(echo|printf|cat|curl|wget|nc|ncat|base64|printenv|env|set\s*\+?x|tee|xxd|od|hexdump|python[23]?\s+-c|node\s+-e)\b|>\s*\$GITHUB_(OUTPUT|STEP_SUMMARY)|::set-output|::add-mask`)
	envDumpRE              = regexp.MustCompile(`(?i)toJSON\s*\(\s*(secrets|env|github)\s*\)|\bprintenv\b|\benv\s*\|\s*(curl|base64|nc)\b`)
	ciWeakenAddRE          = regexp.MustCompile(`(?i)(continue-on-error:\s*true|\|\|\s*true\b|\|\|\s*exit\s+0\b|^\s*if:\s*(false|\$\{\{\s*false\s*\}\})\s*$|--no-verify\b|allow_failure:\s*true|SKIP[_A-Z]*=1|\bset\s+\+e\b|^\s*exit\s+0\s*$|\btrue\s*#\s*(skip|disable|bypass)|--exit-zero\b|\|\|\s*:\s*$)`)
	ciWeakenRemoveRE       = regexp.MustCompile(`(?i)(required|coverage|ratchet|--strict|lint|vet|gosec|codeql|trivy|gitleaks|semgrep|verify|gate|protect|signoff|dco|needs:)`)
	remoteExecRE           = regexp.MustCompile(`(?i)((curl|wget)[^|\n]*\|\s*(sudo\s+)?(ba|z|da|k)?sh\b|eval\s*\(?\s*(base64|atob|Buffer\.from|\$\(\s*(echo|printf)[^)]*base64)|base64\s+(-d|--decode)[^|\n]*\|\s*(sudo\s+)?(ba|z)?sh\b|\bnc\b[^\n]*\s-e\s|/dev/tcp/|\bmkfifo\b[^\n]*\bnc\b|-----BEGIN (RSA |OPENSSH |EC |DSA |PGP )?PRIVATE KEY|\bAKIA[0-9A-Z]{16}\b|ghp_[A-Za-z0-9]{36}|xox[baprs]-[A-Za-z0-9-]{10,}|socket\.socket\([^)]*\)[^\n]*connect|subprocess\.(Popen|call|run)\([^)]*(sh|bash)[^)]*-c|chmod\s+[0-7]*[4-7][0-7]{2,3}\s+/|LD_PRELOAD=)`)
	fenceRE                = regexp.MustCompile("^\\s*(```|~~~)")
)

// ciRemovedLineThreshold is how many gate-looking lines must be removed
// from CI files, with no softened equivalent added, before ci_gate_weakening
// fires on removal alone. One removed line is routine refactoring.
const ciRemovedLineThreshold = 3

// testRemovalDeletionFloor and testRemovalRatio define "gutting" a test
// file: at least this many deleted lines and deletions outnumbering
// additions by the ratio across all test files in the PR.
const (
	testRemovalDeletionFloor = 25
	testRemovalRatio         = 3
)

// File is one changed file in a PR. Patch is the unified diff GitHub returns
// for text files; it is empty for binaries and very large files.
type File struct {
	Path      string
	Status    string
	Additions int
	Deletions int
	Patch     string
}

// PR is the evidence the evaluator inspects.
type PR struct {
	Author string
	Title  string
	Files  []File
}

// Config selects which behaviors fire and which paths count as sensitive.
// Zero values mean "use defaults": nil SensitivePaths is
// DefaultSensitivePaths and a rule absent from Disabled is on.
type Config struct {
	SensitivePaths []string
	// Disabled lists rule names switched off by the operator.
	Disabled []string
	// ExemptLogins are authors never flagged (case-insensitive).
	ExemptLogins []string
}

// Finding is one triggered rule with the evidence that tripped it.
type Finding struct {
	Rule    string   `json:"rule"`
	Summary string   `json:"summary"`
	Paths   []string `json:"paths,omitempty"`
}

// Exempt reports whether author is on the exempt list.
func (c Config) Exempt(author string) bool {
	author = strings.TrimSpace(author)
	if author == "" {
		return false
	}
	for _, l := range c.ExemptLogins {
		if strings.EqualFold(strings.TrimSpace(l), author) {
			return true
		}
	}
	return false
}

func (c Config) enabled(rule string) bool {
	for _, d := range c.Disabled {
		if strings.EqualFold(strings.TrimSpace(d), rule) {
			return false
		}
	}
	return true
}

func (c Config) sensitivePaths() []string {
	if c.SensitivePaths == nil {
		return DefaultSensitivePaths
	}
	return c.SensitivePaths
}

// Evaluate runs every enabled rule over pr and returns findings in AllRules
// order. An exempt author yields nil.
func Evaluate(pr PR, cfg Config) []Finding {
	if cfg.Exempt(pr.Author) || len(pr.Files) == 0 {
		return nil
	}
	checks := map[string]func(PR, Config) *Finding{
		RuleSensitivePath:        checkSensitivePath,
		RuleOwnerSelfNomination:  checkOwnerSelfNomination,
		RulePermissionEscalation: checkPermissionEscalation,
		RuleSecretExposure:       checkSecretExposure,
		RuleCIGateWeakening:      checkCIGateWeakening,
		RuleTestRemoval:          checkTestRemoval,
		RuleSecurityPolicyEdit:   checkSecurityPolicyEdit,
		RuleRemoteCodeExecution:  checkRemoteCodeExecution,
	}
	var out []Finding
	for _, rule := range AllRules {
		if !cfg.enabled(rule) {
			continue
		}
		if f := checks[rule](pr, cfg); f != nil {
			out = append(out, *f)
		}
	}
	return out
}

// Rules returns the rule names in findings, in order.
func Rules(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Rule)
	}
	return out
}

func checkSensitivePath(pr PR, cfg Config) *Finding {
	var hit []string
	for _, f := range pr.Files {
		if intent.PathMatchesAny(f.Path, cfg.sensitivePaths()) {
			hit = append(hit, f.Path)
		}
	}
	if len(hit) == 0 {
		return nil
	}
	return &Finding{Rule: RuleSensitivePath, Summary: fmt.Sprintf("changes %d sensitive path(s)", len(hit)), Paths: hit}
}

func checkOwnerSelfNomination(pr PR, _ Config) *Finding {
	author := strings.ToLower(strings.TrimSpace(pr.Author))
	if author == "" {
		return nil
	}
	var hit []string
	for _, f := range pr.Files {
		if !ownersFileNames[path.Base(f.Path)] {
			continue
		}
		for _, line := range addedLines(f.Patch) {
			if mentionsLogin(line, author) {
				hit = append(hit, f.Path)
				break
			}
		}
	}
	if len(hit) == 0 {
		return nil
	}
	return &Finding{Rule: RuleOwnerSelfNomination, Summary: fmt.Sprintf("@%s adds their own login to %s", pr.Author, strings.Join(hit, ", ")), Paths: hit}
}

// mentionsLogin matches a GitHub login as a whole token: "- login",
// "@login", "login:" or "login," — not as a substring of a longer login.
func mentionsLogin(line, login string) bool {
	line = strings.ToLower(line)
	idx := 0
	for {
		i := strings.Index(line[idx:], login)
		if i < 0 {
			return false
		}
		start := idx + i
		end := start + len(login)
		before := byte(' ')
		if start > 0 {
			before = line[start-1]
		}
		after := byte(' ')
		if end < len(line) {
			after = line[end]
		}
		if !isLoginChar(before) && !isLoginChar(after) {
			return true
		}
		idx = end
	}
}

func isLoginChar(b byte) bool {
	return b == '-' || b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func checkPermissionEscalation(pr PR, _ Config) *Finding {
	hit := filesWithAddedLineMatching(pr, workflowPaths, func(line string) bool {
		return permissionEscalationRE.MatchString(line)
	})
	if len(hit) == 0 {
		return nil
	}
	return &Finding{Rule: RulePermissionEscalation, Summary: "widens workflow permissions or uses pull_request_target", Paths: hit}
}

func checkSecretExposure(pr PR, _ Config) *Finding {
	hit := filesWithAddedLineMatching(pr, workflowPaths, func(line string) bool {
		return envDumpRE.MatchString(line) || (secretRefRE.MatchString(line) && secretSinkRE.MatchString(line))
	})
	if len(hit) == 0 {
		return nil
	}
	return &Finding{Rule: RuleSecretExposure, Summary: "workflow step routes secrets or the environment to a shell sink", Paths: hit}
}

func checkCIGateWeakening(pr PR, _ Config) *Finding {
	var hit []string
	for _, f := range pr.Files {
		if !intent.PathMatchesAny(f.Path, ciPaths) {
			continue
		}
		if f.Status == "removed" {
			hit = append(hit, f.Path)
			continue
		}
		softened := false
		for _, line := range addedLines(f.Patch) {
			if ciWeakenAddRE.MatchString(line) {
				softened = true
				break
			}
		}
		removedGates := 0
		for _, line := range removedLines(f.Patch) {
			if ciWeakenRemoveRE.MatchString(line) {
				removedGates++
			}
		}
		if softened || removedGates >= ciRemovedLineThreshold {
			hit = append(hit, f.Path)
		}
	}
	if len(hit) == 0 {
		return nil
	}
	return &Finding{Rule: RuleCIGateWeakening, Summary: "CI configuration is deleted, softened or has gate checks removed", Paths: uniq(hit)}
}

func checkTestRemoval(pr PR, _ Config) *Finding {
	var removed, touched []string
	adds, dels := 0, 0
	for _, f := range pr.Files {
		if !intent.PathMatchesAny(f.Path, testPaths) {
			continue
		}
		touched = append(touched, f.Path)
		if f.Status == "removed" {
			removed = append(removed, f.Path)
		}
		adds += f.Additions
		dels += f.Deletions
	}
	if len(removed) > 0 {
		return &Finding{Rule: RuleTestRemoval, Summary: fmt.Sprintf("deletes %d test file(s)", len(removed)), Paths: removed}
	}
	if dels >= testRemovalDeletionFloor && dels >= testRemovalRatio*max(adds, 1) {
		return &Finding{Rule: RuleTestRemoval, Summary: fmt.Sprintf("removes %d test lines while adding %d", dels, adds), Paths: touched}
	}
	return nil
}

func checkSecurityPolicyEdit(pr PR, _ Config) *Finding {
	var hit []string
	for _, f := range pr.Files {
		if intent.PathMatchesAny(f.Path, securityPolicyPaths) {
			hit = append(hit, f.Path)
		}
	}
	if len(hit) == 0 {
		return nil
	}
	return &Finding{Rule: RuleSecurityPolicyEdit, Summary: "edits security policy, scanner config, rulesets or hooks", Paths: hit}
}

func checkRemoteCodeExecution(pr PR, _ Config) *Finding {
	var hit []string
	for _, f := range pr.Files {
		if isDocPath(f.Path) {
			continue
		}
		for _, line := range addedLines(f.Patch) {
			if remoteExecRE.MatchString(line) {
				hit = append(hit, f.Path)
				break
			}
		}
	}
	if len(hit) == 0 {
		return nil
	}
	return &Finding{Rule: RuleRemoteCodeExecution, Summary: "adds a remote-fetch-and-execute, decoded payload, reverse shell or embedded credential", Paths: uniq(hit)}
}

// filesWithAddedLineMatching returns the sorted, de-duplicated paths among
// pr.Files matching patterns that have at least one added line satisfying
// match.
func filesWithAddedLineMatching(pr PR, patterns []string, match func(string) bool) []string {
	var hit []string
	for _, f := range pr.Files {
		if !intent.PathMatchesAny(f.Path, patterns) {
			continue
		}
		for _, line := range addedLines(f.Patch) {
			if match(line) {
				hit = append(hit, f.Path)
				break
			}
		}
	}
	if len(hit) == 0 {
		return nil
	}
	return uniq(hit)
}

// isDocPath excludes prose from the shell-pattern rule: docs legitimately
// show `curl … | sh` install one-liners.
func isDocPath(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	return ext == ".md" || ext == ".mdx" || ext == ".rst" || ext == ".txt"
}

// addedLines returns the "+" lines of a unified diff without the marker,
// skipping the "+++" header and Markdown fence lines.
func addedLines(patch string) []string { return diffLines(patch, '+') }

func removedLines(patch string) []string { return diffLines(patch, '-') }

func diffLines(patch string, marker byte) []string {
	if patch == "" {
		return nil
	}
	var out []string
	header := string([]byte{marker, marker, marker})
	for _, raw := range strings.Split(patch, "\n") {
		if len(raw) == 0 || raw[0] != marker || strings.HasPrefix(raw, header) {
			continue
		}
		line := raw[1:]
		if fenceRE.MatchString(line) {
			continue
		}
		out = append(out, line)
	}
	return out
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

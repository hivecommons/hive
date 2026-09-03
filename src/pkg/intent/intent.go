package intent

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/beads"
)

// Tier is the change-authorization tier assigned to an agent-authored PR.
type Tier int

const (
	Tier0 Tier = iota
	Tier1
	Tier2
	Tier3
)

const (
	ReasonTier0Additive       = "tier 0 additive docs/test-only change"
	ReasonTestWeakening       = "test weakening detected"
	ReasonTier3Guardrail      = "guardrail-critical path touched"
	ReasonTier2FeatureSignal  = "feature signal detected"
	ReasonTier1Default        = "bugfix/chore default"
	ReasonLinkedIssue         = "linked issue evidence present"
	ReasonApprovedPlan        = "approved plan evidence present"
	ReasonHumanApproval       = "human approval evidence present"
	ReasonTier3NeedsApproval  = "tier 3 requires human approval"
	ReasonTier2NeedsPlan      = "tier 2 requires approved plan or human approval"
	ReasonTier1NeedsIssue     = "tier 1 requires linked issue"
	ReasonAlignmentMisaligned = "intent alignment check reported misalignment"
	// ReasonAppSelfMergeAuthorized is the Evaluate reason substituted ONLY by
	// EvaluateForAppSelfMerge (see below) — never by Evaluate itself.
	ReasonAppSelfMergeAuthorized = "app self-merge: human approval not required for the App's own PR"
)

const (
	BeadPlanStatusKey      = "plan_status"
	BeadPlanApprovedStatus = "approved"
	BeadParentEpicKey      = "parent_epic"

	AlignmentStatusAligned    = "aligned"
	AlignmentStatusMisaligned = "misaligned"
	AlignmentStatusUnclear    = "unclear"
)

const linkedIssuePattern = `(?i)\b(?:fix(?:e[sd])?|close[sd]?|resolve[sd]?|refs?|part\s+of|related\s+to)\s+(?:(?:([\w.-]+/[\w.-]+)#)|#)(\d+)\b`

var linkedIssueRE = regexp.MustCompile(linkedIssuePattern)

var defaultTestPathPatterns = []string{
	"**/*_test.go",
	"**/*.test.*",
	"**/*.spec.*",
	"**/test/**",
	"**/tests/**",
	"test/**",
	"tests/**",
}

var defaultDocsPathPatterns = []string{
	"**/*.md",
}

var defaultGuardrailPathPatterns = []string{
	".github/workflows/**",
	"**/.github/workflows/**",
	"**/gh-wrapper*",
	"**/gh_wrapper*",
	"**/proxy/rules*",
	"policies/**",
	"**/policies/**",
	"hive.yaml",
	"**/hive.yaml",
	"hive.yaml.dashboard",
	"**/hive.yaml.dashboard",
	"OWNERS",
	"**/OWNERS",
	"CODEOWNERS",
	"**/CODEOWNERS",
	"**/CODEOWNERS/**",
}

var defaultFeatureSignals = []string{"feat", "feature", "enhancement", "new capability", "proposal", "rfc"}

// Config customizes tier path matching. Empty slices keep sane defaults.
type Config struct {
	TestPathPatterns      []string
	DocsPathPatterns      []string
	GuardrailPathPatterns []string
	FeatureSignals        []string
}

// ChangedFile is the PR file evidence used for tier classification.
type ChangedFile struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
}

type PR struct {
	Title       string
	Body        string
	Labels      []string
	Files       []ChangedFile
	Author      string
	AgentAuthor bool
}

type Classification struct {
	Tier    Tier   `json:"tier"`
	Reason  string `json:"reason"`
	AgentPR bool   `json:"agent_pr"`
}

type Evidence struct {
	LinkedIssue   bool
	ApprovedPlan  bool
	HumanApproval bool
}

type Verdict struct {
	Tier       Tier              `json:"tier"`
	Authorized bool              `json:"authorized"`
	Reason     string            `json:"reason"`
	Evidence   Evidence          `json:"evidence"`
	AgentPR    bool              `json:"agent_pr"`
	Alignment  *AlignmentVerdict `json:"alignment,omitempty"`
}

// MergeAllowed reports whether the verdict may enter merge-eligible.json when
// intent.enforce is enabled. It preserves the phase-1 authorization gate and
// adds phase-2 alignment misalignment as an equivalent exclusion reason.
func (v Verdict) MergeAllowed() bool {
	if !v.Authorized {
		return false
	}
	return v.Alignment == nil || !v.Alignment.Misaligned()
}

func Classify(pr PR, cfg Config) Classification {
	if !pr.AgentAuthor {
		return Classification{Tier: Tier0, Reason: "non-agent PR", AgentPR: false}
	}
	cfg = withDefaults(cfg)
	if touchesAny(pr.Files, cfg.GuardrailPathPatterns) {
		return Classification{Tier: Tier3, Reason: ReasonTier3Guardrail, AgentPR: true}
	}
	if allDocsOrTests(pr.Files, cfg) {
		if weakensTests(pr.Files, cfg.TestPathPatterns) {
			return Classification{Tier: Tier1, Reason: ReasonTestWeakening, AgentPR: true}
		}
		return Classification{Tier: Tier0, Reason: ReasonTier0Additive, AgentPR: true}
	}
	if hasFeatureSignal(pr.Title, pr.Labels, cfg.FeatureSignals) {
		return Classification{Tier: Tier2, Reason: ReasonTier2FeatureSignal, AgentPR: true}
	}
	return Classification{Tier: Tier1, Reason: ReasonTier1Default, AgentPR: true}
}

func Evaluate(class Classification, evidence Evidence) Verdict {
	if !class.AgentPR {
		return Verdict{Tier: class.Tier, Authorized: true, Reason: "non-agent PR not subject to intent authorization", Evidence: evidence, AgentPR: false}
	}
	switch class.Tier {
	case Tier0:
		return Verdict{Tier: class.Tier, Authorized: true, Reason: ReasonTier0Additive, Evidence: evidence, AgentPR: true}
	case Tier1:
		if evidence.LinkedIssue {
			return Verdict{Tier: class.Tier, Authorized: true, Reason: ReasonLinkedIssue, Evidence: evidence, AgentPR: true}
		}
		return Verdict{Tier: class.Tier, Authorized: false, Reason: ReasonTier1NeedsIssue, Evidence: evidence, AgentPR: true}
	case Tier2:
		if evidence.ApprovedPlan {
			return Verdict{Tier: class.Tier, Authorized: true, Reason: ReasonApprovedPlan, Evidence: evidence, AgentPR: true}
		}
		if evidence.HumanApproval {
			return Verdict{Tier: class.Tier, Authorized: true, Reason: ReasonHumanApproval, Evidence: evidence, AgentPR: true}
		}
		return Verdict{Tier: class.Tier, Authorized: false, Reason: ReasonTier2NeedsPlan, Evidence: evidence, AgentPR: true}
	case Tier3:
		if evidence.HumanApproval {
			return Verdict{Tier: class.Tier, Authorized: true, Reason: ReasonHumanApproval, Evidence: evidence, AgentPR: true}
		}
		return Verdict{Tier: class.Tier, Authorized: false, Reason: ReasonTier3NeedsApproval, Evidence: evidence, AgentPR: true}
	default:
		return Verdict{Tier: class.Tier, Authorized: false, Reason: fmt.Sprintf("unknown tier %d", class.Tier), Evidence: evidence, AgentPR: true}
	}
}

// EvaluateForAppSelfMerge is Evaluate's counterpart for the App-self-merge
// path (github.SweepSelfAuthoredAutoMerges) ONLY. It exists because Prow
// structurally forbids self-approval: tide's lgtm+approved labels must come
// from a reviewer distinct from the PR author, and on the App's own PR the
// author is always the App itself, so evidence.HumanApproval can never be
// honestly true for these PRs — Evaluate's Tier3 (and, for Tier2, its
// HumanApproval fallback) would permanently deadlock them. This function
// drops ONLY the human-approval requirement, and ONLY for a PR the caller has
// independently confirmed is the App's own (callAllowed=true) — every other
// tier gate is unchanged: Tier0 is still additive-only, Tier1 still needs a
// linked issue, and Tier2 still needs either an approved plan OR this
// self-merge authorization. A human-authored PR, or an App PR the caller has
// not confirmed is self-merge-eligible, MUST keep calling Evaluate — never
// this function — so tier gating for every other flow is untouched.
//
// Signed-head-SHA / head-unchanged-since-eval verification remains entirely
// the caller's responsibility (github.trySweepSelfAuthoredPR re-fetches and
// compares the head SHA immediately before merging); this function only
// answers the tier-authorization question, same as Evaluate.
func EvaluateForAppSelfMerge(class Classification, evidence Evidence, callAllowed bool) Verdict {
	if !callAllowed {
		return Evaluate(class, evidence)
	}
	switch class.Tier {
	case Tier2, Tier3:
		if evidence.HumanApproval || evidence.ApprovedPlan {
			return Evaluate(class, evidence)
		}
		return Verdict{Tier: class.Tier, Authorized: true, Reason: ReasonAppSelfMergeAuthorized, Evidence: evidence, AgentPR: class.AgentPR}
	default:
		return Evaluate(class, evidence)
	}
}

func BuildEvidence(body string, stores map[string]*beads.Store, humanApproval bool) Evidence {
	return BuildEvidenceForRepo(body, "", stores, humanApproval)
}

func BuildEvidenceForRepo(body, defaultRepo string, stores map[string]*beads.Store, humanApproval bool) Evidence {
	refs := LinkedIssueRefs(body, defaultRepo)
	linked := LinkedIssueInBody(body) || LinkedIssueInBeads(stores, refs)
	return Evidence{LinkedIssue: linked, ApprovedPlan: ApprovedPlanInBeads(stores, refs), HumanApproval: humanApproval}
}

func LinkedIssueInBody(body string) bool { return linkedIssueRE.MatchString(body) }

func LinkedIssueNumbers(body string) []int {
	refs := LinkedIssueRefs(body, "")
	out := make([]int, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref.Number)
	}
	return out
}

type IssueRef struct {
	Repo   string
	Number int
}

func LinkedIssueRefs(body, defaultRepo string) []IssueRef {
	matches := linkedIssueRE.FindAllStringSubmatch(body, -1)
	refs := make([]IssueRef, 0, len(matches))
	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		n, err := strconv.Atoi(m[2])
		if err == nil {
			repo := strings.TrimSpace(m[1])
			if repo == "" {
				repo = strings.TrimSpace(defaultRepo)
			}
			refs = append(refs, IssueRef{Repo: repo, Number: n})
		}
	}
	return refs
}

func LinkedIssueInBeads(stores map[string]*beads.Store, refs []IssueRef) bool {
	if len(refs) == 0 {
		return false
	}
	for _, b := range allBeads(stores) {
		if externalRefMatchesIssue(b.ExternalRef, refs) {
			return true
		}
	}
	return false
}

func ApprovedPlanInBeads(stores map[string]*beads.Store, refs []IssueRef) bool {
	if len(refs) == 0 {
		return false
	}
	for _, b := range allBeads(stores) {
		if b.Meta(BeadPlanStatusKey) == BeadPlanApprovedStatus && externalRefMatchesIssue(b.ExternalRef, refs) {
			return true
		}
	}
	return false
}

func externalRefMatchesIssue(ref string, refs []IssueRef) bool {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "gh-")
	for _, issue := range refs {
		suffix := "#" + strconv.Itoa(issue.Number)
		if issue.Repo != "" {
			suffix = strings.TrimSpace(issue.Repo) + suffix
		}
		if strings.HasSuffix(ref, suffix) {
			return true
		}
	}
	return false
}

func withDefaults(cfg Config) Config {
	if len(cfg.TestPathPatterns) == 0 {
		cfg.TestPathPatterns = defaultTestPathPatterns
	}
	if len(cfg.DocsPathPatterns) == 0 {
		cfg.DocsPathPatterns = defaultDocsPathPatterns
	}
	if len(cfg.GuardrailPathPatterns) == 0 {
		cfg.GuardrailPathPatterns = defaultGuardrailPathPatterns
	}
	if len(cfg.FeatureSignals) == 0 {
		cfg.FeatureSignals = defaultFeatureSignals
	}
	return cfg
}

func allDocsOrTests(files []ChangedFile, cfg Config) bool {
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		if !matchesAny(f.Filename, cfg.DocsPathPatterns) && !matchesAny(f.Filename, cfg.TestPathPatterns) {
			return false
		}
	}
	return true
}

func weakensTests(files []ChangedFile, testPatterns []string) bool {
	for _, f := range files {
		if matchesAny(f.Filename, testPatterns) && (strings.EqualFold(f.Status, "removed") || f.Deletions > 0) {
			return true
		}
	}
	return false
}

func touchesAny(files []ChangedFile, patterns []string) bool {
	for _, f := range files {
		if matchesAny(f.Filename, patterns) {
			return true
		}
	}
	return false
}

func hasFeatureSignal(title string, labels, signals []string) bool {
	haystack := strings.ToLower(title + " " + strings.Join(labels, " "))
	for _, signal := range signals {
		if s := strings.ToLower(strings.TrimSpace(signal)); s != "" && strings.Contains(haystack, s) {
			return true
		}
	}
	return false
}

func matchesAny(path string, patterns []string) bool {
	path = filepath.ToSlash(strings.TrimPrefix(path, "./"))
	for _, pattern := range patterns {
		if globMatch(filepath.ToSlash(pattern), path) {
			return true
		}
	}
	return false
}

func globMatch(pattern, path string) bool {
	if ok, _ := filepath.Match(pattern, path); ok {
		return true
	}
	if strings.HasPrefix(pattern, "**/*") {
		return strings.HasSuffix(path, strings.TrimPrefix(pattern, "**/*"))
	}
	if strings.HasPrefix(pattern, "**/") {
		trimmed := strings.TrimPrefix(pattern, "**/")
		if ok, _ := filepath.Match(trimmed, path); ok {
			return true
		}
		return globMatch(trimmed, path)
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return path == prefix || strings.HasPrefix(path, prefix+"/") || strings.Contains(path, "/"+prefix+"/")
	}
	if strings.Contains(pattern, "**/") {
		parts := strings.SplitN(pattern, "**/", 2)
		return strings.HasPrefix(path, parts[0]) && strings.Contains(path, strings.TrimPrefix(parts[1], "/"))
	}
	return false
}

func allBeads(stores map[string]*beads.Store) []*beads.Bead {
	var out []*beads.Bead
	for _, store := range stores {
		if store == nil {
			continue
		}
		out = append(out, store.List(beads.ListFilter{})...)
	}
	return out
}

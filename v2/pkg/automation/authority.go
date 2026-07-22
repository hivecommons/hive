package automation

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

type Mode string

const (
	ModeAdvisory  Mode = "advisory"
	ModeIssues    Mode = "issues"
	ModeRepairPR  Mode = "repair-pr"
	ModeAutoMerge Mode = "auto-merge"
)

type Action string

const (
	ActionAnalyze              Action = "analyze"
	ActionCreateIssue          Action = "create_issue"
	ActionUpdateIssue          Action = "update_issue"
	ActionCloseIssue           Action = "close_issue"
	ActionReopenIssue          Action = "reopen_issue"
	ActionRepairModel          Action = "repair_model"
	ActionApplyPatch           Action = "apply_model_patch"
	ActionCreateBranch         Action = "create_branch"
	ActionCommit               Action = "commit"
	ActionPush                 Action = "push"
	ActionCreatePR             Action = "create_pr"
	ActionRefreshRepairBranch  Action = "refresh_repair_branch"
	ActionCloseRepairPR        Action = "close_repair_pr"
	ActionDeleteRepairBranch   Action = "delete_repair_branch"
	ActionDeleteBaselineBranch Action = "delete_baseline_branch"
	ActionCreateBaselineReview Action = "create_baseline_review"
	ActionApplyBaselineReview  Action = "apply_baseline_review"
	ActionMergePR              Action = "merge_pr"
	ActionSetupBranch          Action = "setup_branch"
	ActionSetupCommit          Action = "setup_commit"
	ActionSetupPush            Action = "setup_push"
	ActionSetupPR              Action = "setup_pr"
	ActionSetupStatus          Action = "setup_status"
)

type RiskTier int

const (
	RiskAutomatic RiskTier = iota
	RiskLow
	RiskMedium
	RiskRestricted
)

type Policy struct {
	ACMMLevel             int
	Mode                  Mode
	Paused                bool
	KillSwitch            bool
	AllowedRepositories   []string
	AllowedAutoMergeRisk  []RiskTier
	AllowedAutoMergePaths []string
	MaxRepairAttempts     int
}

type ActionRequest struct {
	Action                  Action
	Agent                   string
	Repository              string
	Risk                    RiskTier
	ChangedFiles            []string
	RepairAttempts          int
	ExpectedHeadSHA         string
	TestedHeadSHA           string
	ExpectedBaseBranch      string
	TestedBaseBranch        string
	MergeableKnown          bool
	Mergeable               bool
	VisualHiveVerdictGreen  bool
	RequiredCheckNames      []string
	RequiredCheckStates     []string
	BranchProtectionEnabled bool
	Hold                    bool
	HumanReviewRequired     bool
	BaselineChanged         bool
	WorkflowChanged         bool
	SecuritySensitive       bool
	DeploymentChanged       bool
	SetupApproved           bool
}

type Decision struct {
	Allowed     bool      `json:"allowed"`
	Action      Action    `json:"action"`
	Mode        Mode      `json:"mode"`
	ACMMLevel   int       `json:"acmm_level"`
	Reasons     []string  `json:"reasons"`
	EvaluatedAt time.Time `json:"evaluated_at"`
}

func (p Policy) Authorize(request ActionRequest) Decision {
	decision := Decision{Action: request.Action, Mode: p.Mode, ACMMLevel: p.ACMMLevel, EvaluatedAt: time.Now().UTC()}
	reasons := make([]string, 0)
	if p.ACMMLevel < 1 || p.ACMMLevel > 6 {
		reasons = append(reasons, "ACMM level must be between 1 and 6")
	}
	if p.Mode != ModeAdvisory && p.Mode != ModeIssues && p.Mode != ModeRepairPR && p.Mode != ModeAutoMerge {
		reasons = append(reasons, "automation mode is invalid")
	}
	if p.KillSwitch {
		reasons = append(reasons, "Hive emergency kill switch is active")
	}
	if p.Paused && request.Action != ActionAnalyze {
		reasons = append(reasons, "repository automation is paused")
	}
	if len(p.AllowedRepositories) > 0 && !containsFold(p.AllowedRepositories, request.Repository) {
		reasons = append(reasons, "repository is outside the configured write allowlist")
	}
	if isSetupAction(request.Action) && !request.SetupApproved {
		reasons = append(reasons, "setup repository writes require explicit setup approval")
	}

	requiredMode := modeForAction(request.Action)
	if modeRank(p.Mode) < modeRank(requiredMode) {
		reasons = append(reasons, fmt.Sprintf("automation mode %s does not permit %s", p.Mode, request.Action))
	}
	capability := acmmCapability(request.Agent, p.ACMMLevel)
	if modeRank(capability) < modeRank(requiredMode) {
		reasons = append(reasons, fmt.Sprintf("ACMM L%d does not permit agent %s to perform %s", p.ACMMLevel, valueOr(request.Agent, "unknown"), request.Action))
	}

	// The budget limits creation/revision work that can spend another repair
	// attempt. It must not strand an already-created exact-head green PR, nor
	// block post-merge cleanup, merely because the limit was later lowered.
	if request.Action == ActionRepairModel || request.Action == ActionApplyPatch || request.Action == ActionCreateBranch || request.Action == ActionCommit || request.Action == ActionPush || request.Action == ActionCreatePR || request.Action == ActionCreateBaselineReview {
		maxAttempts := p.MaxRepairAttempts
		if maxAttempts <= 0 {
			maxAttempts = 3
		}
		if request.RepairAttempts > maxAttempts {
			reasons = append(reasons, "repair attempt budget is exhausted")
		}
	}
	if request.Action == ActionMergePR {
		reasons = append(reasons, validateMerge(p, request)...)
	}

	sort.Strings(reasons)
	decision.Reasons = unique(reasons)
	decision.Allowed = len(decision.Reasons) == 0
	return decision
}

func validateMerge(policy Policy, request ActionRequest) []string {
	reasons := make([]string, 0)
	if policy.ACMMLevel != 6 || policy.Mode != ModeAutoMerge {
		reasons = append(reasons, "auto-merge requires ACMM L6 and auto-merge mode")
	}
	if request.ExpectedHeadSHA == "" || request.TestedHeadSHA == "" || request.ExpectedHeadSHA != request.TestedHeadSHA {
		reasons = append(reasons, "required checks and Visual Hive verdict must apply to the exact PR head SHA")
	}
	if strings.TrimSpace(request.ExpectedBaseBranch) == "" || !strings.EqualFold(strings.TrimSpace(request.ExpectedBaseBranch), strings.TrimSpace(request.TestedBaseBranch)) {
		reasons = append(reasons, "pull request base branch must match the configured default branch")
	}
	if !request.MergeableKnown {
		reasons = append(reasons, "GitHub pull request mergeability is not yet known")
	} else if !request.Mergeable {
		reasons = append(reasons, "GitHub reports that the pull request is not mergeable")
	}
	if !request.VisualHiveVerdictGreen {
		reasons = append(reasons, "Visual Hive deterministic verdict is not green")
	}
	visualHiveRequired := false
	for _, name := range request.RequiredCheckNames {
		if strings.EqualFold(strings.TrimSpace(name), "visual-hive") {
			visualHiveRequired = true
			break
		}
	}
	if !visualHiveRequired {
		reasons = append(reasons, "branch protection must require the exact visual-hive check")
	}
	if len(request.RequiredCheckStates) == 0 {
		reasons = append(reasons, "no required GitHub checks were observed")
	}
	for _, state := range request.RequiredCheckStates {
		if strings.ToLower(strings.TrimSpace(state)) != "success" {
			reasons = append(reasons, "all required GitHub checks must be successful")
			break
		}
	}
	if !request.BranchProtectionEnabled {
		reasons = append(reasons, "branch protection or an equivalent ruleset is required")
	}
	if request.Hold || request.HumanReviewRequired {
		reasons = append(reasons, "a hold or human-review requirement blocks auto-merge")
	}
	if request.BaselineChanged {
		reasons = append(reasons, "visual baseline changes require separate review")
	}
	if request.WorkflowChanged || request.SecuritySensitive || request.DeploymentChanged {
		reasons = append(reasons, "workflow, security-sensitive, or deployment changes require additional authority")
	}
	if !riskAllowed(policy.AllowedAutoMergeRisk, request.Risk) {
		reasons = append(reasons, fmt.Sprintf("risk tier %d is not allowed for auto-merge", request.Risk))
	}
	for _, file := range request.ChangedFiles {
		if !pathAllowed(policy.AllowedAutoMergePaths, file) {
			reasons = append(reasons, fmt.Sprintf("changed file %s is outside the auto-merge allowlist", file))
		}
	}
	return reasons
}

func modeForAction(action Action) Mode {
	switch action {
	case ActionAnalyze:
		return ModeAdvisory
	case ActionCreateIssue, ActionUpdateIssue, ActionCloseIssue, ActionReopenIssue:
		return ModeIssues
	case ActionRepairModel, ActionApplyPatch, ActionCreateBranch, ActionCommit, ActionPush, ActionCreatePR, ActionRefreshRepairBranch, ActionCloseRepairPR, ActionDeleteRepairBranch, ActionDeleteBaselineBranch, ActionCreateBaselineReview, ActionApplyBaselineReview:
		return ModeRepairPR
	case ActionMergePR:
		return ModeAutoMerge
	case ActionSetupBranch, ActionSetupCommit, ActionSetupPush, ActionSetupPR, ActionSetupStatus:
		return ModeAdvisory
	default:
		return ModeAutoMerge
	}
}

func isSetupAction(action Action) bool {
	return action == ActionSetupBranch || action == ActionSetupCommit || action == ActionSetupPush || action == ActionSetupPR || action == ActionSetupStatus
}

func acmmCapability(agent string, level int) Mode {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "supervisor" || agent == "brainstorm" {
		return ModeAdvisory
	}
	switch level {
	case 1, 2:
		return ModeAdvisory
	case 3:
		if agent == "quality" || agent == "tester" {
			return ModeRepairPR
		}
		return ModeAdvisory
	case 4:
		switch agent {
		case "quality", "tester", "sec-check", "ci-maintainer":
			return ModeRepairPR
		case "scanner", "guide":
			return ModeIssues
		default:
			return ModeAdvisory
		}
	case 5:
		return ModeRepairPR
	case 6:
		return ModeAutoMerge
	default:
		return ModeAdvisory
	}
}

func modeRank(mode Mode) int {
	switch mode {
	case ModeAdvisory:
		return 0
	case ModeIssues:
		return 1
	case ModeRepairPR:
		return 2
	case ModeAutoMerge:
		return 3
	default:
		return -1
	}
}

func riskAllowed(allowed []RiskTier, risk RiskTier) bool {
	if len(allowed) == 0 {
		return risk == RiskAutomatic
	}
	for _, candidate := range allowed {
		if candidate == risk {
			return true
		}
	}
	return false
}

func pathAllowed(patterns []string, file string) bool {
	file = strings.TrimPrefix(strings.ReplaceAll(file, "\\", "/"), "./")
	if file == "" || strings.HasPrefix(file, "../") {
		return false
	}
	if len(patterns) == 0 {
		return defaultSafePath(file)
	}
	for _, pattern := range patterns {
		matched, err := path.Match(pattern, file)
		if err == nil && matched {
			return true
		}
		if strings.HasSuffix(pattern, "/**") && strings.HasPrefix(file, strings.TrimSuffix(pattern, "**")) {
			return true
		}
	}
	return false
}

func defaultSafePath(file string) bool {
	base := path.Base(file)
	return strings.HasPrefix(file, "docs/") || strings.HasPrefix(file, "test/") || strings.HasPrefix(file, "tests/") ||
		strings.Contains(file, "/tests/") || strings.Contains(file, "/__tests__/") ||
		strings.HasSuffix(base, "_test.go") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")
}

func containsFold(values []string, candidate string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}

func unique(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

package github

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

type CheckObservation struct {
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
}

type PullRequestGate struct {
	Number                  int                `json:"number"`
	URL                     string             `json:"url"`
	HeadSHA                 string             `json:"head_sha"`
	BaseBranch              string             `json:"base_branch"`
	Open                    bool               `json:"open"`
	Draft                   bool               `json:"draft"`
	MergeableKnown          bool               `json:"mergeable_known"`
	Mergeable               bool               `json:"mergeable"`
	MergeableState          string             `json:"mergeable_state,omitempty"`
	ChangedFiles            []string           `json:"changed_files"`
	Checks                  []CheckObservation `json:"checks"`
	RequiredCheckStates     []string           `json:"required_check_states"`
	RequiredCheckNames      []string           `json:"required_check_names"`
	VisualHiveVerdictGreen  bool               `json:"visual_hive_verdict_green"`
	BranchProtectionEnabled bool               `json:"branch_protection_enabled"`
	Hold                    bool               `json:"hold"`
	HumanReviewRequired     bool               `json:"human_review_required"`
	BaselineChanged         bool               `json:"baseline_changed"`
	WorkflowChanged         bool               `json:"workflow_changed"`
	SecuritySensitive       bool               `json:"security_sensitive"`
	DeploymentChanged       bool               `json:"deployment_changed"`
}

type BranchProtectionSummary struct {
	Enabled         bool     `json:"enabled"`
	RequiredChecks  []string `json:"required_checks"`
	RequiredReviews int      `json:"required_reviews"`
}

func (c *Client) BranchProtection(ctx context.Context, repository, branch string) (BranchProtectionSummary, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return BranchProtectionSummary{}, err
	}
	required, reviews, enabled, err := c.requiredMergeRules(ctx, owner, repo, branch)
	if err != nil {
		return BranchProtectionSummary{}, err
	}
	return BranchProtectionSummary{Enabled: enabled, RequiredChecks: sortedKeys(required), RequiredReviews: reviews}, nil
}

// InspectPullRequestGate reads every merge-relevant signal for one exact PR
// head. Missing, pending, skipped, neutral, stale, and cancelled required
// checks remain non-success states and therefore cannot be authorized later.
func (c *Client) InspectPullRequestGate(ctx context.Context, repository string, number int) (PullRequestGate, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return PullRequestGate{}, err
	}
	if number <= 0 {
		return PullRequestGate{}, fmt.Errorf("pull request number is required")
	}
	pull, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return PullRequestGate{}, fmt.Errorf("get pull request #%d: %w", number, err)
	}
	gate := PullRequestGate{
		Number: number, URL: pull.GetHTMLURL(), HeadSHA: pull.GetHead().GetSHA(), BaseBranch: pull.GetBase().GetRef(),
		Open: pull.GetState() == "open", Draft: pull.GetDraft(), MergeableState: pull.GetMergeableState(),
	}
	if pull.Mergeable != nil {
		gate.MergeableKnown, gate.Mergeable = true, pull.GetMergeable()
	}
	gate.Hold = gate.Draft || hasHoldLabel(pull.Labels)
	gate.ChangedFiles, err = c.listPullRequestFiles(ctx, owner, repo, number)
	if err != nil {
		return gate, err
	}
	classifyGatePaths(&gate)

	required, requiredReviews, protected, err := c.requiredMergeRules(ctx, owner, repo, gate.BaseBranch)
	if err != nil {
		return gate, err
	}
	gate.BranchProtectionEnabled = protected
	observations, states, err := c.checkObservations(ctx, owner, repo, gate.HeadSHA)
	if err != nil {
		return gate, err
	}
	gate.Checks = observations
	for _, check := range observations {
		if strings.Contains(normalizeName(check.Name), "visual hive") && check.State == "success" {
			gate.VisualHiveVerdictGreen = true
		}
	}
	gate.RequiredCheckNames = sortedKeys(required)
	for _, name := range gate.RequiredCheckNames {
		state := states[normalizeName(name)]
		if state == "" {
			state = "pending"
		}
		gate.RequiredCheckStates = append(gate.RequiredCheckStates, state)
	}
	if requiredReviews > 0 {
		approvals, changesRequested, reviewErr := c.reviewState(ctx, owner, repo, number)
		if reviewErr != nil {
			return gate, reviewErr
		}
		gate.HumanReviewRequired = approvals < requiredReviews || changesRequested
	}
	return gate, nil
}

func (c *Client) MergePullRequestExact(ctx context.Context, repository string, number int, expectedHeadSHA string) (string, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return "", err
	}
	if number <= 0 || strings.TrimSpace(expectedHeadSHA) == "" {
		return "", fmt.Errorf("pull request number and exact expected head SHA are required")
	}
	merged, _, err := c.client.PullRequests.Merge(ctx, owner, repo, number, "", &gh.PullRequestOptions{SHA: expectedHeadSHA, MergeMethod: "squash"})
	if err != nil {
		return "", fmt.Errorf("merge pull request #%d at %s: %w", number, expectedHeadSHA, err)
	}
	if !merged.GetMerged() || merged.GetSHA() == "" {
		return "", fmt.Errorf("GitHub did not merge pull request #%d: %s", number, merged.GetMessage())
	}
	return merged.GetSHA(), nil
}

func (c *Client) listPullRequestFiles(ctx context.Context, owner, repo string, number int) ([]string, error) {
	files := []string{}
	options := &gh.ListOptions{PerPage: 100}
	for {
		page, response, err := c.client.PullRequests.ListFiles(ctx, owner, repo, number, options)
		if err != nil {
			return nil, fmt.Errorf("list files for pull request #%d: %w", number, err)
		}
		for _, file := range page {
			if name := strings.TrimPrefix(file.GetFilename(), "./"); name != "" {
				files = append(files, name)
			}
		}
		if response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	sort.Strings(files)
	return uniqueStrings(files), nil
}

func (c *Client) requiredMergeRules(ctx context.Context, owner, repo, branch string) (map[string]bool, int, bool, error) {
	required := map[string]bool{}
	protection, response, err := c.client.Repositories.GetBranchProtection(ctx, owner, repo, branch)
	if err == nil {
		if checks := protection.GetRequiredStatusChecks(); checks != nil {
			for _, name := range checks.GetContexts() {
				required[name] = true
			}
			for _, check := range checks.GetChecks() {
				required[check.Context] = true
			}
		}
		reviews := 0
		if enforcement := protection.GetRequiredPullRequestReviews(); enforcement != nil {
			reviews = enforcement.RequiredApprovingReviewCount
		}
		return required, reviews, true, nil
	}
	if response == nil || response.StatusCode != http.StatusNotFound {
		return nil, 0, false, fmt.Errorf("read branch protection for %s: %w", branch, err)
	}
	rules, rulesResponse, rulesErr := c.client.Repositories.GetRulesForBranch(ctx, owner, repo, branch, &gh.ListOptions{PerPage: 100})
	if rulesErr != nil {
		if rulesResponse != nil && rulesResponse.StatusCode == http.StatusNotFound {
			return required, 0, false, nil
		}
		return nil, 0, false, fmt.Errorf("read repository rules for %s: %w", branch, rulesErr)
	}
	protected := branchRulesPresent(rules)
	reviews := 0
	for _, rule := range rules.RequiredStatusChecks {
		for _, check := range rule.Parameters.RequiredStatusChecks {
			required[check.Context] = true
		}
	}
	for _, rule := range rules.PullRequest {
		if rule.Parameters.RequiredApprovingReviewCount > reviews {
			reviews = rule.Parameters.RequiredApprovingReviewCount
		}
	}
	return required, reviews, protected, nil
}

func (c *Client) checkObservations(ctx context.Context, owner, repo, sha string) ([]CheckObservation, map[string]string, error) {
	if sha == "" {
		return nil, nil, fmt.Errorf("pull request head SHA is missing")
	}
	states := map[string]string{}
	byName := map[string]CheckObservation{}
	options := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, response, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, options)
		if err != nil {
			return nil, nil, fmt.Errorf("list check runs for %s: %w", sha, err)
		}
		for _, run := range runs.CheckRuns {
			name := run.GetName()
			state := normalizeCheckState(run.GetStatus(), run.GetConclusion())
			key := normalizeName(name)
			states[key] = state
			byName[key] = CheckObservation{Name: name, State: state, URL: valueOr(run.GetHTMLURL(), run.GetDetailsURL())}
		}
		if response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	statusOptions := &gh.ListOptions{PerPage: 100}
	for {
		combined, response, err := c.client.Repositories.GetCombinedStatus(ctx, owner, repo, sha, statusOptions)
		if err != nil {
			return nil, nil, fmt.Errorf("list commit statuses for %s: %w", sha, err)
		}
		for _, status := range combined.Statuses {
			name := status.GetContext()
			key := normalizeName(name)
			states[key] = strings.ToLower(status.GetState())
			byName[key] = CheckObservation{Name: name, State: states[key], URL: status.GetTargetURL()}
		}
		if response.NextPage == 0 {
			break
		}
		statusOptions.Page = response.NextPage
	}
	keys := sortedKeys(byName)
	result := make([]CheckObservation, 0, len(keys))
	for _, key := range keys {
		result = append(result, byName[key])
	}
	return result, states, nil
}

func (c *Client) reviewState(ctx context.Context, owner, repo string, number int) (int, bool, error) {
	reviews, _, err := c.client.PullRequests.ListReviews(ctx, owner, repo, number, &gh.ListOptions{PerPage: 100})
	if err != nil {
		return 0, false, fmt.Errorf("list reviews for pull request #%d: %w", number, err)
	}
	latest := map[string]string{}
	for _, review := range reviews {
		login := strings.ToLower(review.GetUser().GetLogin())
		if login != "" {
			latest[login] = strings.ToLower(review.GetState())
		}
	}
	approvals, changes := 0, false
	for _, state := range latest {
		approvals += boolInt(state == "approved")
		changes = changes || state == "changes_requested"
	}
	return approvals, changes, nil
}

func classifyGatePaths(gate *PullRequestGate) {
	for _, file := range gate.ChangedFiles {
		lower := strings.ToLower(strings.ReplaceAll(file, "\\", "/"))
		gate.WorkflowChanged = gate.WorkflowChanged || strings.HasPrefix(lower, ".github/workflows/")
		gate.BaselineChanged = gate.BaselineChanged || strings.Contains(lower, "baseline") || strings.Contains(lower, "__screenshots__")
		gate.SecuritySensitive = gate.SecuritySensitive || containsPathToken(lower, "auth", "security", "secret", "permission", "rbac", "policy")
		gate.DeploymentChanged = gate.DeploymentChanged || containsPathToken(lower, "deploy", "terraform", "infra", "k8s", "helm") || lower == "dockerfile"
	}
}

func branchRulesPresent(rules *gh.BranchRules) bool {
	if rules == nil {
		return false
	}
	return len(rules.Creation)+len(rules.Update)+len(rules.Deletion)+len(rules.RequiredLinearHistory)+len(rules.MergeQueue)+
		len(rules.RequiredDeployments)+len(rules.RequiredSignatures)+len(rules.PullRequest)+len(rules.RequiredStatusChecks)+
		len(rules.NonFastForward)+len(rules.Workflows)+len(rules.CodeScanning) > 0
}

func hasHoldLabel(labels []*gh.Label) bool {
	for _, label := range labels {
		name := normalizeName(label.GetName())
		if strings.Contains(name, "hold") || strings.Contains(name, "do not merge") || strings.Contains(name, "do-not-merge") {
			return true
		}
	}
	return false
}

func normalizeCheckState(status, conclusion string) string {
	if strings.ToLower(status) != "completed" {
		return "pending"
	}
	if value := strings.ToLower(strings.TrimSpace(conclusion)); value != "" {
		return value
	}
	return "pending"
}

func normalizeName(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func containsPathToken(value string, tokens ...string) bool {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '-' || r == '_' || r == '.' })
	for _, part := range parts {
		for _, token := range tokens {
			if part == token {
				return true
			}
		}
	}
	return false
}

func sortedKeys[T any](values map[string]T) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func uniqueStrings(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func valueOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

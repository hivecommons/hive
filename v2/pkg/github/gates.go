package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const maxPullRequestDiffBytes = 16 << 20

const (
	visualHivePullRequestContext      = "visual-hive"
	visualHivePullRequestWorkflowName = "Visual Hive PR"
	visualHivePullRequestWorkflowPath = ".github/workflows/visual-hive-pr.yml"
	visualHivePullRequestEvent        = "pull_request"
)

type CheckObservation struct {
	ID                 int64  `json:"id,omitempty"`
	Name               string `json:"name"`
	State              string `json:"state"`
	URL                string `json:"url,omitempty"`
	AppID              int64  `json:"app_id,omitempty"`
	ProvenanceVerified bool   `json:"provenance_verified,omitempty"`
	WorkflowRunID      int64  `json:"workflow_run_id,omitempty"`
	WorkflowPath       string `json:"workflow_path,omitempty"`
	WorkflowEvent      string `json:"workflow_event,omitempty"`
}

type PullRequestGate struct {
	Number                       int                `json:"number"`
	URL                          string             `json:"url"`
	HeadSHA                      string             `json:"head_sha"`
	BaseSHA                      string             `json:"base_sha"`
	Merged                       bool               `json:"merged"`
	MergeSHA                     string             `json:"merge_sha,omitempty"`
	MergedBy                     string             `json:"merged_by,omitempty"`
	BaseBranch                   string             `json:"base_branch"`
	Open                         bool               `json:"open"`
	Draft                        bool               `json:"draft"`
	MergeableKnown               bool               `json:"mergeable_known"`
	Mergeable                    bool               `json:"mergeable"`
	MergeableState               string             `json:"mergeable_state,omitempty"`
	ChangedFiles                 []string           `json:"changed_files"`
	Checks                       []CheckObservation `json:"checks"`
	RequiredCheckStates          []string           `json:"required_check_states"`
	RequiredCheckNames           []string           `json:"required_check_names"`
	VisualHiveVerdictGreen       bool               `json:"visual_hive_verdict_green"`
	VisualHiveRequired           bool               `json:"visual_hive_required"`
	VisualHiveProvenanceVerified bool               `json:"visual_hive_provenance_verified"`
	VisualHiveCheckState         string             `json:"visual_hive_check_state,omitempty"`
	VisualHiveCheckRunID         int64              `json:"visual_hive_check_run_id,omitempty"`
	VisualHiveWorkflowRunID      int64              `json:"visual_hive_workflow_run_id,omitempty"`
	VisualHiveWorkflowPath       string             `json:"visual_hive_workflow_path,omitempty"`
	VisualHiveWorkflowEvent      string             `json:"visual_hive_workflow_event,omitempty"`
	// BranchProtectionEnabled is the merge-safety signal consumed by Hive. It
	// is true only when protection is configured and requires the pull request
	// branch to be up to date with its base before merge.
	BranchProtectionEnabled       bool `json:"branch_protection_enabled"`
	BranchProtectionConfigured    bool `json:"branch_protection_configured"`
	BranchProtectionStrict        bool `json:"branch_protection_strict"`
	BranchProtectionAdminEnforced bool `json:"branch_protection_admin_enforced"`
	Hold                          bool `json:"hold"`
	HumanReviewRequired           bool `json:"human_review_required"`
	BaselineChanged               bool `json:"baseline_changed"`
	WorkflowChanged               bool `json:"workflow_changed"`
	SecuritySensitive             bool `json:"security_sensitive"`
	DeploymentChanged             bool `json:"deployment_changed"`
}

type BranchProtectionSummary struct {
	Enabled                 bool                    `json:"enabled"`
	Strict                  bool                    `json:"strict"`
	AdminEnforced           bool                    `json:"admin_enforced"`
	RequiredChecks          []string                `json:"required_checks"`
	RequiredCheckIdentities []RequiredCheckIdentity `json:"required_check_identities"`
	RequiredReviews         int                     `json:"required_reviews"`
}

// RequiredCheckIdentity binds a required status check to the GitHub App that
// is allowed to satisfy it. AppID must be positive; name-only and "any app"
// requirements are intentionally insufficient for Hive auto-merge.
type RequiredCheckIdentity struct {
	Context string `json:"context"`
	AppID   int64  `json:"app_id"`
}

type branchMergeRules struct {
	requiredChecks  map[string]requiredCheck
	requiredReviews int
	enabled         bool
	strict          bool
	adminEnforced   bool
}

type requiredCheck struct {
	Context string
	AppID   int64
}

// MergeGateAuthorizer is invoked with a freshly inspected, complete pull
// request gate at the final mutation boundary. Callers must re-run their
// repository policy and any durable human-approval binding against this gate.
// A nil authorizer is never permitted: exact refs alone are not sufficient
// authority for Hive to merge.
type MergeGateAuthorizer func(PullRequestGate, string) error

// FinalMergeGateError proves that Hive did not reach GitHub's merge mutation:
// the second complete gate/diff read failed or no longer matched the gate that
// the callback authorized. Callers may safely invalidate the just-written
// merge intent instead of treating the outcome as an ambiguous merge.
type FinalMergeGateError struct {
	Cause error
}

func (e *FinalMergeGateError) Error() string {
	if e == nil || e.Cause == nil {
		return "final merge gate changed before mutation"
	}
	return e.Cause.Error()
}

func (e *FinalMergeGateError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// EnsureMinimumBranchProtection creates a conservative default protection rule
// only when the branch has no existing protection or ruleset. Existing policy
// is never overwritten because doing so could silently remove reviews or
// organization controls that Hive does not own.
func (c *Client) EnsureMinimumBranchProtection(ctx context.Context, repository, branch string, requiredChecks []RequiredCheckIdentity) (bool, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return false, err
	}
	for _, required := range requiredChecks {
		if strings.TrimSpace(required.Context) == "" || required.AppID <= 0 {
			return false, fmt.Errorf("required check identity needs a non-empty context and positive GitHub App ID")
		}
	}
	current, err := c.BranchProtection(ctx, repository, branch)
	if err != nil {
		return false, err
	}
	if current.Enabled {
		if len(current.RequiredChecks) == 0 {
			return false, fmt.Errorf("existing branch protection for %s has no required checks; configure at least one check explicitly without replacing repository-owned policy", branch)
		}
		if !current.Strict {
			return false, fmt.Errorf("existing branch protection for %s does not require pull request branches to be up to date; enable strict required status checks without replacing repository-owned policy", branch)
		}
		if !current.AdminEnforced {
			return false, fmt.Errorf("existing branch protection for %s can be bypassed by repository administrators; enable enforce_admins without replacing repository-owned policy", branch)
		}
		for _, required := range uniqueRequiredCheckIdentities(requiredChecks) {
			present := false
			for _, existing := range current.RequiredCheckIdentities {
				if exactCheckName(existing.Context) == exactCheckName(required.Context) && existing.AppID == required.AppID {
					present = true
					break
				}
			}
			if !present {
				return false, fmt.Errorf("existing branch protection for %s does not require the exact %q check from GitHub App ID %d; add that identity without replacing repository-owned policy", branch, required.Context, required.AppID)
			}
		}
		return false, nil
	}
	checks := uniqueRequiredCheckIdentities(requiredChecks)
	if len(checks) == 0 {
		return false, fmt.Errorf("at least one required check is needed to create branch protection")
	}
	requestChecks := make([]*gh.RequiredStatusCheck, 0, len(checks))
	for _, check := range checks {
		appID := check.AppID
		requestChecks = append(requestChecks, &gh.RequiredStatusCheck{Context: strings.TrimSpace(check.Context), AppID: &appID})
	}
	linear, force, deletions, conversations := true, false, false, true
	request := &gh.ProtectionRequest{
		RequiredStatusChecks: &gh.RequiredStatusChecks{Strict: true, Checks: &requestChecks},
		EnforceAdmins:        true, RequireLinearHistory: &linear, AllowForcePushes: &force, AllowDeletions: &deletions,
		RequiredConversationResolution: &conversations,
	}
	if _, _, err := c.client.Repositories.UpdateBranchProtection(ctx, owner, repo, branch, request); err != nil {
		return false, fmt.Errorf("create minimum branch protection for %s: %w", branch, err)
	}
	return true, nil
}

// EnsureMinimumVisualHiveBranchProtection resolves the authoritative GitHub
// Actions App identity from the active GitHub API and binds the PR-only
// visual-hive requirement to it. Callers must independently verify workflow
// provenance before treating a same-name Check Run as the PR verdict.
func (c *Client) EnsureMinimumVisualHiveBranchProtection(ctx context.Context, repository, branch string) (bool, error) {
	appID, err := c.ExpectedGitHubAppID(ctx, "github-actions")
	if err != nil {
		return false, fmt.Errorf("resolve expected Visual Hive check producer: %w", err)
	}
	return c.EnsureMinimumBranchProtection(ctx, repository, branch, []RequiredCheckIdentity{{Context: "visual-hive", AppID: appID}})
}

// ExpectedGitHubAppID returns the immutable numeric identity for an expected
// check producer. Resolving through the configured API also supports GHES,
// where GitHub-owned App IDs can differ from github.com.
func (c *Client) ExpectedGitHubAppID(ctx context.Context, slug string) (int64, error) {
	if c == nil || c.client == nil {
		return 0, fmt.Errorf("GitHub client is required")
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return 0, fmt.Errorf("GitHub App slug is required")
	}
	app, _, err := c.client.Apps.Get(ctx, slug)
	if err != nil {
		return 0, fmt.Errorf("read GitHub App %s: %w", slug, err)
	}
	if app.GetID() <= 0 || !strings.EqualFold(strings.TrimSpace(app.GetSlug()), slug) {
		return 0, fmt.Errorf("GitHub App %s returned an invalid identity", slug)
	}
	return app.GetID(), nil
}

func (c *Client) BranchProtection(ctx context.Context, repository, branch string) (BranchProtectionSummary, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return BranchProtectionSummary{}, err
	}
	rules, err := c.requiredMergeRules(ctx, owner, repo, branch)
	if err != nil {
		return BranchProtectionSummary{}, err
	}
	return BranchProtectionSummary{
		Enabled:                 rules.enabled,
		Strict:                  rules.strict,
		AdminEnforced:           rules.adminEnforced,
		RequiredChecks:          requiredCheckNames(rules.requiredChecks),
		RequiredCheckIdentities: publicRequiredCheckIdentities(rules.requiredChecks),
		RequiredReviews:         rules.requiredReviews,
	}, nil
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
		Number: number, URL: pull.GetHTMLURL(), HeadSHA: pull.GetHead().GetSHA(), BaseSHA: pull.GetBase().GetSHA(), BaseBranch: pull.GetBase().GetRef(),
		Open: pull.GetState() == "open", Merged: pull.GetMerged(), MergeSHA: pull.GetMergeCommitSHA(), MergedBy: pull.GetMergedBy().GetLogin(),
		Draft: pull.GetDraft(), MergeableState: pull.GetMergeableState(),
	}
	if pull.Mergeable != nil {
		gate.MergeableKnown, gate.Mergeable = true, pull.GetMergeable()
	}
	gate.Hold = gate.Draft || hasHoldLabel(pull.Labels)
	gate.ChangedFiles, err = c.listPullRequestFiles(ctx, owner, repo, number, pull.GetChangedFiles())
	if err != nil {
		return gate, err
	}
	classifyGatePaths(&gate)

	rules, err := c.requiredMergeRules(ctx, owner, repo, gate.BaseBranch)
	if err != nil {
		return gate, err
	}
	gate.BranchProtectionConfigured = rules.enabled
	gate.BranchProtectionStrict = rules.strict
	gate.BranchProtectionAdminEnforced = rules.adminEnforced
	gate.BranchProtectionEnabled = rules.enabled && rules.strict && rules.adminEnforced
	expectedVisualAppID, err := c.ExpectedGitHubAppID(ctx, "github-actions")
	if err != nil {
		return gate, fmt.Errorf("resolve expected Visual Hive check producer: %w", err)
	}
	observations, states, candidates, err := c.checkObservations(ctx, owner, repo, gate.HeadSHA)
	if err != nil {
		return gate, err
	}
	gate.Checks = observations
	gate.RequiredCheckNames = requiredCheckNames(rules.requiredChecks)
	identities := sortedRequiredChecks(rules.requiredChecks)
	var visualProof pullRequestCheckProof
	visualProofLoaded, visualProofFound := false, false
	for _, required := range identities {
		state := states[checkIdentityKey(required.Context, required.AppID)]
		if exactCheckName(required.Context) == visualHivePullRequestContext && required.AppID == expectedVisualAppID {
			gate.VisualHiveRequired = true
			if !visualProofLoaded {
				visualProof, visualProofFound, err = c.verifyPullRequestWorkflowCheck(ctx, owner, repo, pull, candidates[checkIdentityKey(required.Context, required.AppID)], expectedVisualAppID)
				if err != nil {
					return gate, err
				}
				visualProofLoaded = true
			}
			state = "pending"
			if visualProofFound {
				state = visualProof.State
				gate.VisualHiveProvenanceVerified = true
				gate.VisualHiveCheckState = state
				gate.VisualHiveCheckRunID = visualProof.CheckRunID
				gate.VisualHiveWorkflowRunID = visualProof.WorkflowRunID
				gate.VisualHiveWorkflowPath = visualProof.WorkflowPath
				gate.VisualHiveWorkflowEvent = visualProof.WorkflowEvent
			}
			gate.Checks = replaceRequiredCheckObservation(gate.Checks, CheckObservation{
				ID: visualProof.CheckRunID, Name: required.Context, State: state, URL: visualProof.URL, AppID: required.AppID,
				ProvenanceVerified: visualProofFound, WorkflowRunID: visualProof.WorkflowRunID,
				WorkflowPath: visualProof.WorkflowPath, WorkflowEvent: visualProof.WorkflowEvent,
			})
		}
		if state == "" {
			state = "pending"
		}
		gate.RequiredCheckStates = append(gate.RequiredCheckStates, state)
		if exactCheckName(required.Context) == visualHivePullRequestContext && required.AppID == expectedVisualAppID && visualProofFound && state == "success" {
			gate.VisualHiveVerdictGreen = true
		}
	}
	// Bootstrap baseline review occurs before Hive activates its conservative
	// required-check protection. Still prove the exact GitHub-Actions-App-bound
	// Visual Hive PR workflow; normal merge policy continues to require branch
	// protection separately and cannot use this as a protection bypass.
	if !visualProofLoaded {
		visualProof, visualProofFound, err = c.verifyPullRequestWorkflowCheck(ctx, owner, repo, pull, candidates[checkIdentityKey(visualHivePullRequestContext, expectedVisualAppID)], expectedVisualAppID)
		if err != nil {
			return gate, err
		}
		state := "pending"
		if visualProofFound {
			state = visualProof.State
			gate.VisualHiveProvenanceVerified = true
			gate.VisualHiveCheckState = state
			gate.VisualHiveCheckRunID = visualProof.CheckRunID
			gate.VisualHiveWorkflowRunID = visualProof.WorkflowRunID
			gate.VisualHiveWorkflowPath = visualProof.WorkflowPath
			gate.VisualHiveWorkflowEvent = visualProof.WorkflowEvent
			gate.VisualHiveVerdictGreen = state == "success"
		}
		gate.Checks = replaceRequiredCheckObservation(gate.Checks, CheckObservation{
			ID: visualProof.CheckRunID, Name: visualHivePullRequestContext, State: state, URL: visualProof.URL, AppID: expectedVisualAppID,
			ProvenanceVerified: visualProofFound, WorkflowRunID: visualProof.WorkflowRunID,
			WorkflowPath: visualProof.WorkflowPath, WorkflowEvent: visualProof.WorkflowEvent,
		})
	}
	approvals, changesRequested, reviewErr := c.reviewState(ctx, owner, repo, number)
	if reviewErr != nil {
		return gate, reviewErr
	}
	gate.HumanReviewRequired = approvals < rules.requiredReviews || changesRequested
	return gate, nil
}

func (c *Client) PullRequestDiffDigest(ctx context.Context, repository string, number int) (string, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return "", err
	}
	if number <= 0 {
		return "", fmt.Errorf("pull request number is required")
	}
	diff, _, err := c.client.PullRequests.GetRaw(ctx, owner, repo, number, gh.RawOptions{Type: gh.Diff})
	if err != nil {
		return "", fmt.Errorf("read raw diff for pull request #%d: %w", number, err)
	}
	if len(diff) == 0 || len(diff) > maxPullRequestDiffBytes {
		return "", fmt.Errorf("raw diff for pull request #%d is empty or exceeds %d bytes", number, maxPullRequestDiffBytes)
	}
	digest := sha256.Sum256([]byte(diff))
	return hex.EncodeToString(digest[:]), nil
}

func (c *Client) MergePullRequestExact(ctx context.Context, repository string, number int, expectedHeadSHA, expectedBaseSHA, expectedBaseBranch string, authorize MergeGateAuthorizer) (string, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return "", err
	}
	expectedHeadSHA = strings.TrimSpace(expectedHeadSHA)
	expectedBaseSHA = strings.TrimSpace(expectedBaseSHA)
	expectedBaseBranch = strings.TrimSpace(expectedBaseBranch)
	if number <= 0 || expectedHeadSHA == "" || expectedBaseSHA == "" || expectedBaseBranch == "" || authorize == nil {
		return "", fmt.Errorf("pull request number, exact expected head and base SHAs, expected base branch, and live gate authorizer are required")
	}

	// GitHub's conditional merge endpoint binds only the head SHA. Re-inspect
	// every merge-relevant signal immediately before that call, then require the
	// caller to re-authorize the complete live gate. This prevents an earlier
	// green snapshot from surviving a newly added hold, changed required check,
	// weakened protection, review requirement, or newly risky diff.
	live, err := c.InspectPullRequestGate(ctx, repository, number)
	if err != nil {
		return "", fmt.Errorf("re-inspect complete pull request #%d gate before merge: %w", number, err)
	}
	if !live.Open || live.Merged {
		return "", fmt.Errorf("refusing to merge pull request #%d because it is no longer open", number)
	}
	if !strings.EqualFold(live.HeadSHA, expectedHeadSHA) {
		return "", fmt.Errorf("refusing to merge pull request #%d: live head SHA %q does not match expected %q", number, live.HeadSHA, expectedHeadSHA)
	}
	if !strings.EqualFold(live.BaseSHA, expectedBaseSHA) {
		return "", fmt.Errorf("refusing to merge pull request #%d: live base SHA %q does not match expected %q", number, live.BaseSHA, expectedBaseSHA)
	}
	if !strings.EqualFold(live.BaseBranch, expectedBaseBranch) {
		return "", fmt.Errorf("refusing to merge pull request #%d: live base branch %q does not match expected %q", number, live.BaseBranch, expectedBaseBranch)
	}
	authorizedDiff, err := c.PullRequestDiffDigest(ctx, repository, number)
	if err != nil {
		return "", fmt.Errorf("bind pull request #%d diff before final live gate authorization: %w", number, err)
	}
	if err := authorize(live, authorizedDiff); err != nil {
		return "", fmt.Errorf("refusing to merge pull request #%d after final live gate authorization: %w", number, err)
	}

	// Authorization can perform several local durability and identity reads.
	// Re-inspect the entire gate and raw diff after that work, then demand exact
	// equivalence. This catches policy/check/review/label/path drift during the
	// callback and shrinks the remaining unavoidable GitHub race to the final
	// complete read followed by the conditional merge request.
	finalGate, err := c.InspectPullRequestGate(ctx, repository, number)
	if err != nil {
		return "", &FinalMergeGateError{Cause: fmt.Errorf("re-inspect pull request #%d after durable authorization: %w", number, err)}
	}
	finalDiff, err := c.PullRequestDiffDigest(ctx, repository, number)
	if err != nil {
		return "", &FinalMergeGateError{Cause: fmt.Errorf("re-read pull request #%d diff after durable authorization: %w", number, err)}
	}
	if !reflect.DeepEqual(live, finalGate) || !strings.EqualFold(authorizedDiff, finalDiff) {
		return "", &FinalMergeGateError{Cause: fmt.Errorf("refusing to merge pull request #%d because its complete gate or diff changed during final authorization", number)}
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

// DeleteRepairBranchExact removes only a Hive repair branch whose current ref
// still points at the exact head that passed merge gates. A moved branch is
// preserved for investigation instead of being deleted by name alone.
func (c *Client) DeleteRepairBranchExact(ctx context.Context, repository, branch, expectedHeadSHA string) error {
	_, err := c.deleteHiveBranchExact(ctx, repository, branch, expectedHeadSHA, "hive/repair-")
	return err
}

// DeleteBaselineBranchExact removes only a Hive baseline-review branch whose
// current ref still points at the exact reviewed proposal head. The bool is
// false when GitHub already removed the branch, making restart reconciliation
// idempotent without obscuring whether a mutation occurred.
func (c *Client) DeleteBaselineBranchExact(ctx context.Context, repository, branch, expectedHeadSHA string) (bool, error) {
	return c.deleteHiveBranchExact(ctx, repository, branch, expectedHeadSHA, "hive/baseline-")
}

// DeleteAuthorizerTransferBranchExact removes only the dedicated transfer ref
// while it still points at the exact durable managed head. It is used by the
// audited old-authorizer cancel path and never accepts a generic Hive branch.
func (c *Client) DeleteAuthorizerTransferBranchExact(ctx context.Context, repository, branch, expectedHeadSHA string) (bool, error) {
	return c.deleteHiveBranchExact(ctx, repository, branch, expectedHeadSHA, "hive/authorizer-transfer-")
}

// DeleteAuthorizerTransferBranchDescendantExact retires a transfer ref after
// an audited cancellation when GitHub updated the PR branch. The current ref is
// deleted only if GitHub proves the durable Hive commit is its ancestor.
func (c *Client) DeleteAuthorizerTransferBranchDescendantExact(ctx context.Context, repository, branch, ownedAncestorSHA string) (bool, string, error) {
	return c.deleteHiveBranchDescendantExact(ctx, repository, branch, ownedAncestorSHA, "hive/authorizer-transfer-")
}

// DeleteUninstallBranchDescendantExact is the equivalent exact descendant-only
// retirement path for a cancelled uninstall cleanup ref.
func (c *Client) DeleteUninstallBranchDescendantExact(ctx context.Context, repository, branch, ownedAncestorSHA string) (bool, string, error) {
	return c.deleteHiveBranchDescendantExact(ctx, repository, branch, ownedAncestorSHA, "hive/uninstall-")
}

// DeleteSetupBaselineBranchDescendantExact retires only the dedicated setup
// baseline review ref at an exact Hive-owned commit or proven descendant.
func (c *Client) DeleteSetupBaselineBranchDescendantExact(ctx context.Context, repository, branch, ownedAncestorSHA string) (bool, string, error) {
	return c.deleteHiveBranchDescendantExact(ctx, repository, branch, ownedAncestorSHA, "hive/setup-baseline-")
}

// DeleteSetupBranchDescendantExact retires only the repository setup ref at
// an exact Hive-owned commit or proven descendant. Setup-baseline refs are a
// distinct lifecycle and are deliberately rejected even though they share the
// "hive/setup-" prefix.
func (c *Client) DeleteSetupBranchDescendantExact(ctx context.Context, repository, branch, ownedAncestorSHA string) (bool, string, error) {
	if strings.HasPrefix(strings.TrimSpace(branch), "hive/setup-baseline-") {
		return false, "", fmt.Errorf("setup baseline branches require their dedicated retirement path")
	}
	return c.deleteHiveBranchDescendantExact(ctx, repository, branch, ownedAncestorSHA, "hive/setup-")
}

func (c *Client) deleteHiveBranchDescendantExact(ctx context.Context, repository, branch, ownedAncestorSHA, requiredPrefix string) (bool, string, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return false, "", err
	}
	branch = strings.TrimSpace(branch)
	ownedAncestorSHA = strings.ToLower(strings.TrimSpace(ownedAncestorSHA))
	if !strings.HasPrefix(branch, requiredPrefix) || !exactManagedSHA(ownedAncestorSHA) {
		return false, "", fmt.Errorf("exact Hive %s branch and immutable owned ancestor are required", strings.TrimSuffix(strings.TrimPrefix(requiredPrefix, "hive/"), "-"))
	}
	ref, response, err := c.client.Git.GetRef(ctx, owner, repo, "heads/"+branch)
	if err != nil {
		if response != nil && response.StatusCode == http.StatusNotFound {
			return false, "", nil
		}
		return false, "", fmt.Errorf("read Hive branch %s for descendant retirement: %w", branch, err)
	}
	currentHead := strings.ToLower(strings.TrimSpace(ref.GetObject().GetSHA()))
	if ref.GetRef() != "refs/heads/"+branch || !exactManagedSHA(currentHead) {
		return false, "", fmt.Errorf("Hive branch %s returned an invalid exact ref", branch)
	}
	if currentHead != ownedAncestorSHA {
		comparison, _, err := c.client.Repositories.CompareCommits(ctx, owner, repo, ownedAncestorSHA, currentHead, nil)
		if err != nil {
			return false, currentHead, fmt.Errorf("prove Hive branch %s descends from owned commit %s: %w", branch, ownedAncestorSHA, err)
		}
		if !strings.EqualFold(comparison.GetStatus(), "ahead") || comparison.GetBehindBy() != 0 {
			return false, currentHead, fmt.Errorf("refusing to delete moved Hive branch %s because %s is not a descendant of owned commit %s", branch, currentHead, ownedAncestorSHA)
		}
	}
	deleted, err := c.deleteHiveBranchExact(ctx, repository, branch, currentHead, requiredPrefix)
	return deleted, currentHead, err
}

func (c *Client) deleteHiveBranchExact(ctx context.Context, repository, branch, expectedHeadSHA, requiredPrefix string) (bool, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return false, err
	}
	branch = strings.TrimSpace(branch)
	expectedHeadSHA = strings.TrimSpace(expectedHeadSHA)
	if !strings.HasPrefix(branch, requiredPrefix) || expectedHeadSHA == "" {
		return false, fmt.Errorf("exact Hive %s branch and expected head SHA are required", strings.TrimSuffix(strings.TrimPrefix(requiredPrefix, "hive/"), "-"))
	}
	ref, response, err := c.client.Git.GetRef(ctx, owner, repo, "heads/"+branch)
	if err != nil {
		if response != nil && response.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("read Hive branch %s: %w", branch, err)
	}
	if ref.GetObject().GetSHA() != expectedHeadSHA {
		return false, fmt.Errorf("refusing to delete moved Hive branch %s: got %s, expected %s", branch, ref.GetObject().GetSHA(), expectedHeadSHA)
	}
	if _, err := c.client.Git.DeleteRef(ctx, owner, repo, "heads/"+branch); err != nil {
		return false, fmt.Errorf("delete Hive branch %s: %w", branch, err)
	}
	return true, nil
}

const (
	maxPullRequestFiles     = 3000
	maxPullRequestFilePages = maxPullRequestFiles / 100
	maxPullRequestReviews   = 10000
	maxPullReviewPages      = maxPullRequestReviews / 100
)

func (c *Client) listPullRequestFiles(ctx context.Context, owner, repo string, number, expectedCount int) ([]string, error) {
	if expectedCount < 0 || expectedCount > maxPullRequestFiles {
		return nil, fmt.Errorf("pull request #%d reports %d changed files; Hive requires a complete inventory of at most %d files", number, expectedCount, maxPullRequestFiles)
	}
	files := make([]string, 0, expectedCount)
	seen := make(map[string]struct{}, expectedCount)
	options := &gh.ListOptions{PerPage: 100}
	pages := 0
	for {
		pages++
		if pages > maxPullRequestFilePages {
			return nil, fmt.Errorf("file inventory for pull request #%d exceeded the bounded %d-page GitHub response", number, maxPullRequestFilePages)
		}
		page, response, err := c.client.PullRequests.ListFiles(ctx, owner, repo, number, options)
		if err != nil {
			return nil, fmt.Errorf("list files for pull request #%d: %w", number, err)
		}
		if response == nil {
			return nil, fmt.Errorf("list files for pull request #%d returned no pagination metadata", number)
		}
		for _, file := range page {
			name := strings.TrimPrefix(strings.TrimSpace(file.GetFilename()), "./")
			if name == "" {
				return nil, fmt.Errorf("file inventory for pull request #%d contains an empty path", number)
			}
			if _, duplicate := seen[name]; duplicate {
				return nil, fmt.Errorf("file inventory for pull request #%d contains duplicate path %s", number, name)
			}
			seen[name] = struct{}{}
			files = append(files, name)
			if len(files) > expectedCount {
				return nil, fmt.Errorf("file inventory for pull request #%d exceeds the reported changed-file count %d", number, expectedCount)
			}
		}
		if response.NextPage == 0 {
			break
		}
		if response.NextPage <= options.Page {
			return nil, fmt.Errorf("file inventory for pull request #%d returned non-progressing pagination", number)
		}
		options.Page = response.NextPage
	}
	if len(files) != expectedCount {
		return nil, fmt.Errorf("file inventory for pull request #%d is incomplete: received %d of %d reported changed files", number, len(files), expectedCount)
	}
	sort.Strings(files)
	return files, nil
}

func (c *Client) requiredMergeRules(ctx context.Context, owner, repo, branch string) (branchMergeRules, error) {
	rules := branchMergeRules{requiredChecks: map[string]requiredCheck{}}
	protection, response, err := c.client.Repositories.GetBranchProtection(ctx, owner, repo, branch)
	if err == nil {
		if checks := protection.GetRequiredStatusChecks(); checks != nil {
			rules.strict = checks.Strict
			specific := map[string]bool{}
			for _, check := range checks.GetChecks() {
				contextName := strings.TrimSpace(check.Context)
				if contextName == "" {
					continue
				}
				specific[exactCheckName(contextName)] = true
				addRequiredCheck(rules.requiredChecks, contextName, requiredAppID(check.GetAppID()))
			}
			for _, name := range checks.GetContexts() {
				if !specific[exactCheckName(name)] {
					addRequiredCheck(rules.requiredChecks, name, -1)
				}
			}
		}
		rules.enabled = true
		rules.adminEnforced = protection.GetEnforceAdmins() != nil && protection.GetEnforceAdmins().Enabled && rules.strict
		if enforcement := protection.GetRequiredPullRequestReviews(); enforcement != nil {
			rules.requiredReviews = enforcement.RequiredApprovingReviewCount
		}
	} else if response == nil || response.StatusCode != http.StatusNotFound {
		return branchMergeRules{}, fmt.Errorf("read branch protection for %s: %w", branch, err)
	}
	branchRules, rulesResponse, rulesErr := c.allRulesForBranch(ctx, owner, repo, branch)
	if rulesErr != nil {
		if rulesResponse != nil && rulesResponse.StatusCode == http.StatusNotFound {
			return rules, nil
		}
		return branchMergeRules{}, fmt.Errorf("read repository rules for %s: %w", branch, rulesErr)
	}
	hasApplicableRuleset := branchRulesPresent(branchRules)
	rules.enabled = rules.enabled || hasApplicableRuleset
	if hasApplicableRuleset {
		// The branch-rules endpoint exposes effective rules but not enough
		// information to prove that Hive's writer cannot use a ruleset bypass,
		// and this client does not yet evaluate every merge-relevant rule type
		// (for example deployments, workflows, and code scanning). Keep the
		// unioned checks/reviews below for diagnostics, but never advertise the
		// combined policy as admin-enforced until both semantics are provable.
		rules.adminEnforced = false
	}
	for _, rule := range branchRules.RequiredStatusChecks {
		rules.strict = rules.strict || rule.Parameters.StrictRequiredStatusChecksPolicy
		for _, check := range rule.Parameters.RequiredStatusChecks {
			addRequiredCheck(rules.requiredChecks, check.Context, requiredAppID(check.GetIntegrationID()))
		}
	}
	for _, rule := range branchRules.PullRequest {
		if rule.Parameters.RequiredApprovingReviewCount > rules.requiredReviews {
			rules.requiredReviews = rule.Parameters.RequiredApprovingReviewCount
		}
	}
	return rules, nil
}

func (c *Client) allRulesForBranch(ctx context.Context, owner, repo, branch string) (*gh.BranchRules, *gh.Response, error) {
	combined := &gh.BranchRules{}
	page := 1
	var last *gh.Response
	for page > 0 {
		current, response, err := c.client.Repositories.GetRulesForBranch(ctx, owner, repo, branch, &gh.ListOptions{Page: page, PerPage: 100})
		if err != nil {
			return nil, response, err
		}
		last = response
		if current != nil {
			destination, source := reflect.ValueOf(combined).Elem(), reflect.ValueOf(current).Elem()
			for index := 0; index < destination.NumField(); index++ {
				if destination.Field(index).Kind() == reflect.Slice {
					destination.Field(index).Set(reflect.AppendSlice(destination.Field(index), source.Field(index)))
				}
			}
		}
		if branchRuleCount(combined) > 512 {
			return nil, response, fmt.Errorf("applicable branch rule inventory exceeds the strict 512-rule bound")
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		page = response.NextPage
	}
	return combined, last, nil
}

func branchRuleCount(rules *gh.BranchRules) int {
	if rules == nil {
		return 0
	}
	value := reflect.ValueOf(rules).Elem()
	total := 0
	for index := 0; index < value.NumField(); index++ {
		if value.Field(index).Kind() == reflect.Slice {
			total += value.Field(index).Len()
		}
	}
	return total
}

func (c *Client) checkObservations(ctx context.Context, owner, repo, sha string) ([]CheckObservation, map[string]string, map[string][]*gh.CheckRun, error) {
	if sha == "" {
		return nil, nil, nil, fmt.Errorf("pull request head SHA is missing")
	}
	states := map[string]string{}
	byName := map[string]CheckObservation{}
	checkRunIDs := map[string]int64{}
	checkRunAnyIDs := map[string]int64{}
	checkRunNames := map[string]bool{}
	checkRunCandidates := map[string][]*gh.CheckRun{}
	options := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, response, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, options)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("list check runs for %s: %w", sha, err)
		}
		for _, run := range runs.CheckRuns {
			name := strings.TrimSpace(run.GetName())
			state := normalizeCheckState(run.GetStatus(), run.GetConclusion())
			appID := run.GetApp().GetID()
			key := checkIdentityKey(name, appID)
			checkRunCandidates[key] = append(checkRunCandidates[key], run)
			if prior, exists := checkRunIDs[key]; exists && run.GetID() <= prior {
				continue
			}
			checkRunIDs[key] = run.GetID()
			states[key] = state
			byName[key] = CheckObservation{ID: run.GetID(), Name: name, State: state, URL: valueOr(run.GetHTMLURL(), run.GetDetailsURL()), AppID: appID}
			nameKey := exactCheckName(name)
			checkRunNames[nameKey] = true
			anyKey := checkIdentityKey(name, -1)
			if prior, exists := checkRunAnyIDs[anyKey]; !exists || run.GetID() > prior {
				checkRunAnyIDs[anyKey] = run.GetID()
				states[anyKey] = state
			}
		}
		if response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	statusIDs := map[string]int64{}
	statusOptions := &gh.ListOptions{PerPage: 100}
	for {
		combined, response, err := c.client.Repositories.GetCombinedStatus(ctx, owner, repo, sha, statusOptions)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("list commit statuses for %s: %w", sha, err)
		}
		for _, status := range combined.Statuses {
			name := strings.TrimSpace(status.GetContext())
			key := checkIdentityKey(name, -1)
			// A Check Run and a legacy commit status can share a display name.
			// Branch protection treats the App-backed Check Run as the relevant
			// context, so a stale legacy status must not overwrite it.
			if checkRunNames[exactCheckName(name)] {
				continue
			}
			if prior, exists := statusIDs[key]; exists && status.GetID() <= prior {
				continue
			}
			statusIDs[key] = status.GetID()
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
	for key := range checkRunCandidates {
		sort.Slice(checkRunCandidates[key], func(i, j int) bool { return checkRunCandidates[key][i].GetID() > checkRunCandidates[key][j].GetID() })
	}
	return result, states, checkRunCandidates, nil
}

type pullRequestCheckProof struct {
	State         string
	CheckRunID    int64
	WorkflowRunID int64
	WorkflowPath  string
	WorkflowEvent string
	URL           string
}

func (c *Client) verifyPullRequestWorkflowCheck(ctx context.Context, owner, repo string, pull *gh.PullRequest, candidates []*gh.CheckRun, expectedAppID int64) (pullRequestCheckProof, bool, error) {
	for _, check := range candidates {
		if check == nil {
			continue
		}
		checkAssociationPresent, checkAssociationMatches := pullRequestAssociationStatus(check.PullRequests, pull)
		if check.GetID() <= 0 || check.GetName() != visualHivePullRequestContext || check.GetApp().GetID() != expectedAppID ||
			check.GetHeadSHA() != pull.GetHead().GetSHA() || check.GetCheckSuite().GetID() <= 0 || (checkAssociationPresent && !checkAssociationMatches) {
			continue
		}
		options := &gh.ListWorkflowRunsOptions{
			Event: visualHivePullRequestEvent, HeadSHA: pull.GetHead().GetSHA(), CheckSuiteID: check.GetCheckSuite().GetID(),
			ListOptions: gh.ListOptions{PerPage: 100},
		}
		var matched *gh.WorkflowRun
		for {
			runs, response, err := c.client.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, "visual-hive-pr.yml", options)
			if err != nil {
				return pullRequestCheckProof{}, false, fmt.Errorf("verify exact Visual Hive PR workflow provenance: %w", err)
			}
			for _, run := range runs.WorkflowRuns {
				if !exactPullRequestWorkflowRun(run, owner+"/"+repo, pull, check) {
					continue
				}
				if matched != nil && matched.GetID() != run.GetID() {
					return pullRequestCheckProof{}, false, fmt.Errorf("Visual Hive PR check run %d maps to multiple workflow runs", check.GetID())
				}
				matched = run
			}
			if response == nil || response.NextPage == 0 {
				break
			}
			options.Page = response.NextPage
		}
		if matched != nil {
			return pullRequestCheckProof{
				State: normalizeCheckState(check.GetStatus(), check.GetConclusion()), CheckRunID: check.GetID(), WorkflowRunID: matched.GetID(),
				WorkflowPath: visualHivePullRequestWorkflowPath, WorkflowEvent: visualHivePullRequestEvent, URL: valueOr(check.GetHTMLURL(), check.GetDetailsURL()),
			}, true, nil
		}
	}
	return pullRequestCheckProof{}, false, nil
}

func replaceRequiredCheckObservation(observations []CheckObservation, replacement CheckObservation) []CheckObservation {
	result := make([]CheckObservation, 0, len(observations)+1)
	replaced := false
	for _, observation := range observations {
		if checkIdentityKey(observation.Name, observation.AppID) == checkIdentityKey(replacement.Name, replacement.AppID) {
			if !replaced {
				result = append(result, replacement)
				replaced = true
			}
			continue
		}
		result = append(result, observation)
	}
	if !replaced {
		result = append(result, replacement)
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := exactCheckName(result[i].Name), exactCheckName(result[j].Name)
		if left == right {
			return result[i].AppID < result[j].AppID
		}
		return left < right
	})
	return result
}

func exactPullRequestWorkflowRun(run *gh.WorkflowRun, repository string, pull *gh.PullRequest, check *gh.CheckRun) bool {
	checkState := normalizeCheckState(check.GetStatus(), check.GetConclusion())
	checkAssociationPresent, checkAssociationMatches := pullRequestAssociationStatus(check.PullRequests, pull)
	runAssociationPresent, runAssociationMatches := pullRequestAssociationStatus(run.PullRequests, pull)
	exactAssociation := (checkAssociationPresent || runAssociationPresent) && (!checkAssociationPresent || checkAssociationMatches) && (!runAssociationPresent || runAssociationMatches)
	return run.GetID() > 0 && run.GetName() == visualHivePullRequestWorkflowName && exactWorkflowPath(run.GetPath()) == visualHivePullRequestWorkflowPath &&
		run.GetEvent() == visualHivePullRequestEvent && run.GetHeadSHA() == pull.GetHead().GetSHA() &&
		(pull.GetHead().GetRef() == "" || run.GetHeadBranch() == pull.GetHead().GetRef()) && run.GetCheckSuiteID() == check.GetCheckSuite().GetID() &&
		normalizeCheckState(run.GetStatus(), run.GetConclusion()) == checkState &&
		(run.GetRepository().GetFullName() == "" || strings.EqualFold(run.GetRepository().GetFullName(), repository)) &&
		(run.GetHeadRepository().GetFullName() == "" || pull.GetHead().GetRepo().GetFullName() == "" || strings.EqualFold(run.GetHeadRepository().GetFullName(), pull.GetHead().GetRepo().GetFullName())) &&
		exactAssociation
}

func pullRequestAssociationStatus(associations []*gh.PullRequest, pull *gh.PullRequest) (bool, bool) {
	if len(associations) == 0 {
		return false, false
	}
	for _, association := range associations {
		if association.GetNumber() != pull.GetNumber() ||
			(association.GetHead().GetSHA() != "" && association.GetHead().GetSHA() != pull.GetHead().GetSHA()) ||
			(association.GetHead().GetRef() != "" && association.GetHead().GetRef() != pull.GetHead().GetRef()) ||
			(association.GetBase().GetSHA() != "" && association.GetBase().GetSHA() != pull.GetBase().GetSHA()) ||
			(association.GetBase().GetRef() != "" && association.GetBase().GetRef() != pull.GetBase().GetRef()) {
			continue
		}
		return true, true
	}
	return true, false
}

func exactWorkflowPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if at := strings.IndexByte(value, '@'); at >= 0 {
		value = value[:at]
	}
	return strings.TrimPrefix(value, "/")
}

func exactCheckName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func requiredAppID(value int64) int64 {
	if value <= 0 {
		return -1
	}
	return value
}

func checkIdentityKey(context string, appID int64) string {
	return fmt.Sprintf("%s\x00%d", exactCheckName(context), appID)
}

func addRequiredCheck(checks map[string]requiredCheck, context string, appID int64) {
	context = strings.TrimSpace(context)
	if context == "" {
		return
	}
	name := exactCheckName(context)
	if appID >= 0 {
		delete(checks, checkIdentityKey(context, -1))
		checks[checkIdentityKey(context, appID)] = requiredCheck{Context: context, AppID: appID}
		return
	}
	for _, existing := range checks {
		if exactCheckName(existing.Context) == name && existing.AppID >= 0 {
			return
		}
	}
	checks[checkIdentityKey(context, -1)] = requiredCheck{Context: context, AppID: -1}
}

func sortedRequiredChecks(checks map[string]requiredCheck) []requiredCheck {
	keys := sortedKeys(checks)
	result := make([]requiredCheck, 0, len(keys))
	for _, key := range keys {
		result = append(result, checks[key])
	}
	return result
}

func requiredCheckNames(checks map[string]requiredCheck) []string {
	result := make([]string, 0, len(checks))
	for _, check := range sortedRequiredChecks(checks) {
		result = append(result, check.Context)
	}
	sort.SliceStable(result, func(i, j int) bool {
		return strings.ToLower(result[i]) < strings.ToLower(result[j])
	})
	return result
}

func publicRequiredCheckIdentities(checks map[string]requiredCheck) []RequiredCheckIdentity {
	result := make([]RequiredCheckIdentity, 0, len(checks))
	for _, check := range sortedRequiredChecks(checks) {
		result = append(result, RequiredCheckIdentity{Context: check.Context, AppID: check.AppID})
	}
	return result
}

func uniqueRequiredCheckIdentities(checks []RequiredCheckIdentity) []RequiredCheckIdentity {
	byIdentity := map[string]RequiredCheckIdentity{}
	for _, check := range checks {
		check.Context = strings.TrimSpace(check.Context)
		if check.Context == "" || check.AppID <= 0 {
			continue
		}
		byIdentity[checkIdentityKey(check.Context, check.AppID)] = check
	}
	keys := sortedKeys(byIdentity)
	result := make([]RequiredCheckIdentity, 0, len(keys))
	for _, key := range keys {
		result = append(result, byIdentity[key])
	}
	return result
}

func (c *Client) reviewState(ctx context.Context, owner, repo string, number int) (int, bool, error) {
	type currentReview struct {
		state       string
		submittedAt time.Time
		id          int64
	}
	latest := map[int64]currentReview{}
	seen := map[int64]struct{}{}
	options := &gh.ListOptions{PerPage: 100}
	pages, total := 0, 0
	for {
		pages++
		if pages > maxPullReviewPages {
			return 0, false, fmt.Errorf("review inventory for pull request #%d exceeded the bounded %d-page GitHub response", number, maxPullReviewPages)
		}
		reviews, response, err := c.client.PullRequests.ListReviews(ctx, owner, repo, number, options)
		if err != nil {
			return 0, false, fmt.Errorf("list reviews for pull request #%d: %w", number, err)
		}
		if response == nil {
			return 0, false, fmt.Errorf("list reviews for pull request #%d returned no pagination metadata", number)
		}
		for _, review := range reviews {
			total++
			if total > maxPullRequestReviews {
				return 0, false, fmt.Errorf("review inventory for pull request #%d exceeds the bounded %d-review limit", number, maxPullRequestReviews)
			}
			reviewID, reviewerID := review.GetID(), review.GetUser().GetID()
			if reviewID <= 0 || reviewerID <= 0 {
				return 0, false, fmt.Errorf("review inventory for pull request #%d contains an incomplete immutable review identity", number)
			}
			if _, duplicate := seen[reviewID]; duplicate {
				return 0, false, fmt.Errorf("review inventory for pull request #%d contains duplicate review ID %d", number, reviewID)
			}
			seen[reviewID] = struct{}{}
			state := strings.ToLower(strings.TrimSpace(review.GetState()))
			if state != "approved" && state != "changes_requested" {
				continue
			}
			submittedAt := review.GetSubmittedAt().Time
			candidate := currentReview{state: state, submittedAt: submittedAt, id: reviewID}
			prior, exists := latest[reviewerID]
			if !exists || candidate.submittedAt.After(prior.submittedAt) || candidate.submittedAt.Equal(prior.submittedAt) && candidate.id > prior.id {
				latest[reviewerID] = candidate
			}
		}
		if response.NextPage == 0 {
			break
		}
		if response.NextPage <= options.Page {
			return 0, false, fmt.Errorf("review inventory for pull request #%d returned non-progressing pagination", number)
		}
		options.Page = response.NextPage
	}
	approvals, changes := 0, false
	for _, review := range latest {
		approvals += boolInt(review.state == "approved")
		changes = changes || review.state == "changes_requested"
	}
	return approvals, changes, nil
}

func classifyGatePaths(gate *PullRequestGate) {
	for _, file := range gate.ChangedFiles {
		lower := strings.ToLower(strings.ReplaceAll(file, "\\", "/"))
		gate.WorkflowChanged = gate.WorkflowChanged || strings.HasPrefix(lower, ".github/workflows/")
		setupSnapshot := strings.HasPrefix(lower, ".visual-hive/snapshots/")
		gate.BaselineChanged = gate.BaselineChanged || setupSnapshot || strings.Contains(lower, "baseline") || strings.Contains(lower, "__screenshots__")
		baselineImage := strings.HasSuffix(lower, ".png") &&
			(setupSnapshot || strings.HasPrefix(lower, "visual-hive.baselines/") || strings.Contains(lower, "/__screenshots__/"))
		if baselineImage {
			// A reviewed snapshot may legitimately describe an auth or deploy
			// screen. Its filename is not evidence that authentication or
			// deployment code changed; baseline authority remains separate.
			continue
		}
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
		len(rules.NonFastForward)+len(rules.CommitMessagePattern)+len(rules.CommitAuthorEmailPattern)+len(rules.CommitterEmailPattern)+
		len(rules.BranchNamePattern)+len(rules.TagNamePattern)+len(rules.FilePathRestriction)+len(rules.MaxFilePathLength)+
		len(rules.FileExtensionRestriction)+len(rules.MaxFileSize)+len(rules.Workflows)+len(rules.CodeScanning) > 0
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
	value = strings.NewReplacer("-", " ", "_", " ").Replace(value)
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

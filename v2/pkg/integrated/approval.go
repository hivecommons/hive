package integrated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

const MergeApprovalSchema = "hive.merge-approval.v1"

type MergeApproval struct {
	SchemaVersion string    `json:"schema_version"`
	Repository    string    `json:"repository"`
	RepositoryID  string    `json:"repository_id"`
	PRNumber      int       `json:"pr_number"`
	HeadSHA       string    `json:"head_sha"`
	BaseSHA       string    `json:"base_sha"`
	BaseBranch    string    `json:"base_branch"`
	DiffDigest    string    `json:"diff_digest"`
	Actor         string    `json:"actor"`
	Reason        string    `json:"reason"`
	ApprovedAt    time.Time `json:"approved_at"`
}

type ApproveMergeOptions struct {
	StateDir        string
	PRNumber        int
	ExpectedHeadSHA string
	ExpectedBaseSHA string
	ExpectedDiff    string
	Reason          string
	PlanOnly        bool
	GitHub          *hivegithub.Client
	HostedAuthority HostedOperatorAuthority
}

type ApproveMergeResult struct {
	SchemaVersion string                     `json:"schema_version"`
	Approval      MergeApproval              `json:"approval"`
	Gate          hivegithub.PullRequestGate `json:"gate"`
	DiffDigest    string                     `json:"diff_digest"`
	Planned       bool                       `json:"planned"`
	ApplyCommand  string                     `json:"apply_command,omitempty"`
	NextCommand   string                     `json:"next_command"`
}

type RevokeMergeApprovalOptions struct {
	StateDir        string
	Reason          string
	GitHub          *hivegithub.Client
	HostedAuthority HostedOperatorAuthority
}

type RevokeMergeApprovalResult struct {
	SchemaVersion string         `json:"schema_version"`
	Repository    string         `json:"repository"`
	Actor         string         `json:"actor"`
	Reason        string         `json:"reason"`
	Revoked       bool           `json:"revoked"`
	Approval      *MergeApproval `json:"approval,omitempty"`
}

func (s *Store) SaveMergeApproval(approval MergeApproval) error {
	approval.SchemaVersion = MergeApprovalSchema
	if err := validateMergeApprovalRecord(approval); err != nil {
		return err
	}
	data, err := json.MarshalIndent(approval, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, "merge-approval.json")
	return writeDurableStateFile(path, append(data, '\n'))
}

func (s *Store) LoadMergeApproval() (MergeApproval, bool, error) {
	path := filepath.Join(s.dir, "merge-approval.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return MergeApproval{}, false, nil
	}
	if err != nil {
		return MergeApproval{}, false, err
	}
	var approval MergeApproval
	if err := json.Unmarshal(data, &approval); err != nil {
		return MergeApproval{}, false, fmt.Errorf("decode merge approval: %w", err)
	}
	if err := validateMergeApprovalRecord(approval); err != nil {
		return MergeApproval{}, false, err
	}
	return approval, true, nil
}

func (s *Store) DeleteMergeApproval() error {
	return durableRemoveStateFile(filepath.Join(s.dir, "merge-approval.json"))
}

func validateMergeApprovalRecord(approval MergeApproval) error {
	if approval.SchemaVersion != MergeApprovalSchema || strings.TrimSpace(approval.Repository) == "" || strings.TrimSpace(approval.RepositoryID) == "" || approval.PRNumber <= 0 ||
		!immutableCommit.MatchString(strings.ToLower(approval.HeadSHA)) || !immutableCommit.MatchString(strings.ToLower(approval.BaseSHA)) || strings.TrimSpace(approval.BaseBranch) == "" ||
		len(approval.DiffDigest) != sha256.Size*2 || strings.TrimSpace(approval.Actor) == "" || strings.TrimSpace(approval.Reason) == "" || approval.ApprovedAt.IsZero() {
		return fmt.Errorf("merge approval is incomplete or invalid")
	}
	if _, err := hex.DecodeString(approval.DiffDigest); err != nil {
		return fmt.Errorf("merge approval diff digest is invalid")
	}
	return nil
}

func ValidateMergeApproval(approval MergeApproval, repository, repositoryID, currentDiffDigest string, gate hivegithub.PullRequestGate) error {
	if err := validateMergeApprovalRecord(approval); err != nil {
		return err
	}
	if !strings.EqualFold(approval.Repository, repository) || approval.RepositoryID != strings.TrimSpace(repositoryID) || approval.PRNumber != gate.Number ||
		!strings.EqualFold(approval.HeadSHA, gate.HeadSHA) || !strings.EqualFold(approval.BaseSHA, gate.BaseSHA) ||
		!strings.EqualFold(approval.BaseBranch, gate.BaseBranch) ||
		!strings.EqualFold(approval.DiffDigest, strings.TrimSpace(currentDiffDigest)) {
		return fmt.Errorf("merge approval no longer matches the exact pull request base, head, or diff")
	}
	return nil
}

func ApproveMerge(ctx context.Context, options ApproveMergeOptions) (ApproveMergeResult, error) {
	result := ApproveMergeResult{SchemaVersion: "hive.approve-merge.v1"}
	if options.GitHub == nil || strings.TrimSpace(options.StateDir) == "" {
		return result, fmt.Errorf("GitHub client and persistent state directory are required")
	}
	options.Reason = strings.TrimSpace(options.Reason)
	options.ExpectedHeadSHA = strings.ToLower(strings.TrimSpace(options.ExpectedHeadSHA))
	options.ExpectedBaseSHA = strings.ToLower(strings.TrimSpace(options.ExpectedBaseSHA))
	options.ExpectedDiff = strings.ToLower(strings.TrimSpace(options.ExpectedDiff))
	if options.PRNumber <= 0 || !immutableCommit.MatchString(options.ExpectedHeadSHA) {
		return result, fmt.Errorf("pull request number and exact 40-character head SHA are required")
	}
	if !options.PlanOnly && (!immutableCommit.MatchString(options.ExpectedBaseSHA) || len(options.ExpectedDiff) != sha256.Size*2 || options.Reason == "") {
		return result, fmt.Errorf("apply requires the reviewed exact base SHA, diff digest, and reason returned by --plan")
	}
	if len(options.Reason) > 2048 {
		return result, fmt.Errorf("merge approval reason exceeds the bounded length")
	}
	release, err := acquireProductionRunLease(options.StateDir, 2*time.Minute)
	if err != nil {
		return result, fmt.Errorf("serialize merge approval with production runs: %w", err)
	}
	defer release()
	store, err := NewStore(filepath.Join(options.StateDir, "integrated"))
	if err != nil {
		return result, err
	}
	config, err := store.Load()
	if err != nil {
		return result, err
	}
	if err := rejectPendingAuthorizerTransfer(store, options.StateDir, "merge approval"); err != nil {
		return result, err
	}
	fail := func(cause error) (ApproveMergeResult, error) {
		if auditErr := store.AuditStrict(AuditEntry{Action: "approve_merge", Allowed: false, Repository: config.Repository, Detail: cause.Error()}); auditErr != nil {
			return result, fmt.Errorf("%v; persist denied approval audit: %w", cause, auditErr)
		}
		return result, cause
	}
	if config.Paused || config.Automation != AutomationAutoMerge || config.ACMMLevel != 6 {
		return fail(fmt.Errorf("merge approval requires active auto-merge automation at ACMM L6"))
	}
	liveRepositoryID, err := verifyLiveRepositoryIdentity(ctx, options.GitHub, config)
	if err != nil {
		return fail(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(filepath.Join(options.StateDir, "visual-hive"))
	if err != nil {
		return fail(err)
	}
	finding, ok, findingErr := findingForPullRequest(lifecycle.Snapshot(), options.PRNumber)
	if findingErr != nil {
		return fail(findingErr)
	}
	if !ok || finding.Status != visualhive.StatusReady || finding.ManualReviewKind != "merge_policy" || !finding.HumanReviewRequired || finding.ObservationHumanReviewRequired {
		return fail(fmt.Errorf("pull request #%d is not a ready Hive repair awaiting merge-policy review", options.PRNumber))
	}
	if !strings.EqualFold(finding.RepairCommitSHA, options.ExpectedHeadSHA) {
		return fail(fmt.Errorf("requested head does not match Hive's exact durable repair head"))
	}
	gate, err := options.GitHub.InspectPullRequestGate(ctx, config.Repository, options.PRNumber)
	if err != nil {
		return fail(err)
	}
	result.Gate = gate
	if !gate.Open || gate.Merged || !strings.EqualFold(gate.HeadSHA, options.ExpectedHeadSHA) || !strings.EqualFold(gate.BaseBranch, config.DefaultBranch) {
		return fail(fmt.Errorf("pull request is not open at the exact approved head"))
	}
	policy := mergePolicyForFinding(integratedPolicy(config), finding)
	request := mergeActionRequest(config, finding, gate)
	decision := policy.Authorize(request)
	approvedDecision, eligible := authorizePathApprovedMerge(policy, request, decision)
	if !eligible || !approvedDecision.Allowed {
		return fail(fmt.Errorf("merge approval cannot override current gates: %s", strings.Join(decision.Reasons, "; ")))
	}
	digest, err := options.GitHub.PullRequestDiffDigest(ctx, config.Repository, gate.Number)
	if err != nil {
		return fail(err)
	}
	result.DiffDigest = digest
	if options.PlanOnly {
		result.Planned = true
		result.ApplyCommand = fmt.Sprintf("hive approve-merge --state-dir %q --pr %d --head %s --base %s --diff-digest %s --reason %q --json", options.StateDir, gate.Number, strings.ToLower(gate.HeadSHA), strings.ToLower(gate.BaseSHA), digest, "REVIEWED_REASON")
		return result, nil
	}
	if !strings.EqualFold(options.ExpectedBaseSHA, gate.BaseSHA) || !strings.EqualFold(options.ExpectedDiff, digest) {
		return fail(fmt.Errorf("reviewed base SHA or diff digest no longer matches the live pull request; rerun approve-merge --plan"))
	}
	operation := "approve-merge-apply"
	if options.PlanOnly {
		operation = "approve-merge-plan"
	}
	operator, err := resolveOperatorIdentity(ctx, options.GitHub, options.HostedAuthority, config.Repository, operation)
	if err != nil {
		return fail(err)
	}
	actor := operator.Login
	approval := MergeApproval{
		SchemaVersion: MergeApprovalSchema, Repository: config.Repository, RepositoryID: liveRepositoryID, PRNumber: gate.Number,
		HeadSHA: strings.ToLower(gate.HeadSHA), BaseSHA: strings.ToLower(gate.BaseSHA), BaseBranch: gate.BaseBranch, DiffDigest: digest,
		Actor: actor, Reason: options.Reason, ApprovedAt: time.Now().UTC(),
	}
	if existing, exists, loadErr := store.LoadMergeApproval(); loadErr != nil {
		return fail(loadErr)
	} else if exists && existing != approval {
		return fail(fmt.Errorf("another exact merge approval is already active for pull request #%d at %s; run Hive or invalidate that approval before approving a different head", existing.PRNumber, existing.HeadSHA))
	}
	detailData, _ := json.Marshal(approval)
	if err := store.AuditStrict(AuditEntry{Action: "approve_merge", Allowed: true, Repository: config.Repository, Detail: string(detailData)}); err != nil {
		return result, fmt.Errorf("persist merge approval audit: %w", err)
	}
	if err := store.SaveMergeApproval(approval); err != nil {
		return result, fmt.Errorf("activate audited merge approval: %w", err)
	}
	lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "approve_merge", true, fmt.Sprintf("actor=%s pr=%d head=%s diff=%s reason=%s", approval.Actor, approval.PRNumber, approval.HeadSHA, approval.DiffDigest, approval.Reason))
	result.Approval = approval
	result.NextCommand = fmt.Sprintf("hive run --state-dir %q --json", options.StateDir)
	return result, nil
}

func RevokeMergeApproval(ctx context.Context, options RevokeMergeApprovalOptions) (RevokeMergeApprovalResult, error) {
	result := RevokeMergeApprovalResult{SchemaVersion: "hive.revoke-merge-approval.v1"}
	options.Reason = strings.TrimSpace(options.Reason)
	if options.GitHub == nil || strings.TrimSpace(options.StateDir) == "" || options.Reason == "" || len(options.Reason) > 2048 {
		return result, fmt.Errorf("GitHub client, persistent state directory, and a bounded reason are required")
	}
	release, err := waitForProductionRunLease(ctx, options.StateDir, 5*time.Minute)
	if err != nil {
		return result, fmt.Errorf("serialize approval revocation with production runs: %w", err)
	}
	defer release()
	store, err := NewStore(filepath.Join(options.StateDir, "integrated"))
	if err != nil {
		return result, err
	}
	config, err := store.Load()
	if err != nil {
		return result, err
	}
	result.Repository = config.Repository
	if _, err := verifyLiveRepositoryIdentity(ctx, options.GitHub, config); err != nil {
		return result, err
	}
	operator, err := resolveOperatorIdentity(ctx, options.GitHub, options.HostedAuthority, config.Repository, "revoke-merge-approval")
	if err != nil {
		return result, err
	}
	actor := operator.Login
	result.Actor, result.Reason = actor, options.Reason
	approval, exists, err := store.LoadMergeApproval()
	if err != nil {
		return result, err
	}
	if !exists {
		return result, nil
	}
	result.Approval = &approval
	detail := fmt.Sprintf("actor=%s pr=%d base=%s head=%s diff=%s reason=%s", actor, approval.PRNumber, approval.BaseSHA, approval.HeadSHA, approval.DiffDigest, options.Reason)
	if err := store.AuditStrict(AuditEntry{Action: "revoke_merge_approval_authorize", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
		return result, err
	}
	if err := store.DeleteMergeApproval(); err != nil {
		return result, err
	}
	if err := store.AuditStrict(AuditEntry{Action: "revoke_merge_approval", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
		return result, err
	}
	if lifecycle, lifecycleErr := visualhive.NewLifecycleStore(filepath.Join(options.StateDir, "visual-hive")); lifecycleErr == nil {
		if finding, ok, _ := findingForPullRequest(lifecycle.Snapshot(), approval.PRNumber); ok {
			lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "revoke_merge_approval", true, detail)
		}
	}
	result.Revoked = true
	return result, nil
}

func reconcileStaleMergeApproval(ctx context.Context, stateDir string, config Config, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client) error {
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return err
	}
	approval, exists, err := store.LoadMergeApproval()
	if err != nil || !exists {
		return err
	}
	finding, found, mappingErr := findingForPullRequest(lifecycle.Snapshot(), approval.PRNumber)
	if mappingErr != nil {
		return mappingErr
	}
	reason := ""
	if !found || finding.Status != visualhive.StatusReady || !finding.HumanReviewRequired || finding.ManualReviewKind != "merge_policy" || finding.ObservationHumanReviewRequired || !strings.EqualFold(finding.RepairCommitSHA, approval.HeadSHA) {
		reason = "approval no longer maps to exactly one ready merge-policy-held finding"
	} else {
		gate, gateErr := client.InspectPullRequestGate(ctx, config.Repository, approval.PRNumber)
		if gateErr != nil {
			return gateErr
		}
		if !gate.Open || gate.Merged {
			reason = "approved pull request is no longer open"
		} else {
			digest, digestErr := client.PullRequestDiffDigest(ctx, config.Repository, approval.PRNumber)
			if digestErr != nil {
				return digestErr
			}
			if validationErr := ValidateMergeApproval(approval, config.Repository, config.RepositoryID, digest, gate); validationErr != nil {
				reason = validationErr.Error()
			}
		}
	}
	if reason == "" {
		return nil
	}
	detail := fmt.Sprintf("pr=%d base=%s head=%s diff=%s reason=%s", approval.PRNumber, approval.BaseSHA, approval.HeadSHA, approval.DiffDigest, reason)
	if err := store.AuditStrict(AuditEntry{Action: "invalidate_merge_approval", Allowed: false, Repository: config.Repository, Detail: detail}); err != nil {
		return err
	}
	if found {
		lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "invalidate_merge_approval", false, detail)
	}
	return store.DeleteMergeApproval()
}

func verifyLiveRepositoryIdentity(ctx context.Context, client *hivegithub.Client, config Config) (string, error) {
	owner, repository, ok := strings.Cut(config.Repository, "/")
	if !ok || strings.TrimSpace(owner) == "" || strings.TrimSpace(repository) == "" || strings.TrimSpace(config.RepositoryID) == "" {
		return "", fmt.Errorf("configured repository identity is incomplete; run hive doctor, then reinstall with an immutable repository ID")
	}
	metadata, _, err := client.GetRepo(ctx, owner, repository)
	if err != nil {
		return "", fmt.Errorf("verify live repository identity: %w", err)
	}
	liveID := fmt.Sprintf("%d", metadata.GetID())
	if metadata.GetID() <= 0 || liveID != config.RepositoryID || !strings.EqualFold(metadata.GetFullName(), config.Repository) {
		return "", fmt.Errorf("live repository identity does not match installed repository %s (expected ID %s, got %s ID %s); refusing mutation", config.Repository, config.RepositoryID, metadata.GetFullName(), liveID)
	}
	return liveID, nil
}

func findingForPullRequest(state visualhive.LifecycleState, number int) (visualhive.FindingLifecycle, bool, error) {
	matches := []visualhive.FindingLifecycle{}
	for _, finding := range state.Findings {
		if finding != nil && finding.PRNumber == number {
			matches = append(matches, *finding)
		}
	}
	if len(matches) > 1 {
		return visualhive.FindingLifecycle{}, false, fmt.Errorf("pull request #%d maps to multiple Hive findings", number)
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	return visualhive.FindingLifecycle{}, false, nil
}

func integratedPolicy(config Config) automation.Policy {
	return automation.Policy{
		ACMMLevel: config.ACMMLevel, Mode: automationMode(config.Automation), Paused: config.Paused,
		AllowedRepositories: []string{config.Repository}, MaxRepairAttempts: repairAttemptLimit(config),
		AllowedAutoMergePaths: config.AllowedAutoMergePaths, AllowedAutoMergeRisk: config.AllowedAutoMergeRisk,
	}
}

// PolicyForConfig returns the existing durable automation authority for reuse
// by the normal Visual Hive intake seam.
func PolicyForConfig(config Config) automation.Policy {
	return integratedPolicy(config)
}

func mergePolicyForFinding(policy automation.Policy, finding visualhive.FindingLifecycle) automation.Policy {
	if strings.EqualFold(strings.TrimSpace(finding.IssueKind), visualhive.RepositoryTestFailureKind) {
		// Repository-test repairs can touch test files that are globally safe for
		// ordinary test-adequacy fixes. They must nevertheless be reviewed because
		// weakening a test could make the original command green. A nil/empty path
		// list intentionally falls back to Hive's ordinary safe-test defaults, so
		// use one valid pattern that cannot match a repository path. This produces
		// only the normal, exactly approvable path-policy hold without expanding or
		// mutating the installed auto-merge policy.
		policy.AllowedAutoMergePaths = []string{"__hive_repository_test_requires_exact_approval__"}
	}
	return policy
}

func mergeActionRequest(config Config, finding visualhive.FindingLifecycle, gate hivegithub.PullRequestGate) automation.ActionRequest {
	return automation.ActionRequest{
		Action: automation.ActionMergePR, Agent: repairActor(finding.OwningAgentHint), Repository: config.Repository,
		Risk: mergeRisk(gate.ChangedFiles), ChangedFiles: gate.ChangedFiles, RepairAttempts: finding.RepairAttempts,
		ExpectedHeadSHA: finding.RepairCommitSHA, TestedHeadSHA: gate.HeadSHA,
		ExpectedBaseBranch: config.DefaultBranch, TestedBaseBranch: gate.BaseBranch,
		MergeableKnown: gate.MergeableKnown, Mergeable: gate.Mergeable, VisualHiveVerdictGreen: gate.VisualHiveVerdictGreen,
		RequiredCheckNames: gate.RequiredCheckNames, RequiredCheckStates: gate.RequiredCheckStates, BranchProtectionEnabled: gate.BranchProtectionEnabled,
		Hold: gate.Hold, HumanReviewRequired: gate.HumanReviewRequired, BaselineChanged: gate.BaselineChanged,
		WorkflowChanged: gate.WorkflowChanged, SecuritySensitive: gate.SecuritySensitive, DeploymentChanged: gate.DeploymentChanged,
	}
}

func authorizePathApprovedMerge(policy automation.Policy, request automation.ActionRequest, denied automation.Decision) (automation.Decision, bool) {
	if denied.Allowed || len(denied.Reasons) == 0 {
		return denied, false
	}
	for _, reason := range denied.Reasons {
		if !strings.HasPrefix(reason, "changed file ") || !strings.HasSuffix(reason, " is outside the auto-merge allowlist") {
			return denied, false
		}
	}
	override := policy
	override.AllowedAutoMergePaths = append(append([]string(nil), policy.AllowedAutoMergePaths...), request.ChangedFiles...)
	return override.Authorize(request), true
}

package integrated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/internal/gittransport"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

const (
	setupBaselineApprovalResultSchema = "hive.approve-setup-baseline.v1"
	setupBaselinePRWorkflowPath       = ".github/workflows/visual-hive-pr.yml"
)

type ApproveSetupBaselineOptions struct {
	StateDir                string
	PlanOnly                bool
	ExpectedRepositoryID    string
	ExpectedCaptureRunID    int64
	ExpectedArtifactID      int64
	ExpectedPRNumber        int
	ExpectedHeadSHA         string
	ExpectedBaseSHA         string
	ExpectedDiffDigest      string
	ExpectedCandidateDigest string
	ExpectedActorID         int64
	ExpectedPlanDigest      string
	Reason                  string
	GitHub                  *hivegithub.Client
	GitTransportToken       string
	HostedAuthority         HostedOperatorAuthority
}

type ApproveSetupBaselineResult struct {
	SchemaVersion   string                                `json:"schema_version"`
	Repository      string                                `json:"repository"`
	RepositoryID    string                                `json:"repository_id"`
	Phase           string                                `json:"phase"`
	Planned         bool                                  `json:"planned"`
	Approved        bool                                  `json:"approved"`
	Merged          bool                                  `json:"merged"`
	CaptureRunID    int64                                 `json:"capture_run_id"`
	ArtifactID      int64                                 `json:"artifact_id"`
	PRNumber        int                                   `json:"pr_number"`
	PRURL           string                                `json:"pr_url"`
	HeadSHA         string                                `json:"head_sha"`
	BaseSHA         string                                `json:"base_sha"`
	DiffDigest      string                                `json:"diff_digest"`
	CandidateDigest string                                `json:"candidate_digest"`
	CaptureDigest   string                                `json:"capture_candidate_digest"`
	Candidates      []SetupBaselineCandidate              `json:"candidates"`
	ActorID         int64                                 `json:"actor_id"`
	ActorLogin      string                                `json:"actor_login"`
	PlanDigest      string                                `json:"plan_digest"`
	Gate            hivegithub.PullRequestGate            `json:"gate"`
	Snapshot        hivegithub.ManagedPullRequestSnapshot `json:"snapshot"`
	MergeSHA        string                                `json:"merge_sha,omitempty"`
	ApplyCommand    string                                `json:"apply_command,omitempty"`
	NextCommand     string                                `json:"next_command,omitempty"`
}

type setupBaselineApprovalBinding struct {
	SchemaVersion   string   `json:"schema_version"`
	Repository      string   `json:"repository"`
	RepositoryID    string   `json:"repository_id"`
	CaptureRunID    int64    `json:"capture_run_id"`
	ArtifactID      int64    `json:"artifact_id"`
	PRNumber        int      `json:"pr_number"`
	PRURL           string   `json:"pr_url"`
	Branch          string   `json:"branch"`
	HeadSHA         string   `json:"head_sha"`
	BaseBranch      string   `json:"base_branch"`
	BaseSHA         string   `json:"base_sha"`
	DiffDigest      string   `json:"diff_digest"`
	CandidateDigest string   `json:"candidate_digest"`
	CaptureDigest   string   `json:"capture_candidate_digest"`
	CandidatePaths  []string `json:"candidate_paths"`
	ActorID         int64    `json:"actor_id"`
}

func ApproveSetupBaseline(ctx context.Context, options ApproveSetupBaselineOptions) (ApproveSetupBaselineResult, error) {
	ctx = gittransport.WithControllerToken(ctx, options.GitTransportToken)
	result := ApproveSetupBaselineResult{SchemaVersion: setupBaselineApprovalResultSchema}
	if options.GitHub == nil || strings.TrimSpace(options.StateDir) == "" {
		return result, fmt.Errorf("GitHub client and persistent state directory are required")
	}
	if !options.PlanOnly && (strings.TrimSpace(options.Reason) == "" || len(options.Reason) > 2048) {
		return result, fmt.Errorf("apply requires a bounded accountable reason")
	}
	release, err := waitForProductionRunLease(ctx, options.StateDir, 5*time.Minute)
	if err != nil {
		return result, fmt.Errorf("serialize setup baseline approval with production runs: %w", err)
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
	intent, exists, err := store.LoadSetupBaselineIntent()
	if err != nil || !exists {
		if err == nil {
			err = fmt.Errorf("no setup baseline approval is pending")
		}
		return result, err
	}
	intent, err = flushSetupBaselineAudit(store, intent)
	if err != nil {
		return result, fmt.Errorf("finish pending setup baseline audit before approval: %w", err)
	}
	result.Repository, result.RepositoryID, result.Phase = config.Repository, config.RepositoryID, intent.Phase
	if _, err := verifyLiveRepositoryIdentity(ctx, options.GitHub, config); err != nil {
		return result, err
	}
	if intent.Repository != config.Repository || intent.RepositoryID != config.RepositoryID || intent.AuthorizerID != config.SetupAuthorizationActorID ||
		(intent.Phase != SetupBaselinePROpen && intent.Phase != SetupBaselineApproved) {
		return result, fmt.Errorf("setup baseline approval state is not bound to the installed repository/authorizer or review phase")
	}
	operation := "approve-baseline-apply"
	if options.PlanOnly {
		operation = "approve-baseline-plan"
	}
	actor, err := resolveOperatorIdentity(ctx, options.GitHub, options.HostedAuthority, config.Repository, operation)
	if err != nil {
		return result, err
	}
	if actor.ID != intent.AuthorizerID {
		return result, fmt.Errorf("setup baseline approval is bound to setup authorizer ID %d, but authenticated user is %d", intent.AuthorizerID, actor.ID)
	}
	currentHead, err := currentDefaultBranchHead(ctx, options.GitHub, config)
	if err != nil {
		return result, err
	}
	if !strings.EqualFold(currentHead, intent.CaptureHeadSHA) {
		return result, fmt.Errorf("default branch moved from setup baseline capture head %s to %s; run Hive to recapture before approval", intent.CaptureHeadSHA, currentHead)
	}
	snapshot, gate, err := inspectExactSetupBaselineApproval(ctx, options.GitHub, config, intent, true)
	if err != nil {
		return result, err
	}
	if intent.Phase == SetupBaselinePROpen && !gate.Hold {
		return result, fmt.Errorf("setup baseline review hold was removed outside its durable specialized approval path")
	}
	binding := setupBaselineApprovalBindingFor(config, intent, snapshot, actor.ID)
	planDigest, err := setupBaselineApprovalDigest(binding)
	if err != nil {
		return result, err
	}
	result.CaptureRunID, result.ArtifactID, result.PRNumber, result.PRURL = intent.CaptureRunID, intent.ArtifactID, intent.PRNumber, intent.PRURL
	result.HeadSHA, result.BaseSHA, result.DiffDigest = intent.CommitSHA, snapshot.BaseSHA, snapshot.DiffDigest
	result.CandidateDigest, result.CaptureDigest, result.Candidates = intent.CandidateDigest, intent.CaptureCandidateDigest, append([]SetupBaselineCandidate(nil), intent.Candidates...)
	result.ActorID, result.ActorLogin, result.PlanDigest, result.Gate, result.Snapshot = actor.ID, actor.Login, planDigest, gate, snapshot
	if options.PlanOnly {
		result.Planned = true
		result.ApplyCommand = setupBaselineApprovalApplyCommand(options.StateDir, binding, planDigest)
		return result, nil
	}
	if err := validateSetupBaselineApprovalApply(options, binding, planDigest); err != nil {
		return result, err
	}
	if intent.Phase == SetupBaselinePROpen {
		intent.Phase, intent.ApprovalPlanDigest, intent.ApprovalActorID, intent.ApprovalReason = SetupBaselineApproved, planDigest, actor.ID, strings.TrimSpace(options.Reason)
		if err := store.AuditStrict(AuditEntry{Action: "setup_baseline_approved", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("actor_id=%d run=%d artifact=%d pr=%d head=%s base=%s diff=%s candidates=%s plan=%s reason=%s", actor.ID, intent.CaptureRunID, intent.ArtifactID, intent.PRNumber, intent.CommitSHA, snapshot.BaseSHA, snapshot.DiffDigest, intent.CandidateDigest, planDigest, intent.ApprovalReason)}); err != nil {
			return result, err
		}
		if err := store.SaveSetupBaselineIntent(intent); err != nil {
			return result, err
		}
	} else if intent.ApprovalActorID != actor.ID || !strings.EqualFold(intent.ApprovalPlanDigest, planDigest) || intent.ApprovalReason != strings.TrimSpace(options.Reason) {
		return result, fmt.Errorf("durable setup baseline approval differs from this exact apply request")
	}
	intent, merged, err := mergeApprovedSetupBaseline(ctx, store, config, intent, options.GitHub)
	result.Approved, result.Merged, result.Phase, result.MergeSHA = true, merged, intent.Phase, intent.MergeSHA
	result.NextCommand = fmt.Sprintf("hive run --state-dir %q --json", options.StateDir)
	return result, err
}

func setupBaselineApprovalBindingFor(config Config, intent SetupBaselineIntent, snapshot hivegithub.ManagedPullRequestSnapshot, actorID int64) setupBaselineApprovalBinding {
	paths := setupBaselineCandidatePaths(intent.Candidates)
	return setupBaselineApprovalBinding{
		SchemaVersion: setupBaselineApprovalResultSchema, Repository: config.Repository, RepositoryID: config.RepositoryID,
		CaptureRunID: intent.CaptureRunID, ArtifactID: intent.ArtifactID, PRNumber: intent.PRNumber, PRURL: intent.PRURL,
		Branch: intent.Branch, HeadSHA: strings.ToLower(intent.CommitSHA), BaseBranch: intent.DefaultBranch, BaseSHA: strings.ToLower(snapshot.BaseSHA),
		DiffDigest: strings.ToLower(snapshot.DiffDigest), CandidateDigest: strings.ToLower(intent.CandidateDigest), CaptureDigest: strings.ToLower(intent.CaptureCandidateDigest), CandidatePaths: paths, ActorID: actorID,
	}
}

func setupBaselineApprovalDigest(binding setupBaselineApprovalBinding) (string, error) {
	binding.CandidatePaths = append([]string(nil), binding.CandidatePaths...)
	sort.Strings(binding.CandidatePaths)
	data, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func setupBaselineApprovalApplyCommand(stateDir string, binding setupBaselineApprovalBinding, planDigest string) string {
	return fmt.Sprintf("hive approve-baseline --state-dir %q --repo-id %s --run-id %d --artifact-id %d --pr %d --head %s --base %s --diff-digest %s --candidate-digest %s --actor-id %d --plan-digest %s --reason %q --json",
		strings.TrimSpace(stateDir), binding.RepositoryID, binding.CaptureRunID, binding.ArtifactID, binding.PRNumber, binding.HeadSHA, binding.BaseSHA,
		binding.DiffDigest, binding.CandidateDigest, binding.ActorID, planDigest, "REVIEWED_REASON")
}

func validateSetupBaselineApprovalApply(options ApproveSetupBaselineOptions, binding setupBaselineApprovalBinding, planDigest string) error {
	if options.ExpectedRepositoryID != binding.RepositoryID || options.ExpectedCaptureRunID != binding.CaptureRunID || options.ExpectedArtifactID != binding.ArtifactID ||
		options.ExpectedPRNumber != binding.PRNumber || !strings.EqualFold(options.ExpectedHeadSHA, binding.HeadSHA) || !strings.EqualFold(options.ExpectedBaseSHA, binding.BaseSHA) ||
		!strings.EqualFold(options.ExpectedDiffDigest, binding.DiffDigest) || !strings.EqualFold(options.ExpectedCandidateDigest, binding.CandidateDigest) ||
		options.ExpectedActorID != binding.ActorID || !strings.EqualFold(options.ExpectedPlanDigest, planDigest) {
		return fmt.Errorf("setup baseline apply bindings changed or are incomplete; rerun approve-baseline --plan")
	}
	return nil
}

func inspectExactSetupBaselineApproval(ctx context.Context, client *hivegithub.Client, config Config, intent SetupBaselineIntent, allowHold bool) (hivegithub.ManagedPullRequestSnapshot, hivegithub.PullRequestGate, error) {
	snapshot, err := client.InspectManagedPullRequestExact(ctx, config.Repository, config.RepositoryID, intent.PRNumber, intent.Marker, intent.Branch, intent.CommitSHA, intent.DefaultBranch)
	if err != nil {
		return snapshot, hivegithub.PullRequestGate{}, err
	}
	paths := setupBaselineCandidatePaths(intent.Candidates)
	if snapshot.Number != intent.PRNumber || snapshot.URL != intent.PRURL || snapshot.State != "open" || snapshot.Merged ||
		!strings.EqualFold(snapshot.HeadSHA, intent.CommitSHA) || !strings.EqualFold(snapshot.BaseSHA, intent.BaseSHA) || snapshot.BaseBranch != intent.DefaultBranch ||
		!strings.EqualFold(snapshot.DiffDigest, intent.DiffDigest) || !equalPaths(snapshot.ChangedFiles, paths) {
		return snapshot, hivegithub.PullRequestGate{}, fmt.Errorf("setup baseline pull request no longer matches its exact durable identity, refs, paths, or raw diff")
	}
	gate, err := client.InspectPullRequestGate(ctx, config.Repository, intent.PRNumber)
	if err != nil {
		return snapshot, gate, err
	}
	if gate.Number != intent.PRNumber || gate.URL != intent.PRURL || !gate.Open || gate.Merged || gate.Draft ||
		!strings.EqualFold(gate.HeadSHA, intent.CommitSHA) || !strings.EqualFold(gate.BaseSHA, intent.BaseSHA) || gate.BaseBranch != intent.DefaultBranch ||
		!equalPaths(gate.ChangedFiles, paths) || !gate.BaselineChanged || gate.WorkflowChanged || gate.SecuritySensitive || gate.DeploymentChanged || (!allowHold && gate.Hold) {
		return snapshot, gate, fmt.Errorf("setup baseline pull request violates the specialized candidate-only review boundary")
	}
	return snapshot, gate, nil
}

func setupBaselineMergeGateReady(gate hivegithub.PullRequestGate) error {
	return setupBaselineMergeGateReadyWithHold(gate, false)
}

// setupBaselineMergeGateReadyWithHold is the sole one-time bootstrap
// exception to Hive's normal protection prerequisite. A brand-new repository
// cannot require the Visual Hive PR check until that workflow and its reviewed
// Linux baselines are on the default branch. The recorded setup authorizer may
// therefore approve and let Hive merge this candidate-only PR when GitHub
// proves that the branch has no classic protection and no applicable ruleset.
// Any configured policy, even a partial one, must already be strict,
// administrator-enforced, and have every exact required check green. Full
// production verification and protection activation still happen after this
// merge and before any lifecycle write.
func setupBaselineMergeGateReadyWithHold(gate hivegithub.PullRequestGate, allowHiveHold bool) error {
	if !gate.Open || gate.Merged || gate.Draft || (!allowHiveHold && gate.Hold) || gate.HumanReviewRequired || !gate.MergeableKnown || !gate.Mergeable {
		return fmt.Errorf("setup baseline pull request is not open, current-base mergeable, review-resolved, and free of an unapproved hold")
	}
	if !gate.VisualHiveVerdictGreen || !gate.VisualHiveProvenanceVerified || gate.VisualHiveCheckState != "success" ||
		gate.VisualHiveCheckRunID <= 0 || gate.VisualHiveWorkflowRunID <= 0 || gate.VisualHiveWorkflowPath != setupBaselinePRWorkflowPath ||
		gate.VisualHiveWorkflowEvent != "pull_request" {
		return fmt.Errorf("setup baseline pull request lacks the exact green GitHub-Actions-App Visual Hive PR workflow check at its current head")
	}
	if !gate.BranchProtectionConfigured {
		// These fields and required-check arrays are derived from the same live
		// classic-protection/ruleset read. Refuse an internally contradictory
		// snapshot instead of treating it as the clean bootstrap case.
		if gate.BranchProtectionStrict || gate.BranchProtectionAdminEnforced || gate.BranchProtectionEnabled || gate.VisualHiveRequired ||
			len(gate.RequiredCheckNames) != 0 || len(gate.RequiredCheckStates) != 0 {
			return fmt.Errorf("setup baseline branch protection snapshot is inconsistent; clean bootstrap requires truly unconfigured protection and rulesets")
		}
		return nil
	}
	if !gate.BranchProtectionStrict || !gate.BranchProtectionAdminEnforced || !gate.BranchProtectionEnabled {
		return fmt.Errorf("configured branch protection or rulesets are too weak for setup baseline merge; require strict up-to-date branches and no administrator/writer bypass")
	}
	if len(gate.RequiredCheckNames) == 0 || len(gate.RequiredCheckStates) != len(gate.RequiredCheckNames) {
		return fmt.Errorf("configured branch protection has no complete exact required-check inventory")
	}
	for _, state := range gate.RequiredCheckStates {
		if state != "success" {
			return fmt.Errorf("setup baseline pull request required checks are not all successful")
		}
	}
	return nil
}

func mergeApprovedSetupBaseline(ctx context.Context, store *Store, config Config, intent SetupBaselineIntent, client *hivegithub.Client) (SetupBaselineIntent, bool, error) {
	if intent.Phase != SetupBaselineApproved || intent.ApprovalActorID != intent.AuthorizerID || len(intent.ApprovalPlanDigest) != sha256.Size*2 || strings.TrimSpace(intent.ApprovalReason) == "" {
		return intent, false, fmt.Errorf("setup baseline merge requires exact durable specialized approval")
	}
	snapshot, gate, err := inspectExactSetupBaselineApproval(ctx, client, config, intent, true)
	if err != nil {
		// A merge response may have been lost after the durable attempted marker.
		// Only the same exact already-merged managed PR can resolve that ambiguity.
		if !intent.MergeAttemptedAt.IsZero() {
			recovered, inspectErr := client.InspectManagedPullRequestExact(ctx, config.Repository, config.RepositoryID, intent.PRNumber, intent.Marker, intent.Branch, intent.CommitSHA, intent.DefaultBranch)
			if inspectErr == nil && recovered.Merged && immutableCommit.MatchString(strings.ToLower(recovered.MergeSHA)) &&
				strings.EqualFold(recovered.HeadSHA, intent.CommitSHA) && strings.EqualFold(recovered.BaseSHA, intent.BaseSHA) && strings.EqualFold(recovered.DiffDigest, intent.DiffDigest) && equalPaths(recovered.ChangedFiles, setupBaselineCandidatePaths(intent.Candidates)) {
				intent.Phase, intent.MergeSHA = SetupBaselineMerged, strings.ToLower(recovered.MergeSHA)
				updated, saveErr := saveSetupBaselineTransition(store, intent, "setup_baseline_merge_recovered", true, fmt.Sprintf("pr=%d merge=%s", intent.PRNumber, intent.MergeSHA))
				return updated, saveErr == nil, saveErr
			}
		}
		return intent, false, err
	}
	if err := setupBaselineMergeGateReadyWithHold(gate, true); err != nil {
		return intent, false, err
	}
	if err := client.ReleaseHeldSetupBaselinePullRequestExact(ctx, config.Repository, intent.PRNumber, intent.Marker, intent.Branch, intent.CommitSHA, intent.DefaultBranch); err != nil {
		return intent, false, err
	}
	snapshot, gate, err = inspectExactSetupBaselineApproval(ctx, client, config, intent, false)
	if err != nil {
		return intent, false, err
	}
	if err := setupBaselineMergeGateReady(gate); err != nil {
		return intent, false, err
	}
	if !strings.EqualFold(snapshot.DiffDigest, intent.DiffDigest) {
		return intent, false, fmt.Errorf("setup baseline raw diff changed after hold release")
	}
	intent.MergeAttemptedAt = time.Now().UTC()
	if err := store.SaveSetupBaselineIntent(intent); err != nil {
		return intent, false, err
	}
	mergeSHA, mergeErr := client.MergePullRequestExact(ctx, config.Repository, intent.PRNumber, intent.CommitSHA, intent.BaseSHA, intent.DefaultBranch, func(live hivegithub.PullRequestGate, liveDiff string) error {
		if !strings.EqualFold(liveDiff, intent.DiffDigest) || !equalPaths(live.ChangedFiles, setupBaselineCandidatePaths(intent.Candidates)) || live.BaselineChanged == false || live.WorkflowChanged || live.SecuritySensitive || live.DeploymentChanged {
			return fmt.Errorf("setup baseline candidate-only diff changed at final merge boundary")
		}
		return setupBaselineMergeGateReady(live)
	})
	if mergeErr != nil {
		var finalGateErr *hivegithub.FinalMergeGateError
		if errors.As(mergeErr, &finalGateErr) {
			return intent, false, mergeErr
		}
		recovered, inspectErr := client.InspectManagedPullRequestExact(ctx, config.Repository, config.RepositoryID, intent.PRNumber, intent.Marker, intent.Branch, intent.CommitSHA, intent.DefaultBranch)
		if inspectErr != nil || !recovered.Merged || !immutableCommit.MatchString(strings.ToLower(recovered.MergeSHA)) ||
			!strings.EqualFold(recovered.HeadSHA, intent.CommitSHA) || !strings.EqualFold(recovered.BaseSHA, intent.BaseSHA) || !strings.EqualFold(recovered.DiffDigest, intent.DiffDigest) || !equalPaths(recovered.ChangedFiles, setupBaselineCandidatePaths(intent.Candidates)) {
			return intent, false, mergeErr
		}
		mergeSHA = recovered.MergeSHA
	}
	if !immutableCommit.MatchString(strings.ToLower(mergeSHA)) {
		return intent, false, fmt.Errorf("setup baseline merge returned no immutable merge commit")
	}
	intent.Phase, intent.MergeSHA = SetupBaselineMerged, strings.ToLower(mergeSHA)
	intent, err = saveSetupBaselineTransition(store, intent, "setup_baseline_merged", true, fmt.Sprintf("actor_id=%d pr=%d head=%s base=%s diff=%s candidates=%s merge=%s", intent.ApprovalActorID, intent.PRNumber, intent.CommitSHA, intent.BaseSHA, intent.DiffDigest, intent.CandidateDigest, intent.MergeSHA))
	return intent, err == nil, err
}

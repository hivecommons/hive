package integrated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

const MergeIntentSchema = "hive.merge-intent.v1"

// MergeIntent is the durable, exact-head checkpoint written before Hive calls
// GitHub's merge API. Recovery accepts a merged repair only when this intent
// proves that Hive authorized the same repository identity, base, head, diff,
// and gate snapshot before the external mutation occurred.
type MergeIntent struct {
	SchemaVersion string                     `json:"schema_version"`
	Repository    string                     `json:"repository"`
	RepositoryID  string                     `json:"repository_id"`
	PRNumber      int                        `json:"pr_number"`
	HeadSHA       string                     `json:"head_sha"`
	BaseSHA       string                     `json:"base_sha"`
	BaseBranch    string                     `json:"base_branch"`
	DiffDigest    string                     `json:"diff_digest"`
	Gate          hivegithub.PullRequestGate `json:"gate"`
	Approval      *MergeApproval             `json:"approval,omitempty"`
	WriterActor   string                     `json:"writer_actor"`
	Policy        automation.Policy          `json:"policy_snapshot"`
	Request       automation.ActionRequest   `json:"request_snapshot"`
	Decision      automation.Decision        `json:"decision"`
	Authorization string                     `json:"authorization_digest"`
	AuthorizedAt  time.Time                  `json:"authorized_at"`
}

type mergeBoundaryResult struct {
	Gate     hivegithub.PullRequestGate
	Approval *MergeApproval
	Decision automation.Decision
	Intent   MergeIntent
	MergeSHA string
}

type mergeBoundaryDeniedError struct {
	Decision automation.Decision
}

func (e *mergeBoundaryDeniedError) Error() string {
	if e == nil {
		return "final live merge authorization was denied"
	}
	return "final live merge authorization was denied: " + strings.Join(e.Decision.Reasons, "; ")
}

func (s *Store) SaveMergeIntent(intent MergeIntent) error {
	intent.SchemaVersion = MergeIntentSchema
	if err := validateMergeIntentRecord(intent); err != nil {
		return err
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	return writeDurableStateFile(filepath.Join(s.dir, "merge-intent.json"), append(data, '\n'))
}

func (s *Store) LoadMergeIntent() (MergeIntent, bool, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, "merge-intent.json"))
	if os.IsNotExist(err) {
		return MergeIntent{}, false, nil
	}
	if err != nil {
		return MergeIntent{}, false, err
	}
	var intent MergeIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return MergeIntent{}, false, fmt.Errorf("decode merge intent: %w", err)
	}
	if err := validateMergeIntentRecord(intent); err != nil {
		return MergeIntent{}, false, err
	}
	return intent, true, nil
}

func (s *Store) DeleteMergeIntent() error {
	return durableRemoveStateFile(filepath.Join(s.dir, "merge-intent.json"))
}

func writeDurableStateFile(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return durableReplaceStateFile(temporary, path)
}

func validateMergeIntentRecord(intent MergeIntent) error {
	if intent.SchemaVersion != MergeIntentSchema || strings.TrimSpace(intent.Repository) == "" || strings.TrimSpace(intent.RepositoryID) == "" || intent.PRNumber <= 0 ||
		!immutableCommit.MatchString(strings.ToLower(intent.HeadSHA)) || !immutableCommit.MatchString(strings.ToLower(intent.BaseSHA)) || strings.TrimSpace(intent.BaseBranch) == "" ||
		len(intent.DiffDigest) != sha256.Size*2 || strings.TrimSpace(intent.WriterActor) == "" || len(intent.Authorization) != sha256.Size*2 ||
		intent.AuthorizedAt.IsZero() || intent.Gate.Number != intent.PRNumber ||
		!strings.EqualFold(intent.Gate.HeadSHA, intent.HeadSHA) || !strings.EqualFold(intent.Gate.BaseSHA, intent.BaseSHA) || !strings.EqualFold(intent.Gate.BaseBranch, intent.BaseBranch) {
		return fmt.Errorf("merge intent is incomplete or invalid")
	}
	if _, err := hex.DecodeString(intent.DiffDigest); err != nil {
		return fmt.Errorf("merge intent diff digest is invalid")
	}
	if _, err := hex.DecodeString(intent.Authorization); err != nil {
		return fmt.Errorf("merge intent authorization digest is invalid")
	}
	if err := validateAuthorizedGateSnapshot(intent); err != nil {
		return err
	}
	wantAuthorization, err := mergeAuthorizationDigest(intent)
	if err != nil {
		return err
	}
	if !strings.EqualFold(intent.Authorization, wantAuthorization) {
		return fmt.Errorf("merge intent authorization snapshot digest does not match")
	}
	if intent.Approval != nil {
		if err := validateMergeApprovalRecord(*intent.Approval); err != nil {
			return fmt.Errorf("merge intent approval: %w", err)
		}
		if intent.Approval.RepositoryID != intent.RepositoryID || !strings.EqualFold(intent.Approval.Repository, intent.Repository) || intent.Approval.PRNumber != intent.PRNumber ||
			!strings.EqualFold(intent.Approval.HeadSHA, intent.HeadSHA) || !strings.EqualFold(intent.Approval.BaseSHA, intent.BaseSHA) ||
			!strings.EqualFold(intent.Approval.BaseBranch, intent.BaseBranch) || !strings.EqualFold(intent.Approval.DiffDigest, intent.DiffDigest) {
			return fmt.Errorf("merge intent approval does not bind the same pull request")
		}
	}
	return nil
}

func validateAuthorizedGateSnapshot(intent MergeIntent) error {
	gate := intent.Gate
	if !gate.Open || gate.Merged || !gate.MergeableKnown || !gate.Mergeable || !gate.VisualHiveVerdictGreen ||
		!gate.VisualHiveProvenanceVerified || !strings.EqualFold(strings.TrimSpace(gate.VisualHiveCheckState), "success") ||
		gate.VisualHiveWorkflowPath != ".github/workflows/visual-hive-pr.yml" || gate.VisualHiveWorkflowEvent != "pull_request" ||
		!gate.BranchProtectionEnabled || !gate.BranchProtectionStrict || !gate.BranchProtectionAdminEnforced || gate.Hold || gate.HumanReviewRequired ||
		gate.BaselineChanged || gate.WorkflowChanged || gate.SecuritySensitive || gate.DeploymentChanged || len(gate.RequiredCheckStates) == 0 {
		return fmt.Errorf("merge intent gate snapshot was not safe and complete at authorization time")
	}
	for _, state := range gate.RequiredCheckStates {
		if !strings.EqualFold(strings.TrimSpace(state), "success") {
			return fmt.Errorf("merge intent gate snapshot contains a non-success required check")
		}
	}
	if !intent.Decision.Allowed || intent.Decision.Action != automation.ActionMergePR || intent.Request.Action != automation.ActionMergePR ||
		!strings.EqualFold(intent.Request.Repository, intent.Repository) || !strings.EqualFold(intent.Request.ExpectedHeadSHA, intent.HeadSHA) ||
		!strings.EqualFold(intent.Request.TestedHeadSHA, intent.HeadSHA) || !strings.EqualFold(intent.Request.ExpectedBaseBranch, intent.BaseBranch) ||
		!strings.EqualFold(intent.Request.TestedBaseBranch, intent.BaseBranch) || !intent.Request.BranchProtectionEnabled ||
		!reflect.DeepEqual(intent.Request.ChangedFiles, gate.ChangedFiles) || !reflect.DeepEqual(intent.Request.RequiredCheckNames, gate.RequiredCheckNames) ||
		!reflect.DeepEqual(intent.Request.RequiredCheckStates, gate.RequiredCheckStates) {
		return fmt.Errorf("merge intent decision or request snapshot is incomplete or inconsistent")
	}
	return nil
}

func mergeAuthorizationDigest(intent MergeIntent) (string, error) {
	payload := struct {
		Repository   string
		RepositoryID string
		PRNumber     int
		HeadSHA      string
		BaseSHA      string
		BaseBranch   string
		DiffDigest   string
		Gate         hivegithub.PullRequestGate
		Approval     *MergeApproval
		WriterActor  string
		Policy       automation.Policy
		Request      automation.ActionRequest
		Decision     automation.Decision
	}{
		intent.Repository, intent.RepositoryID, intent.PRNumber, intent.HeadSHA, intent.BaseSHA, intent.BaseBranch, intent.DiffDigest,
		intent.Gate, intent.Approval, intent.WriterActor, intent.Policy, intent.Request, intent.Decision,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("serialize merge authorization snapshot: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func validateMergeIntent(intent MergeIntent, repository, repositoryID, diffDigest string, gate hivegithub.PullRequestGate) error {
	if err := validateMergeIntentRecord(intent); err != nil {
		return err
	}
	if !strings.EqualFold(intent.Repository, repository) || intent.RepositoryID != strings.TrimSpace(repositoryID) || intent.PRNumber != gate.Number ||
		!strings.EqualFold(intent.HeadSHA, gate.HeadSHA) || !strings.EqualFold(intent.BaseSHA, gate.BaseSHA) || !strings.EqualFold(intent.BaseBranch, gate.BaseBranch) ||
		!strings.EqualFold(intent.DiffDigest, strings.TrimSpace(diffDigest)) || !reflect.DeepEqual(intent.Gate, gate) {
		return fmt.Errorf("merge intent no longer matches the exact pull request repository, base, head, or diff")
	}
	return nil
}

// validateRecoveredMergeIntent validates immutable PR identity after GitHub has
// completed the merge. A branch's live base SHA may legitimately advance to the
// merge commit, so recovery relies on the pre-merge base bound by the intent and
// MergePullRequestExact, while still requiring the exact PR, head, raw diff,
// merge result, and writer identity.
func validateRecoveredMergeIntent(intent MergeIntent, repository, repositoryID, diffDigest string, gate hivegithub.PullRequestGate) error {
	if err := validateMergeIntentRecord(intent); err != nil {
		return err
	}
	if !strings.EqualFold(intent.Repository, repository) || intent.RepositoryID != strings.TrimSpace(repositoryID) ||
		intent.PRNumber != gate.Number || !strings.EqualFold(intent.HeadSHA, gate.HeadSHA) ||
		!strings.EqualFold(intent.BaseBranch, gate.BaseBranch) || !strings.EqualFold(intent.DiffDigest, strings.TrimSpace(diffDigest)) || !gate.Merged || strings.TrimSpace(gate.MergeSHA) == "" {
		return fmt.Errorf("merge intent no longer matches the exact merged pull request repository, head, diff, or result")
	}
	return nil
}

// mergeReadyFindingAtBoundary performs the only pre-merge path used by the
// integrated runner. The GitHub client re-inspects the complete live gate and
// invokes this function's callback immediately before its conditional merge
// request. The callback reloads local authority, re-runs policy, re-binds any
// human approval, and durably checkpoints that final authorization.
func mergeReadyFindingAtBoundary(ctx context.Context, stateDir string, installed Config, expectedFinding visualhive.FindingLifecycle, lifecycle *visualhive.LifecycleStore, expectedGate hivegithub.PullRequestGate, expectedApproval *MergeApproval, client *hivegithub.Client) (mergeBoundaryResult, error) {
	result := mergeBoundaryResult{}
	if client == nil || lifecycle == nil {
		return result, fmt.Errorf("GitHub client and lifecycle store are required for final merge authorization")
	}
	callbackReached, callbackAuthorized := false, false
	mergeSHA, err := client.MergePullRequestExact(ctx, installed.Repository, expectedFinding.PRNumber, expectedGate.HeadSHA, expectedGate.BaseSHA, installed.DefaultBranch, func(liveGate hivegithub.PullRequestGate, liveDiffDigest string) error {
		callbackReached = true
		result.Gate = liveGate
		intent, approval, decision, authorizeErr := authorizeLiveMergeIntent(ctx, stateDir, installed, expectedFinding, lifecycle, liveGate, liveDiffDigest, expectedApproval, client)
		result.Intent, result.Approval, result.Decision = intent, approval, decision
		callbackAuthorized = authorizeErr == nil
		return authorizeErr
	})
	if err != nil {
		// When the callback observed an open, unmerged PR and denied current
		// authority, an older intent can no longer safely authorize recovery of a
		// later out-of-band merge. A transport/API failure after authorization is
		// deliberately different: retain the intent because the merge outcome is
		// ambiguous and must be reconciled on restart.
		var finalGateErr *hivegithub.FinalMergeGateError
		if callbackReached && (!callbackAuthorized || errors.As(err, &finalGateErr)) {
			if invalidateErr := invalidateMergeIntentAtBoundary(stateDir, installed.Repository, err); invalidateErr != nil {
				return result, fmt.Errorf("%v; invalidate obsolete merge intent: %w", err, invalidateErr)
			}
		}
		return result, err
	}
	result.MergeSHA = mergeSHA
	return result, nil
}

func authorizeLiveMergeIntent(ctx context.Context, stateDir string, installed Config, expectedFinding visualhive.FindingLifecycle, lifecycle *visualhive.LifecycleStore, gate hivegithub.PullRequestGate, boundaryDiffDigest string, expectedApproval *MergeApproval, client *hivegithub.Client) (MergeIntent, *MergeApproval, automation.Decision, error) {
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return MergeIntent{}, nil, automation.Decision{}, err
	}
	liveConfig, err := store.Load()
	if err != nil {
		return MergeIntent{}, nil, automation.Decision{}, err
	}
	if !strings.EqualFold(liveConfig.Repository, installed.Repository) || liveConfig.RepositoryID != installed.RepositoryID || !strings.EqualFold(liveConfig.DefaultBranch, installed.DefaultBranch) {
		return MergeIntent{}, nil, automation.Decision{}, fmt.Errorf("installed repository identity or default branch changed during the production run")
	}
	pauseRequested, err := PauseRequested(stateDir)
	if err != nil {
		return MergeIntent{}, nil, automation.Decision{}, err
	}
	if liveConfig.Paused || pauseRequested {
		return MergeIntent{}, nil, automation.Decision{}, fmt.Errorf("repository automation was paused before final live merge authorization")
	}

	finding, exists := lifecycle.Finding(expectedFinding.RepositoryFingerprint)
	if !exists || finding.Status != visualhive.StatusReady || finding.PRNumber != gate.Number ||
		!strings.EqualFold(finding.RepairCommitSHA, gate.HeadSHA) || finding.RepositoryFingerprint != expectedFinding.RepositoryFingerprint {
		return MergeIntent{}, nil, automation.Decision{}, fmt.Errorf("live lifecycle no longer contains the exact ready Hive repair authorized for merge")
	}
	if finding.ObservationHumanReviewRequired {
		return MergeIntent{}, nil, automation.Decision{}, fmt.Errorf("live lifecycle requires observation-level human review that merge-policy approval cannot override")
	}
	if !gate.Open || gate.Merged || !gate.VisualHiveProvenanceVerified || !strings.EqualFold(strings.TrimSpace(gate.VisualHiveCheckState), "success") ||
		gate.VisualHiveWorkflowPath != ".github/workflows/visual-hive-pr.yml" || gate.VisualHiveWorkflowEvent != "pull_request" {
		return MergeIntent{}, nil, automation.Decision{}, fmt.Errorf("live pull request gate lacks an exact successful Visual Hive PR workflow proof")
	}
	diffDigest, err := client.PullRequestDiffDigest(ctx, liveConfig.Repository, gate.Number)
	if err != nil {
		return MergeIntent{}, nil, automation.Decision{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(diffDigest), strings.TrimSpace(boundaryDiffDigest)) {
		return MergeIntent{}, nil, automation.Decision{}, fmt.Errorf("pull request diff changed during final live merge authorization")
	}

	approval, approvalExists, err := store.LoadMergeApproval()
	if err != nil {
		return MergeIntent{}, nil, automation.Decision{}, err
	}
	var activeApproval *MergeApproval
	if approvalExists {
		activeApproval = &approval
	}
	if !approvalsEquivalent(activeApproval, expectedApproval) {
		return MergeIntent{}, activeApproval, automation.Decision{}, fmt.Errorf("active merge approval changed after the prior gate authorization")
	}
	if activeApproval != nil {
		if err := ValidateMergeApproval(*activeApproval, liveConfig.Repository, liveConfig.RepositoryID, diffDigest, gate); err != nil {
			return MergeIntent{}, activeApproval, automation.Decision{}, fmt.Errorf("re-bind active merge approval to final live gate: %w", err)
		}
	}
	if finding.HumanReviewRequired && (finding.ManualReviewKind != "merge_policy" || activeApproval == nil) {
		return MergeIntent{}, activeApproval, automation.Decision{}, fmt.Errorf("durable Hive manual-review hold has no exact active merge approval")
	}

	policy := mergePolicyForFinding(integratedPolicy(liveConfig), finding)
	request := mergeActionRequest(liveConfig, finding, gate)
	decision := policy.Authorize(request)
	if activeApproval != nil && !decision.Allowed {
		if approved, eligible := authorizePathApprovedMerge(policy, request, decision); eligible {
			decision = approved
		}
	}
	if !decision.Allowed {
		return MergeIntent{}, activeApproval, decision, &mergeBoundaryDeniedError{Decision: decision}
	}
	intent, err := prepareMergeIntent(ctx, stateDir, liveConfig, finding, gate, activeApproval, policy, decision, client)
	if err != nil {
		return MergeIntent{}, activeApproval, decision, err
	}
	if !strings.EqualFold(intent.DiffDigest, boundaryDiffDigest) {
		return MergeIntent{}, activeApproval, decision, fmt.Errorf("durable merge intent did not bind the exact final live diff")
	}
	return intent, activeApproval, decision, nil
}

func invalidateMergeIntentAtBoundary(stateDir, repository string, cause error) error {
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return err
	}
	intent, exists, err := store.LoadMergeIntent()
	if err != nil || !exists {
		return err
	}
	detail := fmt.Sprintf("pr=%d base=%s head=%s authorization=%s reason=%v", intent.PRNumber, intent.BaseSHA, intent.HeadSHA, intent.Authorization, cause)
	if err := store.AuditStrict(AuditEntry{Action: "invalidate_merge_intent_at_final_gate", Allowed: false, Repository: repository, Detail: detail}); err != nil {
		return err
	}
	return store.DeleteMergeIntent()
}

func prepareMergeIntent(ctx context.Context, stateDir string, config Config, finding visualhive.FindingLifecycle, gate hivegithub.PullRequestGate, approval *MergeApproval, policy automation.Policy, decision automation.Decision, client *hivegithub.Client) (MergeIntent, error) {
	if finding.PRNumber != gate.Number || !strings.EqualFold(finding.RepairCommitSHA, gate.HeadSHA) {
		return MergeIntent{}, fmt.Errorf("cannot authorize merge intent for a gate that differs from the durable Hive repair finding")
	}
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return MergeIntent{}, err
	}
	liveConfig, err := store.Load()
	if err != nil {
		return MergeIntent{}, err
	}
	if liveConfig.Paused {
		return MergeIntent{}, fmt.Errorf("repository automation was paused before merge intent activation")
	}
	liveID, err := verifyLiveRepositoryIdentity(ctx, client, config)
	if err != nil {
		return MergeIntent{}, err
	}
	digest, err := client.PullRequestDiffDigest(ctx, config.Repository, gate.Number)
	if err != nil {
		return MergeIntent{}, err
	}
	if approval != nil {
		if err := ValidateMergeApproval(*approval, config.Repository, liveID, digest, gate); err != nil {
			return MergeIntent{}, err
		}
	}
	writer, err := client.AuthenticatedLogin(ctx)
	if err != nil {
		return MergeIntent{}, fmt.Errorf("bind merge writer identity: %w", err)
	}
	policy = mergePolicyForFinding(policy, finding)
	request := mergeActionRequest(config, finding, gate)
	if !decision.Allowed || decision.Action != automation.ActionMergePR {
		return MergeIntent{}, fmt.Errorf("cannot persist a merge intent without the exact allowed merge decision")
	}
	intent := MergeIntent{
		SchemaVersion: MergeIntentSchema, Repository: config.Repository, RepositoryID: liveID, PRNumber: gate.Number,
		HeadSHA: strings.ToLower(gate.HeadSHA), BaseSHA: strings.ToLower(gate.BaseSHA), BaseBranch: gate.BaseBranch, DiffDigest: digest,
		Gate: gate, Approval: approval, WriterActor: writer, Policy: policy, Request: request, Decision: decision, AuthorizedAt: time.Now().UTC(),
	}
	intent.Authorization, err = mergeAuthorizationDigest(intent)
	if err != nil {
		return MergeIntent{}, err
	}
	if existing, exists, loadErr := store.LoadMergeIntent(); loadErr != nil {
		return MergeIntent{}, loadErr
	} else if exists {
		if mergeIntentsAuthorizeSameLiveGate(existing, intent) {
			return existing, nil
		}
		if err := store.AuditStrict(AuditEntry{Action: "invalidate_merge_intent", Allowed: false, Repository: config.Repository, Detail: fmt.Sprintf("replaced stale intent for pr=%d head=%s", existing.PRNumber, existing.HeadSHA)}); err != nil {
			return MergeIntent{}, err
		}
		if err := store.DeleteMergeIntent(); err != nil {
			return MergeIntent{}, err
		}
	}
	detail, _ := json.Marshal(intent)
	if err := store.AuditStrict(AuditEntry{Action: "authorize_merge_intent", Allowed: true, Repository: config.Repository, Detail: string(detail)}); err != nil {
		return MergeIntent{}, err
	}
	if err := store.SaveMergeIntent(intent); err != nil {
		return MergeIntent{}, err
	}
	return intent, nil
}

func mergeIntentsAuthorizeSameLiveGate(left, right MergeIntent) bool {
	if validateMergeIntentRecord(left) != nil || validateMergeIntentRecord(right) != nil {
		return false
	}
	left.Decision.EvaluatedAt = time.Time{}
	right.Decision.EvaluatedAt = time.Time{}
	left.AuthorizedAt, right.AuthorizedAt = time.Time{}, time.Time{}
	left.Authorization, right.Authorization = "", ""
	return reflect.DeepEqual(left, right)
}

func approvalsEquivalent(left, right *MergeApproval) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

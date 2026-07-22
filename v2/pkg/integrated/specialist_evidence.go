package integrated

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

var specialistDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type specialistAttemptBinding struct {
	Attempt        uint64
	StartedAt      time.Time
	DeadlineAnchor time.Time
}

// nextSpecialistAttemptBinding mirrors Worker.Run's attempt selection so the
// controller can bind the provider deadline before the Worker persists or
// resumes that same attempt. Once persisted, StartedAt is the durable replay
// anchor and never depends on a later wall-clock retry.
func nextSpecialistAttemptBinding(state *repair.Store, finding visualhive.FindingLifecycle, now time.Time) (specialistAttemptBinding, error) {
	if state == nil {
		return specialistAttemptBinding{}, fmt.Errorf("repair state is required for specialist attempt binding")
	}
	if err := state.Err(); err != nil {
		return specialistAttemptBinding{}, fmt.Errorf("repair state is unavailable: %w", err)
	}
	now = now.UTC()
	if now.IsZero() {
		return specialistAttemptBinding{}, fmt.Errorf("specialist attempt binding requires a current UTC time")
	}
	existing, resumed := state.Get(finding.RepositoryFingerprint)
	recurrenceChanged := resumed && existing.Recurrence != finding.Recurrences
	if resumed && !recurrenceChanged && existing.Stage == repair.StagePROpen && finding.Status == visualhive.StatusNeedsRevision && finding.MergeSHA == "" {
		nextAttempt := max(finding.RepairAttempts+1, existing.Attempt+1)
		if existing.SpecialistRevisionAttempt != 0 {
			if existing.SpecialistRevisionAttempt != nextAttempt || existing.SpecialistRevisionStartedAt.IsZero() ||
				!strings.EqualFold(existing.SpecialistRevisionBaseSHA, finding.RepairCommitSHA) ||
				existing.SpecialistRevisionBranch != finding.Branch || existing.SpecialistRevisionPRNumber != finding.PRNumber {
				return specialistAttemptBinding{}, fmt.Errorf("persisted specialist revision reservation does not match the exact lifecycle PR head")
			}
			startedAt := existing.SpecialistRevisionStartedAt.UTC()
			return specialistAttemptBinding{Attempt: uint64(nextAttempt), StartedAt: startedAt, DeadlineAnchor: startedAt}, nil
		}
		existing.SpecialistRevisionAttempt = nextAttempt
		existing.SpecialistRevisionStartedAt = now
		existing.SpecialistRevisionBaseSHA = strings.ToLower(strings.TrimSpace(finding.RepairCommitSHA))
		existing.SpecialistRevisionBranch = finding.Branch
		existing.SpecialistRevisionPRNumber = finding.PRNumber
		if err := state.Put(existing); err != nil {
			return specialistAttemptBinding{}, fmt.Errorf("persist specialist revision attempt reservation: %w", err)
		}
		return specialistAttemptBinding{Attempt: uint64(nextAttempt), StartedAt: now, DeadlineAnchor: now}, nil
	}
	startNew := !resumed || recurrenceChanged || existing.Stage == repair.StageNoChange
	if !startNew {
		if existing.Attempt <= 0 || existing.StartedAt.IsZero() {
			return specialistAttemptBinding{}, fmt.Errorf("persisted repair attempt has no stable ordinal or start time")
		}
		startedAt := existing.StartedAt.UTC()
		deadlineAnchor := startedAt
		// An audited infrastructure retry resumes the same attempt ordinal and
		// original StartedAt, but it must receive a fresh bounded model-transport
		// window. RetryAuthorizedAt is durable, transaction-bound, and stable
		// across controller restarts, unlike a new wall-clock observation.
		if existing.Stage == repair.StagePrepared && existing.RetryResumeStage == repair.StagePrepared &&
			existing.RetryTransactionID != "" && !existing.RetryAuthorizedAt.IsZero() {
			deadlineAnchor = existing.RetryAuthorizedAt.UTC()
		}
		return specialistAttemptBinding{Attempt: uint64(existing.Attempt), StartedAt: startedAt, DeadlineAnchor: deadlineAnchor}, nil
	}
	attempt := finding.RepairAttempts + 1
	if !recurrenceChanged && resumed && existing.Attempt+1 > attempt {
		attempt = existing.Attempt + 1
	}
	if attempt <= 0 {
		attempt = 1
	}
	return specialistAttemptBinding{Attempt: uint64(attempt), StartedAt: now, DeadlineAnchor: now}, nil
}

// specialistResolutionForFinding routes only immutable fields imported from a
// verified Visual Hive observation. It never infers authority from issue prose.
func specialistResolutionForFinding(finding visualhive.FindingLifecycle) visualhive.SpecialistResolution {
	grounded := !finding.ObservationHumanReviewRequired &&
		strings.TrimSpace(finding.ValidationCommand) != "" &&
		hasExactAffectedContract(finding.AffectedContracts)
	return visualhive.ResolveSpecialist(visualhive.SpecialistRoutingInput{
		IssueKind:                finding.IssueKind,
		OwningAgentHint:          finding.OwningAgentHint,
		GroundedRepairScope:      grounded,
		BaselineChangesForbidden: true,
	})
}

func hasExactAffectedContract(contracts []string) bool {
	if len(contracts) == 0 || len(contracts) > 256 {
		return false
	}
	seen := make(map[string]bool, len(contracts))
	for _, contract := range contracts {
		contract = strings.TrimSpace(contract)
		if contract == "" || len(contract) > 512 || strings.IndexByte(contract, 0) >= 0 || seen[contract] {
			return false
		}
		seen[contract] = true
	}
	return true
}

// specialistEvidenceIdentity converts the independently verified GitHub run,
// artifact, and content-addressed bundle identity into the durable mailbox
// contract. ArtifactSHA256 intentionally identifies the exact manifest bytes;
// BundleSHA256 separately identifies the complete bundle content.
func specialistEvidenceIdentity(verified hivegithub.VerifiedVisualHiveArtifact, finding visualhive.FindingLifecycle) (agent.SpecialistEvidenceIdentity, error) {
	return verified.SpecialistEvidenceIdentity(finding)
}

// specialistRevisionEvidenceIdentity binds one advisory failed PR artifact to
// the exact existing repair PR and head. This identity may ground a bounded
// specialist revision only; it carries no resolution, absence, baseline,
// approval, merge, or lifecycle-writer authority.
func specialistRevisionEvidenceIdentity(verified hivegithub.VerifiedPullRequestArtifact, finding visualhive.FindingLifecycle) (agent.SpecialistEvidenceIdentity, error) {
	if finding.Status != visualhive.StatusNeedsRevision || finding.PRNumber <= 0 || finding.PRNumber != verified.PullRequestNumber {
		return agent.SpecialistEvidenceIdentity{}, fmt.Errorf("specialist revision evidence requires the exact needs-revision pull request")
	}
	if strings.TrimSpace(finding.RepositoryID) == "" || finding.RepositoryID != verified.RepositoryID {
		return agent.SpecialistEvidenceIdentity{}, fmt.Errorf("specialist revision evidence repository identity does not match the lifecycle")
	}
	if strings.TrimSpace(finding.Branch) == "" || finding.Branch != verified.HeadBranch ||
		!validSpecialistGitObject(finding.RepairCommitSHA) || !strings.EqualFold(finding.RepairCommitSHA, verified.CommitSHA) {
		return agent.SpecialistEvidenceIdentity{}, fmt.Errorf("specialist revision evidence does not match the exact lifecycle PR head")
	}
	if verified.ReviewSchemaVersion != visualhive.ReviewEvidenceSchemaVersion ||
		!specialistDigestPattern.MatchString(strings.ToLower(strings.TrimSpace(verified.ArtifactIndexSHA256))) {
		return agent.SpecialistEvidenceIdentity{}, fmt.Errorf("specialist revision evidence is not a complete content-addressed review artifact")
	}
	if verified.WorkflowRunID <= 0 || verified.WorkflowRunAttempt <= 0 || verified.ArtifactID <= 0 ||
		verified.WorkflowName != "Visual Hive PR" || !workflowPathMatchesForSpecialist(verified.WorkflowPath, ".github/workflows/visual-hive-pr.yml") ||
		verified.ArtifactName != "visual-hive-pr" || verified.Conclusion != "failure" {
		return agent.SpecialistEvidenceIdentity{}, fmt.Errorf("specialist revision evidence workflow identity is invalid")
	}

	receiptBody := struct {
		SchemaVersion      string `json:"schema_version"`
		Repository         string `json:"repository"`
		RepositoryID       string `json:"repository_id"`
		PullRequestNumber  int    `json:"pull_request_number"`
		HeadSHA            string `json:"head_sha"`
		HeadBranch         string `json:"head_branch"`
		WorkflowRunID      int64  `json:"workflow_run_id"`
		WorkflowRunAttempt int    `json:"workflow_run_attempt"`
		WorkflowName       string `json:"workflow_name"`
		WorkflowPath       string `json:"workflow_path"`
		Conclusion         string `json:"conclusion"`
		ArtifactID         int64  `json:"artifact_id"`
		ArtifactName       string `json:"artifact_name"`
		ArtifactIndexSHA   string `json:"artifact_index_sha256"`
	}{
		SchemaVersion: verified.ReviewSchemaVersion, Repository: finding.Repository, RepositoryID: verified.RepositoryID,
		PullRequestNumber: verified.PullRequestNumber, HeadSHA: strings.ToLower(verified.CommitSHA), HeadBranch: verified.HeadBranch,
		WorkflowRunID: verified.WorkflowRunID, WorkflowRunAttempt: verified.WorkflowRunAttempt, WorkflowName: verified.WorkflowName,
		WorkflowPath: verified.WorkflowPath, Conclusion: verified.Conclusion, ArtifactID: verified.ArtifactID,
		ArtifactName: verified.ArtifactName, ArtifactIndexSHA: strings.ToLower(verified.ArtifactIndexSHA256),
	}
	encoded, err := json.Marshal(receiptBody)
	if err != nil {
		return agent.SpecialistEvidenceIdentity{}, fmt.Errorf("encode specialist revision evidence receipt: %w", err)
	}
	receiptDigest := sha256.Sum256(encoded)
	return agent.SpecialistEvidenceIdentity{
		Variant:             visualhive.SpecialistEvidenceVariantRevision,
		BundleSchemaVersion: verified.ReviewSchemaVersion, BundleSHA256: strings.ToLower(verified.ArtifactIndexSHA256),
		VerificationReceiptSHA256: hex.EncodeToString(receiptDigest[:]), WorkflowRunID: uint64(verified.WorkflowRunID),
		WorkflowRunAttempt: uint64(verified.WorkflowRunAttempt), WorkflowRunHeadSHA: strings.ToLower(verified.CommitSHA),
		ArtifactID: uint64(verified.ArtifactID), ArtifactName: verified.ArtifactName, ArtifactSHA256: strings.ToLower(verified.ArtifactIndexSHA256),
	}, nil
}

func workflowPathMatchesForSpecialist(actual, expected string) bool {
	actual = strings.ReplaceAll(strings.TrimSpace(strings.Split(actual, "@")[0]), `\`, "/")
	expected = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(expected), `\`, "/"), "./")
	return actual == expected || strings.HasSuffix(actual, "/"+expected)
}

func validSpecialistGitObject(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

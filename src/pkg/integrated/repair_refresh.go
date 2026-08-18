package integrated

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kubestellar/hive/pkg/automation"
	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/repair"
	"github.com/kubestellar/hive/pkg/visualhive"
)

const (
	RepairRefreshSchema    = "hive.repair-refresh.v2"
	repairRefreshFile      = "repair-refresh.json"
	maxBaseRefreshesPerRun = 3
)

type RepairRefreshIntent struct {
	SchemaVersion           string    `json:"schema_version"`
	Repository              string    `json:"repository"`
	RepositoryID            string    `json:"repository_id"`
	RepositoryFingerprint   string    `json:"repository_fingerprint"`
	Recurrence              int       `json:"recurrence"`
	Attempt                 int       `json:"attempt"`
	PRNumber                int       `json:"pr_number"`
	Branch                  string    `json:"branch"`
	OldHeadSHA              string    `json:"old_head_sha"`
	CurrentHeadSHA          string    `json:"current_head_sha"`
	BaseBranch              string    `json:"base_branch"`
	BaseSHA                 string    `json:"base_sha"`
	ChangedFiles            []string  `json:"changed_files"`
	ContributionPatchID     string    `json:"contribution_patch_id,omitempty"`
	CandidateHeadSHA        string    `json:"candidate_head_sha,omitempty"`
	ChainDepth              int       `json:"chain_depth,omitempty"`
	LegacyOwnershipAdoption bool      `json:"legacy_ownership_adoption,omitempty"`
	LegacyAdoptionAudited   bool      `json:"legacy_adoption_audited,omitempty"`
	ConflictFailureID       string    `json:"conflict_failure_id,omitempty"`
	ConflictReason          string    `json:"conflict_reason,omitempty"`
	FinalOwnedHeadSHA       string    `json:"final_owned_head_sha,omitempty"`
	AuthorizedAt            time.Time `json:"authorized_at"`
	LocalValidationComplete time.Time `json:"local_validation_completed_at,omitempty"`
}

func (s *Store) SaveRepairRefreshIntent(intent RepairRefreshIntent) error {
	intent.SchemaVersion = RepairRefreshSchema
	if err := validateRepairRefreshIntent(intent); err != nil {
		return err
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	return writeDurableStateFile(filepath.Join(s.dir, repairRefreshFile), append(data, '\n'))
}

func (s *Store) LoadRepairRefreshIntent() (RepairRefreshIntent, bool, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, repairRefreshFile))
	if os.IsNotExist(err) {
		return RepairRefreshIntent{}, false, nil
	}
	if err != nil {
		return RepairRefreshIntent{}, false, err
	}
	var intent RepairRefreshIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return RepairRefreshIntent{}, false, fmt.Errorf("decode repair refresh intent: %w", err)
	}
	if err := validateRepairRefreshIntent(intent); err != nil {
		return RepairRefreshIntent{}, false, err
	}
	return intent, true, nil
}

func (s *Store) DeleteRepairRefreshIntent() error {
	return durableRemoveStateFile(filepath.Join(s.dir, repairRefreshFile))
}

func validateRepairRefreshIntent(intent RepairRefreshIntent) error {
	if intent.SchemaVersion != RepairRefreshSchema || strings.TrimSpace(intent.Repository) == "" || strings.TrimSpace(intent.RepositoryID) == "" ||
		strings.TrimSpace(intent.RepositoryFingerprint) == "" || intent.Recurrence < 0 || intent.Attempt <= 0 || intent.PRNumber <= 0 ||
		!strings.HasPrefix(intent.Branch, "hive/repair-") || !immutableCommit.MatchString(strings.ToLower(intent.OldHeadSHA)) || !immutableCommit.MatchString(strings.ToLower(intent.CurrentHeadSHA)) ||
		strings.TrimSpace(intent.BaseBranch) == "" || !immutableCommit.MatchString(strings.ToLower(intent.BaseSHA)) || len(intent.ChangedFiles) == 0 || intent.AuthorizedAt.IsZero() {
		return fmt.Errorf("repair refresh intent is incomplete or invalid")
	}
	if intent.CandidateHeadSHA != "" && (!immutableCommit.MatchString(strings.ToLower(intent.CandidateHeadSHA)) || intent.ContributionPatchID == "") {
		return fmt.Errorf("repair refresh intent has an invalid local candidate")
	}
	if intent.ContributionPatchID != "" && !immutableCommit.MatchString(strings.ToLower(intent.ContributionPatchID)) {
		return fmt.Errorf("repair refresh intent has an invalid contribution patch ID")
	}
	if intent.FinalOwnedHeadSHA != "" && (!immutableCommit.MatchString(strings.ToLower(intent.FinalOwnedHeadSHA)) || intent.CandidateHeadSHA == "") {
		return fmt.Errorf("repair refresh intent has an invalid owned checkpoint SHA")
	}
	if !intent.LocalValidationComplete.IsZero() && intent.CandidateHeadSHA == "" {
		return fmt.Errorf("repair refresh validation has no local candidate")
	}
	if (intent.ConflictFailureID == "") != (intent.ConflictReason == "") || intent.ConflictFailureID != "" && len(intent.ConflictFailureID) != 32 {
		return fmt.Errorf("repair refresh conflict checkpoint is incomplete")
	}
	return nil
}

func refreshOwnedRepairBranchIfBehind(ctx context.Context, stateDir string, config Config, finding visualhive.FindingLifecycle, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client, policy automation.Policy) (visualhive.FindingLifecycle, bool, error) {
	integratedStore, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return finding, false, err
	}
	repairStore, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		return finding, false, err
	}
	attempt, exists := repairStore.Get(finding.RepositoryFingerprint)
	if !exists {
		return finding, false, fmt.Errorf("repair lifecycle has no exact durable worker checkpoint")
	}
	intent, recovering, err := integratedStore.LoadRepairRefreshIntent()
	if err != nil {
		return finding, false, err
	}
	if recovering {
		if err := validateRepairRefreshRecovery(config, finding, attempt, intent); err != nil {
			return finding, false, err
		}
		if intent.ConflictFailureID != "" {
			return reconcileRepairRefreshConflict(stateDir, config, finding, lifecycle, integratedStore, repairStore, attempt, intent)
		}
	} else {
		gate, gateErr := client.InspectPullRequestGate(ctx, config.Repository, finding.PRNumber)
		if gateErr != nil {
			return finding, false, gateErr
		}
		if gate.HeadSHA != finding.RepairCommitSHA {
			return finding, false, fmt.Errorf("repair PR head does not match exact lifecycle state before base refresh")
		}
		if !strings.EqualFold(gate.MergeableState, "behind") {
			return finding, false, nil
		}
		if err := validateRefreshSourceState(config, finding, attempt, gate); err != nil {
			return finding, false, err
		}
		liveFiles, staleFiles, reconcileErr := reconcileLiveRepairFiles(attempt.ChangedFiles, gate.ChangedFiles)
		if reconcileErr != nil {
			return finding, false, reconcileErr
		}
		if len(staleFiles) > 0 {
			detail := fmt.Sprintf("pr=%d head=%s retained=%s pruned_stale_state=%s", finding.PRNumber, finding.RepairCommitSHA, strings.Join(liveFiles, ","), strings.Join(staleFiles, ","))
			if err := integratedStore.AuditStrict(AuditEntry{Action: "reconcile_repair_changed_files", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
				return finding, false, err
			}
			attempt.ChangedFiles = append([]string(nil), liveFiles...)
			if err := repairStore.Put(attempt); err != nil {
				return finding, false, err
			}
			lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "reconcile_repair_changed_files", true, detail)
		}
		decision := policy.Authorize(automation.ActionRequest{
			Action: automation.ActionRefreshRepairBranch, Agent: repairActor(finding.OwningAgentHint), Repository: config.Repository,
			Risk: mergeRisk(liveFiles), ChangedFiles: liveFiles, RepairAttempts: finding.RepairAttempts,
		})
		detail := strings.Join(decision.Reasons, "; ")
		if detail == "" {
			detail = fmt.Sprintf("refresh exact Hive repair PR #%d at %s onto protected base %s", finding.PRNumber, finding.RepairCommitSHA, gate.BaseSHA)
		}
		if err := integratedStore.AuditStrict(AuditEntry{Action: string(automation.ActionRefreshRepairBranch), Allowed: decision.Allowed, Repository: config.Repository, Detail: detail}); err != nil {
			return finding, false, err
		}
		lifecycle.RecordAuthorization(finding.RepositoryFingerprint, string(automation.ActionRefreshRepairBranch), decision.Allowed, detail)
		if !decision.Allowed {
			return finding, false, fmt.Errorf("repair branch refresh denied: %s", strings.Join(decision.Reasons, "; "))
		}
		intent = RepairRefreshIntent{
			SchemaVersion: RepairRefreshSchema, Repository: config.Repository, RepositoryID: config.RepositoryID,
			RepositoryFingerprint: finding.RepositoryFingerprint, Recurrence: attempt.Recurrence, Attempt: attempt.Attempt,
			PRNumber: finding.PRNumber, Branch: attempt.Branch, OldHeadSHA: attempt.CommitSHA, CurrentHeadSHA: attempt.CommitSHA,
			BaseBranch: gate.BaseBranch, BaseSHA: gate.BaseSHA, ChangedFiles: append([]string(nil), liveFiles...),
			LegacyOwnershipAdoption: attempt.LegacyOwnershipAdoption, AuthorizedAt: time.Now().UTC(),
		}
		if err := integratedStore.SaveRepairRefreshIntent(intent); err != nil {
			return finding, false, err
		}
	}

	if err := invalidateRefreshMergeAuthority(integratedStore, lifecycle, config, finding, intent); err != nil {
		return finding, false, err
	}
	baselineProtection, err := repair.InspectVisualBaselineProtection(ctx, config.CheckoutDir)
	if err != nil {
		return finding, false, fmt.Errorf("inspect trusted visual baseline protection before repair refresh: %w", err)
	}
	commands := repairRefreshCommands(config)
	baseAdvances := 0
	for intent.FinalOwnedHeadSHA == "" {
		observation, observeErr := client.InspectRepairPullRequestRefreshExact(ctx, hivegithub.RepairBranchRefreshRequest{
			Repository: intent.Repository, RepositoryID: intent.RepositoryID, RepositoryFingerprint: intent.RepositoryFingerprint,
			PRNumber: intent.PRNumber, Branch: intent.Branch, ExpectedHeadSHA: intent.CurrentHeadSHA, CandidateHeadSHA: intent.CandidateHeadSHA,
			ExpectedBaseBranch: intent.BaseBranch, ExpectedBaseSHA: intent.BaseSHA, ExpectedChangedFiles: intent.ChangedFiles,
		})
		if observeErr != nil {
			return finding, false, observeErr
		}
		candidatePushed := intent.CandidateHeadSHA != "" && strings.EqualFold(observation.HeadSHA, intent.CandidateHeadSHA)
		if !candidatePushed && !strings.EqualFold(observation.HeadSHA, intent.CurrentHeadSHA) {
			return finding, false, fmt.Errorf("repair PR head changed outside the exact refresh checkpoint")
		}
		if observation.BaseMoved {
			if baseAdvances >= maxBaseRefreshesPerRun {
				return finding, false, fmt.Errorf("protected base advanced more than %d times during one safe refresh; durable checkpoint retained, retry with hive run --state-dir %q --json", maxBaseRefreshesPerRun, stateDir)
			}
			if candidatePushed {
				checkpoint := repair.RefreshedBranchCheckpoint{HeadSHA: intent.CandidateHeadSHA, BaseSHA: intent.BaseSHA, ContributionPatchID: intent.ContributionPatchID, ChangedFiles: append([]string(nil), intent.ChangedFiles...)}
				if err := repair.VerifyRefreshedRepairBranch(ctx, refreshCheckpointRequest(config, attempt, intent, commands, baselineProtection), checkpoint); err != nil {
					return finding, false, err
				}
				var syncErr error
				attempt, finding, syncErr = syncIntermediateRepairRefreshHead(repairStore, lifecycle, attempt, finding, intent.CandidateHeadSHA, intent.ChangedFiles)
				if syncErr != nil {
					return finding, false, syncErr
				}
				intent.CurrentHeadSHA = intent.CandidateHeadSHA
				intent.LegacyOwnershipAdoption = false
			}
			if err := repair.ResetRefreshedRepairBranch(ctx, attempt.Worktree, intent.CurrentHeadSHA); err != nil {
				return finding, false, err
			}
			intent.BaseSHA, intent.CandidateHeadSHA, intent.FinalOwnedHeadSHA = observation.BaseSHA, "", ""
			intent.LocalValidationComplete = time.Time{}
			intent.ChainDepth++
			baseAdvances++
			if err := integratedStore.SaveRepairRefreshIntent(intent); err != nil {
				return finding, false, err
			}
			continue
		}
		if candidatePushed {
			checkpoint := repair.RefreshedBranchCheckpoint{HeadSHA: intent.CandidateHeadSHA, BaseSHA: intent.BaseSHA, ContributionPatchID: intent.ContributionPatchID, ChangedFiles: append([]string(nil), intent.ChangedFiles...)}
			if err := repair.VerifyRefreshedRepairBranch(ctx, refreshCheckpointRequest(config, attempt, intent, commands, baselineProtection), checkpoint); err != nil {
				return finding, false, err
			}
			intent.FinalOwnedHeadSHA = intent.CandidateHeadSHA
			if err := integratedStore.SaveRepairRefreshIntent(intent); err != nil {
				return finding, false, err
			}
			break
		}
		if intent.CandidateHeadSHA == "" {
			request := refreshCheckpointRequest(config, attempt, intent, commands, baselineProtection)
			if request.AllowLegacyHeadAdoption && !intent.LegacyAdoptionAudited {
				detail := fmt.Sprintf("one-time migrated repair head adoption repo_id=%s pr=%d branch=%s head=%s base=%s files=%s", intent.RepositoryID, intent.PRNumber, intent.Branch, intent.CurrentHeadSHA, intent.BaseSHA, strings.Join(intent.ChangedFiles, ","))
				if err := integratedStore.AuditStrict(AuditEntry{Action: "adopt_legacy_repair_head_for_refresh", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
					return finding, false, err
				}
				lifecycle.RecordAuthorization(intent.RepositoryFingerprint, "adopt_legacy_repair_head_for_refresh", true, detail)
				intent.LegacyAdoptionAudited = true
				if err := integratedStore.SaveRepairRefreshIntent(intent); err != nil {
					return finding, false, err
				}
			}
			checkpoint, prepareErr := repair.PrepareRefreshedRepairBranch(ctx, request)
			if prepareErr != nil {
				if repair.IsRepairBranchRefreshConflict(prepareErr) {
					return checkpointRepairRefreshConflict(stateDir, config, finding, lifecycle, integratedStore, repairStore, attempt, intent, prepareErr)
				}
				if repair.IsRepairBranchRefreshRefMoved(prepareErr) {
					if baseAdvances >= maxBaseRefreshesPerRun {
						return finding, false, fmt.Errorf("repository refs moved more than %d times during one safe refresh; no unvalidated remote mutation occurred, retry with hive run --state-dir %q --json", maxBaseRefreshesPerRun, stateDir)
					}
					baseAdvances++
					continue // re-read the exact live PR/base before selecting another SHA
				}
				return finding, false, prepareErr
			}
			if intent.ContributionPatchID != "" && !strings.EqualFold(intent.ContributionPatchID, checkpoint.ContributionPatchID) {
				return finding, false, fmt.Errorf("chained base refresh changed the exact repair contribution")
			}
			intent.ContributionPatchID, intent.CandidateHeadSHA = checkpoint.ContributionPatchID, checkpoint.HeadSHA
			intent.LocalValidationComplete = time.Now().UTC()
			if err := integratedStore.SaveRepairRefreshIntent(intent); err != nil {
				return finding, false, err
			}
			continue // mandatory live PR/base re-read immediately before push
		}
		checkpoint := repair.RefreshedBranchCheckpoint{HeadSHA: intent.CandidateHeadSHA, BaseSHA: intent.BaseSHA, ContributionPatchID: intent.ContributionPatchID, ChangedFiles: append([]string(nil), intent.ChangedFiles...)}
		if err := repair.PushRefreshedRepairBranchExact(ctx, refreshCheckpointRequest(config, attempt, intent, commands, baselineProtection), checkpoint); err != nil {
			return finding, false, err
		}
		// The next iteration re-reads the exact PR after the push. A base move
		// chains another local refresh; an ambiguous process stop is recovered
		// because CandidateHeadSHA was durable before this mutation.
	}

	if attempt.CommitSHA != intent.FinalOwnedHeadSHA {
		if attempt.CommitSHA != intent.OldHeadSHA && attempt.CommitSHA != intent.CurrentHeadSHA {
			return finding, false, fmt.Errorf("repair checkpoint head changed ambiguously during base refresh")
		}
		attempt.CommitSHA = intent.FinalOwnedHeadSHA
		attempt.ChangedFiles = append([]string(nil), intent.ChangedFiles...)
		attempt.LegacyOwnershipAdoption = false
		if err := repairStore.Put(attempt); err != nil {
			return finding, false, err
		}
	}
	current, exists := lifecycle.Finding(intent.RepositoryFingerprint)
	if !exists {
		return finding, false, fmt.Errorf("repair lifecycle disappeared during base refresh")
	}
	if current.RepairCommitSHA != intent.FinalOwnedHeadSHA {
		if current.RepairCommitSHA != intent.OldHeadSHA && current.RepairCommitSHA != intent.CurrentHeadSHA {
			return finding, false, fmt.Errorf("repair lifecycle head changed ambiguously during base refresh")
		}
		if err := lifecycle.MarkPRHeadUpdated(intent.RepositoryFingerprint, intent.FinalOwnedHeadSHA); err != nil {
			return finding, false, err
		}
	}
	detail := fmt.Sprintf("pr=%d old_head=%s owned_head=%s base=%s patch_id=%s chain_depth=%d files=%s model_attempt=%d", intent.PRNumber, intent.OldHeadSHA, intent.FinalOwnedHeadSHA, intent.BaseSHA, intent.ContributionPatchID, intent.ChainDepth, strings.Join(intent.ChangedFiles, ","), intent.Attempt)
	if err := integratedStore.AuditStrict(AuditEntry{Action: "refresh_repair_branch_complete", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
		return finding, false, err
	}
	lifecycle.RecordAuthorization(intent.RepositoryFingerprint, "refresh_repair_branch_complete", true, detail)
	if err := integratedStore.DeleteRepairRefreshIntent(); err != nil {
		return finding, false, err
	}
	updated, exists := lifecycle.Finding(intent.RepositoryFingerprint)
	if !exists {
		return finding, false, fmt.Errorf("refreshed repair lifecycle is unavailable")
	}
	return updated, true, nil
}

func syncIntermediateRepairRefreshHead(repairStore *repair.Store, lifecycle *visualhive.LifecycleStore, attempt repair.Attempt, finding visualhive.FindingLifecycle, headSHA string, files []string) (repair.Attempt, visualhive.FindingLifecycle, error) {
	if !immutableCommit.MatchString(strings.ToLower(strings.TrimSpace(headSHA))) {
		return attempt, finding, fmt.Errorf("intermediate repair refresh head is invalid")
	}
	if !strings.EqualFold(attempt.CommitSHA, headSHA) {
		attempt.CommitSHA = headSHA
		attempt.ChangedFiles = append([]string(nil), files...)
		attempt.LegacyOwnershipAdoption = false
		if err := repairStore.Put(attempt); err != nil {
			return attempt, finding, err
		}
	}
	current, exists := lifecycle.Finding(finding.RepositoryFingerprint)
	if !exists {
		return attempt, finding, fmt.Errorf("repair lifecycle disappeared during chained base refresh")
	}
	if !strings.EqualFold(current.RepairCommitSHA, headSHA) {
		if err := lifecycle.MarkPRHeadUpdated(finding.RepositoryFingerprint, headSHA); err != nil {
			return attempt, finding, err
		}
	}
	updated, _ := lifecycle.Finding(finding.RepositoryFingerprint)
	return attempt, updated, nil
}

func repairRefreshCommands(config Config) []repair.Command {
	return []repair.Command{{Name: "git", Args: []string{"diff", "--check"}}}
}

func refreshCheckpointRequest(config Config, attempt repair.Attempt, intent RepairRefreshIntent, commands []repair.Command, baselineProtection repair.BaselineProtection) repair.RefreshedBranchCheckpointRequest {
	return repair.RefreshedBranchCheckpointRequest{
		Worktree: attempt.Worktree, Branch: intent.Branch, RepositoryID: intent.RepositoryID,
		ExpectedRemoteURL:  RepositoryCloneURL(config.Repository),
		BaselineProtection: baselineProtection,
		RemoteHeadSHA:      intent.CurrentHeadSHA, BaseBranch: intent.BaseBranch, BaseSHA: intent.BaseSHA,
		ExpectedChangedFiles: append([]string(nil), intent.ChangedFiles...), ExpectedContributionPatchID: intent.ContributionPatchID,
		AllowLegacyHeadAdoption: intent.LegacyOwnershipAdoption && strings.EqualFold(intent.CurrentHeadSHA, intent.OldHeadSHA),
		ValidationCommands:      commands, CommandTimeout: 15 * time.Minute,
	}
}

func checkpointRepairRefreshConflict(stateDir string, config Config, finding visualhive.FindingLifecycle, lifecycle *visualhive.LifecycleStore, integratedStore *Store, repairStore *repair.Store, attempt repair.Attempt, intent RepairRefreshIntent, conflict error) (visualhive.FindingLifecycle, bool, error) {
	reason := fmt.Sprintf("Hive's local merge of exact protected base %s into exact repair head %s conflicted before any remote mutation: %v", intent.BaseSHA, intent.CurrentHeadSHA, conflict)
	failureID, err := repair.RepairRefreshConflictFailureID(attempt, intent.CurrentHeadSHA, intent.BaseSHA, reason)
	if err != nil {
		return finding, false, err
	}
	intent.ConflictFailureID, intent.ConflictReason = failureID, reason
	intent.CandidateHeadSHA, intent.FinalOwnedHeadSHA = "", ""
	intent.LocalValidationComplete = time.Time{}
	if err := integratedStore.SaveRepairRefreshIntent(intent); err != nil {
		return finding, false, err
	}
	return reconcileRepairRefreshConflict(stateDir, config, finding, lifecycle, integratedStore, repairStore, attempt, intent)
}

func reconcileRepairRefreshConflict(stateDir string, config Config, finding visualhive.FindingLifecycle, lifecycle *visualhive.LifecycleStore, integratedStore *Store, repairStore *repair.Store, attempt repair.Attempt, intent RepairRefreshIntent) (visualhive.FindingLifecycle, bool, error) {
	if intent.ConflictFailureID == "" || intent.ConflictReason == "" {
		return finding, false, fmt.Errorf("repair refresh conflict recovery checkpoint is incomplete")
	}
	if attempt.Stage == repair.StagePROpen {
		paused, err := repair.CheckpointRepairRefreshConflict(repairStore, attempt, intent.CurrentHeadSHA, intent.BaseSHA, intent.ConflictReason)
		if err != nil {
			return finding, false, err
		}
		attempt = paused
	}
	if attempt.Stage != repair.StageFailed || attempt.ResumeStage != repair.StagePROpen || attempt.LastFailureClass != repair.FailureInfrastructure || attempt.LastFailureID != intent.ConflictFailureID {
		return finding, false, fmt.Errorf("durable repair refresh conflict does not match its resumable repair checkpoint")
	}
	current, exists := lifecycle.Finding(intent.RepositoryFingerprint)
	if !exists {
		return finding, false, fmt.Errorf("repair lifecycle disappeared during conflict recovery")
	}
	if current.Status == visualhive.StatusReady {
		if err := lifecycle.MarkPRHeadUpdated(intent.RepositoryFingerprint, intent.OldHeadSHA); err != nil {
			return finding, false, err
		}
		current, _ = lifecycle.Finding(intent.RepositoryFingerprint)
	}
	if current.Status != visualhive.StatusNeedsRevision {
		if err := lifecycle.MarkChecksWithEvidence(intent.RepositoryFingerprint, intent.OldHeadSHA, false, intent.ConflictReason, nil); err != nil {
			return finding, false, err
		}
	}
	current, _ = lifecycle.Finding(intent.RepositoryFingerprint)
	if !current.HumanReviewRequired || current.ManualReviewKind != "repair_base_conflict" {
		if current.HumanReviewRequired && current.ManualReviewKind != "merge_policy" {
			return finding, false, fmt.Errorf("repair refresh conflict cannot replace unrelated manual hold %q", current.ManualReviewKind)
		}
		if current.HumanReviewRequired && current.ManualReviewKind == "merge_policy" {
			if err := lifecycle.MarkManualReviewComplete(intent.RepositoryFingerprint, "merge_policy"); err != nil {
				return finding, false, err
			}
		}
		if err := lifecycle.MarkManualReviewRequired(intent.RepositoryFingerprint, "repair_base_conflict", intent.ConflictReason); err != nil {
			return finding, false, err
		}
	}
	detail := fmt.Sprintf("pr=%d head=%s base=%s failure_id=%s remote_unchanged=true", intent.PRNumber, intent.CurrentHeadSHA, intent.BaseSHA, intent.ConflictFailureID)
	if err := integratedStore.AuditStrict(AuditEntry{Action: "refresh_repair_branch_conflict", Allowed: false, Repository: config.Repository, Detail: detail}); err != nil {
		return finding, false, err
	}
	lifecycle.RecordAuthorization(intent.RepositoryFingerprint, "refresh_repair_branch_conflict", false, detail)
	if err := integratedStore.DeleteRepairRefreshIntent(); err != nil {
		return finding, false, err
	}
	updated, _ := lifecycle.Finding(intent.RepositoryFingerprint)
	resume := fmt.Sprintf("hive retry-repair --state-dir %q --finding %q --recurrence %d --attempt %d --failure-class infrastructure --failure-id %q --reason %q --json", stateDir, intent.RepositoryFingerprint, intent.Recurrence, intent.Attempt, intent.ConflictFailureID, "reviewed protected-base conflict; authorize same-PR revision")
	return updated, false, &repair.ResumableFailureError{
		RepositoryFingerprint: intent.RepositoryFingerprint, Recurrence: intent.Recurrence, Attempt: intent.Attempt,
		Class: repair.FailureInfrastructure, FailureID: intent.ConflictFailureID, Cause: fmt.Errorf("%s; resume with %s", intent.ConflictReason, resume),
	}
}

func validateRefreshSourceState(config Config, finding visualhive.FindingLifecycle, attempt repair.Attempt, gate hivegithub.PullRequestGate) error {
	if finding.Repository != config.Repository || finding.RepositoryID != config.RepositoryID || finding.MergeSHA != "" || finding.PRNumber <= 0 ||
		attempt.Repository != config.Repository || attempt.RepositoryFingerprint != finding.RepositoryFingerprint || attempt.Recurrence != finding.Recurrences ||
		attempt.Attempt != finding.RepairAttempts || !attempt.AttemptCounted || attempt.Stage != repair.StagePROpen || !attempt.LifecyclePROpen ||
		attempt.PRNumber != finding.PRNumber || attempt.PRURL != finding.PRURL || attempt.Branch != finding.Branch || attempt.CommitSHA != finding.RepairCommitSHA ||
		!strings.HasPrefix(attempt.Branch, "hive/repair-") || attempt.BaselineReview != nil || gate.Number != finding.PRNumber || gate.HeadSHA != attempt.CommitSHA ||
		gate.BaseBranch != config.DefaultBranch || !gate.Open || gate.Merged || !gate.BranchProtectionConfigured || !gate.BranchProtectionStrict || !gate.BranchProtectionAdminEnforced {
		return fmt.Errorf("repair branch refresh state is incomplete, ambiguous, or not strict-protected")
	}
	switch finding.Status {
	case visualhive.StatusPROpen, visualhive.StatusChecksRunning, visualhive.StatusNeedsRevision, visualhive.StatusReady:
		return nil
	default:
		return fmt.Errorf("cannot refresh repair branch from lifecycle status %s", finding.Status)
	}
}

func validateRepairRefreshRecovery(config Config, finding visualhive.FindingLifecycle, attempt repair.Attempt, intent RepairRefreshIntent) error {
	if intent.Repository != config.Repository || intent.RepositoryID != config.RepositoryID || intent.RepositoryFingerprint != finding.RepositoryFingerprint ||
		intent.PRNumber != finding.PRNumber || intent.Branch != finding.Branch || intent.Recurrence != finding.Recurrences || intent.Attempt != finding.RepairAttempts ||
		attempt.Repository != config.Repository || attempt.RepositoryFingerprint != intent.RepositoryFingerprint || attempt.PRNumber != intent.PRNumber ||
		attempt.Branch != intent.Branch || attempt.Recurrence != intent.Recurrence || attempt.Attempt != intent.Attempt || !attempt.AttemptCounted ||
		!sameStringSet(attempt.ChangedFiles, intent.ChangedFiles) || attempt.BaselineReview != nil {
		return fmt.Errorf("durable repair refresh intent does not match current repair state")
	}
	if intent.ConflictFailureID == "" && attempt.Stage != repair.StagePROpen {
		return fmt.Errorf("active repair refresh recovery is not PR-open")
	}
	if intent.ConflictFailureID != "" && attempt.Stage != repair.StagePROpen &&
		(attempt.Stage != repair.StageFailed || attempt.ResumeStage != repair.StagePROpen || attempt.LastFailureID != intent.ConflictFailureID) {
		return fmt.Errorf("repair refresh conflict recovery does not match its paused checkpoint")
	}
	allowedHeads := map[string]bool{intent.OldHeadSHA: true, intent.CurrentHeadSHA: true}
	if intent.CandidateHeadSHA != "" {
		allowedHeads[intent.CandidateHeadSHA] = true
	}
	if intent.FinalOwnedHeadSHA != "" {
		allowedHeads[intent.FinalOwnedHeadSHA] = true
	}
	if !allowedHeads[attempt.CommitSHA] || !allowedHeads[finding.RepairCommitSHA] {
		return fmt.Errorf("repair refresh recovery observed an unbound state head")
	}
	return nil
}

func reconcileLiveRepairFiles(authorized, live []string) ([]string, []string, error) {
	normalize := func(values []string) []string {
		result := make([]string, 0, len(values))
		seen := map[string]bool{}
		for _, value := range values {
			value = filepath.ToSlash(strings.TrimPrefix(strings.TrimSpace(value), "./"))
			if value != "" && !seen[value] {
				seen[value] = true
				result = append(result, value)
			}
		}
		sort.Strings(result)
		return result
	}
	authorized, live = normalize(authorized), normalize(live)
	if len(live) == 0 {
		return nil, nil, fmt.Errorf("live repair PR has no semantic changed files")
	}
	authorizedSet := map[string]bool{}
	for _, value := range authorized {
		authorizedSet[value] = true
	}
	for _, value := range live {
		if !authorizedSet[value] {
			return nil, nil, fmt.Errorf("live repair PR added unauthorized path %s", value)
		}
		delete(authorizedSet, value)
	}
	stale := make([]string, 0, len(authorizedSet))
	for value := range authorizedSet {
		stale = append(stale, value)
	}
	sort.Strings(stale)
	return live, stale, nil
}

func invalidateRefreshMergeAuthority(store *Store, lifecycle *visualhive.LifecycleStore, config Config, finding visualhive.FindingLifecycle, refresh RepairRefreshIntent) error {
	approval, exists, err := store.LoadMergeApproval()
	if err != nil {
		return err
	}
	if exists {
		if approval.Repository != config.Repository || approval.RepositoryID != config.RepositoryID || approval.PRNumber != refresh.PRNumber || approval.HeadSHA != refresh.OldHeadSHA {
			return fmt.Errorf("unrelated or ambiguous merge approval blocks repair branch refresh")
		}
		detail := fmt.Sprintf("invalidated approval for PR #%d head %s before exact base refresh", approval.PRNumber, approval.HeadSHA)
		if err := store.AuditStrict(AuditEntry{Action: "invalidate_merge_approval_for_refresh", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
			return err
		}
		if err := store.DeleteMergeApproval(); err != nil {
			return err
		}
		lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "invalidate_merge_approval_for_refresh", true, detail)
	}
	intent, exists, err := store.LoadMergeIntent()
	if err != nil {
		return err
	}
	if exists {
		if intent.Repository != config.Repository || intent.RepositoryID != config.RepositoryID || intent.PRNumber != refresh.PRNumber || intent.HeadSHA != refresh.OldHeadSHA {
			return fmt.Errorf("unrelated or ambiguous merge intent blocks repair branch refresh")
		}
		detail := fmt.Sprintf("invalidated merge intent for PR #%d head %s before exact base refresh", intent.PRNumber, intent.HeadSHA)
		if err := store.AuditStrict(AuditEntry{Action: "invalidate_merge_intent_for_refresh", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
			return err
		}
		if err := store.DeleteMergeIntent(); err != nil {
			return err
		}
		lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "invalidate_merge_intent_for_refresh", true, detail)
	}
	current, found := lifecycle.Finding(refresh.RepositoryFingerprint)
	if !found {
		return fmt.Errorf("repair lifecycle disappeared while invalidating refresh merge authority")
	}
	if current.HumanReviewRequired && current.ManualReviewKind == "merge_policy" {
		if current.PRNumber != refresh.PRNumber || !strings.EqualFold(current.RepairCommitSHA, refresh.OldHeadSHA) {
			return fmt.Errorf("merge-policy hold changed before repair branch refresh")
		}
		detail := fmt.Sprintf("invalidated merge-policy hold for PR #%d head %s before exact base refresh", current.PRNumber, current.RepairCommitSHA)
		if err := store.AuditStrict(AuditEntry{Action: "invalidate_merge_policy_hold_for_refresh", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
			return err
		}
		if err := lifecycle.MarkManualReviewComplete(refresh.RepositoryFingerprint, "merge_policy"); err != nil {
			return err
		}
		lifecycle.RecordAuthorization(refresh.RepositoryFingerprint, "invalidate_merge_policy_hold_for_refresh", true, detail)
	}
	return nil
}

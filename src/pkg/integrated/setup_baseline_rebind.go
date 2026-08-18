package integrated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/pkg/automation"
	hivegithub "github.com/kubestellar/hive/pkg/github"
)

const (
	SetupBaselineRebindSchema           = "hive.setup-baseline-rebind.v1"
	SetupBaselineRebindPrepared         = "prepared"
	SetupBaselineRebindResourcesRetired = "resources_retired"
	setupBaselineRebindFile             = "setup-baseline-rebind.json"
	setupBaselineForceCancelAttempts    = 6
)

// SetupBaselineRebindIntent is the crash-safe bridge between an active hosted
// baseline capture and a changed managed setup. The complete source intent is
// retained as the exact authority for cancellation; TargetConfig is retained
// so status can print a reproducible setup retry without asking a user to edit
// durable JSON by hand.
type SetupBaselineRebindIntent struct {
	SchemaVersion      string                     `json:"schema_version"`
	Phase              string                     `json:"phase"`
	Repository         string                     `json:"repository"`
	RepositoryID       string                     `json:"repository_id"`
	Source             SetupBaselineIntent        `json:"source"`
	TargetConfig       Config                     `json:"target_config"`
	TargetConfigDigest string                     `json:"target_config_digest"`
	CreatedAt          time.Time                  `json:"created_at"`
	UpdatedAt          time.Time                  `json:"updated_at"`
	ResourcesRetiredAt time.Time                  `json:"resources_retired_at,omitempty"`
	PendingAudit       *setupBaselineAuditReceipt `json:"pending_audit,omitempty"`
}

func (s *Store) SaveSetupBaselineRebindIntent(intent SetupBaselineRebindIntent) error {
	intent.SchemaVersion = SetupBaselineRebindSchema
	intent.UpdatedAt = time.Now().UTC()
	if err := validateSetupBaselineRebindIntent(intent); err != nil {
		return err
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	return writeDurableStateFile(filepath.Join(s.dir, setupBaselineRebindFile), append(data, '\n'))
}

func (s *Store) LoadSetupBaselineRebindIntent() (SetupBaselineRebindIntent, bool, error) {
	data, err := readOrdinaryStateFile(filepath.Join(s.dir, setupBaselineRebindFile))
	if os.IsNotExist(err) {
		return SetupBaselineRebindIntent{}, false, nil
	}
	if err != nil {
		return SetupBaselineRebindIntent{}, false, err
	}
	var intent SetupBaselineRebindIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return SetupBaselineRebindIntent{}, false, fmt.Errorf("decode setup baseline rebind intent: %w", err)
	}
	if err := validateSetupBaselineRebindIntent(intent); err != nil {
		return SetupBaselineRebindIntent{}, false, err
	}
	return intent, true, nil
}

func (s *Store) DeleteSetupBaselineRebindIntent() error {
	return durableRemoveStateFile(filepath.Join(s.dir, setupBaselineRebindFile))
}

func validateSetupBaselineRebindIntent(intent SetupBaselineRebindIntent) error {
	if intent.SchemaVersion != SetupBaselineRebindSchema || (intent.Phase != SetupBaselineRebindPrepared && intent.Phase != SetupBaselineRebindResourcesRetired) ||
		strings.TrimSpace(intent.Repository) == "" || strings.TrimSpace(intent.RepositoryID) == "" || intent.CreatedAt.IsZero() || intent.UpdatedAt.IsZero() ||
		intent.Repository != intent.Source.Repository || intent.RepositoryID != intent.Source.RepositoryID {
		return fmt.Errorf("setup baseline rebind intent is incomplete or invalid")
	}
	if intent.Source.Phase == SetupBaselineProductionVerified || validateSetupBaselineIntent(intent.Source) != nil {
		return fmt.Errorf("setup baseline rebind source is not an exact active intent")
	}
	if err := validateSetupBaselineAuditReceipt(intent.PendingAudit); err != nil {
		return err
	}
	digest, err := setupBaselineRebindTargetDigest(intent.TargetConfig)
	if err != nil || !strings.EqualFold(digest, intent.TargetConfigDigest) {
		return fmt.Errorf("setup baseline rebind target binding is invalid")
	}
	if intent.Phase == SetupBaselineRebindResourcesRetired && intent.ResourcesRetiredAt.IsZero() {
		return fmt.Errorf("setup baseline rebind retirement checkpoint is incomplete")
	}
	return nil
}

func setupBaselineRebindTargetDigest(config Config) (string, error) {
	// Setup PR identity is created only after old resources are retired. Timestamps
	// and that future identity are deliberately excluded; every user-controlled
	// managed setup value remains bound.
	config.InstalledAt, config.UpdatedAt = time.Time{}, time.Time{}
	config.SetupBranch, config.SetupPRNumber, config.SetupPRURL, config.SetupHeadSHA = "", 0, "", ""
	data, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func reconcileActiveSetupBaselineReconfiguration(ctx context.Context, store *Store, candidate Config, candidateHead string, idempotent bool, client *hivegithub.Client) error {
	intent, exists, err := store.LoadSetupBaselineIntent()
	if err != nil {
		return err
	}
	if exists {
		intent, err = flushSetupBaselineAudit(store, intent)
		if err != nil {
			return fmt.Errorf("finish pending setup baseline audit before reconfiguration: %w", err)
		}
	}
	rebind, rebindExists, err := store.LoadSetupBaselineRebindIntent()
	if err != nil {
		return err
	}
	if rebindExists {
		rebind, err = flushSetupBaselineRebindAudit(store, rebind)
		if err != nil {
			return fmt.Errorf("finish pending setup baseline rebind audit: %w", err)
		}
	}
	if !rebindExists {
		if !exists || intent.Phase == SetupBaselineProductionVerified {
			return nil
		}
		exactBinding := intent.Repository == candidate.Repository && intent.RepositoryID == candidate.RepositoryID && intent.AuthorizerID == candidate.SetupAuthorizationActorID &&
			intent.SetupPRNumber == candidate.SetupPRNumber && intent.SetupPRURL == candidate.SetupPRURL && strings.EqualFold(intent.SetupHeadSHA, candidateHead) &&
			strings.EqualFold(intent.VisualHiveConfigDigest, candidate.VisualHiveConfigDigest) && strings.EqualFold(intent.ScreenshotContractDigest, candidate.SetupBaselineContractDigest) &&
			strings.EqualFold(intent.InitialBaselineDigest, candidate.SetupBaselineInitialDigest) && equalSetupBaselineCandidates(intent.InitialBaselineCandidates, candidate.SetupBaselineInitialCandidates)
		if idempotent && exactBinding {
			return nil
		}
		targetDigest, digestErr := setupBaselineRebindTargetDigest(candidate)
		if digestErr != nil {
			return digestErr
		}
		now := time.Now().UTC()
		rebind = SetupBaselineRebindIntent{SchemaVersion: SetupBaselineRebindSchema, Phase: SetupBaselineRebindPrepared, Repository: intent.Repository,
			RepositoryID: intent.RepositoryID, Source: intent, TargetConfig: candidate, TargetConfigDigest: targetDigest, CreatedAt: now, UpdatedAt: now}
		rebind, err = saveSetupBaselineRebindTransition(store, rebind, "setup_baseline_rebind_prepared", true,
			fmt.Sprintf("source_phase=%s source_setup_pr=%d source_capture=%s target=%s", intent.Phase, intent.SetupPRNumber, intent.CaptureCorrelation, targetDigest))
		if err != nil {
			return fmt.Errorf("persist and audit setup baseline cancellation before external cleanup: %w", err)
		}
	}
	if rebind.Repository != candidate.Repository || rebind.RepositoryID != candidate.RepositoryID {
		return fmt.Errorf("pending setup baseline rebind belongs to a different repository identity")
	}
	targetDigest, err := setupBaselineRebindTargetDigest(candidate)
	if err != nil {
		return err
	}
	if !strings.EqualFold(rebind.TargetConfigDigest, targetDigest) {
		return fmt.Errorf("pending setup baseline reconfiguration is immutably bound to a different target; retry %s before starting another setup change", SetupBaselineRebindNextCommand(rebind))
	}
	if rebind.Phase == SetupBaselineRebindPrepared {
		if err := retireSetupBaselineRebindResources(ctx, store, client, &rebind); err != nil {
			return fmt.Errorf("setup baseline cancellation is durably prepared and retryable: %w; retry %s", err, SetupBaselineRebindNextCommand(rebind))
		}
		rebind.Phase, rebind.ResourcesRetiredAt = SetupBaselineRebindResourcesRetired, time.Now().UTC()
		rebind, err = saveSetupBaselineRebindTransition(store, rebind, "setup_baseline_rebind_resources_retired", true,
			fmt.Sprintf("source_phase=%s source_capture=%s", rebind.Source.Phase, rebind.Source.CaptureCorrelation))
		if err != nil {
			return fmt.Errorf("persist completed setup baseline resource retirement: %w", err)
		}
	}
	if exists {
		if intent.Repository != rebind.Source.Repository || intent.RepositoryID != rebind.Source.RepositoryID || intent.CreatedAt != rebind.Source.CreatedAt {
			return fmt.Errorf("active setup baseline intent changed after rebind preparation")
		}
		if err := store.DeleteSetupBaselineIntent(); err != nil {
			return fmt.Errorf("retire old setup baseline intent after external cleanup: %w", err)
		}
	}
	return nil
}

func retireSetupBaselineRebindResources(ctx context.Context, store *Store, client *hivegithub.Client, rebind *SetupBaselineRebindIntent) error {
	if rebind == nil {
		return fmt.Errorf("setup baseline rebind intent is required")
	}
	source := rebind.Source
	dispatch, dispatchExists, err := store.LoadWorkflowDispatchIntent()
	if err != nil {
		return err
	}
	if dispatchExists && dispatch.Operation == setupBaselineWorkflowOperation {
		if dispatch.CorrelationID != source.CaptureCorrelation {
			return fmt.Errorf("a different workflow dispatch must be recovered before setup baseline rebind")
		}
		if source.CaptureRunID > 0 && dispatch.RunID > 0 && source.CaptureRunID != dispatch.RunID {
			return fmt.Errorf("obsolete setup baseline source and acknowledged dispatch have different exact run IDs")
		}
	}
	captureRetired := source.CaptureCorrelation != "" && containsExact(source.RetiredCaptureCorrelations, source.CaptureCorrelation)
	if source.CaptureRunID > 0 && !captureRetired {
		var acknowledged *WorkflowDispatchIntent
		if dispatchExists && dispatch.Operation == setupBaselineWorkflowOperation && dispatch.RunID == source.CaptureRunID && !dispatch.DispatchAcknowledgedAt.IsZero() {
			acknowledged = &dispatch
		}
		if err := cancelSetupBaselineCaptureRunExactBound(ctx, client, source, acknowledged); err != nil {
			return err
		}
	}
	if source.Branch != "" {
		if source.Phase != SetupBaselineMerged {
			if _, _, err := client.CloseManagedPullRequestForCancellation(ctx, source.Repository, source.RepositoryID, source.PRNumber, source.PRURL, source.Marker, source.Branch, source.DefaultBranch); err != nil {
				return fmt.Errorf("close exact obsolete setup baseline PR: %w", err)
			}
		}
		if _, _, err := client.DeleteSetupBaselineBranchDescendantExact(ctx, source.Repository, source.Branch, source.CommitSHA); err != nil {
			return fmt.Errorf("delete exact obsolete setup baseline branch: %w", err)
		}
	}
	if dispatchExists && dispatch.Operation == "production" {
		if err := retireTerminalProductionDispatchForSetupRebind(ctx, store, client, rebind, dispatch); err != nil {
			return err
		}
		dispatchExists = false
	}
	if dispatchExists {
		if dispatch.Operation != setupBaselineWorkflowOperation || dispatch.CorrelationID != source.CaptureCorrelation {
			return fmt.Errorf("a different workflow dispatch must be recovered before setup baseline rebind")
		}
		if source.CaptureRunID > 0 && dispatch.RunID > 0 && source.CaptureRunID != dispatch.RunID {
			return fmt.Errorf("obsolete setup baseline source and acknowledged dispatch have different exact run IDs")
		}
		if source.CaptureRunID == 0 {
			candidateRunID := dispatch.RunID
			if candidateRunID == 0 {
				owner, repo, _ := strings.Cut(source.Repository, "/")
				discovered, discoverErr := findCorrelatedWorkflowRun(ctx, client, owner, repo, dispatch.WorkflowFile, dispatch.Ref, dispatch)
				if discoverErr != nil {
					return fmt.Errorf("discover ambiguous obsolete setup baseline dispatch before revocation: %w", discoverErr)
				}
				if discovered != nil {
					candidateRunID = discovered.GetID()
				}
			}
			if candidateRunID > 0 {
				source.CaptureRunID, source.CaptureRunURL = candidateRunID, dispatch.RunURL
				if source.CaptureRunURL == "" {
					source.CaptureRunURL = fmt.Sprintf("https://github.com/%s/actions/runs/%d", source.Repository, candidateRunID)
				}
				source.DispatchAcknowledgedAt = dispatch.DispatchAcknowledgedAt
				if source.DispatchAcknowledgedAt.IsZero() {
					// Older servers do not acknowledge with a run ID. In that path
					// candidateRunID came from exact correlation discovery above.
					source.DispatchAcknowledgedAt = time.Now().UTC()
				}
				rebind.Source = source
				if err := store.SaveSetupBaselineRebindIntent(*rebind); err != nil {
					return fmt.Errorf("bind discovered obsolete setup baseline run before cancellation: %w", err)
				}
				var acknowledged *WorkflowDispatchIntent
				if dispatch.RunID == candidateRunID && !dispatch.DispatchAcknowledgedAt.IsZero() {
					acknowledged = &dispatch
				}
				if err := cancelSetupBaselineCaptureRunExactBound(ctx, client, source, acknowledged); err != nil {
					return err
				}
			}
		}
		if !containsExact(source.RetiredCaptureCorrelations, dispatch.CorrelationID) {
			source.RetiredCaptureCorrelations = append(source.RetiredCaptureCorrelations, dispatch.CorrelationID)
			if len(source.RetiredCaptureCorrelations) > 32 {
				source.RetiredCaptureCorrelations = append([]string(nil), source.RetiredCaptureCorrelations[len(source.RetiredCaptureCorrelations)-32:]...)
			}
			rebind.Source = source
			if err := store.SaveSetupBaselineRebindIntent(*rebind); err != nil {
				return fmt.Errorf("persist obsolete setup baseline dispatch tombstone: %w", err)
			}
		}
		if err := store.DeleteWorkflowDispatchIntent(); err != nil {
			return fmt.Errorf("retire exact obsolete setup baseline dispatch: %w", err)
		}
	}
	return nil
}

func retireTerminalProductionDispatchForSetupRebind(ctx context.Context, store *Store, client *hivegithub.Client, rebind *SetupBaselineRebindIntent, dispatch WorkflowDispatchIntent) error {
	if rebind == nil || dispatch.Operation != "production" || dispatch.RunID <= 0 {
		return fmt.Errorf("a different workflow dispatch must be recovered before setup baseline rebind")
	}
	if err := validateWorkflowDispatchIntent(dispatch); err != nil {
		return fmt.Errorf("validate terminal production dispatch before setup baseline rebind: %w", err)
	}
	if err := validateWorkflowDispatchOperationBinding(dispatch, rebind.TargetConfig, "hive-visual-hive.yml", rebind.Source.DefaultBranch, "production"); err != nil {
		return fmt.Errorf("validate terminal production dispatch before setup baseline rebind: %w", err)
	}
	source := rebind.Source
	expectedHead := source.MergeSHA
	pending := exactPreCaptureSetupBaselinePending(source)
	if source.Phase != SetupBaselineMerged && !pending {
		return fmt.Errorf("a different workflow dispatch must be recovered before setup baseline rebind")
	}
	if pending && (dispatch.DispatchAcknowledgedAt.IsZero() || dispatch.MatchedAt.IsZero() ||
		!dispatch.PreparedAt.Before(source.CreatedAt) || !dispatch.DispatchAttemptedAt.Before(source.CreatedAt) ||
		!dispatch.DispatchAcknowledgedAt.Before(source.CreatedAt) || !dispatch.MatchedAt.Before(source.CreatedAt) ||
		dispatch.PreparedAt.After(dispatch.DispatchAttemptedAt) || dispatch.DispatchAttemptedAt.After(dispatch.DispatchAcknowledgedAt) ||
		dispatch.DispatchAcknowledgedAt.After(dispatch.MatchedAt)) {
		return fmt.Errorf("production workflow dispatch does not exactly predate the pending setup baseline")
	}
	owner, repo, ok := strings.Cut(source.Repository, "/")
	if !ok || owner == "" || repo == "" || client == nil || client.GoGitHub() == nil {
		return fmt.Errorf("inspect terminal production dispatch before setup baseline rebind: GitHub access is required")
	}
	if pending {
		pull, _, err := client.GoGitHub().PullRequests.Get(ctx, owner, repo, source.SetupPRNumber)
		if err != nil {
			return fmt.Errorf("inspect source setup pull request before setup baseline rebind: %w", err)
		}
		expectedHead, err = exactPendingSetupProductionHead(pull, source)
		if err != nil {
			return err
		}
	}
	run, _, err := client.GoGitHub().Actions.GetWorkflowRunByID(ctx, owner, repo, dispatch.RunID)
	if err != nil {
		return fmt.Errorf("inspect terminal production dispatch before setup baseline rebind: %w", err)
	}
	missing, bindingErr := exactWorkflowRunBindingState(run, dispatch)
	if bindingErr != nil || len(missing) > 0 || run.GetPath() != visualHiveProductionWorkflowPath ||
		!strings.EqualFold(run.GetRepository().GetFullName(), source.Repository) || run.GetRepository().GetID() <= 0 ||
		strconv.FormatInt(run.GetRepository().GetID(), 10) != strings.TrimSpace(source.RepositoryID) || !strings.EqualFold(run.GetHeadSHA(), expectedHead) {
		return fmt.Errorf("production workflow dispatch no longer matches its exact durable setup baseline binding")
	}
	if !strings.EqualFold(run.GetStatus(), "completed") || strings.TrimSpace(run.GetConclusion()) == "" {
		return fmt.Errorf("a different workflow dispatch must be recovered before setup baseline rebind")
	}
	if !containsExact(rebind.Source.RetiredCaptureCorrelations, dispatch.CorrelationID) {
		rebind.Source.RetiredCaptureCorrelations = append(rebind.Source.RetiredCaptureCorrelations, dispatch.CorrelationID)
		if len(rebind.Source.RetiredCaptureCorrelations) > 32 {
			rebind.Source.RetiredCaptureCorrelations = append([]string(nil), rebind.Source.RetiredCaptureCorrelations[len(rebind.Source.RetiredCaptureCorrelations)-32:]...)
		}
		updated, saveErr := saveSetupBaselineRebindTransition(store, *rebind, "setup_baseline_rebind_terminal_production_retired", true,
			fmt.Sprintf("correlation=%s run=%d head=%s conclusion=%s", dispatch.CorrelationID, dispatch.RunID, run.GetHeadSHA(), run.GetConclusion()))
		if saveErr != nil {
			return fmt.Errorf("persist terminal production dispatch tombstone: %w", saveErr)
		}
		*rebind = updated
	}
	if err := store.DeleteWorkflowDispatchIntent(); err != nil {
		return fmt.Errorf("retire exact terminal production workflow dispatch: %w", err)
	}
	return nil
}

func exactPreCaptureSetupBaselinePending(source SetupBaselineIntent) bool {
	return source.Phase == SetupBaselinePending && source.CaptureCorrelation == "" && source.CaptureHeadSHA == "" && source.CaptureRunID == 0 && source.CaptureRunURL == "" &&
		source.ArtifactID == 0 && source.ArtifactName == "" && source.ArtifactRoot == "" && source.CaptureCandidateDigest == "" && len(source.CaptureCandidates) == 0 &&
		source.CandidateDigest == "" && len(source.Candidates) == 0 && source.Branch == "" && source.CommitSHA == "" && source.Marker == "" && source.PRNumber == 0 &&
		source.PRURL == "" && source.BaseSHA == "" && source.DiffDigest == "" && source.ApprovalPlanDigest == "" && source.ApprovalActorID == 0 && source.ApprovalReason == "" &&
		source.MergeAttemptedAt.IsZero() && source.MergeSHA == "" && source.ProductionRunID == 0 && source.ProductionHeadSHA == "" && !source.ExistingHeadReverified &&
		source.DispatchAttemptedAt.IsZero() && source.DispatchAcknowledgedAt.IsZero() && source.PendingAudit == nil
}

func exactPendingSetupProductionHead(pull *gh.PullRequest, source SetupBaselineIntent) (string, error) {
	if pull == nil || pull.GetHead() == nil || pull.GetBase() == nil || pull.GetHead().GetRepo() == nil || pull.GetBase().GetRepo() == nil {
		return "", fmt.Errorf("source setup pull request no longer matches its exact durable pending baseline binding")
	}
	head, base := pull.GetHead(), pull.GetBase()
	headRepo, baseRepo := head.GetRepo(), base.GetRepo()
	baseSHA := strings.ToLower(strings.TrimSpace(base.GetSHA()))
	if pull.GetNumber() != source.SetupPRNumber || pull.GetHTMLURL() != source.SetupPRURL || pull.GetState() != "open" ||
		pull.Merged == nil || pull.GetMerged() || pull.Draft == nil || pull.GetDraft() || head.GetRef() != managedOperationBranch("setup", source.RepositoryID) ||
		!strings.EqualFold(head.GetSHA(), source.SetupHeadSHA) || base.GetRef() != source.DefaultBranch ||
		!strings.EqualFold(headRepo.GetFullName(), source.Repository) || !strings.EqualFold(baseRepo.GetFullName(), source.Repository) ||
		headRepo.GetID() <= 0 || baseRepo.GetID() <= 0 || strconv.FormatInt(headRepo.GetID(), 10) != strings.TrimSpace(source.RepositoryID) ||
		strconv.FormatInt(baseRepo.GetID(), 10) != strings.TrimSpace(source.RepositoryID) || !immutableCommit.MatchString(baseSHA) {
		return "", fmt.Errorf("source setup pull request no longer matches its exact durable pending baseline binding")
	}
	return baseSHA, nil
}

func cancelSetupBaselineCaptureRunExact(ctx context.Context, client *hivegithub.Client, source SetupBaselineIntent) error {
	return cancelSetupBaselineCaptureRunExactWithTiming(ctx, client, source, 250*time.Millisecond, 20*time.Second)
}

func cancelSetupBaselineCaptureRunExactWithTiming(ctx context.Context, client *hivegithub.Client, source SetupBaselineIntent, pollInterval, timeout time.Duration) error {
	return cancelSetupBaselineCaptureRunExactBoundWithTiming(ctx, client, source, nil, pollInterval, timeout)
}

func cancelSetupBaselineCaptureRunExactBound(ctx context.Context, client *hivegithub.Client, source SetupBaselineIntent, dispatch *WorkflowDispatchIntent) error {
	return cancelSetupBaselineCaptureRunExactBoundWithTiming(ctx, client, source, dispatch, 250*time.Millisecond, 20*time.Second)
}

func cancelSetupBaselineCaptureRunExactBoundWithTiming(ctx context.Context, client *hivegithub.Client, source SetupBaselineIntent, dispatch *WorkflowDispatchIntent, pollInterval, timeout time.Duration) error {
	owner, repo, ok := strings.Cut(source.Repository, "/")
	if !ok || owner == "" || repo == "" || client == nil || client.GoGitHub() == nil || pollInterval <= 0 || timeout <= 0 {
		return fmt.Errorf("exact setup baseline capture cancellation requires GitHub access")
	}
	run, response, err := client.GoGitHub().Actions.GetWorkflowRunByID(ctx, owner, repo, source.CaptureRunID)
	if err != nil {
		if response != nil && response.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("inspect exact obsolete setup baseline run %d: %w", source.CaptureRunID, err)
	}
	if !exactSetupBaselineCaptureRunForCancellation(run, source, dispatch) {
		return fmt.Errorf("obsolete setup baseline workflow run no longer matches its exact durable identity")
	}
	if strings.EqualFold(run.GetStatus(), "completed") {
		return nil
	}
	response, err = client.GoGitHub().Actions.CancelWorkflowRunByID(ctx, owner, repo, source.CaptureRunID)
	if response != nil && (response.StatusCode == http.StatusConflict || response.StatusCode == http.StatusInternalServerError) {
		retryRejectedForceCancel := response.StatusCode == http.StatusInternalServerError
		live, _, liveErr := client.GoGitHub().Actions.GetWorkflowRunByID(ctx, owner, repo, source.CaptureRunID)
		if liveErr != nil {
			return fmt.Errorf("reinspect exact obsolete setup baseline run %d after cancellation rejection: %w", source.CaptureRunID, liveErr)
		}
		if !exactSetupBaselineCaptureRunForCancellation(live, source, dispatch) {
			return fmt.Errorf("obsolete setup baseline workflow run changed identity after cancellation rejection")
		}
		if strings.EqualFold(live.GetStatus(), "completed") {
			return nil
		}
		response, err = forceCancelWorkflowRunByID(ctx, client, owner, repo, source.CaptureRunID)
		if retryRejectedForceCancel && err != nil && response != nil && response.StatusCode == http.StatusInternalServerError {
			response, err = retryForceCancelExactSetupBaselinePlaceholder(ctx, client, owner, repo, source, dispatch, response, err, pollInterval)
		}
	}
	if err != nil && (response == nil || response.StatusCode != http.StatusAccepted) {
		return fmt.Errorf("cancel exact obsolete setup baseline run %d: %w", source.CaptureRunID, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		live, _, readErr := client.GoGitHub().Actions.GetWorkflowRunByID(waitCtx, owner, repo, source.CaptureRunID)
		if readErr == nil {
			if !exactSetupBaselineCaptureRunForCancellation(live, source, dispatch) {
				return fmt.Errorf("obsolete setup baseline workflow run changed identity during cancellation")
			}
			if strings.EqualFold(live.GetStatus(), "completed") {
				return nil
			}
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("wait for exact obsolete setup baseline run %d to become terminal: %w", source.CaptureRunID, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func retryForceCancelExactSetupBaselinePlaceholder(ctx context.Context, client *hivegithub.Client, owner, repo string, source SetupBaselineIntent, dispatch *WorkflowDispatchIntent, response *gh.Response, forceErr error, retryDelay time.Duration) (*gh.Response, error) {
	lastResponse, lastErr := response, forceErr
	delay := retryDelay
	for attempt := 2; attempt <= setupBaselineForceCancelAttempts; attempt++ {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		live, _, readErr := client.GoGitHub().Actions.GetWorkflowRunByID(ctx, owner, repo, source.CaptureRunID)
		if readErr != nil {
			return nil, fmt.Errorf("reinspect exact obsolete setup baseline run %d before force-cancel retry: %w", source.CaptureRunID, readErr)
		}
		if !exactSetupBaselineCaptureRunForCancellation(live, source, dispatch) {
			return nil, fmt.Errorf("obsolete setup baseline workflow run changed identity before force-cancel retry")
		}
		if strings.EqualFold(live.GetStatus(), "completed") {
			return nil, nil
		}
		if exactSetupBaselineCaptureRun(live, source) || !strings.EqualFold(live.GetStatus(), "queued") || strings.TrimSpace(live.GetConclusion()) != "" {
			return lastResponse, fmt.Errorf("refusing to retry force-cancel because exact run %d is no longer the acknowledged queued static placeholder: %w", source.CaptureRunID, lastErr)
		}
		lastResponse, lastErr = forceCancelWorkflowRunByID(ctx, client, owner, repo, source.CaptureRunID)
		if lastResponse == nil || lastResponse.StatusCode != http.StatusInternalServerError {
			return lastResponse, lastErr
		}
		if delay < 2*time.Second {
			delay *= 2
			if delay > 2*time.Second {
				delay = 2 * time.Second
			}
		}
	}
	requestID := ""
	if lastResponse != nil {
		requestID = strings.TrimSpace(lastResponse.Header.Get("X-GitHub-Request-Id"))
	}
	return lastResponse, fmt.Errorf("GitHub force-cancel returned HTTP 500 after %d exact attempts (request_id=%q): %w", setupBaselineForceCancelAttempts, requestID, lastErr)
}

func forceCancelWorkflowRunByID(ctx context.Context, client *hivegithub.Client, owner, repo string, runID int64) (*gh.Response, error) {
	endpoint := fmt.Sprintf("repos/%s/%s/actions/runs/%d/force-cancel", owner, repo, runID)
	request, err := client.GoGitHub().NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	return client.GoGitHub().Do(ctx, request, nil)
}

func exactSetupBaselineCaptureRun(run *gh.WorkflowRun, source SetupBaselineIntent) bool {
	return run != nil && run.GetID() == source.CaptureRunID && strings.EqualFold(run.GetRepository().GetFullName(), source.Repository) &&
		strings.EqualFold(run.GetHeadSHA(), source.CaptureHeadSHA) && run.GetEvent() == "workflow_dispatch" &&
		run.GetPath() == visualHiveProductionWorkflowPath && run.GetDisplayTitle() == workflowDispatchDisplayTitle(source.CaptureCorrelation)
}

func exactSetupBaselineCaptureRunForCancellation(run *gh.WorkflowRun, source SetupBaselineIntent, dispatch *WorkflowDispatchIntent) bool {
	if exactSetupBaselineCaptureRun(run, source) {
		return true
	}
	if run == nil || dispatch == nil || validateWorkflowDispatchIntent(*dispatch) != nil || dispatch.Operation != setupBaselineWorkflowOperation ||
		dispatch.DispatchAcknowledgedAt.IsZero() || dispatch.RunID <= 0 || source.CaptureRunID != dispatch.RunID ||
		source.CaptureCorrelation != dispatch.CorrelationID || source.Repository != dispatch.Repository || source.RepositoryID != dispatch.RepositoryID ||
		source.DefaultBranch != dispatch.Ref || dispatch.WorkflowFile != "hive-visual-hive.yml" {
		return false
	}
	missing, err := exactWorkflowRunBindingState(run, *dispatch)
	if err != nil || len(missing) != 1 || missing[0] != "display_title" || run.GetDisplayTitle() != visualHiveProductionWorkflowName {
		return false
	}
	return run.GetName() == visualHiveProductionWorkflowName && run.GetWorkflowID() > 0 &&
		run.GetPath() == visualHiveProductionWorkflowPath && strings.EqualFold(run.GetHeadSHA(), source.CaptureHeadSHA) &&
		strings.EqualFold(run.GetRepository().GetFullName(), source.Repository) && run.GetRepository().GetID() > 0 &&
		strconv.FormatInt(run.GetRepository().GetID(), 10) == strings.TrimSpace(source.RepositoryID)
}

func completeSetupBaselineRebind(store *Store, config Config) error {
	rebind, exists, err := store.LoadSetupBaselineRebindIntent()
	if err != nil || !exists {
		return err
	}
	rebind, err = flushSetupBaselineRebindAudit(store, rebind)
	if err != nil {
		return fmt.Errorf("finish pending setup baseline rebind audit before completion: %w", err)
	}
	if rebind.Phase != SetupBaselineRebindResourcesRetired {
		return fmt.Errorf("setup baseline rebind external cleanup is not complete; retry %s", SetupBaselineRebindNextCommand(rebind))
	}
	digest, err := setupBaselineRebindTargetDigest(config)
	if err != nil || !strings.EqualFold(digest, rebind.TargetConfigDigest) {
		return fmt.Errorf("saved setup does not match the durable baseline rebind target")
	}
	if config.SetupBaselineRequired {
		baseline, baselineExists, loadErr := store.LoadSetupBaselineIntent()
		if loadErr != nil || !baselineExists {
			return fmt.Errorf("new setup baseline intent was not durably created before rebind completion: %w", loadErr)
		}
		if baseline.Repository != config.Repository || baseline.RepositoryID != config.RepositoryID || baseline.SetupPRNumber != config.SetupPRNumber ||
			!strings.EqualFold(baseline.SetupHeadSHA, config.SetupHeadSHA) || !strings.EqualFold(baseline.VisualHiveConfigDigest, config.VisualHiveConfigDigest) ||
			!strings.EqualFold(baseline.ScreenshotContractDigest, config.SetupBaselineContractDigest) {
			return fmt.Errorf("new setup baseline intent does not match the saved reconfiguration")
		}
		for _, correlation := range rebind.Source.RetiredCaptureCorrelations {
			if !containsExact(baseline.RetiredCaptureCorrelations, correlation) {
				baseline.RetiredCaptureCorrelations = append(baseline.RetiredCaptureCorrelations, correlation)
			}
		}
		if len(baseline.RetiredCaptureCorrelations) > 32 {
			baseline.RetiredCaptureCorrelations = append([]string(nil), baseline.RetiredCaptureCorrelations[len(baseline.RetiredCaptureCorrelations)-32:]...)
		}
		if err := store.SaveSetupBaselineIntent(baseline); err != nil {
			return fmt.Errorf("carry obsolete setup baseline dispatch tombstones into replacement intent: %w", err)
		}
	}
	rebind, err = saveSetupBaselineRebindTransition(store, rebind, "setup_baseline_rebind_completed", true,
		fmt.Sprintf("target=%s setup_pr=%d setup_head=%s baseline_required=%t", digest, config.SetupPRNumber, config.SetupHeadSHA, config.SetupBaselineRequired))
	if err != nil {
		return err
	}
	return store.DeleteSetupBaselineRebindIntent()
}

func SetupBaselineRebindNextCommand(intent SetupBaselineRebindIntent) string {
	config := intent.TargetConfig
	parts := []string{"hive setup", "--repo " + strconv.Quote(config.Repository), "--coverage " + strconv.Quote(string(config.Coverage)),
		"--automation " + strconv.Quote(string(config.Automation)), "--provider " + strconv.Quote(config.Provider),
		"--state-dir " + strconv.Quote(config.StateDir), "--visual-hive=" + strconv.FormatBool(config.VisualHive),
		"--max-active-issues " + strconv.Itoa(config.MaxActiveIssues), "--max-repair-attempts " + strconv.Itoa(config.MaxRepairAttempts), "--json"}
	if config.ProviderCommand != "" {
		parts = append(parts, "--provider-command "+strconv.Quote(config.ProviderCommand))
	}
	for _, arg := range config.ProviderArgs {
		parts = append(parts, "--provider-arg "+strconv.Quote(arg))
	}
	if config.VisualHive {
		parts = append(parts, "--visual-hive-command "+strconv.Quote(config.VisualHiveCommand), "--visual-hive-repo "+strconv.Quote(config.VisualHiveRepo), "--visual-hive-ref "+strconv.Quote(config.VisualHiveRef))
		for _, arg := range config.VisualHiveArgs {
			parts = append(parts, "--visual-hive-arg "+strconv.Quote(arg))
		}
	}
	for _, path := range config.AllowedAutoMergePaths {
		parts = append(parts, "--auto-merge-path "+strconv.Quote(path))
	}
	for _, risk := range config.AllowedAutoMergeRisk {
		parts = append(parts, "--auto-merge-risk "+strconv.Quote(setupBaselineRebindRiskName(risk)))
	}
	return strings.Join(parts, " ")
}

func setupBaselineRebindRiskName(risk automation.RiskTier) string {
	switch risk {
	case automation.RiskAutomatic:
		return "automatic"
	case automation.RiskLow:
		return "low"
	case automation.RiskMedium:
		return "medium"
	case automation.RiskRestricted:
		return "restricted"
	default:
		return strconv.Itoa(int(risk))
	}
}

func equalSetupBaselineCandidates(left, right []SetupBaselineCandidate) bool {
	leftDigest, leftErr := setupBaselineCandidateDigest(left)
	rightDigest, rightErr := setupBaselineCandidateDigest(right)
	return leftErr == nil && rightErr == nil && strings.EqualFold(leftDigest, rightDigest)
}

package integrated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

const (
	ProtectionActivationSchema        = "hive.protection-activation.v1"
	protectionActivationFile          = "protection-activation.json"
	visualHivePRCheckContext          = "visual-hive"
	visualHiveProductionCheckContext  = "visual-hive-production"
	visualHiveProductionWorkflowName  = "Hive Visual Hive Production"
	visualHiveProductionWorkflowPath  = ".github/workflows/hive-visual-hive.yml"
	visualHiveProductionWorkflowEvent = "workflow_dispatch"
	githubActionsAppSlug              = "github-actions"
)

var ErrProtectionActivationRunStale = errors.New("protection activation workflow run is stale")

type ProtectionActivationState struct {
	SchemaVersion     string    `json:"schema_version"`
	Repository        string    `json:"repository"`
	RepositoryID      string    `json:"repository_id"`
	DefaultBranch     string    `json:"default_branch"`
	BindingDigest     string    `json:"binding_digest"`
	CheckContext      string    `json:"check_context"`
	RequiredContext   string    `json:"required_context,omitempty"`
	CheckAppID        int64     `json:"check_app_id"`
	WorkflowRunID     int64     `json:"workflow_run_id"`
	WorkflowName      string    `json:"workflow_name,omitempty"`
	WorkflowPath      string    `json:"workflow_path,omitempty"`
	WorkflowEvent     string    `json:"workflow_event,omitempty"`
	WorkflowHeadSHA   string    `json:"workflow_head_sha"`
	CheckRunID        int64     `json:"check_run_id"`
	SeedCheckRunID    int64     `json:"seed_check_run_id,omitempty"`
	ProtectionCreated bool      `json:"protection_created"`
	ActivatedAt       time.Time `json:"activated_at"`
}

type ProtectionActivationResult struct {
	Required          bool                       `json:"required"`
	Activated         bool                       `json:"activated"`
	Idempotent        bool                       `json:"idempotent"`
	ProtectionCreated bool                       `json:"protection_created"`
	State             *ProtectionActivationState `json:"state,omitempty"`
}

func (s *Store) SaveProtectionActivation(state ProtectionActivationState) error {
	state.SchemaVersion = ProtectionActivationSchema
	if err := validateProtectionActivation(state); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeDurableStateFile(filepath.Join(s.dir, protectionActivationFile), append(data, '\n'))
}

func (s *Store) LoadProtectionActivation() (ProtectionActivationState, bool, error) {
	return loadProtectionActivationFile(filepath.Join(s.dir, protectionActivationFile))
}

// ReadProtectionActivation is a read-only status path used by hive doctor.
// It never creates the state directory or repairs missing state.
func ReadProtectionActivation(stateDir string) (ProtectionActivationState, bool, error) {
	if strings.TrimSpace(stateDir) == "" {
		return ProtectionActivationState{}, false, fmt.Errorf("persistent state directory is missing")
	}
	return loadProtectionActivationFile(filepath.Join(stateDir, "integrated", protectionActivationFile))
}

func loadProtectionActivationFile(path string) (ProtectionActivationState, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ProtectionActivationState{}, false, nil
	}
	if err != nil {
		return ProtectionActivationState{}, false, err
	}
	var state ProtectionActivationState
	if err := json.Unmarshal(data, &state); err != nil {
		return ProtectionActivationState{}, false, fmt.Errorf("decode protection activation: %w", err)
	}
	if err := validateProtectionActivation(state); err != nil {
		return ProtectionActivationState{}, false, err
	}
	return state, true, nil
}

func validateProtectionActivation(state ProtectionActivationState) error {
	legacyBinding := state.CheckContext == visualHivePRCheckContext && state.RequiredContext == "" && state.WorkflowName == "" && state.WorkflowPath == "" && state.WorkflowEvent == ""
	productionBinding := state.CheckContext == visualHiveProductionCheckContext && state.RequiredContext == visualHivePRCheckContext &&
		state.WorkflowName == visualHiveProductionWorkflowName && state.WorkflowPath == visualHiveProductionWorkflowPath && state.WorkflowEvent == visualHiveProductionWorkflowEvent
	if state.SchemaVersion != ProtectionActivationSchema || strings.TrimSpace(state.Repository) == "" || !validActivationRepositoryID(state.RepositoryID) ||
		strings.TrimSpace(state.DefaultBranch) == "" || len(state.BindingDigest) != sha256.Size*2 || (!legacyBinding && !productionBinding) ||
		state.CheckAppID <= 0 || state.WorkflowRunID <= 0 || !immutableCommit.MatchString(strings.ToLower(state.WorkflowHeadSHA)) ||
		state.CheckRunID <= 0 || (productionBinding && state.SeedCheckRunID <= 0) || state.ActivatedAt.IsZero() {
		return fmt.Errorf("protection activation state is incomplete or invalid")
	}
	return nil
}

func validActivationRepositoryID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value[0] < '1' || value[0] > '9' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func activationBindingDigest(config Config) (string, error) {
	binding := struct {
		Repository             string     `json:"repository"`
		RepositoryID           string     `json:"repository_id"`
		DefaultBranch          string     `json:"default_branch"`
		Coverage               Coverage   `json:"coverage"`
		Automation             Automation `json:"automation"`
		VisualHiveRepo         string     `json:"visual_hive_repository"`
		VisualHiveRef          string     `json:"visual_hive_ref"`
		VisualHiveConfigDigest string     `json:"visual_hive_config_digest"`
	}{
		Repository: config.Repository, RepositoryID: config.RepositoryID, DefaultBranch: config.DefaultBranch,
		Coverage: config.Coverage, Automation: config.Automation, VisualHiveRepo: config.VisualHiveRepo,
		VisualHiveRef: config.VisualHiveRef, VisualHiveConfigDigest: config.VisualHiveConfigDigest,
	}
	data, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func ProtectionActivationMatchesConfig(state ProtectionActivationState, config Config) bool {
	digest, err := activationBindingDigest(config)
	return err == nil && state.SchemaVersion == ProtectionActivationSchema && strings.EqualFold(state.Repository, config.Repository) &&
		state.RepositoryID == strings.TrimSpace(config.RepositoryID) && state.DefaultBranch == strings.TrimSpace(config.DefaultBranch) &&
		state.BindingDigest == digest && state.CheckContext == visualHiveProductionCheckContext && state.RequiredContext == visualHivePRCheckContext &&
		state.WorkflowName == visualHiveProductionWorkflowName && state.WorkflowPath == visualHiveProductionWorkflowPath &&
		state.WorkflowEvent == visualHiveProductionWorkflowEvent && state.CheckAppID > 0
}

// ActivateAutoMergeProtection is the second phase of setup. The managed files
// must already be installed on the default branch, and workflow must be the
// exact completed dispatch retained in Hive's durable dispatch intent. The
// caller must not apply evidence or perform any lifecycle mutation first.
func ActivateAutoMergeProtection(ctx context.Context, store *Store, client *hivegithub.Client, config Config, workflow WorkflowRunEvidence) (ProtectionActivationResult, error) {
	result := ProtectionActivationResult{Required: config.Automation == AutomationAutoMerge}
	if !result.Required {
		result.Idempotent = true
		return result, nil
	}
	if store == nil || client == nil || client.GoGitHub() == nil {
		return result, fmt.Errorf("activation requires the integrated state store and GitHub client")
	}
	if err := VerifyInstalledSetup(ctx, client, config); err != nil {
		return result, fmt.Errorf("%w: exact managed setup is not installed on the default branch: %v", ErrProtectionActivationRunStale, err)
	}
	intent, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil {
		return result, err
	}
	if !exists || validateWorkflowDispatchBinding(intent, config, "hive-visual-hive.yml", config.DefaultBranch) != nil ||
		intent.RunID != workflow.RunID || intent.CorrelationID != workflow.CorrelationID {
		return result, fmt.Errorf("protection activation requires the exact durable workflow dispatch binding")
	}
	appID, err := client.ExpectedGitHubAppID(ctx, githubActionsAppSlug)
	if err != nil {
		return result, fmt.Errorf("resolve expected Visual Hive check producer: %w", err)
	}
	checks, err := verifyActivationWorkflowCheck(ctx, client, config, workflow, intent, appID)
	if err != nil {
		return result, err
	}
	bindingDigest, err := activationBindingDigest(config)
	if err != nil {
		return result, err
	}
	existing, exists, err := store.LoadProtectionActivation()
	if err != nil {
		return result, err
	}
	protection, err := client.BranchProtection(ctx, config.Repository, config.DefaultBranch)
	if err != nil {
		return result, fmt.Errorf("inspect branch protection before activation: %w", err)
	}
	if !protection.Enabled {
		detail := fmt.Sprintf("branch=%s run=%d head=%s production_check_run=%d seed_check_run=%d app_id=%d", config.DefaultBranch, workflow.RunID, workflow.HeadSHA, checks.ProductionCheckRunID, checks.SeedCheckRunID, appID)
		if err := store.AuditStrict(AuditEntry{Action: "authorize_protection_activation", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
			return result, err
		}
	}
	created, activationErr := client.EnsureMinimumBranchProtection(ctx, config.Repository, config.DefaultBranch, []hivegithub.RequiredCheckIdentity{{Context: visualHivePRCheckContext, AppID: appID}})
	if activationErr != nil {
		detail := fmt.Sprintf("branch=%s app_id=%d error=%s", config.DefaultBranch, appID, activationErr)
		if err := store.AuditStrict(AuditEntry{Action: "activate_branch_protection", Allowed: false, Repository: config.Repository, Detail: detail}); err != nil {
			return result, err
		}
		return result, fmt.Errorf("auto-merge protection activation refused the policy on %s: %w. Hive did not replace repository-owned policy; require strict up-to-date checks, enforce administrators, and bind the PR-only %q context to GitHub Actions App ID %d, then rerun hive run", config.DefaultBranch, activationErr, visualHivePRCheckContext, appID)
	}
	if exists && ProtectionActivationMatchesConfig(existing, config) && existing.CheckAppID == appID && !created {
		result.Activated, result.Idempotent, result.State = true, true, &existing
		return result, nil
	}
	state := ProtectionActivationState{
		SchemaVersion: ProtectionActivationSchema, Repository: config.Repository, RepositoryID: config.RepositoryID,
		DefaultBranch: config.DefaultBranch, BindingDigest: bindingDigest, CheckContext: visualHiveProductionCheckContext, RequiredContext: visualHivePRCheckContext, CheckAppID: appID,
		WorkflowName: visualHiveProductionWorkflowName, WorkflowPath: visualHiveProductionWorkflowPath, WorkflowEvent: visualHiveProductionWorkflowEvent,
		WorkflowRunID: workflow.RunID, WorkflowHeadSHA: strings.ToLower(workflow.HeadSHA), CheckRunID: checks.ProductionCheckRunID, SeedCheckRunID: checks.SeedCheckRunID,
		ProtectionCreated: created, ActivatedAt: time.Now().UTC(),
	}
	detail := fmt.Sprintf("branch=%s run=%d head=%s production_check_run=%d seed_check_run=%d app_id=%d created=%t", state.DefaultBranch, state.WorkflowRunID, state.WorkflowHeadSHA, state.CheckRunID, state.SeedCheckRunID, state.CheckAppID, created)
	if err := store.AuditStrict(AuditEntry{Action: "activate_branch_protection", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
		return result, err
	}
	if err := store.SaveProtectionActivation(state); err != nil {
		return result, err
	}
	result.Activated, result.ProtectionCreated, result.State = true, created, &state
	return result, nil
}

type activationWorkflowChecks struct {
	ProductionCheckRunID int64
	SeedCheckRunID       int64
}

func verifyActivationWorkflowCheck(ctx context.Context, client *hivegithub.Client, config Config, workflow WorkflowRunEvidence, intent WorkflowDispatchIntent, expectedAppID int64) (activationWorkflowChecks, error) {
	result := activationWorkflowChecks{}
	owner, repo, ok := strings.Cut(strings.TrimSpace(config.Repository), "/")
	if !ok || owner == "" || repo == "" || workflow.RunID <= 0 || (workflow.Conclusion != "success" && workflow.Conclusion != "failure") ||
		!immutableCommit.MatchString(strings.ToLower(strings.TrimSpace(workflow.HeadSHA))) || expectedAppID <= 0 {
		return result, fmt.Errorf("protection activation requires exact completed managed workflow evidence")
	}
	_, _, repositoryTestDigest, repositoryTestErr := visualhive.EncodeRepositoryTestEvidence(workflow.RepositoryTests, workflow.RepositoryTestOverall)
	if repositoryTestErr != nil || !workflowDispatchCorrelationPattern.MatchString(workflow.RepositoryTestDigest) || repositoryTestDigest != workflow.RepositoryTestDigest ||
		((workflow.RepositoryTestOverall == 0) != (workflow.Conclusion == "success")) {
		return result, fmt.Errorf("protection activation requires runner-controlled repository-test evidence consistent with the workflow conclusion")
	}
	branch, _, err := client.GoGitHub().Repositories.GetBranch(ctx, owner, repo, config.DefaultBranch, 0)
	if err != nil {
		return result, fmt.Errorf("read live default branch before protection activation: %w", err)
	}
	if !strings.EqualFold(branch.GetCommit().GetSHA(), workflow.HeadSHA) {
		return result, fmt.Errorf("%w: workflow head %s is not current default-branch head %s", ErrProtectionActivationRunStale, workflow.HeadSHA, branch.GetCommit().GetSHA())
	}
	run, _, err := client.GoGitHub().Actions.GetWorkflowRunByID(ctx, owner, repo, workflow.RunID)
	if err != nil {
		return result, fmt.Errorf("read exact activation workflow run: %w", err)
	}
	expectedDisplayTitle := workflowDispatchDisplayTitle(intent.CorrelationID)
	if run.GetID() != workflow.RunID || run.GetName() != visualHiveProductionWorkflowName || exactActivationWorkflowPath(run.GetPath()) != visualHiveProductionWorkflowPath ||
		run.GetEvent() != visualHiveProductionWorkflowEvent || run.GetHeadBranch() != config.DefaultBranch || !strings.EqualFold(run.GetHeadSHA(), workflow.HeadSHA) ||
		run.GetStatus() != "completed" || (run.GetConclusion() != "success" && run.GetConclusion() != "failure") || run.GetDisplayTitle() != expectedDisplayTitle ||
		(run.GetRepository().GetFullName() != "" && !strings.EqualFold(run.GetRepository().GetFullName(), config.Repository)) {
		return result, fmt.Errorf("exact activation run is not the completed managed production workflow path/name/event/ref/head")
	}
	if run.GetWorkflowID() <= 0 {
		return result, fmt.Errorf("exact activation run did not identify its managed workflow definition")
	}
	definition, _, err := client.GoGitHub().Actions.GetWorkflowByID(ctx, owner, repo, run.GetWorkflowID())
	if err != nil {
		return result, fmt.Errorf("read exact activation workflow definition: %w", err)
	}
	if definition.GetID() != run.GetWorkflowID() || definition.GetName() != visualHiveProductionWorkflowName ||
		exactActivationWorkflowPath(definition.GetPath()) != visualHiveProductionWorkflowPath || definition.GetState() != "active" {
		return result, fmt.Errorf("exact activation workflow definition is not the active managed production workflow path/name")
	}
	var productionJob, seedJob *gh.WorkflowJob
	options := &gh.ListWorkflowJobsOptions{Filter: "latest", ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		jobs, response, err := client.GoGitHub().Actions.ListWorkflowJobs(ctx, owner, repo, workflow.RunID, options)
		if err != nil {
			return result, fmt.Errorf("list exact activation workflow jobs: %w", err)
		}
		for _, job := range jobs.Jobs {
			switch job.GetName() {
			case visualHiveProductionCheckContext:
				if productionJob != nil && productionJob.GetID() != job.GetID() {
					return result, fmt.Errorf("exact activation workflow emitted multiple %q jobs", visualHiveProductionCheckContext)
				}
				productionJob = job
			case visualHivePRCheckContext:
				if seedJob != nil && seedJob.GetID() != job.GetID() {
					return result, fmt.Errorf("exact activation workflow emitted multiple %q seed jobs", visualHivePRCheckContext)
				}
				seedJob = job
			}
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	if !validActivationWorkflowJob(productionJob, workflow, config.DefaultBranch, visualHiveProductionCheckContext) {
		return result, fmt.Errorf("exact activation workflow did not emit one successful default-branch %q job", visualHiveProductionCheckContext)
	}
	if !validActivationWorkflowJob(seedJob, workflow, config.DefaultBranch, visualHivePRCheckContext) {
		return result, fmt.Errorf("exact activation workflow did not emit one successful actively guarded %q seed job", visualHivePRCheckContext)
	}
	result.ProductionCheckRunID, err = checkRunIDFromURL(productionJob.GetCheckRunURL())
	if err != nil {
		return result, err
	}
	result.SeedCheckRunID, err = checkRunIDFromURL(seedJob.GetCheckRunURL())
	if err != nil {
		return result, err
	}
	productionCheck, _, err := client.GoGitHub().Checks.GetCheckRun(ctx, owner, repo, result.ProductionCheckRunID)
	if err != nil {
		return result, fmt.Errorf("read exact activation production Check Run: %w", err)
	}
	seedCheck, _, err := client.GoGitHub().Checks.GetCheckRun(ctx, owner, repo, result.SeedCheckRunID)
	if err != nil {
		return result, fmt.Errorf("read exact activation seed Check Run: %w", err)
	}
	if !validActivationCheckRun(productionCheck, result.ProductionCheckRunID, visualHiveProductionCheckContext, workflow.HeadSHA, expectedAppID) {
		return result, fmt.Errorf("exact activation Check Run is not a successful %q result from GitHub Actions App ID %d", visualHiveProductionCheckContext, expectedAppID)
	}
	if !validActivationCheckRun(seedCheck, result.SeedCheckRunID, visualHivePRCheckContext, workflow.HeadSHA, expectedAppID) {
		return result, fmt.Errorf("exact activation seed Check Run is not a successful actively guarded %q result from GitHub Actions App ID %d", visualHivePRCheckContext, expectedAppID)
	}
	return result, nil
}

func validActivationWorkflowJob(job *gh.WorkflowJob, workflow WorkflowRunEvidence, branch, context string) bool {
	return job != nil && job.GetName() == context && job.GetRunID() == workflow.RunID && strings.EqualFold(job.GetHeadSHA(), workflow.HeadSHA) &&
		job.GetHeadBranch() == branch && job.GetStatus() == "completed" && job.GetConclusion() == "success" &&
		job.GetWorkflowName() == visualHiveProductionWorkflowName
}

func validActivationCheckRun(check *gh.CheckRun, id int64, context, headSHA string, expectedAppID int64) bool {
	return check != nil && check.GetID() == id && check.GetName() == context && strings.EqualFold(check.GetHeadSHA(), headSHA) &&
		check.GetStatus() == "completed" && check.GetConclusion() == "success" && check.GetApp().GetID() == expectedAppID
}

func exactActivationWorkflowPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if at := strings.IndexByte(value, '@'); at >= 0 {
		value = value[:at]
	}
	return strings.TrimPrefix(value, "/")
}

func checkRunIDFromURL(raw string) (int64, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return 0, fmt.Errorf("activation workflow job has an invalid Check Run URL")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != "check-runs" {
		return 0, fmt.Errorf("activation workflow job has an unrecognized Check Run URL")
	}
	id, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("activation workflow job has an invalid Check Run ID")
	}
	return id, nil
}

// Package hostedstatus performs a read-only, fail-closed inspection of a
// repository's hosted Hive controller. It never executes repository code and
// never mutates GitHub state.
package hostedstatus

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/kubestellar/hive/v2/pkg/integrated"
)

const (
	SchemaVersion             = "hive.hosted-status.v1"
	stateSecretName           = "HIVE_HOSTED_STATE_KEY"
	completedRunLimit         = 25
	activeRunLimit            = 2
	minimumFreshnessWindow    = 30 * time.Minute
	allowedFutureClockSkew    = 5 * time.Minute
	CheckManagedWorkflow      = "managed_workflow"
	CheckStateBranch          = "state_branch"
	CheckStateSecret          = "state_secret"
	CheckControllerIdentities = "controller_run_identities"
	CheckControllerOverlap    = "controller_run_overlap"
	CheckLatestCycleSuccess   = "latest_cycle_success"
	CheckLatestCycleFresh     = "latest_cycle_fresh"
	CheckLatestCycleHead      = "latest_cycle_default_head"
	CheckRemotePauseState     = "remote_pause_state"
	maxControllerArtifact     = int64(2 << 20)
	maxControllerReceipt      = int64(1 << 20)
)

var (
	exactSHA            = regexp.MustCompile(`^[a-f0-9]{40}$`)
	hostedDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
	controllerRequestID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// Check is one independently reviewable production-readiness assertion.
type Check struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Details string `json:"details"`
}

// RunEvidence is the immutable identity and timing evidence retained for a
// controller run. It deliberately excludes logs and target-repository data.
type RunEvidence struct {
	ID                   int64     `json:"id"`
	URL                  string    `json:"url,omitempty"`
	Event                string    `json:"event"`
	Status               string    `json:"status"`
	Conclusion           string    `json:"conclusion,omitempty"`
	Path                 string    `json:"path"`
	HeadBranch           string    `json:"head_branch"`
	HeadSHA              string    `json:"head_sha"`
	RunNumber            int       `json:"run_number"`
	RunAttempt           int       `json:"run_attempt"`
	DisplayTitle         string    `json:"display_title,omitempty"`
	ActorID              int64     `json:"actor_id,omitempty"`
	ActorLogin           string    `json:"actor_login,omitempty"`
	ActorType            string    `json:"actor_type,omitempty"`
	TriggeringActorID    int64     `json:"triggering_actor_id,omitempty"`
	TriggeringActorLogin string    `json:"triggering_actor_login,omitempty"`
	TriggeringActorType  string    `json:"triggering_actor_type,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	RunStartedAt         time.Time `json:"run_started_at,omitempty"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// StateReceiptEvidence is a bounded, runner-produced receipt from the latest
// completed controller run. GitHub binds the artifact to the exact managed
// workflow run; the receipt binds the restored signed state to the operation,
// state-branch commit, and pause state observed by that runner.
type StateReceiptEvidence struct {
	Run                    RunEvidence                        `json:"run"`
	Job                    string                             `json:"job"`
	ArtifactID             int64                              `json:"artifact_id"`
	ArtifactName           string                             `json:"artifact_name"`
	Operation              string                             `json:"operation"`
	RequestID              string                             `json:"request_id"`
	RequestSHA256          string                             `json:"request_sha256,omitempty"`
	StateBefore            string                             `json:"state_before"`
	StateAfter             string                             `json:"state_after"`
	Sequence               uint64                             `json:"sequence"`
	Paused                 bool                               `json:"paused"`
	Release                integrated.HostedReleaseIdentity   `json:"release"`
	Actor                  *integrated.HostedOperatorIdentity `json:"actor,omitempty"`
	RunResult              json.RawMessage                    `json:"run_result,omitempty"`
	DoctorResult           json.RawMessage                    `json:"doctor_result,omitempty"`
	StatusResult           json.RawMessage                    `json:"status_result,omitempty"`
	OperatorResult         json.RawMessage                    `json:"operator_result,omitempty"`
	OperatorStatus         string                             `json:"operator_status,omitempty"`
	OperatorTerminalSHA256 string                             `json:"operator_terminal_sha256,omitempty"`
	OperatorLedgerSequence uint64                             `json:"operator_ledger_sequence,omitempty"`
	OperatorReplayed       bool                               `json:"operator_replayed,omitempty"`
}

// Result is suitable for direct inclusion in Hive status and doctor output.
type Result struct {
	SchemaVersion          string                `json:"schema_version"`
	Repository             string                `json:"repository"`
	CheckedAt              time.Time             `json:"checked_at"`
	ProductionReady        bool                  `json:"production_ready"`
	Checks                 []Check               `json:"checks"`
	ManagedWorkflowSHA256  string                `json:"managed_workflow_sha256,omitempty"`
	DefaultHeadSHA         string                `json:"default_head_sha,omitempty"`
	StateBranch            string                `json:"state_branch"`
	StateBranchHeadSHA     string                `json:"state_branch_head_sha,omitempty"`
	StateSecretUpdatedAt   time.Time             `json:"state_secret_updated_at,omitempty"`
	ActiveRuns             []RunEvidence         `json:"active_runs,omitempty"`
	LatestCompletedCycle   *RunEvidence          `json:"latest_completed_cycle,omitempty"`
	LatestCycleAgeSeconds  int64                 `json:"latest_cycle_age_seconds,omitempty"`
	FreshnessWindowSeconds int64                 `json:"freshness_window_seconds"`
	Paused                 bool                  `json:"paused"`
	LatestStateReceipt     *StateReceiptEvidence `json:"latest_state_receipt,omitempty"`
}

// Inspect verifies the exact generated workflow, dedicated state branch,
// state-secret metadata, controller run identities, overlap, and the latest
// completed cycle. The caller supplies now so status evidence is deterministic
// and straightforward to test.
func Inspect(ctx context.Context, client *gh.Client, config integrated.Config, now time.Time) (Result, error) {
	result := Result{
		SchemaVersion: SchemaVersion,
		Repository:    config.Repository,
		CheckedAt:     now.UTC(),
		StateBranch:   config.HostedStateBranch,
		// Fail closed until an exact current runner receipt proves otherwise.
		Paused: true,
	}
	if client == nil {
		return result, errors.New("hosted status requires a GitHub client")
	}
	if now.IsZero() {
		return result, errors.New("hosted status inspection time must be non-zero")
	}
	if config.ExecutionMode != integrated.ExecutionHosted {
		return result, fmt.Errorf("hosted status requires execution mode %q", integrated.ExecutionHosted)
	}
	owner, repository, err := splitRepository(config.Repository)
	if err != nil {
		return result, err
	}
	window, err := FreshnessWindow(config.RunIntervalSeconds)
	if err != nil {
		return result, err
	}
	result.FreshnessWindowSeconds = int64(window / time.Second)

	if config.HostedControllerProtocol != integrated.HostedControllerProtocol || !hostedDigestPattern.MatchString(config.HostedWorkflowSHA256) {
		return result, errors.New("hosted status requires a supported controller protocol and exact managed workflow digest")
	}
	result.ManagedWorkflowSHA256 = config.HostedWorkflowSHA256

	workflowCheck, err := inspectWorkflow(ctx, client, owner, repository, config.DefaultBranch, config.HostedWorkflowSHA256)
	if err != nil {
		return result, err
	}
	result.Checks = append(result.Checks, workflowCheck)
	defaultHead, err := inspectDefaultBranchHead(ctx, client, owner, repository, config.DefaultBranch)
	if err != nil {
		return result, err
	}
	result.DefaultHeadSHA = defaultHead

	stateCheck, stateHead, err := inspectStateBranch(ctx, client, owner, repository, config.HostedStateBranch)
	if err != nil {
		return result, err
	}
	result.StateBranchHeadSHA = stateHead
	result.Checks = append(result.Checks, stateCheck)

	secretCheck, secretUpdatedAt, err := inspectStateSecret(ctx, client, owner, repository)
	if err != nil {
		return result, err
	}
	result.StateSecretUpdatedAt = secretUpdatedAt
	result.Checks = append(result.Checks, secretCheck)

	runs, err := inspectRuns(ctx, client, owner, repository, config, now.UTC(), window)
	if err != nil {
		return result, err
	}
	result.ActiveRuns = runs.active
	result.LatestCompletedCycle = runs.latestCycle
	result.LatestCycleAgeSeconds = runs.latestAgeSeconds
	result.Checks = append(result.Checks, runs.checks...)
	if runs.latestCycle != nil && runs.latestCycle.HeadSHA == defaultHead {
		result.Checks = append(result.Checks, Check{Name: CheckLatestCycleHead, Passed: true, Details: "latest completed cycle is bound to the exact current default-branch head"})
	} else {
		result.Checks = append(result.Checks, Check{Name: CheckLatestCycleHead, Details: "latest completed cycle is not bound to the exact current default-branch head"})
	}

	receipt, receiptCheck, err := inspectLatestStateReceipt(ctx, client, owner, repository, config, defaultHead, stateHead, runs)
	if err != nil {
		return result, err
	}
	result.Checks = append(result.Checks, receiptCheck)
	if receipt != nil {
		result.LatestStateReceipt = receipt
		result.Paused = receipt.Paused
	}

	result.ProductionReady = true
	for _, check := range result.Checks {
		if !check.Passed {
			result.ProductionReady = false
			break
		}
	}
	return result, nil
}

func inspectDefaultBranchHead(ctx context.Context, client *gh.Client, owner, repository, branch string) (string, error) {
	value, _, err := client.Repositories.GetBranch(ctx, owner, repository, branch, 0)
	if err != nil {
		return "", fmt.Errorf("read hosted default-branch head: %w", err)
	}
	if value == nil || value.GetCommit() == nil {
		return "", errors.New("hosted default branch returned a malformed commit identity")
	}
	sha := strings.ToLower(value.GetCommit().GetSHA())
	if !exactSHA.MatchString(sha) {
		return "", errors.New("hosted default branch returned a malformed commit identity")
	}
	return sha, nil
}

// FreshnessWindow derives the maximum acceptable cycle age. Three cadence
// periods tolerate ordinary GitHub scheduling delay; a 30-minute floor avoids
// making short cadences spuriously unhealthy.
func FreshnessWindow(runIntervalSeconds int64) (time.Duration, error) {
	if runIntervalSeconds <= 0 {
		return 0, errors.New("hosted run interval must be positive")
	}
	if runIntervalSeconds > int64((time.Duration(1<<63-1)/time.Second)/3) {
		return 0, errors.New("hosted run interval is too large")
	}
	interval := time.Duration(runIntervalSeconds) * time.Second
	window := 3 * interval
	if window < interval || window < minimumFreshnessWindow {
		window = minimumFreshnessWindow
	}
	return window, nil
}

func inspectWorkflow(ctx context.Context, client *gh.Client, owner, repository, branch, expectedSHA256 string) (Check, error) {
	content, directory, response, err := client.Repositories.GetContents(ctx, owner, repository, integrated.HostedControllerWorkflowPath, &gh.RepositoryContentGetOptions{Ref: branch})
	if err != nil {
		if isNotFound(response, err) {
			return Check{Name: CheckManagedWorkflow, Details: "managed hosted controller workflow is missing from the default branch"}, nil
		}
		return Check{}, fmt.Errorf("read hosted controller workflow: %w", err)
	}
	if content == nil || directory != nil || content.GetType() != "file" || content.GetPath() != integrated.HostedControllerWorkflowPath {
		return Check{Name: CheckManagedWorkflow, Details: "managed hosted controller path did not resolve to the exact ordinary workflow file"}, nil
	}
	actual, err := content.GetContent()
	if err != nil {
		return Check{Name: CheckManagedWorkflow, Details: "managed hosted controller content could not be decoded"}, nil
	}
	digest := sha256.Sum256([]byte(actual))
	if hex.EncodeToString(digest[:]) != expectedSHA256 {
		return Check{Name: CheckManagedWorkflow, Details: "managed hosted controller bytes differ from the durable content-bound workflow identity"}, nil
	}
	return Check{Name: CheckManagedWorkflow, Passed: true, Details: "managed hosted controller bytes exactly match the durable content-bound workflow identity"}, nil
}

func inspectStateBranch(ctx context.Context, client *gh.Client, owner, repository, branch string) (Check, string, error) {
	ref, response, err := client.Git.GetRef(ctx, owner, repository, "refs/heads/"+branch)
	if err != nil {
		if isNotFound(response, err) {
			return Check{Name: CheckStateBranch, Details: "dedicated hosted state branch is missing"}, "", nil
		}
		return Check{}, "", fmt.Errorf("read hosted state branch: %w", err)
	}
	wantRef := "refs/heads/" + branch
	if ref == nil || ref.GetRef() != wantRef || ref.GetObject().GetType() != "commit" || !exactSHA.MatchString(ref.GetObject().GetSHA()) {
		return Check{Name: CheckStateBranch, Details: "dedicated hosted state branch returned a malformed ref identity"}, "", nil
	}
	return Check{Name: CheckStateBranch, Passed: true, Details: "dedicated hosted state branch exists with an exact commit identity"}, ref.GetObject().GetSHA(), nil
}

func inspectStateSecret(ctx context.Context, client *gh.Client, owner, repository string) (Check, time.Time, error) {
	secret, response, err := client.Actions.GetRepoSecret(ctx, owner, repository, stateSecretName)
	if err != nil {
		if isNotFound(response, err) {
			return Check{Name: CheckStateSecret, Details: "hosted state-key secret metadata is missing"}, time.Time{}, nil
		}
		return Check{}, time.Time{}, fmt.Errorf("read hosted state-key secret metadata: %w", err)
	}
	if secret == nil || secret.Name != stateSecretName {
		return Check{Name: CheckStateSecret, Details: "hosted state-key secret metadata returned an unexpected identity"}, time.Time{}, nil
	}
	return Check{Name: CheckStateSecret, Passed: true, Details: "hosted state-key secret metadata exists (secret value was not read)"}, secret.UpdatedAt.Time, nil
}

type runInspection struct {
	active           []RunEvidence
	completed        []completedRun
	latestCycle      *RunEvidence
	latestAgeSeconds int64
	checks           []Check
	identitiesValid  bool
}

type completedRun struct {
	run      *gh.WorkflowRun
	evidence RunEvidence
}

func inspectRuns(ctx context.Context, client *gh.Client, owner, repository string, config integrated.Config, now time.Time, window time.Duration) (runInspection, error) {
	inspection := runInspection{}
	identityErrors := []string{}
	activeByID := map[int64]RunEvidence{}
	activeOverflow := false
	for _, status := range []string{"requested", "waiting", "pending", "queued", "in_progress"} {
		runs, response, err := client.Actions.ListWorkflowRunsByFileName(ctx, owner, repository, integrated.HostedControllerWorkflowPath, &gh.ListWorkflowRunsOptions{
			Branch: config.DefaultBranch, Status: status, ExcludePullRequests: true,
			ListOptions: gh.ListOptions{PerPage: activeRunLimit},
		})
		if err != nil {
			return inspection, fmt.Errorf("list %s hosted controller runs: %w", status, err)
		}
		if response != nil && response.NextPage != 0 {
			activeOverflow = true
		}
		if runs == nil {
			return inspection, fmt.Errorf("list %s hosted controller runs: GitHub returned an empty response", status)
		}
		for _, run := range runs.WorkflowRuns {
			evidence, validationErr := validateRun(run, config, status)
			if validationErr != nil {
				identityErrors = append(identityErrors, validationErr.Error())
				continue
			}
			if previous, exists := activeByID[evidence.ID]; exists && previous.Status != evidence.Status {
				identityErrors = append(identityErrors, fmt.Sprintf("controller run %d appeared with conflicting active statuses", evidence.ID))
				continue
			}
			activeByID[evidence.ID] = evidence
		}
	}
	for _, evidence := range activeByID {
		inspection.active = append(inspection.active, evidence)
	}
	sort.Slice(inspection.active, func(i, j int) bool { return inspection.active[i].ID < inspection.active[j].ID })

	completed, _, err := client.Actions.ListWorkflowRunsByFileName(ctx, owner, repository, integrated.HostedControllerWorkflowPath, &gh.ListWorkflowRunsOptions{
		Branch: config.DefaultBranch, Status: "completed", ExcludePullRequests: true,
		ListOptions: gh.ListOptions{PerPage: completedRunLimit},
	})
	if err != nil {
		return inspection, fmt.Errorf("list completed hosted controller runs: %w", err)
	}
	if completed == nil {
		return inspection, errors.New("list completed hosted controller runs: GitHub returned an empty response")
	}
	var latest *RunEvidence
	for _, run := range completed.WorkflowRuns {
		evidence, validationErr := validateRun(run, config, "completed")
		if validationErr != nil {
			identityErrors = append(identityErrors, validationErr.Error())
			continue
		}
		inspection.completed = append(inspection.completed, completedRun{run: run, evidence: evidence})
		cycle, cycleErr := isCycleRun(ctx, client, owner, repository, run)
		if cycleErr != nil {
			return inspection, cycleErr
		}
		if !cycle {
			continue
		}
		if latest == nil || evidence.UpdatedAt.After(latest.UpdatedAt) || (evidence.UpdatedAt.Equal(latest.UpdatedAt) && evidence.ID > latest.ID) {
			copy := evidence
			latest = &copy
		}
	}

	if len(identityErrors) == 0 {
		inspection.identitiesValid = true
		inspection.checks = append(inspection.checks, Check{Name: CheckControllerIdentities, Passed: true, Details: "all bounded controller run results have exact repository, workflow, branch, SHA, event, status, attempt, and timestamp identities"})
	} else {
		inspection.checks = append(inspection.checks, Check{Name: CheckControllerIdentities, Details: strings.Join(identityErrors, "; ")})
	}
	if !activeOverflow && len(inspection.active) <= 1 {
		inspection.checks = append(inspection.checks, Check{Name: CheckControllerOverlap, Passed: true, Details: fmt.Sprintf("%d active hosted controller run(s); no overlap", len(inspection.active))})
	} else {
		inspection.checks = append(inspection.checks, Check{Name: CheckControllerOverlap, Details: "more than one hosted controller run is active"})
	}
	inspection.latestCycle = latest
	if latest == nil {
		inspection.checks = append(inspection.checks,
			Check{Name: CheckLatestCycleSuccess, Details: fmt.Sprintf("no completed cycle was identifiable within the latest %d completed controller runs", completedRunLimit)},
			Check{Name: CheckLatestCycleFresh, Details: "no completed cycle is available for freshness verification"},
		)
		return inspection, nil
	}
	if latest.Conclusion == "success" {
		inspection.checks = append(inspection.checks, Check{Name: CheckLatestCycleSuccess, Passed: true, Details: fmt.Sprintf("latest completed cycle %d concluded successfully", latest.ID)})
	} else {
		inspection.checks = append(inspection.checks, Check{Name: CheckLatestCycleSuccess, Details: fmt.Sprintf("latest completed cycle %d concluded %q", latest.ID, latest.Conclusion)})
	}
	age := now.Sub(latest.UpdatedAt)
	inspection.latestAgeSeconds = int64(age / time.Second)
	if age >= -allowedFutureClockSkew && age <= window {
		inspection.checks = append(inspection.checks, Check{Name: CheckLatestCycleFresh, Passed: true, Details: fmt.Sprintf("latest completed cycle is %s old within the %s freshness window", age.Round(time.Second), window)})
	} else {
		inspection.checks = append(inspection.checks, Check{Name: CheckLatestCycleFresh, Details: fmt.Sprintf("latest completed cycle age %s is outside the %s freshness window", age.Round(time.Second), window)})
	}
	return inspection, nil
}

type controllerReceipt struct {
	SchemaVersion          string                             `json:"schema_version"`
	Repository             string                             `json:"repository"`
	Operation              string                             `json:"operation"`
	RequestID              string                             `json:"request_id"`
	RequestSHA256          string                             `json:"request_sha256,omitempty"`
	StateBranch            string                             `json:"state_branch"`
	StateBefore            string                             `json:"state_before"`
	StateAfter             string                             `json:"state_after"`
	Sequence               uint64                             `json:"sequence"`
	Paused                 *bool                              `json:"paused"`
	Release                integrated.HostedReleaseIdentity   `json:"release"`
	Skipped                bool                               `json:"skipped,omitempty"`
	Duplicate              bool                               `json:"duplicate,omitempty"`
	Run                    json.RawMessage                    `json:"run,omitempty"`
	Doctor                 json.RawMessage                    `json:"doctor,omitempty"`
	Status                 json.RawMessage                    `json:"status,omitempty"`
	Operator               json.RawMessage                    `json:"operator,omitempty"`
	Actor                  *integrated.HostedOperatorIdentity `json:"actor,omitempty"`
	OperatorStatus         string                             `json:"operator_status,omitempty"`
	OperatorTerminalSHA256 string                             `json:"operator_terminal_sha256,omitempty"`
	OperatorLedgerSequence uint64                             `json:"operator_ledger_sequence,omitempty"`
	OperatorReplayed       bool                               `json:"operator_replayed,omitempty"`
	Error                  string                             `json:"error,omitempty"`
}

func inspectLatestStateReceipt(ctx context.Context, client *gh.Client, owner, repository string, config integrated.Config, defaultHead, stateHead string, runs runInspection) (*StateReceiptEvidence, Check, error) {
	fail := func(details string) (*StateReceiptEvidence, Check, error) {
		return nil, Check{Name: CheckRemotePauseState, Details: details}, nil
	}
	if !runs.identitiesValid {
		return fail("remote pause state is unknown because controller run identity validation failed")
	}
	if len(runs.active) != 0 {
		return fail("remote pause state is changing or unverifiable while a controller run is active")
	}
	if len(runs.completed) == 0 {
		return fail("no completed controller run is available for a remote pause-state receipt")
	}
	sort.Slice(runs.completed, func(i, j int) bool {
		left, right := runs.completed[i].evidence, runs.completed[j].evidence
		return left.UpdatedAt.After(right.UpdatedAt) || (left.UpdatedAt.Equal(right.UpdatedAt) && left.ID > right.ID)
	})
	latest := runs.completed[0]
	if latest.evidence.HeadSHA != defaultHead {
		return fail("latest completed controller run is not bound to the exact current default-branch head")
	}
	if latest.evidence.Conclusion != "success" {
		return fail(fmt.Sprintf("latest completed controller run %d concluded %q; remote pause state is unknown", latest.evidence.ID, latest.evidence.Conclusion))
	}
	job, err := inspectControllerJob(ctx, client, owner, repository, latest.run)
	if err != nil {
		return fail(err.Error())
	}
	artifact, err := findControllerArtifact(ctx, client, owner, repository, config, latest.evidence, job)
	if err != nil {
		return fail(err.Error())
	}
	receipt, err := downloadControllerReceipt(ctx, client, owner, repository, artifact, latest.evidence, job)
	if err != nil {
		return fail(err.Error())
	}
	if err := validateControllerReceipt(receipt, config, latest.evidence, job, stateHead); err != nil {
		return fail(err.Error())
	}
	evidence := &StateReceiptEvidence{
		Run: latest.evidence, Job: job, ArtifactID: artifact.GetID(), ArtifactName: artifact.GetName(),
		Operation: receipt.Operation, RequestID: receipt.RequestID, RequestSHA256: receipt.RequestSHA256, StateBefore: receipt.StateBefore,
		StateAfter: receipt.StateAfter, Sequence: receipt.Sequence, Paused: *receipt.Paused, Release: receipt.Release, Actor: receipt.Actor,
		RunResult: receipt.Run, DoctorResult: receipt.Doctor, StatusResult: receipt.Status, OperatorResult: receipt.Operator,
		OperatorStatus: receipt.OperatorStatus, OperatorTerminalSHA256: receipt.OperatorTerminalSHA256,
		OperatorLedgerSequence: receipt.OperatorLedgerSequence, OperatorReplayed: receipt.OperatorReplayed,
	}
	state := "active"
	if evidence.Paused {
		state = "paused"
	}
	return evidence, Check{Name: CheckRemotePauseState, Passed: !evidence.Paused, Details: fmt.Sprintf("latest exact runner receipt %d proves hosted automation is %s at state commit %s", evidence.Run.ID, state, evidence.StateAfter)}, nil
}

func inspectControllerJob(ctx context.Context, client *gh.Client, owner, repository string, run *gh.WorkflowRun) (string, error) {
	jobs, response, err := client.Actions.ListWorkflowJobs(ctx, owner, repository, run.GetID(), &gh.ListWorkflowJobsOptions{
		Filter: "latest", ListOptions: gh.ListOptions{PerPage: 10},
	})
	if err != nil {
		return "", fmt.Errorf("inspect latest controller job for run %d: %w", run.GetID(), err)
	}
	if jobs == nil || jobs.GetTotalCount() != 3 || len(jobs.Jobs) != 3 || (response != nil && response.NextPage != 0) {
		return "", fmt.Errorf("controller run %d has an incomplete or unexpected job inventory", run.GetID())
	}
	expected := map[string]bool{"cycle": false, "control": false, "status": false}
	executed := ""
	for _, job := range jobs.Jobs {
		if job == nil {
			return "", fmt.Errorf("controller run %d job inventory contains a null entry", run.GetID())
		}
		name := job.GetName()
		if _, ok := expected[name]; !ok || expected[name] {
			return "", fmt.Errorf("controller run %d job inventory contains an unexpected or duplicate job", run.GetID())
		}
		expected[name] = true
		if job.GetStatus() != "completed" {
			return "", fmt.Errorf("controller run %d job %q is not completed", run.GetID(), name)
		}
		if job.GetConclusion() == "skipped" {
			continue
		}
		if job.GetConclusion() != "success" || executed != "" {
			return "", fmt.Errorf("controller run %d lacks exactly one successful operation job", run.GetID())
		}
		executed = name
	}
	if executed == "" {
		return "", fmt.Errorf("controller run %d has no successful operation job", run.GetID())
	}
	return executed, nil
}

func findControllerArtifact(ctx context.Context, client *gh.Client, owner, repository string, config integrated.Config, run RunEvidence, job string) (*gh.Artifact, error) {
	artifacts, response, err := client.Actions.ListWorkflowRunArtifacts(ctx, owner, repository, run.ID, &gh.ListOptions{PerPage: 100})
	if err != nil {
		return nil, fmt.Errorf("list controller result artifacts for run %d: %w", run.ID, err)
	}
	if artifacts == nil || artifacts.GetTotalCount() != 1 || len(artifacts.Artifacts) != 1 || (response != nil && response.NextPage != 0) {
		return nil, fmt.Errorf("controller run %d does not have exactly one bounded result artifact", run.ID)
	}
	artifact := artifacts.Artifacts[0]
	wantName := controllerArtifactName(job, run.ID, run.RunAttempt)
	if artifact == nil || artifact.GetID() <= 0 || artifact.GetName() != wantName || artifact.GetExpired() || artifact.GetSizeInBytes() <= 0 || artifact.GetSizeInBytes() > maxControllerArtifact {
		return nil, fmt.Errorf("controller run %d result artifact metadata is malformed, expired, oversized, or has the wrong identity", run.ID)
	}
	numericRepositoryID, err := strconv.ParseInt(config.RepositoryID, 10, 64)
	if err != nil || numericRepositoryID <= 0 {
		return nil, errors.New("hosted repository numeric identity is invalid")
	}
	bound := artifact.WorkflowRun
	if bound == nil || bound.GetID() != run.ID || bound.GetRepositoryID() != numericRepositoryID || bound.GetHeadBranch() != run.HeadBranch || bound.GetHeadSHA() != run.HeadSHA {
		return nil, fmt.Errorf("controller run %d result artifact is not bound to the exact repository, run, branch, and head", run.ID)
	}
	return artifact, nil
}

func downloadControllerReceipt(ctx context.Context, client *gh.Client, owner, repository string, artifact *gh.Artifact, run RunEvidence, job string) (controllerReceipt, error) {
	downloadURL, _, err := client.Actions.DownloadArtifact(ctx, owner, repository, artifact.GetID(), 3)
	if err != nil {
		return controllerReceipt{}, fmt.Errorf("request controller result artifact download: %w", err)
	}
	if !safeControllerArtifactURL(downloadURL) {
		return controllerReceipt{}, errors.New("controller result artifact download URL is not HTTPS")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL.String(), nil)
	if err != nil {
		return controllerReceipt{}, err
	}
	downloadClient := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := downloadClient.Do(request)
	if err != nil {
		return controllerReceipt{}, fmt.Errorf("download controller result artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return controllerReceipt{}, fmt.Errorf("download controller result artifact: HTTP %d", response.StatusCode)
	}
	archive, err := io.ReadAll(io.LimitReader(response.Body, maxControllerArtifact+1))
	if err != nil || int64(len(archive)) > maxControllerArtifact {
		return controllerReceipt{}, errors.New("controller result artifact archive exceeded its strict bound or could not be read")
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(reader.File) != 1 {
		return controllerReceipt{}, errors.New("controller result artifact must be an exact one-file ZIP archive")
	}
	file := reader.File[0]
	wantFile := controllerArtifactName(job, run.ID, run.RunAttempt) + ".json"
	if file.Name != wantFile || file.FileInfo().IsDir() || !file.FileInfo().Mode().IsRegular() || file.UncompressedSize64 == 0 || file.UncompressedSize64 > uint64(maxControllerReceipt) {
		return controllerReceipt{}, errors.New("controller result artifact contains an unsafe or unexpected file")
	}
	opened, err := file.Open()
	if err != nil {
		return controllerReceipt{}, errors.New("open controller result receipt")
	}
	data, readErr := io.ReadAll(io.LimitReader(opened, maxControllerReceipt+1))
	closeErr := opened.Close()
	if readErr != nil || closeErr != nil || int64(len(data)) > maxControllerReceipt {
		return controllerReceipt{}, errors.New("controller result receipt exceeded its strict bound or could not be read")
	}
	var receipt controllerReceipt
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return controllerReceipt{}, fmt.Errorf("decode controller result receipt: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return controllerReceipt{}, err
	}
	return receipt, nil
}

func validateControllerReceipt(receipt controllerReceipt, config integrated.Config, run RunEvidence, job, stateHead string) error {
	if receipt.SchemaVersion != "hive.hosted-cycle.v1" || receipt.Repository != config.Repository || receipt.StateBranch != config.HostedStateBranch || receipt.Error != "" || receipt.Paused == nil {
		return errors.New("controller result receipt has an invalid schema, repository, state branch, error, or pause field")
	}
	if !receiptMatchesHostedRelease(receipt.Release, config) {
		return errors.New("controller result receipt does not match the exact immutable hosted release")
	}
	if !controllerRequestID.MatchString(receipt.RequestID) || !exactSHA.MatchString(receipt.StateBefore) || !exactSHA.MatchString(receipt.StateAfter) || receipt.StateAfter != stateHead || receipt.Sequence == 0 {
		return errors.New("controller result receipt is not bound to the exact request and current state-branch commit")
	}
	allowed := map[string]map[string]bool{
		"cycle": {"cycle": true},
		"control": {"pause": true, "resume": true, "approve-baseline-plan": true, "approve-baseline-apply": true,
			"approve-merge-plan": true, "approve-merge-apply": true, "retry-repair": true,
			"recover-dispatch-plan": true, "recover-dispatch-apply": true},
		"status": {"status": true, "doctor": true},
	}
	if !allowed[job][receipt.Operation] {
		return fmt.Errorf("controller result receipt operation %q does not match successful job %q", receipt.Operation, job)
	}
	if receipt.Operation == "pause" && !*receipt.Paused {
		return errors.New("pause controller receipt did not prove a paused state")
	}
	if receipt.Operation == "resume" && *receipt.Paused {
		return errors.New("resume controller receipt did not prove an active state")
	}
	operatorOperation := strings.HasPrefix(receipt.Operation, "approve-") || receipt.Operation == "retry-repair" || strings.HasPrefix(receipt.Operation, "recover-dispatch-")
	if operatorOperation {
		if !hostedDigestPattern.MatchString(receipt.RequestSHA256) || receipt.Actor == nil || receipt.Actor.ID <= 0 || strings.TrimSpace(receipt.Actor.Login) == "" {
			return errors.New("hosted operator receipt lacks its exact request digest or human actor")
		}
		if err := validateHostedOperatorTerminalReceipt(receipt); err != nil {
			return err
		}
	} else if receipt.RequestSHA256 != "" || receipt.Actor != nil || len(receipt.Operator) != 0 || receipt.OperatorStatus != "" ||
		receipt.OperatorTerminalSHA256 != "" || receipt.OperatorLedgerSequence != 0 || receipt.OperatorReplayed {
		return errors.New("non-operator controller receipt contains unexpected operator authority")
	}
	if run.Event == "schedule" && (job != "cycle" || receipt.Operation != "cycle") {
		return errors.New("scheduled controller receipt is not an ordinary cycle")
	}
	return nil
}

func receiptMatchesHostedRelease(release integrated.HostedReleaseIdentity, config integrated.Config) bool {
	return release.Version == config.HiveReleaseVersion && strings.EqualFold(release.HiveCommit, config.HiveCommit) &&
		strings.EqualFold(release.VisualHiveCommit, config.VisualHiveRef) &&
		strings.EqualFold(release.DistributionManifestSHA256, config.DistributionManifestSHA256) &&
		release.HostedControllerProtocol == config.HostedControllerProtocol && release.HostedControllerProtocol == integrated.HostedControllerProtocol
}

type hostedOperatorTerminalReceipt struct {
	SchemaVersion string          `json:"schema_version"`
	Outcome       string          `json:"outcome"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         string          `json:"error,omitempty"`
}

func validateHostedOperatorTerminalReceipt(receipt controllerReceipt) error {
	if receipt.OperatorStatus != "succeeded" && receipt.OperatorStatus != "failed" || !hostedDigestPattern.MatchString(receipt.OperatorTerminalSHA256) ||
		receipt.OperatorLedgerSequence == 0 || receipt.OperatorLedgerSequence > receipt.Sequence || !receipt.OperatorReplayed && receipt.OperatorLedgerSequence != receipt.Sequence ||
		receipt.OperatorReplayed && !receipt.Duplicate {
		return errors.New("hosted operator receipt lacks an exact terminal ledger binding")
	}
	terminal := hostedOperatorTerminalReceipt{SchemaVersion: "hive.hosted-operator-terminal.v1", Outcome: receipt.OperatorStatus, Result: receipt.Operator, Error: receipt.Error}
	if receipt.OperatorStatus == "succeeded" && (len(receipt.Operator) == 0 || receipt.Error != "") ||
		receipt.OperatorStatus == "failed" && receipt.Error == "" {
		return errors.New("hosted operator terminal outcome does not match its typed result or error")
	}
	encoded, err := json.Marshal(terminal)
	if err != nil {
		return errors.New("encode hosted operator terminal receipt")
	}
	digest := sha256.Sum256(encoded)
	if hex.EncodeToString(digest[:]) != receipt.OperatorTerminalSHA256 {
		return errors.New("hosted operator terminal digest does not match its exact result and error")
	}
	return nil
}

func controllerArtifactName(job string, runID int64, attempt int) string {
	return fmt.Sprintf("hive-hosted-result-%s-%d-%d", job, runID, attempt)
}

func safeControllerArtifactURL(value *url.URL) bool {
	if value == nil || value.User != nil || value.Hostname() == "" || value.RawQuery == "" && value.Path == "" {
		return false
	}
	if value.Scheme == "https" {
		return true
	}
	host := strings.ToLower(value.Hostname())
	return value.Scheme == "http" && (host == "127.0.0.1" || host == "localhost" || host == "::1")
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("controller result receipt contains more than one JSON value")
		}
		return fmt.Errorf("decode controller result receipt trailer: %w", err)
	}
	return nil
}

func isCycleRun(ctx context.Context, client *gh.Client, owner, repository string, run *gh.WorkflowRun) (bool, error) {
	if run.GetEvent() == "schedule" {
		return true, nil
	}
	if run.GetEvent() != "workflow_dispatch" {
		return false, nil
	}
	jobs, response, err := client.Actions.ListWorkflowJobs(ctx, owner, repository, run.GetID(), &gh.ListWorkflowJobsOptions{
		Filter: "latest", ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return false, fmt.Errorf("inspect jobs for hosted controller run %d: %w", run.GetID(), err)
	}
	if jobs == nil || (response != nil && response.NextPage != 0) {
		return false, fmt.Errorf("inspect jobs for hosted controller run %d: job result exceeds the bounded page", run.GetID())
	}
	for _, job := range jobs.Jobs {
		if job.GetName() == "cycle" && job.GetStatus() == "completed" && job.GetConclusion() != "skipped" {
			return true, nil
		}
	}
	return false, nil
}

func validateRun(run *gh.WorkflowRun, config integrated.Config, expectedStatus string) (RunEvidence, error) {
	if run == nil {
		return RunEvidence{}, errors.New("controller runs contained a null entry")
	}
	evidence := RunEvidence{
		ID: run.GetID(), URL: run.GetHTMLURL(), Event: run.GetEvent(), Status: run.GetStatus(), Conclusion: run.GetConclusion(),
		Path: run.GetPath(), HeadBranch: run.GetHeadBranch(), HeadSHA: run.GetHeadSHA(), RunNumber: run.GetRunNumber(), RunAttempt: run.GetRunAttempt(),
		DisplayTitle: run.GetDisplayTitle(), ActorID: run.GetActor().GetID(), ActorLogin: run.GetActor().GetLogin(), ActorType: run.GetActor().GetType(),
		TriggeringActorID: run.GetTriggeringActor().GetID(), TriggeringActorLogin: run.GetTriggeringActor().GetLogin(), TriggeringActorType: run.GetTriggeringActor().GetType(),
		CreatedAt: run.GetCreatedAt().Time, RunStartedAt: run.GetRunStartedAt().Time, UpdatedAt: run.GetUpdatedAt().Time,
	}
	if evidence.ID <= 0 || evidence.RunNumber <= 0 || evidence.RunAttempt <= 0 {
		return evidence, fmt.Errorf("controller run has a non-positive ID, run number, or attempt")
	}
	if evidence.Path != integrated.HostedControllerWorkflowPath || evidence.HeadBranch != config.DefaultBranch || !exactSHA.MatchString(evidence.HeadSHA) {
		return evidence, fmt.Errorf("controller run %d is not exactly bound to workflow %q, branch %q, and a lowercase commit SHA", evidence.ID, integrated.HostedControllerWorkflowPath, config.DefaultBranch)
	}
	if evidence.Event != "schedule" && evidence.Event != "workflow_dispatch" {
		return evidence, fmt.Errorf("controller run %d has unsupported event %q", evidence.ID, evidence.Event)
	}
	if evidence.Status != expectedStatus {
		return evidence, fmt.Errorf("controller run %d status %q does not match filtered status %q", evidence.ID, evidence.Status, expectedStatus)
	}
	if evidence.CreatedAt.IsZero() || evidence.UpdatedAt.IsZero() || evidence.UpdatedAt.Before(evidence.CreatedAt) {
		return evidence, fmt.Errorf("controller run %d has malformed timestamps", evidence.ID)
	}
	if evidence.Status == "completed" && evidence.Conclusion == "" {
		return evidence, fmt.Errorf("completed controller run %d has no conclusion", evidence.ID)
	}
	if repository := run.GetRepository().GetFullName(); repository != "" && !strings.EqualFold(repository, config.Repository) {
		return evidence, fmt.Errorf("controller run %d is bound to unexpected repository %q", evidence.ID, repository)
	}
	return evidence, nil
}

func splitRepository(repository string) (string, string, error) {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) != parts[0] || strings.TrimSpace(parts[1]) != parts[1] || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("hosted repository must be one exact owner/name pair")
	}
	return parts[0], parts[1], nil
}

func isNotFound(response *gh.Response, err error) bool {
	if response != nil && response.StatusCode == 404 {
		return true
	}
	var errorResponse *gh.ErrorResponse
	return errors.As(err, &errorResponse) && errorResponse.Response != nil && errorResponse.Response.StatusCode == 404
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/checkpoint"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/hostedcontrol"
	"github.com/kubestellar/hive/v2/pkg/hostedstate"
	"github.com/kubestellar/hive/v2/pkg/integrated"
)

const (
	hostedRequestSchema            = "hive.hosted-request-ledger.v2"
	hostedRequestLegacySchema      = "hive.hosted-request-ledger.v1"
	hostedRequestMaxBytes          = 2 << 20
	hostedRequestRetainedTerminals = 512
)

var hostedRequestPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type hostedCommandResult struct {
	SchemaVersion          string                             `json:"schema_version"`
	Repository             string                             `json:"repository"`
	Operation              string                             `json:"operation"`
	RequestID              string                             `json:"request_id"`
	RequestSHA256          string                             `json:"request_sha256,omitempty"`
	StateBranch            string                             `json:"state_branch"`
	Release                integrated.HostedReleaseIdentity   `json:"release"`
	StateBefore            string                             `json:"state_before,omitempty"`
	StateAfter             string                             `json:"state_after,omitempty"`
	Sequence               uint64                             `json:"sequence"`
	Paused                 bool                               `json:"paused"`
	Skipped                bool                               `json:"skipped,omitempty"`
	Duplicate              bool                               `json:"duplicate,omitempty"`
	Run                    *integrated.RunResult              `json:"run,omitempty"`
	Doctor                 any                                `json:"doctor,omitempty"`
	Status                 any                                `json:"status,omitempty"`
	Operator               any                                `json:"operator,omitempty"`
	Actor                  *integrated.HostedOperatorIdentity `json:"actor,omitempty"`
	OperatorStatus         string                             `json:"operator_status,omitempty"`
	OperatorTerminalSHA256 string                             `json:"operator_terminal_sha256,omitempty"`
	OperatorLedgerSequence uint64                             `json:"operator_ledger_sequence,omitempty"`
	OperatorReplayed       bool                               `json:"operator_replayed,omitempty"`
	Error                  string                             `json:"error,omitempty"`
}

type hostedRequestLedger struct {
	SchemaVersion string                `json:"schema_version"`
	Requests      []hostedRequestRecord `json:"requests"`
}

type hostedRequestRecord struct {
	ID                   string    `json:"id"`
	Operation            string    `json:"operation"`
	Status               string    `json:"status"`
	StartedAt            time.Time `json:"started_at"`
	CompletedAt          time.Time `json:"completed_at,omitempty"`
	ControllerRunID      uint64    `json:"controller_run_id,omitempty"`
	ControllerRunAttempt uint64    `json:"controller_run_attempt,omitempty"`
	MigratedLegacy       bool      `json:"migrated_legacy,omitempty"`
}

type hostedRequestPreparation struct {
	New       bool
	Resume    bool
	Duplicate bool
	Status    string
}

func runHostedCycleCommand(args []string) int {
	flags := flag.NewFlagSet("hive hosted-cycle", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repository := flags.String("repo", "", "exact GitHub repository owner/name")
	repositoryID := flags.String("repository-id", "", "exact numeric GitHub repository ID")
	stateBranch := flags.String("state-branch", "", "dedicated hosted state branch")
	stateCheckout := flags.String("state-dir", "", "checked-out hosted state branch")
	targetCheckout := flags.String("checkout-dir", "", "ephemeral default-branch target checkout")
	stateKeyEnv := flags.String("state-key-env", "HIVE_HOSTED_STATE_KEY", "environment containing the hosted state key")
	operation := flags.String("operation", "cycle", "cycle, status, doctor, pause, resume, or a typed operator operation")
	requestID := flags.String("request-id", "", "bounded idempotency key")
	requestPayload := flags.String("request-payload", "", "base64url content-bound hosted operator request")
	requestSHA256 := flags.String("request-sha256", "", "SHA-256 of the exact hosted operator request bytes")
	restoreVersion := flags.String("restore-release-version", "", "exact predecessor release version for one transition")
	restoreHiveCommit := flags.String("restore-hive-commit", "", "exact predecessor Hive commit for one transition")
	restoreVisualCommit := flags.String("restore-visual-commit", "", "exact predecessor Visual Hive commit for one transition")
	restoreManifestSHA256 := flags.String("restore-manifest-sha256", "", "exact predecessor manifest digest for one transition")
	restoreControllerProtocol := flags.Int("restore-controller-protocol", 0, "exact predecessor hosted controller protocol for one transition")
	jsonOutput := flags.Bool("json", false, "emit one machine-readable JSON document")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	result := hostedCommandResult{SchemaVersion: "hive.hosted-cycle.v1", Repository: *repository, Operation: *operation, RequestID: *requestID, StateBranch: *stateBranch}
	fail := func(err error) int {
		result.Error = err.Error()
		_ = encodeJSON(result)
		return 1
	}
	if !*jsonOutput {
		return fail(errors.New("hosted-cycle is an internal JSON-only controller command"))
	}
	if err := validateHostedInvocation(*repository, *repositoryID, *stateBranch, *stateKeyEnv, *operation, *requestID); err != nil {
		return fail(err)
	}
	identity, err := loadInstalledReleaseIdentity()
	if err != nil {
		return fail(err)
	}
	result.Release = integrated.HostedReleaseIdentity{
		Version: identity.Version, HiveCommit: identity.HiveCommit, VisualHiveCommit: identity.VisualHiveCommit,
		DistributionManifestSHA256: identity.ManifestSHA256, HostedControllerProtocol: identity.HostedControllerProtocol,
	}
	restoreRelease, restoreManifest, err := hostedRestoreRelease(*restoreVersion, *restoreHiveCommit, *restoreVisualCommit, *restoreManifestSHA256, *restoreControllerProtocol, identity)
	if err != nil {
		return fail(err)
	}
	numericRepositoryID, _ := strconv.ParseInt(*repositoryID, 10, 64)
	runID, _ := strconv.ParseUint(os.Getenv("GITHUB_RUN_ID"), 10, 64)
	attempt, _ := strconv.ParseUint(os.Getenv("GITHUB_RUN_ATTEMPT"), 10, 64)
	controller := hostedstate.ControllerIdentity{
		RunID: runID, Attempt: attempt, Event: os.Getenv("GITHUB_EVENT_NAME"),
		WorkflowPath: integrated.HostedControllerWorkflowPath,
		WorkflowSHA:  strings.ToLower(os.Getenv("GITHUB_SHA")), DefaultHeadSHA: strings.ToLower(os.Getenv("GITHUB_SHA")),
	}
	runnerTemp := strings.TrimSpace(os.Getenv("RUNNER_TEMP"))
	if runnerTemp == "" {
		return fail(errors.New("GitHub runner temporary directory is unavailable"))
	}
	runtimeRoot, err := os.MkdirTemp(runnerTemp, "hive-hosted-runtime-")
	if err != nil {
		return fail(fmt.Errorf("create hosted runtime state: %w", err))
	}
	defer os.RemoveAll(runtimeRoot)
	if err := os.Chmod(runtimeRoot, 0o700); err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 58*time.Minute)
	defer cancel()
	if err := validateHostedTargetCheckout(ctx, *targetCheckout, os.Getenv("GITHUB_SHA")); err != nil {
		return fail(err)
	}
	manager, err := hostedcontrol.Open(ctx, hostedcontrol.Config{
		CheckoutDir: *stateCheckout, RuntimeRoot: runtimeRoot, StateBranch: *stateBranch,
		Secret: []byte(os.Getenv(*stateKeyEnv)), Repository: hostedstate.RepositoryIdentity{FullName: *repository, ID: numericRepositoryID},
		Release: hostedstate.ReleaseIdentity{
			Version: identity.Version, HiveCommit: identity.HiveCommit, VisualHiveCommit: identity.VisualHiveCommit,
			DistributionManifestSHA256: identity.ManifestSHA256, HostedControllerProtocol: identity.HostedControllerProtocol,
		},
		RestoreRelease: restoreRelease,
		Controller:     controller, TrustedWorkflowPath: integrated.HostedControllerWorkflowPath,
	})
	if err != nil {
		return fail(fmt.Errorf("open authenticated hosted state: %w", err))
	}
	result.StateBefore = manager.HeadSHA()
	installRoot := filepath.Dir(identity.ManifestPath)
	node := filepath.Join(installRoot, "runtime", "node")
	visual := filepath.Join(installRoot, "visual-hive", "visual-hive.mjs")
	requireProvider := *operation == "cycle"
	config, err := integrated.PrepareHostedRuntime(ctx, runtimeRoot, *targetCheckout, node, []string{visual}, strings.TrimSpace(os.Getenv("HIVE_HOSTED_CODEX")), nil, requireProvider,
		integrated.HostedReleaseIdentity{Version: identity.Version, HiveCommit: identity.HiveCommit, VisualHiveCommit: identity.VisualHiveCommit, DistributionManifestSHA256: identity.ManifestSHA256, HostedControllerProtocol: identity.HostedControllerProtocol}, restoreManifest)
	if err != nil {
		return fail(err)
	}
	if config.Repository != *repository || config.RepositoryID != *repositoryID || config.ExecutionMode != integrated.ExecutionHosted ||
		config.HostedStateBranch != *stateBranch || config.HiveReleaseVersion != identity.Version || config.HiveCommit != identity.HiveCommit ||
		config.VisualHiveRef != identity.VisualHiveCommit || config.DistributionManifestSHA256 != identity.ManifestSHA256 ||
		config.HostedControllerProtocol != identity.HostedControllerProtocol {
		return fail(errors.New("signed hosted policy does not match controller repository and immutable release identity"))
	}
	if err := validateHostedPolicyContext(config); err != nil {
		return fail(err)
	}
	var operatorRequest *hostedOperationRequest
	var operatorAuthority integrated.HostedOperatorAuthority
	var operatorIdentity *integrated.HostedOperatorIdentity
	var operatorPreparation hostedOperatorPreparation
	if isHostedOperatorOperation(*operation) {
		request, requestBytes, requestErr := decodeHostedOperationRequest(*requestPayload, *requestSHA256)
		if requestErr != nil {
			return fail(requestErr)
		}
		if requestErr = validateHostedOperationRequestEnvelope(request, config, identity, *operation, *requestID); requestErr != nil {
			return fail(requestErr)
		}
		var verifiedIdentity integrated.HostedOperatorIdentity
		operatorAuthority, verifiedIdentity, requestErr = verifyHostedWorkflowOperator(ctx, hostedGitHubClient(), config, request, strings.ToLower(strings.TrimSpace(*requestSHA256)))
		if requestErr != nil {
			return fail(requestErr)
		}
		operatorIdentity = &verifiedIdentity
		operatorRequest = &request
		result.RequestSHA256 = strings.ToLower(strings.TrimSpace(*requestSHA256))
		result.Actor = operatorIdentity
		operatorPreparation, requestErr = prepareHostedOperatorRequest(runtimeRoot, []byte(os.Getenv(*stateKeyEnv)), config, request, requestBytes,
			result.RequestSHA256, verifiedIdentity, manager.HeadSHA(), manager.Sequence(), controller, time.Now().UTC())
		if requestErr != nil {
			return fail(requestErr)
		}
		if operatorPreparation.Terminal {
			result.Duplicate, result.Skipped, result.OperatorReplayed = true, true, true
			result.OperatorStatus, result.OperatorTerminalSHA256, result.OperatorLedgerSequence = operatorPreparation.Status, operatorPreparation.TerminalSHA256, operatorPreparation.TerminalSequence
			if len(operatorPreparation.Result) != 0 {
				result.Operator = operatorPreparation.Result
			}
			result.StateAfter, result.Sequence = manager.HeadSHA(), manager.Sequence()
			if operatorPreparation.Error != "" {
				return fail(errors.New(operatorPreparation.Error))
			}
			return encodeJSON(result)
		}
	} else if strings.TrimSpace(*requestPayload) != "" || strings.TrimSpace(*requestSHA256) != "" {
		return fail(errors.New("non-operator hosted operations must not carry an operator request"))
	}
	operatorLedgerSummary, operatorPending, err := hostedOperatorLedgerStatus(runtimeRoot, []byte(os.Getenv(*stateKeyEnv)), config)
	if err != nil {
		return fail(fmt.Errorf("inspect hosted operator request state: %w", err))
	}
	operatorRequestActive := isHostedOperatorOperation(*operation) && (operatorPreparation.New || operatorPreparation.Resume)
	if operatorPending && !operatorRequestActive && *operation != "status" && *operation != "doctor" && *operation != "pause" {
		return fail(errors.New("hosted lifecycle execution is blocked by an accepted operator request that must be resumed with its exact request ID"))
	}
	// Every runner receipt carries the pause state restored from authenticated
	// hosted state. This remains explicit when false so external status never
	// has to infer "active" from a missing JSON field or stale desktop config.
	result.Paused = config.Paused
	mutating := *operation == "cycle" || *operation == "pause" || *operation == "resume" || isHostedOperatorOperation(*operation)
	if mutating {
		if isHostedOperatorOperation(*operation) {
			if operatorPreparation.New {
				if err := manager.Checkpoint(ctx, "accept hosted operator request "+*requestID); err != nil {
					return fail(err)
				}
			}
		} else {
			preparation, ledgerErr := beginHostedRequest(runtimeRoot, *requestID, *operation, controller.RunID, controller.Attempt, time.Now().UTC())
			if ledgerErr != nil {
				return fail(ledgerErr)
			}
			if preparation.Duplicate {
				result.Duplicate, result.Skipped, result.StateAfter, result.Sequence = true, true, manager.HeadSHA(), manager.Sequence()
				if preparation.Status != "succeeded" {
					return fail(errors.New("the exact hosted request already reached a non-success terminal state; reconcile status and use a new request ID"))
				}
				return encodeJSON(result)
			}
			if err := manager.Checkpoint(ctx, "accept request "+*requestID); err != nil {
				return fail(err)
			}
		}
	}
	operationCtx := checkpoint.WithCallback(ctx, manager.Callback)
	var operationErr error
	switch *operation {
	case "cycle":
		if config.Paused && *operation == "cycle" {
			result.Paused, result.Skipped = true, true
			break
		}
		run, runErr := integrated.RunOnce(operationCtx, integrated.RunOptions{StateDir: runtimeRoot, Timeout: 50 * time.Minute, GitHub: hostedGitHubClient(), GitTransportToken: os.Getenv("HIVE_GITHUB_TOKEN")})
		result.Run, operationErr = &run, runErr
	case "pause", "resume":
		updated, pauseErr := integrated.SetPaused(operationCtx, runtimeRoot, *operation == "pause")
		result.Paused, operationErr = updated.Paused, pauseErr
	case "doctor":
		checks := publicHostedDoctorChecks(collectIntegratedDoctorChecks(runtimeRoot, "HIVE_GITHUB_TOKEN", "", false))
		lifecycle := collectIntegratedLifecycleStatus(runtimeRoot, config, "")
		lifecycle["hosted_operator_requests"] = operatorLedgerSummary
		if operatorPending {
			lifecycle["production_ready"] = false
			addLifecycleQuiescenceBlocker(lifecycle, "hosted_operator_requests")
		}
		lifecycleReady, _ := lifecycle["production_ready"].(bool)
		result.Doctor = map[string]any{"schema_version": "hive.doctor.v1", "production_ready": doctorChecksReady(checks) && lifecycleReady, "checks": checks, "lifecycle": lifecycle}
	case "status":
		lifecycle := collectIntegratedLifecycleStatus(runtimeRoot, config, "")
		lifecycle["hosted_operator_requests"] = operatorLedgerSummary
		if operatorPending {
			lifecycle["production_ready"] = false
			addLifecycleQuiescenceBlocker(lifecycle, "hosted_operator_requests")
		}
		result.Status = lifecycle
	default:
		if operatorRequest == nil || operatorAuthority == nil || operatorIdentity == nil {
			operationErr = fmt.Errorf("unsupported hosted operation %q", *operation)
		} else {
			result.Operator, operationErr = executeHostedOperator(operationCtx, runtimeRoot, *operatorRequest, operatorAuthority, hostedGitHubClient())
		}
	}
	if mutating {
		if isHostedOperatorOperation(*operation) {
			terminal, terminalErr := finishHostedOperatorRequest(runtimeRoot, []byte(os.Getenv(*stateKeyEnv)), config, *operatorRequest, result.RequestSHA256,
				*operatorIdentity, result.Operator, operationErr, manager.Sequence(), controller, time.Now().UTC())
			if terminalErr != nil {
				if operationErr == nil {
					operationErr = terminalErr
				} else {
					operationErr = fmt.Errorf("%v; persist hosted operator terminal result: %w", operationErr, terminalErr)
				}
			} else {
				result.OperatorStatus, result.OperatorTerminalSHA256, result.OperatorLedgerSequence = terminal.Status, terminal.TerminalSHA256, terminal.TerminalSequence
				if len(terminal.Result) != 0 {
					result.Operator = terminal.Result
				}
				if terminal.Error != "" {
					operationErr = errors.New(terminal.Error)
				}
			}
		} else {
			terminalErr := completeHostedRequest(runtimeRoot, *requestID, *operation, operationErr == nil, controller.RunID, controller.Attempt, time.Now().UTC())
			if terminalErr != nil {
				if operationErr == nil {
					operationErr = terminalErr
				} else {
					operationErr = fmt.Errorf("%v; persist ordinary hosted request outcome: %w", operationErr, terminalErr)
				}
			}
		}
		checkpointCtx, checkpointCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		checkpointErr := manager.Checkpoint(checkpointCtx, "finish request "+*requestID)
		checkpointCancel()
		if operationErr == nil {
			operationErr = checkpointErr
		} else if checkpointErr != nil {
			operationErr = fmt.Errorf("%v; final hosted checkpoint failed: %w", operationErr, checkpointErr)
		}
	}
	result.StateAfter, result.Sequence = manager.HeadSHA(), manager.Sequence()
	if operationErr != nil {
		return fail(operationErr)
	}
	return encodeJSON(result)
}

// publicHostedDoctorChecks keeps the receipt useful without publishing
// ephemeral runner paths or provider stderr. The exact controller log remains
// the privileged diagnostic surface; the public receipt reports the failing
// subsystem and its supported remediation.
func publicHostedDoctorChecks(checks []doctorCheck) []doctorCheck {
	result := make([]doctorCheck, 0, len(checks))
	for _, check := range checks {
		if check.OK {
			result = append(result, doctorCheck{Name: check.Name, OK: true, Message: "hosted check passed"})
			continue
		}
		message := "hosted check failed; inspect the exact controller run logs"
		switch check.Name {
		case "provider":
			message = "Codex provider installation or authentication is unavailable; verify the managed OPENAI_API_KEY repository secret and retry doctor"
		case "visual_hive_runtime", "visual_hive_pin":
			message = "the immutable Visual Hive runtime is unavailable or does not match the installed release"
		case "checkout":
			message = "the hosted target checkout is unavailable"
		case "github_auth":
			message = "the hosted GitHub token cannot read the configured repository"
		}
		result = append(result, doctorCheck{Name: check.Name, OK: false, Message: message})
	}
	return result
}

func validateHostedTargetCheckout(ctx context.Context, checkout, expectedSHA string) error {
	checkout = strings.TrimSpace(checkout)
	expectedSHA = strings.ToLower(strings.TrimSpace(expectedSHA))
	if checkout == "" || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(expectedSHA) {
		return errors.New("hosted target checkout identity is invalid")
	}
	command := exec.CommandContext(ctx, "git", "-C", checkout, "rev-parse", "HEAD")
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := command.Output()
	if err != nil || strings.ToLower(strings.TrimSpace(string(output))) != expectedSHA {
		return errors.New("hosted target checkout does not match the exact workflow SHA")
	}
	return nil
}

func hostedRestoreRelease(version, hiveCommit, visualCommit, manifestSHA256 string, protocol int, current installedReleaseIdentity) (*hostedstate.ReleaseIdentity, *integrated.HostedReleaseIdentity, error) {
	values := []string{strings.TrimSpace(version), strings.ToLower(strings.TrimSpace(hiveCommit)), strings.ToLower(strings.TrimSpace(visualCommit)), strings.ToLower(strings.TrimSpace(manifestSHA256))}
	empty := 0
	for _, value := range values {
		if value == "" {
			empty++
		}
	}
	if empty == len(values) && protocol == 0 {
		return nil, nil, nil
	}
	if empty != 0 || protocol != integrated.HostedControllerProtocol || !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-integrated\.[0-9]+$`).MatchString(values[0]) ||
		!regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(values[1]) || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(values[2]) ||
		!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(values[3]) {
		return nil, nil, errors.New("hosted predecessor release identity is incomplete or invalid")
	}
	restore := &hostedstate.ReleaseIdentity{
		Version: values[0], HiveCommit: values[1], VisualHiveCommit: values[2],
		DistributionManifestSHA256: values[3], HostedControllerProtocol: protocol,
	}
	currentRelease := hostedstate.ReleaseIdentity{
		Version: current.Version, HiveCommit: strings.ToLower(current.HiveCommit), VisualHiveCommit: strings.ToLower(current.VisualHiveCommit),
		DistributionManifestSHA256: strings.ToLower(current.ManifestSHA256), HostedControllerProtocol: current.HostedControllerProtocol,
	}
	if *restore == currentRelease {
		return nil, nil, errors.New("hosted predecessor release must differ from the installed release")
	}
	manifest := &integrated.HostedReleaseIdentity{Version: values[0], HiveCommit: values[1], VisualHiveCommit: values[2], DistributionManifestSHA256: values[3], HostedControllerProtocol: protocol}
	return restore, manifest, nil
}

func validateHostedInvocation(repository, repositoryID, stateBranch, stateKeyEnv, operation, requestID string) error {
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_REPOSITORY") != repository || os.Getenv("GITHUB_REF") == "" ||
		os.Getenv("GITHUB_SHA") == "" || os.Getenv("GITHUB_RUN_ID") == "" || os.Getenv("GITHUB_RUN_ATTEMPT") == "" {
		return errors.New("hosted-cycle requires the bound GitHub Actions controller context")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repository) || !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(repositoryID) ||
		!strings.HasPrefix(stateBranch, "hive/state-") || strings.Contains(stateBranch, "..") {
		return errors.New("hosted-cycle repository or state-branch identity is invalid")
	}
	event := os.Getenv("GITHUB_EVENT_NAME")
	if event != "schedule" && event != "workflow_dispatch" {
		return errors.New("hosted-cycle event is not trusted")
	}
	workflowRef := os.Getenv("GITHUB_WORKFLOW_REF")
	if !strings.HasPrefix(workflowRef, repository+"/"+integrated.HostedControllerWorkflowPath+"@refs/heads/") {
		return errors.New("hosted-cycle workflow ref is not the managed controller on a branch")
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(strings.ToLower(os.Getenv("GITHUB_SHA"))) {
		return errors.New("hosted-cycle workflow SHA is invalid")
	}
	runID, runErr := strconv.ParseUint(os.Getenv("GITHUB_RUN_ID"), 10, 64)
	attempt, attemptErr := strconv.ParseUint(os.Getenv("GITHUB_RUN_ATTEMPT"), 10, 64)
	if runErr != nil || attemptErr != nil || runID == 0 || attempt == 0 {
		return errors.New("hosted-cycle workflow run identity is invalid")
	}
	if !regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`).MatchString(stateKeyEnv) || len(os.Getenv(stateKeyEnv)) < 32 {
		return errors.New("hosted state authentication secret is missing or invalid")
	}
	if strings.TrimSpace(os.Getenv("HIVE_GITHUB_TOKEN")) == "" {
		return errors.New("hosted-cycle GitHub lifecycle token is missing")
	}
	if !hostedRequestPattern.MatchString(requestID) {
		return errors.New("hosted request ID is invalid")
	}
	allowed := map[string]bool{"cycle": true, "status": true, "doctor": true, "pause": true, "resume": true,
		hostedApproveBaselinePlan: true, hostedApproveBaselineApply: true, hostedApproveMergePlan: true, hostedApproveMergeApply: true,
		hostedRetryRepair: true, hostedRecoverDispatchPlan: true, hostedRecoverDispatchApply: true}
	if !allowed[operation] || event == "schedule" && operation != "cycle" {
		return errors.New("hosted operation is not permitted for this event")
	}
	return nil
}

func validateHostedPolicyContext(config integrated.Config) error {
	branch := strings.TrimSpace(config.DefaultBranch)
	if branch == "" || os.Getenv("GITHUB_REF") != "refs/heads/"+branch {
		return errors.New("hosted-cycle ref does not match the signed default branch")
	}
	wantWorkflowRef := config.Repository + "/" + integrated.HostedControllerWorkflowPath + "@refs/heads/" + branch
	if os.Getenv("GITHUB_WORKFLOW_REF") != wantWorkflowRef {
		return errors.New("hosted-cycle workflow ref does not match the signed managed controller and default branch")
	}
	return nil
}

func hostedGitHubClient() *hivegithub.Client {
	return hivegithub.NewClient(os.Getenv("HIVE_GITHUB_TOKEN"), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
}

func beginHostedRequest(root, id, operation string, runID, runAttempt uint64, now time.Time) (hostedRequestPreparation, error) {
	var result hostedRequestPreparation
	if !hostedRequestPattern.MatchString(id) || !ordinaryHostedOperation(operation) || runID == 0 || runAttempt == 0 || now.IsZero() {
		return result, errors.New("ordinary hosted request identity is invalid")
	}
	now = now.UTC()
	ledger, err := loadHostedRequestLedger(root)
	if err != nil {
		return result, err
	}
	for index := range ledger.Requests {
		record := &ledger.Requests[index]
		if record.ID == id {
			if record.Operation != operation {
				return result, errors.New("hosted request ID was already bound to a different operation")
			}
			result.Status = record.Status
			if record.Status != "started" {
				result.Duplicate = true
				return result, nil
			}
			if record.ControllerRunID > runID || record.ControllerRunID == runID && record.ControllerRunAttempt > runAttempt {
				return result, errors.New("ordinary hosted request cannot resume from an older controller identity")
			}
			record.ControllerRunID, record.ControllerRunAttempt = runID, runAttempt
			record.MigratedLegacy = false
			if err := saveHostedRequestLedger(root, ledger); err != nil {
				return result, err
			}
			result.Resume = true
			return result, nil
		}
	}
	for index := range ledger.Requests {
		record := &ledger.Requests[index]
		if record.Status != "started" {
			continue
		}
		if record.ControllerRunID > 0 && record.ControllerRunID >= runID {
			return result, fmt.Errorf("ordinary hosted request %s (%s) is still owned by controller run %d", record.ID, record.Operation, record.ControllerRunID)
		}
		// The generated workflow serializes every controller run. Reaching a
		// newer run proves the older runner is terminal, so preserve a bounded
		// abandoned receipt instead of blocking hosted cadence forever.
		record.Status, record.CompletedAt = "abandoned", now
	}
	ledger.Requests = compactHostedRequestLedger(ledger.Requests, hostedRequestRetainedTerminals-1)
	ledger.Requests = append(ledger.Requests, hostedRequestRecord{
		ID: id, Operation: operation, Status: "started", StartedAt: now,
		ControllerRunID: runID, ControllerRunAttempt: runAttempt,
	})
	if err := saveHostedRequestLedger(root, ledger); err != nil {
		return result, err
	}
	result.New, result.Status = true, "started"
	return result, nil
}

func completeHostedRequest(root, id, operation string, succeeded bool, runID, runAttempt uint64, now time.Time) error {
	if !hostedRequestPattern.MatchString(id) || !ordinaryHostedOperation(operation) || runID == 0 || runAttempt == 0 || now.IsZero() {
		return errors.New("ordinary hosted request terminal identity is invalid")
	}
	ledger, err := loadHostedRequestLedger(root)
	if err != nil {
		return err
	}
	for index := range ledger.Requests {
		record := &ledger.Requests[index]
		if record.ID == id && record.Operation == operation {
			if record.Status != "started" || record.ControllerRunID != runID || record.ControllerRunAttempt != runAttempt {
				return errors.New("hosted request ledger terminal update does not match its active controller")
			}
			record.Status = "failed"
			if succeeded {
				record.Status = "succeeded"
			}
			record.CompletedAt = now.UTC()
			ledger.Requests = compactHostedRequestLedger(ledger.Requests, hostedRequestRetainedTerminals)
			return saveHostedRequestLedger(root, ledger)
		}
	}
	return errors.New("hosted request ledger lost the active request")
}

func ordinaryHostedOperation(operation string) bool {
	return operation == "cycle" || operation == "pause" || operation == "resume"
}

func compactHostedRequestLedger(records []hostedRequestRecord, terminalLimit int) []hostedRequestRecord {
	if terminalLimit < 0 {
		terminalLimit = 0
	}
	active := make([]hostedRequestRecord, 0, 1)
	terminal := make([]hostedRequestRecord, 0, len(records))
	for _, record := range records {
		if record.Status == "started" {
			active = append(active, record)
		} else {
			terminal = append(terminal, record)
		}
	}
	sort.Slice(terminal, func(i, j int) bool {
		if terminal[i].CompletedAt.Equal(terminal[j].CompletedAt) {
			return terminal[i].ID < terminal[j].ID
		}
		return terminal[i].CompletedAt.Before(terminal[j].CompletedAt)
	})
	if len(terminal) > terminalLimit {
		terminal = terminal[len(terminal)-terminalLimit:]
	}
	return append(terminal, active...)
}

func loadHostedRequestLedger(root string) (hostedRequestLedger, error) {
	path := filepath.Join(root, "integrated", "hosted-requests.json")
	data, err := readBoundedRegularFile(path, hostedRequestMaxBytes)
	if os.IsNotExist(err) {
		return hostedRequestLedger{SchemaVersion: hostedRequestSchema}, nil
	}
	if err != nil {
		return hostedRequestLedger{}, err
	}
	var identity struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &identity); err != nil {
		return hostedRequestLedger{}, errors.New("hosted request ledger is invalid")
	}
	var ledger hostedRequestLedger
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ledger); err != nil || (identity.SchemaVersion != hostedRequestSchema && identity.SchemaVersion != hostedRequestLegacySchema) {
		return hostedRequestLedger{}, errors.New("hosted request ledger is invalid")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return hostedRequestLedger{}, errors.New("hosted request ledger has trailing content")
	}
	if identity.SchemaVersion == hostedRequestLegacySchema {
		for index := range ledger.Requests {
			if ledger.Requests[index].Status == "completed" {
				ledger.Requests[index].Status = "succeeded"
			}
			ledger.Requests[index].MigratedLegacy = true
		}
		ledger.SchemaVersion = hostedRequestSchema
	}
	if len(ledger.Requests) > hostedRequestRetainedTerminals+1 {
		return hostedRequestLedger{}, errors.New("hosted request ledger exceeds its bounded retention")
	}
	seen := map[string]bool{}
	active := 0
	for _, record := range ledger.Requests {
		if seen[record.ID] || !hostedRequestPattern.MatchString(record.ID) || !ordinaryHostedOperation(record.Operation) || record.StartedAt.IsZero() {
			return hostedRequestLedger{}, errors.New("hosted request ledger contains an invalid or duplicate request")
		}
		seen[record.ID] = true
		switch record.Status {
		case "started":
			active++
			if !record.CompletedAt.IsZero() {
				return hostedRequestLedger{}, errors.New("active hosted request contains terminal state")
			}
		case "succeeded", "failed", "abandoned":
			if record.CompletedAt.IsZero() || record.CompletedAt.Before(record.StartedAt) {
				return hostedRequestLedger{}, errors.New("terminal hosted request has invalid timestamps")
			}
		default:
			return hostedRequestLedger{}, errors.New("hosted request ledger contains an invalid status")
		}
		if (record.ControllerRunID == 0 || record.ControllerRunAttempt == 0) && !record.MigratedLegacy {
			return hostedRequestLedger{}, errors.New("hosted request ledger lacks controller identity")
		}
		if record.MigratedLegacy && (record.ControllerRunID != 0 || record.ControllerRunAttempt != 0) {
			return hostedRequestLedger{}, errors.New("hosted request ledger contains an invalid legacy migration marker")
		}
	}
	if active > 1 {
		return hostedRequestLedger{}, errors.New("hosted request ledger contains multiple active requests")
	}
	return ledger, nil
}

func saveHostedRequestLedger(root string, ledger hostedRequestLedger) error {
	directory := filepath.Join(root, "integrated")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil || len(data) > hostedRequestMaxBytes {
		return errors.New("hosted request ledger exceeds its strict encoded bound")
	}
	path := filepath.Join(directory, "hosted-requests.json")
	temporary, err := os.CreateTemp(directory, ".hosted-requests-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

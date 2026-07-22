package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/internal/gittransport"
	"github.com/kubestellar/hive/v2/pkg/automation"
	"github.com/kubestellar/hive/v2/pkg/beads"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

type visualImportOutput struct {
	visualhive.Validation
	Applied bool `json:"applied"`
	Created int  `json:"created"`
	Skipped int  `json:"skipped"`
}

const (
	trustedVisualHiveWorkflowName = "Hive Visual Hive Production"
	trustedVisualHiveWorkflowPath = ".github/workflows/hive-visual-hive.yml"
)

var exactVisualHiveProducerCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)

type visualLifecycleOutput struct {
	visualhive.ApplyLifecycleResult
	PendingOutbox int                                    `json:"pendingOutbox"`
	Findings      int                                    `json:"findings"`
	Provenance    *hivegithub.VerifiedVisualHiveArtifact `json:"provenance,omitempty"`
}

func runVisualCommand(args []string) int {
	if len(args) >= 2 && args[0] == "lifecycle" {
		return runVisualLifecycleCommand(args[1:])
	}
	if len(args) == 0 || args[0] != "import" || len(args) < 2 || (args[1] != "validate" && args[1] != "apply") {
		fmt.Fprintln(os.Stderr, "usage: hive visual import <validate|apply> ... | hive visual lifecycle <fetch-apply|apply|sync|repair|status> ...")
		return 2
	}
	action := args[1]
	flags := flag.NewFlagSet("hive visual import "+action, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	bundlePath := flags.String("bundle", "", "path to a Visual Hive bundle manifest")
	beadsDir := flags.String("beads-dir", "/data/beads/quality", "target Hive beads directory")
	maxACMM := flags.Int("max-acmm", 3, "maximum ACMM level this Hive permits")
	allowLocal := flags.Bool("allow-local", false, "allow explicit local proof bundles (never use for an untrusted artifact)")
	flags.Bool("json", false, "emit machine-readable JSON")
	if err := parseExactFlags(flags, args[2:]); err != nil {
		return 2
	}
	if *bundlePath == "" {
		fmt.Fprintln(os.Stderr, "--bundle is required")
		return 2
	}
	bundle, err := visualhive.ValidateBundle(*bundlePath, visualhive.ValidationOptions{MaxACMM: *maxACMM, AllowLocal: *allowLocal})
	if err != nil {
		fmt.Fprintln(os.Stderr, "visual evidence rejected:", err)
		return 1
	}
	output := visualImportOutput{Validation: bundle.Validation}
	if action == "apply" {
		store, err := beads.NewStore(*beadsDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open beads store:", err)
			return 1
		}
		result, err := bundle.Import(store)
		if err != nil {
			fmt.Fprintln(os.Stderr, "import visual evidence:", err)
			return 1
		}
		output.Applied, output.Created, output.Skipped = true, result.Created, result.Skipped
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func runVisualLifecycleCommand(args []string) int {
	if len(args) == 0 || (args[0] != "fetch-apply" && args[0] != "apply" && args[0] != "status" && args[0] != "sync" && args[0] != "repair") {
		fmt.Fprintln(os.Stderr, "usage: hive visual lifecycle <fetch-apply|apply|sync|repair|status> [--repo owner/name] [--run-id N] [--artifact-id N] [--source-artifact-id N] [--visual-hive-ref <40-char-commit>] [--bundle <manifest.json>] [--state-dir <dir>] [--beads-dir <dir>] [--target-ref main] [--max-acmm 3] [--allow-local]")
		return 2
	}
	action := args[0]
	flags := flag.NewFlagSet("hive visual lifecycle "+action, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	bundlePath := flags.String("bundle", "", "path to a Visual Hive bundle manifest")
	stateDir := flags.String("state-dir", "/data/visual-hive", "persistent Visual Hive lifecycle directory")
	beadsDir := flags.String("beads-dir", "/data/beads/quality", "persistent Hive beads directory")
	targetRef := flags.String("target-ref", "main", "authoritative target branch/ref")
	maxACMM := flags.Int("max-acmm", 3, "maximum ACMM level this Hive permits")
	allowLocal := flags.Bool("allow-local", false, "allow explicit local proof bundles")
	repository := flags.String("repo", "", "GitHub repository owner/name allowed for lifecycle writes")
	automationMode := flags.String("automation", "issues", "advisory, issues, repair-pr, or auto-merge")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	workflowRunID := flags.Int64("run-id", 0, "GitHub Actions workflow run ID for trusted artifact ingestion")
	artifactID := flags.Int64("artifact-id", 0, "GitHub Actions artifact ID for trusted artifact ingestion")
	sourceArtifactID := flags.Int64("source-artifact-id", 0, "required GitHub Actions full evidence artifact ID bound inside the bundle")
	visualHiveRef := flags.String("visual-hive-ref", "", "required immutable 40-character Visual Hive producer commit")
	artifactDir := flags.String("artifact-dir", "/data/visual-hive/artifacts", "trusted artifact cache directory")
	paused := flags.Bool("paused", false, "deny lifecycle writes while repository automation is paused")
	killSwitch := flags.Bool("kill-switch", false, "deny all lifecycle writes")
	maxAttempts := flags.Int("max-repair-attempts", 3, "maximum model-backed repair attempts")
	fingerprint := flags.String("fingerprint", "", "repository-scoped finding fingerprint to repair")
	repositoryDir := flags.String("repo-dir", "", "local target repository clone used by the trusted repair worker")
	worktreeDir := flags.String("worktree-dir", "", "isolated repair worktree root (defaults under state-dir)")
	providerCommand := flags.String("provider-command", "codex", "model provider executable")
	flags.Bool("json", false, "emit machine-readable JSON")
	var providerArgs stringListFlag
	var allowedRepairPaths stringListFlag
	var checkJSON stringListFlag
	flags.Var(&providerArgs, "provider-arg", "native Codex option; repeat as needed (script/package-runner wrappers are rejected)")
	flags.Var(&allowedRepairPaths, "repair-path", "allowed repair path glob; repeatable")
	flags.Var(&checkJSON, "check-json", `required validation command as a JSON string array, for example ["npm","test"]; repeatable`)
	if err := parseExactFlags(flags, args[1:]); err != nil {
		return 2
	}
	lifecycle, err := visualhive.NewLifecycleStore(*stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open lifecycle store:", err)
		return 1
	}
	if action == "status" {
		return encodeJSON(lifecycle.Snapshot())
	}
	if action == "repair" {
		if strings.TrimSpace(*repository) == "" || strings.TrimSpace(*fingerprint) == "" || strings.TrimSpace(*repositoryDir) == "" {
			fmt.Fprintln(os.Stderr, "--repo, --fingerprint, and --repo-dir are required")
			return 2
		}
		mode, ok := parseAutomationMode(*automationMode)
		if !ok {
			fmt.Fprintln(os.Stderr, "--automation must be advisory, issues, repair-pr, or auto-merge")
			return 2
		}
		finding, exists := lifecycle.Finding(*fingerprint)
		if !exists {
			fmt.Fprintln(os.Stderr, "finding not found in persistent lifecycle state")
			return 1
		}
		if !strings.EqualFold(finding.Repository, *repository) {
			fmt.Fprintln(os.Stderr, "finding repository does not match --repo")
			return 1
		}
		checks, parseErr := parseRepairCommands(checkJSON)
		if parseErr != nil {
			fmt.Fprintln(os.Stderr, parseErr)
			return 2
		}
		if len(checks) == 0 {
			fmt.Fprintln(os.Stderr, "at least one --check-json command from the validated repository test plan is required")
			return 2
		}
		token := os.Getenv(*githubTokenEnv)
		if token == "" {
			fmt.Fprintf(os.Stderr, "%s is required for repair PR creation\n", *githubTokenEnv)
			return 2
		}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		client := hivegithub.NewClient(token, "", nil, logger, *githubAPIURL)
		repairState, stateErr := repair.NewStore(filepath.Join(*stateDir, "repair"))
		if stateErr != nil {
			fmt.Fprintln(os.Stderr, "open repair worker state:", stateErr)
			return 1
		}
		root := *worktreeDir
		if strings.TrimSpace(root) == "" {
			root = filepath.Join(*stateDir, "repair", "worktrees")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
		defer cancel()
		ctx = gittransport.WithControllerToken(ctx, token)
		baselineProtection, protectionErr := repair.InspectVisualBaselineProtection(ctx, *repositoryDir)
		if protectionErr != nil {
			fmt.Fprintln(os.Stderr, "inspect trusted visual baseline protection:", protectionErr)
			return 1
		}
		worker := repair.Worker{
			Config: repair.Config{
				RepositoryDir: *repositoryDir, WorktreeRoot: root, BaseBranch: strings.TrimPrefix(*targetRef, "refs/heads/"),
				ExpectedRemoteURL:  integrated.RepositoryCloneURL(*repository),
				BaselineProtection: baselineProtection,
				Policy: automation.Policy{
					ACMMLevel: *maxACMM, Mode: mode, Paused: *paused, KillSwitch: *killSwitch,
					AllowedRepositories: []string{*repository}, MaxRepairAttempts: *maxAttempts,
				},
				AllowedRepairPaths: append([]string(nil), allowedRepairPaths...), ValidationCommands: checks,
				ModelTimeout: 20 * time.Minute, CommandTimeout: 15 * time.Minute,
			},
			Provider: repair.CodexProvider{Command: *providerCommand, Prefix: append([]string(nil), providerArgs...)},
			State:    repairState, Lifecycle: lifecycle, GitHub: client,
		}
		result, runErr := worker.Run(ctx, finding)
		if runErr != nil {
			fmt.Fprintln(os.Stderr, "repair failed:", runErr)
			return 1
		}
		return encodeJSON(result)
	}
	if action == "sync" {
		if strings.TrimSpace(*repository) == "" {
			fmt.Fprintln(os.Stderr, "--repo is required")
			return 2
		}
		mode, ok := parseAutomationMode(*automationMode)
		if !ok {
			fmt.Fprintln(os.Stderr, "--automation must be advisory, issues, repair-pr, or auto-merge")
			return 2
		}
		token := os.Getenv(*githubTokenEnv)
		if token == "" {
			fmt.Fprintf(os.Stderr, "%s is required for GitHub lifecycle writes\n", *githubTokenEnv)
			return 2
		}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		client := hivegithub.NewClient(token, "", nil, logger, *githubAPIURL)
		beadStore, err := beads.NewStore(*beadsDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open beads store:", err)
			return 1
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		result := visualhive.ProcessOutbox(ctx, lifecycle, beadStore, automation.Policy{
			ACMMLevel: *maxACMM, Mode: mode, Paused: *paused, KillSwitch: *killSwitch,
			AllowedRepositories: []string{*repository}, MaxRepairAttempts: *maxAttempts,
		}, client)
		if result.Failed > 0 {
			_ = encodeJSON(result)
			return 1
		}
		return encodeJSON(result)
	}
	var bundle *visualhive.ValidatedBundle
	var provenance *hivegithub.VerifiedVisualHiveArtifact
	if action == "fetch-apply" {
		if err := validateVisualFetchApplyIdentity(*repository, *workflowRunID, *artifactID, *sourceArtifactID, *visualHiveRef); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		token := os.Getenv(*githubTokenEnv)
		if token == "" {
			fmt.Fprintf(os.Stderr, "%s is required for trusted artifact ingestion\n", *githubTokenEnv)
			return 2
		}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		client := hivegithub.NewClient(token, "", nil, logger, *githubAPIURL)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		verifiedBundle, verified, fetchErr := client.FetchAndVerifyVisualHiveBundle(ctx, hivegithub.VisualHiveArtifactRequest{
			Repository: *repository, WorkflowRunID: *workflowRunID, ArtifactID: *artifactID, SourceArtifactID: *sourceArtifactID,
			FetchSourceArtifact: true, DestinationDir: *artifactDir, TargetRef: *targetRef, MaxACMM: *maxACMM,
			ExpectedProducerGitCommit: *visualHiveRef,
			ExpectedWorkflowName:      trustedVisualHiveWorkflowName,
			ExpectedWorkflowPath:      trustedVisualHiveWorkflowPath,
			ExpectedEvent:             "workflow_dispatch",
		})
		if fetchErr != nil {
			fmt.Fprintln(os.Stderr, "trusted visual evidence rejected:", fetchErr)
			return 1
		}
		bundle = verifiedBundle
		provenance = &verified
	} else {
		if *bundlePath == "" {
			fmt.Fprintln(os.Stderr, "--bundle is required")
			return 2
		}
		validated, validateErr := visualhive.ValidateBundle(*bundlePath, visualhive.ValidationOptions{MaxACMM: *maxACMM, AllowLocal: *allowLocal})
		if validateErr != nil {
			fmt.Fprintln(os.Stderr, "visual evidence rejected:", validateErr)
			return 1
		}
		bundle = validated
	}
	beadStore, err := beads.NewStore(*beadsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open beads store:", err)
		return 1
	}
	applyOptions := visualhive.ApplyLifecycleOptions{TargetRef: *targetRef}
	if provenance != nil {
		applyOptions.VerificationRunID = provenance.WorkflowRunID
		applyOptions.VerificationURL = provenance.RunURL
	}
	result, err := lifecycle.ApplyBundle(bundle, beadStore, applyOptions)
	if err != nil {
		fmt.Fprintln(os.Stderr, "apply visual lifecycle:", err)
		return 1
	}
	snapshot := lifecycle.Snapshot()
	output := visualLifecycleOutput{ApplyLifecycleResult: result, PendingOutbox: len(lifecycle.PendingOutbox()), Findings: len(snapshot.Findings), Provenance: provenance}
	return encodeJSON(output)
}

func validateVisualFetchApplyIdentity(repository string, workflowRunID, artifactID, sourceArtifactID int64, expectedProducerCommit string) error {
	if strings.TrimSpace(repository) == "" || workflowRunID <= 0 || artifactID <= 0 {
		return fmt.Errorf("--repo, --run-id, and --artifact-id are required")
	}
	if sourceArtifactID <= 0 {
		return fmt.Errorf("--source-artifact-id is required for full trusted evidence verification")
	}
	if !exactVisualHiveProducerCommit.MatchString(strings.TrimSpace(expectedProducerCommit)) {
		return fmt.Errorf("--visual-hive-ref must be the exact 40-character Visual Hive producer commit")
	}
	return nil
}

func parseAutomationMode(value string) (automation.Mode, bool) {
	switch automation.Mode(value) {
	case automation.ModeAdvisory, automation.ModeIssues, automation.ModeRepairPR, automation.ModeAutoMerge:
		return automation.Mode(value), true
	default:
		return "", false
	}
}

func encodeJSON(value interface{}) int {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }
func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func parseRepairCommands(values []string) ([]repair.Command, error) {
	commands := make([]repair.Command, 0, len(values))
	for _, value := range values {
		var parts []string
		if err := json.Unmarshal([]byte(value), &parts); err != nil || len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
			return nil, fmt.Errorf("invalid --check-json %q: expected a non-empty JSON string array", value)
		}
		commands = append(commands, repair.Command{Name: parts[0], Args: append([]string(nil), parts[1:]...)})
	}
	return commands, nil
}

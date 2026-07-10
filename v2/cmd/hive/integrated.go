package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func runIntegratedCommand(command string, args []string) int {
	switch command {
	case "setup":
		return runSetupCommand(args)
	case "status":
		return runIntegratedStatus(args)
	case "doctor":
		return runIntegratedDoctor(args)
	case "pause", "resume":
		return runIntegratedPause(command, args)
	case "set-coverage", "set-automation":
		return runIntegratedSetting(command, args)
	case "run":
		return runIntegratedRun(args)
	case "upgrade", "rollback", "uninstall":
		return runIntegratedManagement(command, args)
	default:
		fmt.Fprintf(os.Stderr, "unknown integrated Hive command %q\n", command)
		return 2
	}
}

func runIntegratedManagement(command string, args []string) int {
	flags := flag.NewFlagSet("hive "+command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	visualRef := ""
	flags.StringVar(&visualRef, "version", "", "immutable Visual Hive commit SHA")
	flags.StringVar(&visualRef, "visual-hive-ref", "", "immutable Visual Hive commit SHA")
	deleteState := flags.Bool("delete-state", false, "permanently delete managed local state after opening the uninstall PR")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if command != "uninstall" && visualRef == "" && command != "rollback" {
		fmt.Fprintln(os.Stderr, "--visual-hive-ref is required")
		return 2
	}
	token := resolveGitHubToken(*githubTokenEnv)
	if token == "" {
		fmt.Fprintln(os.Stderr, "GitHub authorization is required")
		return 2
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), *githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	result, err := integrated.RunManagement(ctx, integrated.ManagementOptions{
		Operation: integrated.ManagementOperation(command), StateDir: *stateDir, VisualHiveRef: visualRef,
		DeleteState: *deleteState, GitHub: client,
	})
	if err != nil {
		if *jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": "hive.management.v1", "operation": command, "error": err.Error(), "partial": result})
		} else {
			fmt.Fprintln(os.Stderr, command+" failed:", err)
		}
		return 1
	}
	if *jsonOutput {
		return encodeJSON(result)
	}
	if result.Idempotent && result.PRURL == "" {
		fmt.Printf("%s already at the requested state.\n", command)
	} else {
		fmt.Printf("%s PR ready: %s\n", command, result.PRURL)
	}
	return 0
}

func runIntegratedRun(args []string) int {
	flags := flag.NewFlagSet("hive run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	timeout := flags.Duration("timeout", 45*time.Minute, "maximum hosted run and lifecycle duration")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	token := resolveGitHubToken(*githubTokenEnv)
	if token == "" {
		fmt.Fprintln(os.Stderr, "GitHub authorization is required")
		return 2
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := hivegithub.NewClient(token, "", nil, logger, *githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+time.Minute)
	defer cancel()
	result, err := integrated.RunOnce(ctx, integrated.RunOptions{StateDir: *stateDir, Timeout: *timeout, GitHub: client})
	if err != nil {
		if *jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": "hive.production-run.v1", "error": err.Error(), "partial": result})
		} else {
			fmt.Fprintln(os.Stderr, "Hive production run failed:", err)
		}
		return 1
	}
	if *jsonOutput {
		return encodeJSON(result)
	}
	fmt.Printf("Run %s completed: findings=%d issues-updated=%d repairs=%d\n", result.Workflow.RunURL, result.Lifecycle.Created+result.Lifecycle.Updated, result.Outbox.Succeeded, len(result.Repairs))
	return 0
}

func runSetupCommand(args []string) int {
	flags := flag.NewFlagSet("hive setup", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repository := flags.String("repo", "", "GitHub repository owner/name")
	coverageValue := flags.String("coverage", "", "essential, standard, comprehensive, or custom")
	automationValue := flags.String("automation", "", "advisory, issues, repair-pr, or auto-merge")
	provider := flags.String("provider", "codex", "repair model provider")
	providerCommand := flags.String("provider-command", "codex", "repair provider executable")
	visualHive := flags.Bool("visual-hive", true, "install Visual Hive deterministic testing")
	visualCommand := flags.String("visual-hive-command", "node", "Visual Hive CLI launcher")
	visualRepo := flags.String("visual-hive-repo", valueOrEnv("VISUAL_HIVE_REPOSITORY", "DavidDiaz0317/visual-hive"), "Visual Hive source repository")
	visualRef := flags.String("visual-hive-ref", os.Getenv("VISUAL_HIVE_REF"), "immutable Visual Hive commit SHA")
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	planOnly := flags.Bool("plan", false, "produce a read-only setup plan")
	start := flags.Bool("start", false, "start after the setup PR is merged and doctor is green")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	var providerArgs stringListFlag
	var visualArgs stringListFlag
	flags.Var(&providerArgs, "provider-arg", "repair provider launcher argument; repeatable")
	flags.Var(&visualArgs, "visual-hive-arg", "Visual Hive launcher argument before the CLI subcommand; repeatable")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if cli := os.Getenv("VISUAL_HIVE_CLI"); cli != "" && len(visualArgs) == 0 {
		visualArgs = append(visualArgs, cli)
	}
	if *repository == "" || *coverageValue == "" || *automationValue == "" {
		if !isInteractiveTerminal() {
			fmt.Fprintln(os.Stderr, "--repo, --coverage, and --automation are required in noninteractive mode")
			return 2
		}
		reader := bufio.NewReader(os.Stdin)
		if *repository == "" {
			*repository = prompt(reader, "Repository (owner/name)")
		}
		if *coverageValue == "" {
			*coverageValue = prompt(reader, "Coverage depth (essential, standard, comprehensive, custom)")
		}
		if *automationValue == "" {
			*automationValue = prompt(reader, "Automation authority (advisory, issues, repair-pr, auto-merge)")
		}
	}
	coverage := integrated.Coverage(strings.ToLower(strings.TrimSpace(*coverageValue)))
	automationMode := integrated.Automation(strings.ToLower(strings.TrimSpace(*automationValue)))
	token := resolveGitHubToken(*githubTokenEnv)
	var client *hivegithub.Client
	if token != "" {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		client = hivegithub.NewClient(token, "", nil, logger, *githubAPIURL)
	}
	if !*planOnly {
		if client == nil {
			fmt.Fprintf(os.Stderr, "GitHub authorization is required. Set %s or sign in once with gh auth login.\n", *githubTokenEnv)
			return 2
		}
	}
	acmm := acmmForIntegratedAutomation(automationMode)
	mode := automationModeForIntegrated(automationMode)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	result, err := integrated.RunSetup(ctx, integrated.SetupOptions{
		Repository: *repository, Coverage: coverage, Automation: automationMode, Provider: *provider,
		ProviderCommand: *providerCommand, ProviderArgs: append([]string(nil), providerArgs...),
		VisualHive: *visualHive, StateDir: *stateDir, Apply: !*planOnly, Start: *start,
		VisualHiveCommand: *visualCommand, VisualHiveArgs: append([]string(nil), visualArgs...),
		VisualHiveRepo: *visualRepo, VisualHiveRef: *visualRef, GitHub: client,
		Policy: automation.Policy{ACMMLevel: acmm, Mode: mode, AllowedRepositories: []string{*repository}, MaxRepairAttempts: 3},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup failed:", err)
		return 1
	}
	if *jsonOutput {
		return encodeJSON(result)
	}
	if result.Applied {
		fmt.Printf("Setup PR ready: %s\nPersistent state: %s\n", result.PRURL, *stateDir)
		if *start {
			fmt.Println("Start is pending setup-PR merge and a green hive doctor result.")
		}
	} else {
		fmt.Printf("Setup plan for %s: coverage=%s automation=%s ACMM=L%d\n", result.Plan.Repository, result.Plan.Coverage, result.Plan.Automation, result.Plan.ACMMLevel)
	}
	return 0
}

func runIntegratedStatus(args []string) int {
	flags := flag.NewFlagSet("hive status", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	store, err := integrated.NewStore(filepath.Join(*stateDir, "integrated"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	config, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Hive is not set up:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var client *hivegithub.Client
	if token := resolveGitHubToken(*githubTokenEnv); token != "" {
		client = hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), *githubAPIURL)
	}
	liveChecks := liveRepositoryChecks(ctx, client, config)
	ready := !config.Paused && len(config.VisualHiveRef) == 40
	for _, check := range liveChecks {
		ready = ready && check.OK
	}
	provider := repair.CodexProvider{Command: config.ProviderCommand, Prefix: config.ProviderArgs}
	providerCtx, providerCancel := context.WithTimeout(ctx, 45*time.Second)
	providerErr := provider.Health(providerCtx)
	providerCancel()
	ready = ready && providerErr == nil
	status := map[string]any{
		"schema_version": "hive.status.v1", "config": config, "paused": config.Paused, "production_ready": ready,
		"readiness_checks": liveChecks, "provider_ready": providerErr == nil, "provider_message": errorOr(providerErr, "provider authenticated"),
	}
	lifecyclePath := filepath.Join(*stateDir, "visual-hive", "visual-hive-lifecycle.json")
	if _, statErr := os.Stat(lifecyclePath); statErr == nil {
		if lifecycle, lifecycleErr := visualhive.NewLifecycleStore(filepath.Dir(lifecyclePath)); lifecycleErr == nil {
			snapshot := lifecycle.Snapshot()
			counts := map[visualhive.LifecycleStatus]int{}
			items := []map[string]any{}
			for _, finding := range snapshot.Findings {
				if finding == nil {
					continue
				}
				counts[finding.Status]++
				items = append(items, map[string]any{"fingerprint": finding.RepositoryFingerprint, "status": finding.Status, "issue_url": finding.IssueURL, "pr_url": finding.PRURL, "repair_attempts": finding.RepairAttempts})
			}
			status["lifecycle"] = map[string]any{"counts": counts, "findings": items, "pending_outbox": len(lifecycle.PendingOutbox())}
		}
	}
	repairPath := filepath.Join(*stateDir, "repair", "repair-worker-state.json")
	if _, statErr := os.Stat(repairPath); statErr == nil {
		if repairState, repairErr := repair.NewStore(filepath.Dir(repairPath)); repairErr == nil {
			status["repairs"] = repairState.Snapshot()
		}
	}
	if *jsonOutput {
		return encodeJSON(status)
	}
	fmt.Printf("%s: coverage=%s automation=%s ACMM=L%d paused=%t setup_pr=%s\n", config.Repository, config.Coverage, config.Automation, config.ACMMLevel, config.Paused, config.SetupPRURL)
	return 0
}

type doctorCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func runIntegratedDoctor(args []string) int {
	flags := flag.NewFlagSet("hive doctor", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	checks := []doctorCheck{}
	store, storeErr := integrated.NewStore(filepath.Join(*stateDir, "integrated"))
	config, configErr := integrated.Config{}, storeErr
	if storeErr == nil {
		config, configErr = store.Load()
	}
	checks = append(checks, doctorCheck{Name: "config", OK: configErr == nil, Message: errorOr(configErr, "persistent config loaded")})
	if configErr == nil {
		_, gitErr := os.Stat(filepath.Join(config.CheckoutDir, ".git"))
		checks = append(checks, doctorCheck{Name: "checkout", OK: gitErr == nil, Message: errorOr(gitErr, "managed checkout is present")})
		immutable := len(config.VisualHiveRef) == 40
		checks = append(checks, doctorCheck{Name: "visual_hive_pin", OK: immutable, Message: ternary(immutable, "Visual Hive is pinned to an immutable commit", "Visual Hive ref is not immutable")})
		provider := repair.CodexProvider{Command: config.ProviderCommand, Prefix: config.ProviderArgs}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		providerErr := provider.Health(ctx)
		cancel()
		checks = append(checks, doctorCheck{Name: "provider", OK: providerErr == nil, Message: errorOr(providerErr, "Codex provider is authenticated")})
		var client *hivegithub.Client
		if token := resolveGitHubToken(*githubTokenEnv); token != "" {
			client = hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), *githubAPIURL)
		}
		ctx, cancelLive := context.WithTimeout(context.Background(), 45*time.Second)
		checks = append(checks, liveRepositoryChecks(ctx, client, config)...)
		cancelLive()
	}
	ready := true
	for _, check := range checks {
		ready = ready && check.OK
	}
	output := map[string]any{"schema_version": "hive.doctor.v1", "production_ready": ready, "checks": checks}
	if *jsonOutput {
		_ = encodeJSON(output)
	} else {
		for _, check := range checks {
			fmt.Printf("[%s] %s: %s\n", ternary(check.OK, "ok", "fail"), check.Name, check.Message)
		}
	}
	if !ready {
		return 1
	}
	return 0
}

func liveRepositoryChecks(ctx context.Context, client *hivegithub.Client, config integrated.Config) []doctorCheck {
	if client == nil || client.GoGitHub() == nil {
		return []doctorCheck{{Name: "github_auth", OK: false, Message: "GitHub authorization is required"}}
	}
	owner, repo, ok := strings.Cut(config.Repository, "/")
	if !ok {
		return []doctorCheck{{Name: "github_auth", OK: false, Message: "configured repository is invalid"}}
	}
	checks := []doctorCheck{}
	metadata, _, repoErr := client.GoGitHub().Repositories.Get(ctx, owner, repo)
	checks = append(checks, doctorCheck{Name: "github_auth", OK: repoErr == nil, Message: errorOr(repoErr, "GitHub repository access verified")})
	if repoErr != nil {
		return checks
	}
	if metadata.GetID() > 0 && config.RepositoryID != "" && fmt.Sprintf("%d", metadata.GetID()) != config.RepositoryID {
		checks = append(checks, doctorCheck{Name: "repository_identity", OK: false, Message: "GitHub repository ID no longer matches persistent configuration"})
	} else {
		checks = append(checks, doctorCheck{Name: "repository_identity", OK: true, Message: "repository identity matches"})
	}
	setupMerged := false
	setupMessage := "setup PR is missing"
	if config.SetupPRNumber > 0 {
		pull, _, pullErr := client.GoGitHub().PullRequests.Get(ctx, owner, repo, config.SetupPRNumber)
		setupMerged = pullErr == nil && pull.GetMerged()
		setupMessage = errorOr(pullErr, ternary(setupMerged, "setup PR is merged", "setup PR has not been merged"))
	}
	checks = append(checks, doctorCheck{Name: "setup_pr_merged", OK: setupMerged, Message: setupMessage})
	content, _, _, workflowErr := client.GoGitHub().Repositories.GetContents(ctx, owner, repo, ".github/workflows/hive-visual-hive.yml", &gh.RepositoryContentGetOptions{Ref: config.DefaultBranch})
	checks = append(checks, doctorCheck{Name: "workflow_installed", OK: workflowErr == nil && content != nil, Message: errorOr(workflowErr, "production workflow is installed on the target branch")})
	if config.Automation == integrated.AutomationAutoMerge {
		protection, protectionErr := client.BranchProtection(ctx, config.Repository, config.DefaultBranch)
		protected := protectionErr == nil && protection.Enabled && len(protection.RequiredChecks) > 0
		message := errorOr(protectionErr, fmt.Sprintf("protection=%t required_checks=%v required_reviews=%d", protection.Enabled, protection.RequiredChecks, protection.RequiredReviews))
		checks = append(checks, doctorCheck{Name: "branch_protection", OK: protected, Message: message})
	}
	return checks
}

func runIntegratedPause(command string, args []string) int {
	flags := flag.NewFlagSet("hive "+command, flag.ContinueOnError)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	store, err := integrated.NewStore(filepath.Join(*stateDir, "integrated"))
	if err != nil {
		return 1
	}
	config, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	config.Paused = command == "pause"
	if err := store.Save(config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	store.Audit(integrated.AuditEntry{Action: command, Allowed: true, Repository: config.Repository})
	if *jsonOutput {
		return encodeJSON(map[string]any{"repository": config.Repository, "paused": config.Paused})
	}
	fmt.Printf("Hive automation for %s is %s.\n", config.Repository, ternary(config.Paused, "paused", "active"))
	return 0
}

func runIntegratedSetting(command string, args []string) int {
	flags := flag.NewFlagSet("hive "+command, flag.ContinueOnError)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	value := flags.String("value", "", "new setting value")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil || *value == "" {
		fmt.Fprintln(os.Stderr, "--value is required")
		return 2
	}
	store, err := integrated.NewStore(filepath.Join(*stateDir, "integrated"))
	if err != nil {
		return 1
	}
	config, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if command == "set-coverage" {
		candidate := integrated.Coverage(*value)
		if candidate != integrated.CoverageEssential && candidate != integrated.CoverageStandard && candidate != integrated.CoverageComprehensive && candidate != integrated.CoverageCustom {
			fmt.Fprintln(os.Stderr, "invalid coverage")
			return 2
		}
		config.Coverage = candidate
	} else {
		candidate := integrated.Automation(*value)
		if candidate != integrated.AutomationAdvisory && candidate != integrated.AutomationIssues && candidate != integrated.AutomationRepairPR && candidate != integrated.AutomationAutoMerge {
			fmt.Fprintln(os.Stderr, "invalid automation")
			return 2
		}
		config.Automation, config.ACMMLevel = candidate, acmmForIntegratedAutomation(candidate)
	}
	if err := store.Save(config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	store.Audit(integrated.AuditEntry{Action: command, Allowed: true, Repository: config.Repository, Detail: *value})
	if *jsonOutput {
		return encodeJSON(config)
	}
	fmt.Printf("%s updated to %s for %s.\n", command, *value, config.Repository)
	return 0
}

func resolveGitHubToken(envName string) string {
	if token := strings.TrimSpace(os.Getenv(envName)); token != "" {
		return token
	}
	command := exec.Command("gh", "auth", "token")
	command.Stderr = io.Discard
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func defaultIntegratedStateDir() string {
	if value := os.Getenv("HIVE_STATE_DIR"); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".hive"
	}
	return filepath.Join(home, ".hive")
}

func isInteractiveTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func prompt(reader *bufio.Reader, label string) string {
	fmt.Fprintf(os.Stderr, "%s: ", label)
	value, _ := reader.ReadString('\n')
	return strings.TrimSpace(value)
}

func acmmForIntegratedAutomation(value integrated.Automation) int {
	switch value {
	case integrated.AutomationAdvisory:
		return 2
	case integrated.AutomationIssues:
		return 4
	case integrated.AutomationRepairPR:
		return 5
	case integrated.AutomationAutoMerge:
		return 6
	default:
		return 1
	}
}

func automationModeForIntegrated(value integrated.Automation) automation.Mode {
	switch value {
	case integrated.AutomationIssues:
		return automation.ModeIssues
	case integrated.AutomationRepairPR:
		return automation.ModeRepairPR
	case integrated.AutomationAutoMerge:
		return automation.ModeAutoMerge
	default:
		return automation.ModeAdvisory
	}
}

func valueOrEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func errorOr(err error, success string) string {
	if err != nil {
		return err.Error()
	}
	return success
}

func ternary[T any](condition bool, yes, no T) T {
	if condition {
		return yes
	}
	return no
}

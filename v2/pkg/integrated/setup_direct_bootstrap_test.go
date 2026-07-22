package integrated

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
	"github.com/kubestellar/hive/v2/pkg/checkpoint"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

func TestDirectBootstrapInstallsExactDefaultHeadWithoutSetupPullRequest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required to exercise the installed Visual Hive bundle")
	}
	const visualRef = "3e8e02bf676d702a658cf2426f78229126175578"
	root := t.TempDir()
	entrypoint := writeSetupAcceptanceVisualBundle(t, filepath.Join(root, "visual-hive"), visualRef)
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	runIntegratedGit(t, root, "init", "--bare", remote)
	runIntegratedGit(t, root, "init", "-b", "main", seed)
	writeFixture(t, seed, "README.md", "# Disposable Visual Hive demo\n")
	writeFixture(t, seed, "package.json", "{\"scripts\":{\"test\":\"node --test\"}}\n")
	runIntegratedGit(t, seed, "add", ".")
	runIntegratedGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "seed")
	runIntegratedGit(t, seed, "remote", "add", "origin", remote)
	runIntegratedGit(t, seed, "push", "-u", "origin", "main")
	runIntegratedGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	seedSHA := strings.TrimSpace(integratedGitOutput(t, seed, "rev-parse", "HEAD"))

	api := newDirectBootstrapGitHub(t, remote, visualRef)
	defer api.server.Close()
	client := hivegithub.NewClientForTest(api.server.URL, "owner", []string{"repo"}, slog.Default())
	previousCloneURL := setupRepositoryCloneURL
	setupRepositoryCloneURL = func(string) string { return remote }
	t.Cleanup(func() { setupRepositoryCloneURL = previousCloneURL })
	stateDir := filepath.Join(root, "hive-direct-state")
	options := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageEssential, Automation: AutomationRepairPR,
		Provider: "codex", ProviderCommand: os.Args[0], ExecutionMode: ExecutionLocal,
		RunInterval: time.Minute, VisualHive: true, StateDir: stateDir, Apply: true,
		DirectBootstrap: true, ExpectedSeedSHA: seedSHA, Start: false,
		VisualHiveCommand: node, VisualHiveArgs: []string{entrypoint},
		VisualHiveRepo: "DavidDiaz0317/visual-hive", VisualHiveRef: visualRef,
		MaxActiveIssues: 5, MaxRepairAttempts: 4, GitHub: client,
		Policy: automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 4},
	}

	result, err := RunSetup(context.Background(), options)
	if err != nil {
		t.Fatalf("direct bootstrap: %v", err)
	}
	if !result.Applied || result.Idempotent || result.Config == nil || result.Branch != "" || result.PRNumber != 0 || result.PRURL != "" {
		t.Fatalf("direct-bootstrap result = %+v", result)
	}
	if result.Config.SetupBranch != "main" || result.Config.SetupPRNumber != 0 || result.Config.SetupPRURL != "" || result.Config.SetupHeadSHA != result.CommitSHA {
		t.Fatalf("direct-bootstrap durable setup identity = %+v", result.Config)
	}
	if strings.Join(result.Config.ProviderArgs, "\x00") != "--model=gpt-5.6-sol" {
		t.Fatalf("direct-bootstrap setup did not persist the normalized provider model: %v", result.Config.ProviderArgs)
	}
	options, err = NormalizeNormalHiveProvider(options)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(integratedGitOutput(t, remote, "rev-parse", "refs/heads/main")); got != result.CommitSHA {
		t.Fatalf("default head = %s, want %s", got, result.CommitSHA)
	}
	if got := strings.TrimSpace(integratedGitOutput(t, remote, "rev-list", "--count", "refs/heads/main")); got != "2" {
		t.Fatalf("direct-bootstrap history count = %s, want seed + setup", got)
	}
	if branches := strings.TrimSpace(integratedGitOutput(t, remote, "for-each-ref", "--format=%(refname)", "refs/heads")); branches != "refs/heads/main" {
		t.Fatalf("direct bootstrap created an unexpected remote branch: %q", branches)
	}
	api.mu.Lock()
	postPulls, getPulls := api.postPulls, api.getPulls
	api.mu.Unlock()
	if postPulls != 0 || getPulls < 2 {
		t.Fatalf("pull request API calls: GET=%d POST=%d", getPulls, postPulls)
	}
	if err := VerifyInstalledSetup(context.Background(), client, *result.Config); err != nil {
		t.Fatalf("verify direct-bootstrap installation: %v", err)
	}
	audit, err := os.ReadFile(filepath.Join(stateDir, "integrated", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	auditText := string(audit)
	for _, expected := range []string{"direct_bootstrap_authorized", "direct_bootstrap_completed", result.CommitSHA, `\"setup_pr_number\":0`} {
		if !strings.Contains(auditText, expected) {
			t.Fatalf("direct-bootstrap audit lacks %q: %s", expected, auditText)
		}
	}

	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	durable, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	proof := directBootstrapProof{RepositoryID: durable.RepositoryID, DefaultBranch: durable.DefaultBranch, BaseSHA: seedSHA, SeedCommits: 1}
	intent, err := newDirectBootstrapIntent(context.Background(), options, durable, proof, durable.CheckoutDir, durable.SetupHeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	intentDetail, err := directBootstrapAuditDetail(intent)
	if err != nil {
		t.Fatal(err)
	}
	if recorded, err := directBootstrapAuditRecorded(store, "direct_bootstrap_authorized", durable.Repository, intentDetail); err != nil || !recorded {
		audit, _ := os.ReadFile(store.auditPath)
		t.Fatalf("reconstructed intent lost pre-push audit binding: recorded=%t err=%v\nwant=%s\naudit=%s", recorded, err, intentDetail, audit)
	}

	// Simulate a crash after the exact leased push but before config save. The
	// prepared intent and pre-push audit are durable; retry must observe the
	// exact published head, skip a second push/PR, verify, and finalize.
	removeDirectBootstrapAuditAction(t, store.auditPath, "direct_bootstrap_completed")
	if err := store.saveDirectBootstrapIntent(intent); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.configPath); err != nil {
		t.Fatal(err)
	}
	pushes := 0
	recoveryContext := checkpoint.WithCallback(context.Background(), func(_ context.Context, detail string) error {
		if detail == "Git push" {
			pushes++
		}
		return nil
	})
	recovered, err := RunSetup(recoveryContext, options)
	if err != nil {
		t.Fatalf("recover pushed-before-config direct bootstrap: %v", err)
	}
	if !recovered.Applied || !recovered.Idempotent || recovered.CommitSHA != result.CommitSHA || pushes != 0 {
		t.Fatalf("pushed-before-config recovery = %+v pushes=%d", recovered, pushes)
	}

	// Simulate config save succeeding before the completion audit. The exact
	// config-saved intent must flush only that receipt and perform no remote
	// mutation.
	durable, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	intent, err = newDirectBootstrapIntent(context.Background(), options, durable, proof, durable.CheckoutDir, durable.SetupHeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	intent.Phase = directBootstrapConfigSaved
	removeDirectBootstrapAuditAction(t, store.auditPath, "direct_bootstrap_completed")
	if err := store.saveDirectBootstrapIntent(intent); err != nil {
		t.Fatal(err)
	}
	pushes = 0
	recovered, err = RunSetup(recoveryContext, options)
	if err != nil {
		t.Fatalf("recover config-before-audit direct bootstrap: %v", err)
	}
	if !recovered.Idempotent || pushes != 0 || countDirectBootstrapAuditAction(t, store.auditPath, "direct_bootstrap_completed") != 1 {
		t.Fatalf("config-before-audit recovery = %+v pushes=%d", recovered, pushes)
	}

	// A response-loss retry after full completion is an exact read/verify
	// replay bound to the completion receipt, never a new bootstrap.
	pushes = 0
	replayed, err := RunSetup(recoveryContext, options)
	if err != nil {
		t.Fatalf("replay completed direct bootstrap: %v", err)
	}
	if !replayed.Idempotent || replayed.CommitSHA != result.CommitSHA || pushes != 0 || countDirectBootstrapAuditAction(t, store.auditPath, "direct_bootstrap_completed") != 1 {
		t.Fatalf("completed replay = %+v pushes=%d", replayed, pushes)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.postPulls != 0 {
		t.Fatalf("direct-bootstrap recovery created %d pull requests", api.postPulls)
	}
}

func TestDirectBootstrapAdoptsReviewedSeedBaselinesThroughResponseLossReplay(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required to exercise the installed Visual Hive bundle")
	}
	const visualRef = "3e8e02bf676d702a658cf2426f78229126175578"
	const baselinePath = ".visual-hive/snapshots/dashboard__home__desktop.png"
	root := t.TempDir()
	entrypoint := writeSetupAcceptanceVisualBundle(t, filepath.Join(root, "visual-hive"), visualRef)
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	runIntegratedGit(t, root, "init", "--bare", remote)
	runIntegratedGit(t, root, "init", "-b", "main", seed)
	writeFixture(t, seed, "README.md", "# Reviewed Visual Hive baseline demo\n")
	writeFixture(t, seed, "package.json", "{\"scripts\":{\"test\":\"node --test\"}}\n")
	writeFixture(t, seed, "visual-hive.config.yaml", `project:
  name: direct-reviewed-baseline
  setupProfile: free-local
visual:
  baselinePlatform: shared
viewports:
  desktop:
    width: 1280
    height: 800
contracts:
  - id: dashboard
    screenshots:
      - name: home
        route: /
        viewport: desktop
targets: []
`)
	baselineBytes := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, []byte("reviewed-direct-bootstrap-baseline")...)
	writeFixtureBytes(t, seed, baselinePath, baselineBytes)
	runIntegratedGit(t, seed, "add", ".")
	runIntegratedGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "reviewed seed")
	runIntegratedGit(t, seed, "remote", "add", "origin", remote)
	runIntegratedGit(t, seed, "push", "-u", "origin", "main")
	runIntegratedGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	seedSHA := strings.TrimSpace(integratedGitOutput(t, seed, "rev-parse", "HEAD"))
	reviewedCandidates, reviewedDigest, err := validateDirectBootstrapReviewedBaselines(context.Background(), seed, seedSHA)
	if err != nil {
		t.Fatalf("bind reviewed seed baseline inventory: %v", err)
	}
	if len(reviewedCandidates) != 1 || reviewedCandidates[0].Path != baselinePath || reviewedCandidates[0].Bytes != int64(len(baselineBytes)) || reviewedDigest == "" {
		t.Fatalf("reviewed seed binding = candidates=%+v digest=%q", reviewedCandidates, reviewedDigest)
	}

	api := newDirectBootstrapGitHub(t, remote, visualRef)
	defer api.server.Close()
	client := hivegithub.NewClientForTest(api.server.URL, "owner", []string{"repo"}, slog.Default())
	previousCloneURL := setupRepositoryCloneURL
	setupRepositoryCloneURL = func(string) string { return remote }
	t.Cleanup(func() { setupRepositoryCloneURL = previousCloneURL })
	stateDir := filepath.Join(root, "hive-direct-reviewed-state")
	options := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageEssential, Automation: AutomationRepairPR,
		Provider: "codex", ProviderCommand: os.Args[0], ProviderArgs: []string{"--model=gpt-5.6-sol"}, ExecutionMode: ExecutionLocal,
		RunInterval: time.Minute, VisualHive: true, StateDir: stateDir, Apply: true,
		DirectBootstrap: true, AdoptReviewedBaselines: true, ExpectedSeedSHA: seedSHA,
		ReviewedBaselineDigest: reviewedDigest, Start: false,
		VisualHiveCommand: node, VisualHiveArgs: []string{entrypoint},
		VisualHiveRepo: "DavidDiaz0317/visual-hive", VisualHiveRef: visualRef,
		MaxActiveIssues: 5, MaxRepairAttempts: 4, GitHub: client,
		Policy: automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 4},
	}

	result, err := RunSetup(context.Background(), options)
	if err != nil {
		t.Fatalf("direct bootstrap with reviewed seed baselines: %v", err)
	}
	if !result.Applied || result.Idempotent || result.Config == nil || result.Branch != "" || result.PRNumber != 0 || result.PRURL != "" || result.SetupBaselinePending {
		t.Fatalf("reviewed-baseline direct-bootstrap result = %+v", result)
	}
	if !result.Plan.DirectBootstrap || !result.Plan.AdoptReviewedBaselines || result.Plan.ExpectedSeedSHA != seedSHA || result.Plan.ReviewedBaselineDigest != reviewedDigest {
		t.Fatalf("reviewed-baseline direct-bootstrap plan lost explicit bindings: %+v", result.Plan)
	}
	if result.Config.SetupBaselineRequired || len(result.Config.SetupBaselineContractDigest) != 64 || result.Config.SetupBaselineInitialDigest != reviewedDigest || !equalSetupBaselineCandidates(result.Config.SetupBaselineInitialCandidates, reviewedCandidates) {
		t.Fatalf("generated direct-bootstrap config lost reviewed baseline binding: %+v", result.Config)
	}
	if got := strings.TrimSpace(integratedGitOutput(t, remote, "rev-parse", "refs/heads/main")); got != result.CommitSHA {
		t.Fatalf("reviewed-baseline default head = %s, want %s", got, result.CommitSHA)
	}
	if got := strings.TrimSpace(integratedGitOutput(t, remote, "rev-list", "--count", "refs/heads/main")); got != "2" {
		t.Fatalf("reviewed-baseline bootstrap history count = %s, want seed + setup", got)
	}
	if branches := strings.TrimSpace(integratedGitOutput(t, remote, "for-each-ref", "--format=%(refname)", "refs/heads")); branches != "refs/heads/main" {
		t.Fatalf("reviewed-baseline bootstrap created an unexpected remote branch: %q", branches)
	}
	if got := integratedGitOutput(t, remote, "show", result.CommitSHA+":"+baselinePath); got != string(baselineBytes) {
		t.Fatalf("reviewed seed baseline bytes changed during direct bootstrap: got %x want %x", []byte(got), baselineBytes)
	}
	if err := VerifyInstalledSetupAtCommit(context.Background(), client, *result.Config, result.CommitSHA); err != nil {
		t.Fatalf("verify reviewed-baseline installation at exact head: %v", err)
	}

	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	durable, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if durable.SetupBaselineRequired || durable.SetupBaselineContractDigest != result.Config.SetupBaselineContractDigest || durable.SetupBaselineInitialDigest != reviewedDigest || !equalSetupBaselineCandidates(durable.SetupBaselineInitialCandidates, reviewedCandidates) {
		t.Fatalf("durable config lost reviewed baseline binding: %+v", durable)
	}
	if _, exists, err := store.loadDirectBootstrapIntent(); err != nil || exists {
		t.Fatalf("completed direct-bootstrap intent remains: exists=%t err=%v", exists, err)
	}
	if _, exists, err := store.LoadSetupBaselineIntent(); err != nil || exists {
		t.Fatalf("reviewed seed adoption created a baseline-capture intent: exists=%t err=%v", exists, err)
	}
	requestDigest, err := directBootstrapRequestDigest(options)
	if err != nil {
		t.Fatal(err)
	}
	configDigest, err := directBootstrapConfigDigest(durable)
	if err != nil {
		t.Fatal(err)
	}
	binding, found, err := findCompletedDirectBootstrapAudit(store, durable.Repository, requestDigest, configDigest, durable.SetupHeadSHA)
	if err != nil || !found {
		t.Fatalf("find reviewed-baseline completion receipt: found=%t err=%v", found, err)
	}
	if binding.SetupPRNumber != 0 || binding.ReviewedBaselineDigest != reviewedDigest || binding.ScreenshotContractDigest != durable.SetupBaselineContractDigest || !equalSetupBaselineCandidates(binding.ReviewedBaselines, reviewedCandidates) {
		t.Fatalf("completion receipt lost reviewed baseline binding: %+v", binding)
	}

	// The caller can lose the successful response after every durable effect.
	// An exact retry must verify and return the same installation without a
	// second push, PR, setup commit, baseline intent, or completion receipt.
	pushes := 0
	recoveryContext := checkpoint.WithCallback(context.Background(), func(_ context.Context, detail string) error {
		if detail == "Git push" {
			pushes++
		}
		return nil
	})
	replayed, err := RunSetup(recoveryContext, options)
	if err != nil {
		t.Fatalf("replay completed reviewed-baseline direct bootstrap: %v", err)
	}
	if !replayed.Applied || !replayed.Idempotent || replayed.Config == nil || replayed.CommitSHA != result.CommitSHA || replayed.PRNumber != 0 || replayed.PRURL != "" || pushes != 0 {
		t.Fatalf("reviewed-baseline response-loss replay = %+v pushes=%d", replayed, pushes)
	}
	if replayed.Config.SetupBaselineInitialDigest != reviewedDigest || !equalSetupBaselineCandidates(replayed.Config.SetupBaselineInitialCandidates, reviewedCandidates) {
		t.Fatalf("response-loss replay lost reviewed baseline binding: %+v", replayed.Config)
	}
	if got := strings.TrimSpace(integratedGitOutput(t, remote, "rev-parse", "refs/heads/main")); got != result.CommitSHA {
		t.Fatalf("response-loss replay changed default head: got %s want %s", got, result.CommitSHA)
	}
	if got := strings.TrimSpace(integratedGitOutput(t, remote, "rev-list", "--count", "refs/heads/main")); got != "2" {
		t.Fatalf("response-loss replay created an extra commit: history count %s", got)
	}
	if countDirectBootstrapAuditAction(t, store.auditPath, "direct_bootstrap_completed") != 1 {
		t.Fatalf("response-loss replay duplicated the completion receipt")
	}
	if _, exists, err := store.LoadSetupBaselineIntent(); err != nil || exists {
		t.Fatalf("response-loss replay created a baseline-capture intent: exists=%t err=%v", exists, err)
	}

	// The display fields in the durable receipt are redundant with its config
	// digest, but corruption must still fail closed instead of presenting a
	// different reviewed inventory during an otherwise matching replay.
	tampered := binding
	tampered.ReviewedBaselines = append([]SetupBaselineCandidate(nil), binding.ReviewedBaselines...)
	tampered.ReviewedBaselines[0].SHA256 = strings.Repeat("f", 64)
	tamperedDetail, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AuditStrict(AuditEntry{Action: "direct_bootstrap_completed", Allowed: true, Repository: durable.Repository, Detail: string(tamperedDetail)}); err != nil {
		t.Fatal(err)
	}
	pushes = 0
	if _, err := RunSetup(recoveryContext, options); err == nil || !strings.Contains(err.Error(), "receipt does not match") {
		t.Fatalf("tampered reviewed-baseline completion receipt error = %v", err)
	}
	if pushes != 0 {
		t.Fatalf("tampered completion receipt attempted %d pushes", pushes)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.postPulls != 0 {
		t.Fatalf("reviewed-baseline bootstrap/replay created %d pull requests", api.postPulls)
	}
}

func TestValidateDirectBootstrapOptionsFailClosed(t *testing.T) {
	valid := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageEssential, Automation: AutomationRepairPR,
		Provider: "test", ExecutionMode: ExecutionLocal, RunInterval: time.Minute,
		VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40),
		StateDir: filepath.Join(t.TempDir(), "direct-state"), MaxActiveIssues: 1, MaxRepairAttempts: 1,
		DirectBootstrap: true, ExpectedSeedSHA: strings.Repeat("b", 40),
	}
	if err := validateSetupOptions(valid); err != nil {
		t.Fatalf("valid direct-bootstrap options: %v", err)
	}
	if err := validateDirectBootstrapExpectedSeed(valid, directBootstrapProof{BaseSHA: strings.Repeat("c", 40)}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("reviewed seed drift error = %v", err)
	}
	tests := []struct {
		name string
		edit func(*SetupOptions)
		want string
	}{
		{name: "hosted", edit: func(value *SetupOptions) { value.ExecutionMode = ExecutionHosted }, want: "local execution"},
		{name: "advisory", edit: func(value *SetupOptions) { value.Automation = AutomationAdvisory }, want: "repair-pr"},
		{name: "start", edit: func(value *SetupOptions) { value.Start = true }, want: "cannot start"},
		{name: "missing seed", edit: func(value *SetupOptions) { value.ExpectedSeedSHA = "" }, want: "seed commit"},
		{name: "missing reviewed digest", edit: func(value *SetupOptions) { value.AdoptReviewedBaselines = true }, want: "inventory SHA-256"},
		{name: "adoption without direct", edit: func(value *SetupOptions) { value.DirectBootstrap = false; value.AdoptReviewedBaselines = true }, want: "restricted"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.edit(&candidate)
			if err := validateSetupOptions(candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDirectBootstrapReviewedBaselineInventoryIsExactAndBoundToSeed(t *testing.T) {
	root := t.TempDir()
	runIntegratedGit(t, root, "init", "-b", "main")
	writeFixture(t, root, "visual-hive.config.yaml", `project:
  name: direct-baseline
visual:
  baselinePlatform: platform
contracts:
  - id: dashboard visual
    screenshots:
      - name: home card
        viewport: desktop
`)
	baseline := ".visual-hive/snapshots/linux/dashboard-visual__home-card__desktop.png"
	writeFixtureBytes(t, root, baseline, append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, []byte("reviewed")...))
	runIntegratedGit(t, root, "add", ".")
	runIntegratedGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "reviewed seed")
	seedSHA := strings.TrimSpace(integratedGitOutput(t, root, "rev-parse", "HEAD"))

	candidates, digest, err := validateDirectBootstrapReviewedBaselines(context.Background(), root, seedSHA)
	if err != nil {
		t.Fatalf("validate reviewed seed baselines: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Path != baseline || candidates[0].SHA256 == "" || candidates[0].Bytes <= 8 || len(digest) != 64 {
		t.Fatalf("reviewed candidates=%+v digest=%s", candidates, digest)
	}
	review := SetupOptions{ReviewedBaselineDigest: digest}
	if err := validateDirectBootstrapReviewedDigest(review, digest); err != nil {
		t.Fatalf("exact reviewed baseline digest: %v", err)
	}
	review.ReviewedBaselineDigest = strings.Repeat("f", 64)
	if err := validateDirectBootstrapReviewedDigest(review, digest); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("reviewed baseline digest mismatch error = %v", err)
	}

	writeFixtureBytes(t, root, ".visual-hive/snapshots/linux/unrelated.png", append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, 'x'))
	runIntegratedGit(t, root, "add", ".")
	runIntegratedGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "unreviewed extra")
	extraSHA := strings.TrimSpace(integratedGitOutput(t, root, "rev-parse", "HEAD"))
	if _, _, err := validateDirectBootstrapReviewedBaselines(context.Background(), root, extraSHA); err == nil || !strings.Contains(err.Error(), "exactly 1") {
		t.Fatalf("extra baseline error = %v", err)
	}

	writeFixture(t, root, "visual-hive.config.yaml", "project:\n  name: no-screenshots\ncontracts: []\n")
	if _, _, err := validateDirectBootstrapReviewedBaselines(context.Background(), root, seedSHA); err == nil || !strings.Contains(err.Error(), "at least one screenshot") {
		t.Fatalf("no-contract adoption error = %v", err)
	}
}

func TestDirectBootstrapExactLeaseDoesNotOverwriteDrift(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	checkout := filepath.Join(root, "checkout")
	racer := filepath.Join(root, "racer")
	runIntegratedGit(t, root, "init", "--bare", remote)
	runIntegratedGit(t, root, "init", "-b", "main", seed)
	writeFixture(t, seed, "README.md", "seed\n")
	runIntegratedGit(t, seed, "add", ".")
	runIntegratedGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "seed")
	runIntegratedGit(t, seed, "remote", "add", "origin", remote)
	runIntegratedGit(t, seed, "push", "-u", "origin", "main")
	runIntegratedGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runIntegratedGit(t, root, "clone", remote, checkout)
	baseSHA := strings.TrimSpace(integratedGitOutput(t, checkout, "rev-parse", "HEAD"))
	writeFixture(t, checkout, "managed.txt", "candidate\n")
	runIntegratedGit(t, checkout, "add", ".")
	runIntegratedGit(t, checkout, "-c", "user.name=Hive", "-c", "user.email=hive@example.com", "commit", "-m", "candidate")
	headSHA := strings.TrimSpace(integratedGitOutput(t, checkout, "rev-parse", "HEAD"))

	runIntegratedGit(t, root, "clone", remote, racer)
	writeFixture(t, racer, "drift.txt", "drift\n")
	runIntegratedGit(t, racer, "add", ".")
	runIntegratedGit(t, racer, "-c", "user.name=Racer", "-c", "user.email=racer@example.com", "commit", "-m", "drift")
	runIntegratedGit(t, racer, "push", "origin", "main")
	driftSHA := strings.TrimSpace(integratedGitOutput(t, remote, "rev-parse", "refs/heads/main"))

	previousCloneURL := setupRepositoryCloneURL
	setupRepositoryCloneURL = func(string) string { return remote }
	t.Cleanup(func() { setupRepositoryCloneURL = previousCloneURL })
	if err := pushDirectBootstrapDefaultBranch(context.Background(), checkout, "owner/repo", "main", baseSHA, headSHA); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("drifted exact-lease push error = %v", err)
	}
	if got := strings.TrimSpace(integratedGitOutput(t, remote, "rev-parse", "refs/heads/main")); got != driftSHA {
		t.Fatalf("leased push overwrote drift: got %s want %s", got, driftSHA)
	}
}

func TestDirectBootstrapRepositoryShapeGatesAreFailClosed(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	checkout := filepath.Join(root, "checkout")
	runIntegratedGit(t, root, "init", "--bare", remote)
	runIntegratedGit(t, root, "init", "-b", "main", seed)
	writeFixture(t, seed, "README.md", "seed\n")
	runIntegratedGit(t, seed, "add", ".")
	runIntegratedGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "seed")
	runIntegratedGit(t, seed, "remote", "add", "origin", remote)
	runIntegratedGit(t, seed, "push", "-u", "origin", "main")
	runIntegratedGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runIntegratedGit(t, root, "clone", remote, checkout)

	api := newDirectBootstrapGitHub(t, remote, strings.Repeat("a", 40))
	defer api.server.Close()
	client := hivegithub.NewClientForTest(api.server.URL, "owner", []string{"repo"}, slog.Default())
	previousCloneURL := setupRepositoryCloneURL
	setupRepositoryCloneURL = func(string) string { return remote }
	t.Cleanup(func() { setupRepositoryCloneURL = previousCloneURL })
	if _, err := inspectDirectBootstrapTarget(context.Background(), client, "owner/repo", "123", checkout, "main"); err != nil {
		t.Fatalf("valid disposable repository shape: %v", err)
	}

	tests := []struct {
		name string
		set  func(*directBootstrapGitHub)
		want string
	}{
		{name: "public", set: func(api *directBootstrapGitHub) { api.private = false }, want: "private"},
		{name: "open pull", set: func(api *directBootstrapGitHub) { api.openPull = true }, want: "no open pull requests"},
		{name: "protected", set: func(api *directBootstrapGitHub) { api.protected = true }, want: "existing protection"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api.mu.Lock()
			api.private, api.openPull, api.protected = true, false, false
			test.set(api)
			api.mu.Unlock()
			if _, err := inspectDirectBootstrapTarget(context.Background(), client, "owner/repo", "123", checkout, "main"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("shape error = %v, want %q", err, test.want)
			}
		})
	}
}

func writeFixtureBytes(t *testing.T, root, relative string, data []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func removeDirectBootstrapAuditAction(t *testing.T, auditPath, action string) {
	t.Helper()
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	kept := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry AuditEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Action != action {
			kept = append(kept, line)
		}
	}
	output := ""
	if len(kept) > 0 {
		output = strings.Join(kept, "\n") + "\n"
	}
	if err := os.WriteFile(auditPath, []byte(output), 0o600); err != nil {
		t.Fatal(err)
	}
}

func countDirectBootstrapAuditAction(t *testing.T, auditPath, action string) int {
	t.Helper()
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry AuditEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Action == action {
			count++
		}
	}
	return count
}

type directBootstrapGitHub struct {
	server    *httptest.Server
	remote    string
	visualRef string
	mu        sync.Mutex
	getPulls  int
	postPulls int
	private   bool
	openPull  bool
	protected bool
}

func newDirectBootstrapGitHub(t *testing.T, remote, visualRef string) *directBootstrapGitHub {
	t.Helper()
	api := &directBootstrapGitHub{remote: remote, visualRef: visualRef, private: true}
	api.server = httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	return api
}

func (api *directBootstrapGitHub) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/user":
		_, _ = io.WriteString(writer, `{"id":456,"login":"setup-operator","type":"User"}`)
	case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
		api.mu.Lock()
		private := api.private
		api.mu.Unlock()
		_, _ = fmt.Fprintf(writer, `{"id":123,"full_name":"owner/repo","private":%t,"fork":false,"archived":false,"disabled":false,"default_branch":"main","permissions":{"admin":true,"push":true,"pull":true}}`, private)
	case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/languages":
		_, _ = io.WriteString(writer, `{"JavaScript":100}`)
	case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main/protection":
		api.mu.Lock()
		protected := api.protected
		api.mu.Unlock()
		if protected {
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"contexts":[]},"enforce_admins":{"enabled":true},"required_pull_request_reviews":{"required_approving_review_count":1}}`)
			return
		}
		http.Error(writer, `{"message":"not protected"}`, http.StatusNotFound)
	case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/rules/branches/main":
		http.Error(writer, `{"message":"no rules"}`, http.StatusNotFound)
	case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main":
		sha, err := setupAcceptanceGitOutput(api.remote, "rev-parse", "refs/heads/main")
		if err != nil {
			http.Error(writer, `{"message":"missing branch"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"name": "main", "commit": map[string]any{"sha": strings.TrimSpace(sha)}})
	case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
		api.mu.Lock()
		api.getPulls++
		openPull := api.openPull
		api.mu.Unlock()
		if openPull {
			_, _ = io.WriteString(writer, `[{"number":1,"state":"open"}]`)
			return
		}
		_, _ = io.WriteString(writer, `[]`)
	case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/pulls":
		api.mu.Lock()
		api.postPulls++
		api.mu.Unlock()
		http.Error(writer, `{"message":"direct bootstrap must not create a PR"}`, http.StatusConflict)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/repos/DavidDiaz0317/visual-hive/commits/"):
		ref := strings.TrimPrefix(request.URL.Path, "/repos/DavidDiaz0317/visual-hive/commits/")
		if ref != api.visualRef && ref != visualHivePullRequestProducerCommit {
			http.Error(writer, `{"message":"missing commit"}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(writer, `{"sha":"`+ref+`"}`)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/repos/owner/repo/contents/"):
		relative := strings.TrimPrefix(request.URL.Path, "/repos/owner/repo/contents/")
		ref := request.URL.Query().Get("ref")
		if ref == "" {
			ref = "main"
		}
		contents, err := setupAcceptanceGitOutput(api.remote, "show", ref+":"+relative)
		if err != nil {
			http.Error(writer, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(contents))})
	default:
		http.Error(writer, fmt.Sprintf(`{"message":%q}`, request.Method+" "+request.URL.Path), http.StatusNotFound)
	}
}

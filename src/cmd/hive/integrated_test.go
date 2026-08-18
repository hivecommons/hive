package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/automation"
	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/integrated"
	"github.com/kubestellar/hive/pkg/repair"
)

func TestAutoMergeProtectionDoctorDistinguishesSetupActivationAndExistingPolicy(t *testing.T) {
	config := integrated.Config{Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", Automation: integrated.AutomationAutoMerge}
	pendingSetup := autoMergeProtectionDoctorCheck(false, config, hivegithub.BranchProtectionSummary{}, 0, integrated.ProtectionActivationState{}, false, nil)
	if pendingSetup.OK || !strings.Contains(pendingSetup.Message, "merge the exact managed setup/upgrade PR") {
		t.Fatalf("doctor did not report pending setup: %+v", pendingSetup)
	}
	pendingActivation := autoMergeProtectionDoctorCheck(true, config, hivegithub.BranchProtectionSummary{}, 42, integrated.ProtectionActivationState{}, false, nil)
	if pendingActivation.OK || !strings.Contains(pendingActivation.Message, "protection activation is pending") {
		t.Fatalf("doctor did not report pending protection activation: %+v", pendingActivation)
	}
	ownedPolicy := hivegithub.BranchProtectionSummary{
		Enabled: true, Strict: true, AdminEnforced: true,
		RequiredChecks: []string{"visual-hive"}, RequiredCheckIdentities: []hivegithub.RequiredCheckIdentity{{Context: "visual-hive", AppID: -1}},
	}
	rejected := autoMergeProtectionDoctorCheck(true, config, ownedPolicy, 42, integrated.ProtectionActivationState{}, false, nil)
	if rejected.OK || !strings.Contains(rejected.Message, "Hive will not replace it") || !strings.Contains(rejected.Message, "App ID 42") {
		t.Fatalf("doctor did not explain repository-owned policy rejection: %+v", rejected)
	}
}

func TestOperationalStateStatusAndDoctorFailClosedOnUnreadableStores(t *testing.T) {
	stateDir := t.TempDir()
	for _, path := range []string{
		filepath.Join(stateDir, "visual-hive", "visual-hive-lifecycle.json"),
		filepath.Join(stateDir, "repair", "repair-worker-state.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{not-json\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	status := map[string]any{"production_ready": true}
	populateOperationalStateStatus(status, stateDir)
	if ready, _ := status["production_ready"].(bool); ready {
		t.Fatalf("status remained production-ready with invalid durable state: %+v", status)
	}
	for _, key := range []string{"lifecycle_state_error", "repair_state_error"} {
		if strings.TrimSpace(fmt.Sprint(status[key])) == "" {
			t.Fatalf("status omitted %s: %+v", key, status)
		}
	}

	checks := operationalStateDoctorChecks(stateDir)
	if len(checks) != 2 || checks[0].OK || checks[1].OK {
		t.Fatalf("doctor accepted invalid lifecycle or repair state: %+v", checks)
	}
}

func TestWorkflowDispatchDoctorFailsClosedOnAmbiguousIntent(t *testing.T) {
	store, err := integrated.NewStore(filepath.Join(t.TempDir(), "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	correlation := strings.Repeat("a", 64)
	now := time.Now().UTC()
	intent := integrated.WorkflowDispatchIntent{
		SchemaVersion: integrated.WorkflowDispatchSchema, Repository: "owner/repo", RepositoryID: "123",
		WorkflowFile: "hive-visual-hive.yml", Ref: "main", CorrelationID: correlation,
		ExpectedDisplayTitle: "Hive Visual Hive Production [" + correlation + "]", PreparedAt: now, DispatchAttemptedAt: now.Add(time.Second),
	}
	if err := store.SaveWorkflowDispatchIntent(intent); err != nil {
		t.Fatal(err)
	}
	check := workflowDispatchDoctorCheck(store)
	if check.OK || !strings.Contains(check.Message, "hive status --json") {
		t.Fatalf("doctor accepted or did not explain ambiguous dispatch recovery: %+v", check)
	}
}

func TestResumableRepairStatusReturnsSafeJSONTemplateAndRequiredAttestation(t *testing.T) {
	attempt := repair.Attempt{
		RepositoryFingerprint: "owner/repo:finding", Recurrence: 2, Attempt: 4,
		Stage: repair.StageFailed, LastFailureClass: repair.FailurePatchEngine, LastFailureID: "failure-4",
		RecoveredPatchAttempt: 3, RecoveredProviderSHA256: strings.Repeat("b", 64),
	}
	status := resumableRepairStatus(`C:\state`, attempt)
	wantAttestation := "attest-historical-read-only-codex:" + strings.Repeat("b", 64)
	if status["required_reason_attestation"] != wantAttestation {
		t.Fatalf("recovery status omitted exact provider attestation: %+v", status)
	}
	command := fmt.Sprint(status["next_command_template"])
	for _, required := range []string{"retry-repair", "--failure-class patch_engine", "--json", wantAttestation} {
		if !strings.Contains(command, required) {
			t.Fatalf("retry command template omitted %q: %s", required, command)
		}
	}
	if strings.Contains(command, "<reason>") || status["next_command"] != status["next_command_template"] {
		t.Fatalf("retry command is shell-unsafe or compatibility field drifted: %+v", status)
	}
}

func TestPublicRepairAndHostedDoctorStatusRedactRunnerInternals(t *testing.T) {
	attempt := repair.Attempt{
		Repository: "owner/repo", RepositoryFingerprint: "finding", Attempt: 2, Branch: "hive/repair-finding",
		Stage: repair.StageFailed, Provider: "codex", Worktree: `C:\runner\private\worktree`,
		ModelSummary: "provider-private-summary", ModelPatch: "provider-private-patch",
		PortableBundlePath: `/home/runner/private/repair.bundle`, LastFailure: "private stderr with /home/runner/path",
		StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	encoded, err := json.Marshal(publicRepairAttempt(attempt))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private\\worktree", "provider-private", "repair.bundle", "/home/runner/path", "model_patch", "worktree"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("public repair status exposed %q: %s", forbidden, encoded)
		}
	}
	for _, required := range []string{"finding", "hive/repair-finding", `"stage":"failed"`} {
		if !strings.Contains(string(encoded), required) {
			t.Fatalf("public repair status omitted %q: %s", required, encoded)
		}
	}

	checks := publicHostedDoctorChecks([]doctorCheck{
		{Name: "provider", OK: false, Message: "failed under /home/runner/private with raw stderr"},
		{Name: "visual_hive_pin", OK: true, Message: `C:\runner\visual-hive.mjs`},
	})
	checksJSON, err := json.Marshal(checks)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(checksJSON), "/home/runner") || strings.Contains(string(checksJSON), `C:\\runner`) || strings.Contains(string(checksJSON), "raw stderr") {
		t.Fatalf("public hosted doctor exposed runner diagnostics: %s", checksJSON)
	}
	if checks[0].OK || !strings.Contains(checks[0].Message, "OPENAI_API_KEY") || !checks[1].OK {
		t.Fatalf("public hosted doctor lost actionable readiness: %+v", checks)
	}
}

func TestResolveVisualHiveLauncherUsesPackagedRuntime(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "visual-hive")
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join(home, "visual-hive.mjs")
	if err := os.WriteFile(entrypoint, []byte("console.log('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nodeName := "node"
	if runtime.GOOS == "windows" {
		nodeName = "node.exe"
	}
	node := filepath.Join(runtimeDir, nodeName)
	if err := os.WriteFile(node, []byte("runtime"), 0o700); err != nil {
		t.Fatal(err)
	}

	command, args, err := resolveVisualHiveLauncher("", nil, home)
	if err != nil {
		t.Fatal(err)
	}
	if command != node || len(args) != 1 || args[0] != entrypoint {
		t.Fatalf("packaged launcher = %q %v", command, args)
	}
}

func TestValidateVisualHiveReleaseRejectsTampering(t *testing.T) {
	root := t.TempDir()
	entrypoint := filepath.Join(root, "visual-hive.mjs")
	contents := []byte("console.log('ok')\n")
	if err := os.WriteFile(entrypoint, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	manifest := map[string]any{
		"schemaVersion":     "visual-hive.release.v1",
		"name":              "visual-hive",
		"version":           "0.2.0",
		"gitCommit":         commit,
		"node":              ">=22",
		"entrypoint":        "visual-hive.mjs",
		"playwrightVersion": "1.60.0",
		"files": []map[string]any{{
			"path": "visual-hive.mjs", "sha256": fmt.Sprintf("%x", sha256.Sum256(contents)), "size": len(contents),
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release-manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, message := validateVisualHiveRelease(entrypoint, commit); !ok {
		t.Fatalf("valid release rejected: %s", message)
	}
	if err := os.WriteFile(entrypoint, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, message := validateVisualHiveRelease(entrypoint, commit); ok || !strings.Contains(message, "inventory") {
		t.Fatalf("tampered release = %t, %q", ok, message)
	}
}

func TestResolveSetupVisualHiveUsesPackagedManifestPinForPlanAndApply(t *testing.T) {
	root := t.TempDir()
	entrypoint := filepath.Join(root, "visual-hive.mjs")
	contents := []byte("console.log('ok')\n")
	if err := os.WriteFile(entrypoint, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("c", 40)
	manifest := map[string]any{
		"schemaVersion": integrated.VisualHiveReleaseSchema, "name": "visual-hive", "version": "0.3.0",
		"gitCommit": commit, "node": ">=22", "entrypoint": "visual-hive.mjs", "playwrightVersion": "1.60.0",
		"files": []map[string]any{{"path": "visual-hive.mjs", "sha256": fmt.Sprintf("%x", sha256.Sum256(contents)), "size": len(contents)}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release-manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	command, args, resolvedRef, err := resolveSetupVisualHive(os.Args[0], []string{entrypoint}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if command != os.Args[0] || len(args) != 1 || args[0] != entrypoint || resolvedRef != commit {
		t.Fatalf("resolved setup dependency = %q %v @ %s", command, args, resolvedRef)
	}
	if _, _, _, err := resolveSetupVisualHive(os.Args[0], []string{entrypoint}, "", strings.Repeat("d", 40)); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("configured pin mismatch was accepted: %v", err)
	}
}

func TestSetupPlanCLIResolvesAndEmitsInstalledVisualHiveDependency(t *testing.T) {
	const visualRef = "3e8e02bf676d702a658cf2426f78229126175578"
	const canonicalRemote = "https://github.com/owner/repo.git"
	t.Setenv("VISUAL_HIVE_REF", "")
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	runSetupPlanTestGit(t, root, "init", "--bare", remote)
	runSetupPlanTestGit(t, root, "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("plan fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runSetupPlanTestGit(t, seed, "add", "README.md")
	runSetupPlanTestGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "seed")
	runSetupPlanTestGit(t, seed, "remote", "add", "origin", remote)
	runSetupPlanTestGit(t, seed, "push", "-u", "origin", "main")
	runSetupPlanTestGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")

	entrypoint := filepath.Join(root, "visual-hive.mjs")
	contents := []byte("console.log('plan')\n")
	if err := os.WriteFile(entrypoint, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"schemaVersion": integrated.VisualHiveReleaseSchema, "name": "visual-hive", "version": "0.3.0",
		"gitCommit": visualRef, "node": ">=22", "entrypoint": "visual-hive.mjs", "playwrightVersion": "1.60.0",
		"files": []map[string]any{{"path": "visual-hive.mjs", "sha256": fmt.Sprintf("%x", sha256.Sum256(contents)), "size": len(contents)}},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release-manifest.json"), manifestData, 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo","default_branch":"main","permissions":{"pull":true}}`)
		case "/repos/owner/repo/languages":
			_, _ = io.WriteString(writer, `{}`)
		case "/repos/owner/repo/branches/main/protection":
			http.Error(writer, `{"message":"not protected"}`, http.StatusNotFound)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("HIVE_GITHUB_TOKEN", "test-token")
	stateDir := filepath.Join(root, "state")
	checkout := filepath.Join(stateDir, "integrated", "checkout")
	if err := os.MkdirAll(filepath.Dir(checkout), 0o700); err != nil {
		t.Fatal(err)
	}
	runSetupPlanTestGit(t, root, "clone", "--origin", "origin", remote, checkout)
	runSetupPlanTestGit(t, checkout, "remote", "set-url", "origin", canonicalRemote)
	stateOwner, _ := json.Marshal(map[string]any{"schema_version": "hive.state-owner.v1", "repository": "owner/repo", "repository_id": "123"})
	if err := os.WriteFile(filepath.Join(stateDir, ".hive-state-owner.json"), append(stateOwner, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	checkoutOwner, _ := json.Marshal(map[string]any{"schema_version": "hive.checkout-owner.v1", "repository": "owner/repo"})
	if err := os.WriteFile(filepath.Join(checkout, ".git", "hive-checkout-owner.json"), append(checkoutOwner, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	proxyRecord := installSetupPlanGitProxy(t, canonicalRemote, remote)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdout := os.Stdout
	os.Stdout = writer
	code := runSetupCommand([]string{
		"--repo", "owner/repo", "--coverage", "comprehensive", "--automation", "repair-pr", "--plan", "--json",
		"--runtime", "local",
		"--direct-bootstrap", "--adopt-reviewed-baselines",
		"--expected-seed-sha", strings.Repeat("a", 40), "--reviewed-baseline-digest", strings.Repeat("b", 64),
		"--state-dir", stateDir, "--github-api-url", server.URL,
		"--visual-hive-command", os.Args[0], "--visual-hive-arg", entrypoint,
	})
	_ = writer.Close()
	os.Stdout = originalStdout
	output, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if _, err := os.Stat(proxyRecord); err != nil {
		t.Fatalf("canonical setup transport did not use the local Git fixture: %v", err)
	}
	if code != 0 {
		t.Fatalf("setup --plan exit = %d, output=%s", code, output)
	}
	var result integrated.SetupResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode setup plan JSON: %v: %s", err, output)
	}
	if result.Applied || !result.Plan.ReadOnly || !result.Plan.DirectBootstrap || !result.Plan.AdoptReviewedBaselines || result.Plan.ExpectedSeedSHA != strings.Repeat("a", 40) || result.Plan.ReviewedBaselineDigest != strings.Repeat("b", 64) || result.Plan.StateDir != stateDir || result.Plan.VisualHiveRepository != defaultVisualHiveRepository || result.Plan.VisualHiveRef != visualRef {
		t.Fatalf("setup plan did not emit the installed dependency: %+v", result.Plan)
	}
}

func TestParseAutoMergeRiskFlags(t *testing.T) {
	values, err := parseAutoMergeRiskFlags([]string{"automatic", "LOW", "medium", "restricted"})
	if err != nil || len(values) != 4 || values[0] != automation.RiskAutomatic || values[3] != automation.RiskRestricted {
		t.Fatalf("auto-merge risks = %+v, %v", values, err)
	}
	if _, err := parseAutoMergeRiskFlags([]string{"critical"}); err == nil || !strings.Contains(err.Error(), "automatic, low, medium, or restricted") {
		t.Fatalf("unknown risk was accepted: %v", err)
	}
}

func TestPersistentSetupRejectsEnvironmentOnlyGitHubToken(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "ephemeral-test-token")
	original := githubCLIToken
	githubCLIToken = func() string { return "" }
	t.Cleanup(func() { githubCLIToken = original })
	if resolveGitHubToken("HIVE_GITHUB_TOKEN") == "" {
		t.Fatal("fixture did not establish environment-only setup authorization")
	}
	if err := requirePersistentGitHubAuthorization(false); err != nil {
		t.Fatalf("nonpersistent setup rejected an otherwise valid environment token: %v", err)
	}
	if err := requirePersistentGitHubAuthorization(true); err == nil || !strings.Contains(err.Error(), "gh auth login") || !strings.Contains(err.Error(), "environment-only") {
		t.Fatalf("persistent setup accepted an unavailable service credential: %v", err)
	}
}

func TestDeferredSchedulerIntentDoesNotStartBeforeDoctorPrerequisites(t *testing.T) {
	stateDir := t.TempDir()
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	config := integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: stateDir,
		CheckoutDir: filepath.Join(stateDir, "missing-checkout"), Paused: true,
		ProviderCommand: filepath.Join(stateDir, "missing-provider"), VisualHiveRef: strings.Repeat("a", 40),
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	if err := recordDeferredSchedulerStart(stateDir, config.Repository, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if status := readIntegratedDaemonStatus(stateDir); status.Running || status.RuntimeRunning || daemonServiceReady(status.Service) {
		t.Fatalf("recording --start installed or ran the scheduler before readiness: %+v", status)
	}
	activated, err := activateDeferredSchedulerIfReady(stateDir, "HIVE_TEST_MISSING_TOKEN", "")
	if err != nil {
		t.Fatal(err)
	}
	if activated {
		t.Fatal("scheduler activated while doctor prerequisites were red")
	}
	if _, exists, err := store.LoadSchedulerStartIntent(); err != nil || !exists {
		t.Fatalf("deferred scheduler intent was not preserved: exists=%t err=%v", exists, err)
	}
	if err := clearDeferredSchedulerStart(stateDir); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.LoadSchedulerStartIntent(); err != nil || exists {
		t.Fatalf("explicit stop semantics did not cancel deferred start: exists=%t err=%v", exists, err)
	}
}

func runSetupPlanTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func TestSetupPlanGitProxyProcess(t *testing.T) {
	if os.Getenv("HIVE_SETUP_PLAN_GIT_PROXY") != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		os.Exit(124)
	}
	canonical := os.Getenv("HIVE_SETUP_PLAN_CANONICAL_REMOTE")
	local := os.Getenv("HIVE_SETUP_PLAN_LOCAL_REMOTE")
	arguments := append([]string(nil), os.Args[separator+1:]...)
	rewritten := false
	for index, argument := range arguments {
		if argument == canonical {
			arguments[index] = local
			rewritten = true
		}
	}
	if rewritten && canonical != local {
		for index, argument := range arguments {
			if argument == "protocol.https.allow=always" {
				arguments[index] = "protocol.file.allow=always"
			}
		}
	}
	if rewritten {
		if err := os.WriteFile(os.Getenv("HIVE_SETUP_PLAN_GIT_PROXY_RECORD"), []byte("local\n"), 0o600); err != nil {
			os.Exit(123)
		}
	}
	command := exec.Command(os.Getenv("HIVE_SETUP_PLAN_REAL_GIT"), arguments...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		os.Exit(127)
	}
	os.Exit(0)
}

func installSetupPlanGitProxy(t *testing.T, canonicalRemote, localRemote string) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	name := "git"
	var script string
	if runtime.GOOS == "windows" {
		name += ".cmd"
		script = fmt.Sprintf("@echo off\r\n\"%s\" -test.run=TestSetupPlanGitProxyProcess -- %%*\r\n", executable)
	} else {
		script = fmt.Sprintf("#!/bin/sh\nexec %s -test.run=TestSetupPlanGitProxyProcess -- \"$@\"\n", setupPlanShellQuote(executable))
	}
	proxy := filepath.Join(bin, name)
	if err := os.WriteFile(proxy, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(proxy, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "canonical-transport-used")
	t.Setenv("HIVE_SETUP_PLAN_GIT_PROXY", "1")
	t.Setenv("HIVE_SETUP_PLAN_REAL_GIT", realGit)
	t.Setenv("HIVE_SETUP_PLAN_CANONICAL_REMOTE", canonicalRemote)
	t.Setenv("HIVE_SETUP_PLAN_LOCAL_REMOTE", localRemote)
	t.Setenv("HIVE_SETUP_PLAN_GIT_PROXY_RECORD", record)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

func setupPlanShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

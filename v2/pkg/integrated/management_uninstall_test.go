package integrated

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestUninstallDeleteStateFirstCallOnlyPreparesExactCleanup(t *testing.T) {
	fixture := newUninstallFixture(t)
	defer fixture.server.Close()

	result, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.StateDeleted || !result.FinalizationPending || result.PRNumber != 17 || result.CommitSHA == "" || result.NextCommand == "" || result.CancelCommand == "" {
		t.Fatalf("first uninstall result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(fixture.stateDir, "integrated", "config.json")); err != nil {
		t.Fatalf("first --delete-state removed durable state: %v", err)
	}
	store, err := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	intent, exists, err := store.LoadUninstallIntent()
	if err != nil || !exists || intent.Phase != uninstallPROpen || intent.PRNumber != 17 || !strings.EqualFold(intent.CleanupCommitSHA, result.CommitSHA) || intent.DiffDigest == "" {
		t.Fatalf("durable uninstall intent = %+v, exists=%t, err=%v", intent, exists, err)
	}
}

func TestUninstallWorkflowDispatchDrainRequiresExactBoundRun(t *testing.T) {
	for _, test := range []struct {
		name      string
		bindRun   bool
		runStatus string
		wantError bool
	}{
		{name: "completed bound run", bindRun: true, runStatus: "completed"},
		{name: "active bound run", bindRun: true, runStatus: "in_progress", wantError: true},
		{name: "ambiguous dispatch", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()
			store, err := NewStore(filepath.Join(stateDir, "integrated"))
			if err != nil {
				t.Fatal(err)
			}
			config := Config{Repository: "owner/repo", RepositoryID: "123"}
			intent, err := newWorkflowDispatchIntent(config, "hive-visual-hive.yml", "main")
			if err != nil {
				t.Fatal(err)
			}
			intent.DispatchAttemptedAt = time.Now().UTC()
			if test.bindRun {
				intent.DispatchAcknowledgedAt = intent.DispatchAttemptedAt.Add(time.Second)
				intent.RunID = 42
				intent.RunURL = "https://github.com/owner/repo/actions/runs/42"
				intent.MatchedAt = intent.DispatchAttemptedAt.Add(2 * time.Second)
			}
			if err := store.SaveWorkflowDispatchIntent(intent); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/repos/owner/repo/actions/runs/42" {
					t.Fatalf("unexpected workflow dispatch path %s", request.URL.Path)
				}
				conclusion := ""
				if test.runStatus == "completed" {
					conclusion = "success"
				}
				_, _ = fmt.Fprintf(writer, `{"id":42,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","head_sha":%q,"status":%q,"conclusion":%q,"html_url":"https://github.com/owner/repo/actions/runs/42"}`,
					intent.ExpectedDisplayTitle, strings.Repeat("a", 40), test.runStatus, conclusion)
			}))
			defer server.Close()
			client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
			err = drainWorkflowDispatchForUninstall(context.Background(), store, config, client)
			if test.wantError {
				if err == nil {
					t.Fatal("unsafe dispatch was retired")
				}
				if _, exists, loadErr := store.LoadWorkflowDispatchIntent(); loadErr != nil || !exists {
					t.Fatalf("unsafe dispatch was not preserved: exists=%t err=%v", exists, loadErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, exists, loadErr := store.LoadWorkflowDispatchIntent(); loadErr != nil || exists {
				t.Fatalf("exact bound dispatch was not retired: exists=%t err=%v", exists, loadErr)
			}
		})
	}
}

func TestPreparedUninstallResumeCreatesAuthorizationBeforeAdvancingPhase(t *testing.T) {
	fixture := newUninstallFixture(t)
	defer fixture.server.Close()
	fixture.mu.Lock()
	fixture.statusFailures = 1
	fixture.mu.Unlock()

	first, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, GitHub: fixture.client,
	})
	if err == nil || !strings.Contains(err.Error(), "authorize exact setup pull request status") || first.StateDeleted {
		t.Fatalf("first interrupted uninstall result=%+v err=%v", first, err)
	}
	store, storeErr := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	intent, exists, loadErr := store.LoadUninstallIntent()
	if loadErr != nil || !exists || intent.Phase != uninstallPrepared || intent.PRNumber != 0 {
		t.Fatalf("failed authorization advanced durable uninstall intent: %+v exists=%t err=%v", intent, exists, loadErr)
	}

	resumed, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, GitHub: fixture.client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.FinalizationPending || resumed.PRNumber != 17 || resumed.SetupAuthorizationStatusID != 901 || resumed.SetupAuthorizationCreatorID != 55 || resumed.SetupAuthorizationContext == "" {
		t.Fatalf("resumed uninstall lacks exact authorization proof: %+v", resumed)
	}
	intent, exists, loadErr = store.LoadUninstallIntent()
	if loadErr != nil || !exists || intent.Phase != uninstallPROpen || intent.PRNumber != 17 {
		t.Fatalf("authorized resume did not bind durable pull: %+v exists=%t err=%v", intent, exists, loadErr)
	}
}

func TestUninstallFinalizationRequiresUntamperedMergedEvidence(t *testing.T) {
	fixture := newUninstallFixture(t)
	defer fixture.server.Close()
	prepared, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, GitHub: fixture.client})
	if err != nil {
		t.Fatal(err)
	}
	fixture.merge(t, prepared.CommitSHA)
	fixture.mu.Lock()
	fixture.diff = "tampered diff\n"
	fixture.mu.Unlock()

	result, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client,
	})
	if err == nil || !strings.Contains(err.Error(), "diff") || result.StateDeleted {
		t.Fatalf("tampered finalization result=%+v err=%v", result, err)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.stateDir, "integrated", "config.json")); statErr != nil {
		t.Fatalf("tampered evidence deleted state: %v", statErr)
	}
}

func TestUninstallFinalizationDeletesOnlyAfterMergedCleanQuiescentTarget(t *testing.T) {
	fixture := newUninstallFixture(t)
	defer fixture.server.Close()
	prepared, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client})
	if err != nil {
		t.Fatal(err)
	}
	fixture.merge(t, prepared.CommitSHA)

	result, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.StateDeleted || result.FinalizationPending || !result.CleanupVerified || !result.ProtectionReconciled {
		t.Fatalf("final uninstall result = %+v", result)
	}
	if _, err := os.Stat(fixture.stateDir); !os.IsNotExist(err) {
		t.Fatalf("finalized state root still exists: %v", err)
	}
	if _, err := integratedGitOutputError(fixture.remote, "rev-parse", "refs/heads/"+prepared.Branch); err == nil {
		t.Fatalf("finalized uninstall cleanup branch %s still exists", prepared.Branch)
	}
}

func TestUninstallRemovesHiveCreatedBaselinesAndRestoresEmptyInventory(t *testing.T) {
	fixture := newUninstallFixture(t)
	defer fixture.server.Close()

	const baselinePath = ".visual-hive/snapshots/linux/new-install.png"
	clone := filepath.Join(filepath.Dir(fixture.remote), "baseline-install")
	runIntegratedGit(t, filepath.Dir(fixture.remote), "clone", fixture.remote, clone)
	baseline := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, []byte("hive-created-baseline")...)
	writeFixtureBytes(t, clone, baselinePath, baseline)
	runIntegratedGit(t, clone, "add", "-f", baselinePath)
	runIntegratedGit(t, clone, "-c", "user.name=Hive Setup", "-c", "user.email=hive-setup@example.test", "commit", "-m", "approve baseline")
	runIntegratedGit(t, clone, "push", "origin", "main")
	fixture.mu.Lock()
	fixture.baseSHA = strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", "refs/heads/main"))
	fixture.cleanupFiles = sortedUniquePaths(append(fixture.cleanupFiles, baselinePath))
	fixture.mu.Unlock()

	store, err := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	config.SetupBaselineInitialDigest, err = setupBaselineCandidateDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	config.SetupBaselineInitialCandidates = nil
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}

	prepared, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, GitHub: fixture.client,
	})
	if err != nil {
		t.Fatal(err)
	}
	intentStore, err := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	intent, exists, err := intentStore.LoadUninstallIntent()
	if err != nil || !exists {
		t.Fatalf("load uninstall intent: exists=%t err=%v", exists, err)
	}
	baselineChanged := false
	for _, changed := range intent.ChangedFiles {
		if changed == baselinePath {
			baselineChanged = true
			break
		}
	}
	if !baselineChanged {
		t.Fatalf("cleanup omitted Hive-created baseline: %v", intent.ChangedFiles)
	}
	checkout := filepath.Join(filepath.Dir(fixture.remote), "inspect-baseline-cleanup")
	runIntegratedGit(t, filepath.Dir(fixture.remote), "clone", fixture.remote, checkout)
	if _, err := integratedGitOutputError(checkout, "cat-file", "-e", prepared.CommitSHA+":"+baselinePath); err == nil {
		t.Fatalf("cleanup commit %s retained Hive-created baseline %s", prepared.CommitSHA, baselinePath)
	}

	fixture.mergeAsCommitNotInCheckout(t, prepared.CommitSHA)
	result, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.CleanupVerified || !result.StateDeleted {
		t.Fatalf("baseline-restoring uninstall result = %+v", result)
	}
}

func TestUninstallRestoresRepositoryOwnedBaselineFromExactSetupBase(t *testing.T) {
	fixture := newUninstallFixtureWithManagedSetup(t, false)
	defer fixture.server.Close()

	const baselinePath = ".visual-hive/snapshots/linux/repository-owned.png"
	originalBaseline := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, []byte("repository-owned-baseline")...)
	approvedBaseline := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, []byte("hive-approved-replacement")...)
	preinstall := filepath.Join(filepath.Dir(fixture.remote), "baseline-preinstall")
	runIntegratedGit(t, filepath.Dir(fixture.remote), "clone", fixture.remote, preinstall)
	writeFixtureBytes(t, preinstall, baselinePath, originalBaseline)
	runIntegratedGit(t, preinstall, "add", "-f", baselinePath)
	runIntegratedGit(t, preinstall, "-c", "user.name=Repository Owner", "-c", "user.email=owner@example.test", "commit", "-m", "add repository baseline")
	runIntegratedGit(t, preinstall, "push", "origin", "main")
	preinstallSHA := strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", "refs/heads/main"))

	setup := filepath.Join(filepath.Dir(fixture.remote), "baseline-setup")
	runIntegratedGit(t, filepath.Dir(fixture.remote), "clone", fixture.remote, setup)
	runIntegratedGit(t, setup, "switch", "-C", "hive/setup-123", "origin/main")
	for _, relative := range fixture.files {
		writeFixture(t, setup, relative, "managed "+relative+"\n")
	}
	runIntegratedGit(t, setup, "add", ".")
	runIntegratedGit(t, setup, "-c", "user.name=Hive Setup", "-c", "user.email=hive-setup@example.test", "commit", "-m", "install")
	runIntegratedGit(t, setup, "push", "--force", "origin", "hive/setup-123")
	setupHead := strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", "refs/heads/hive/setup-123"))
	runIntegratedGit(t, fixture.remote, "update-ref", "refs/heads/main", setupHead)

	approved := filepath.Join(filepath.Dir(fixture.remote), "baseline-approved")
	runIntegratedGit(t, filepath.Dir(fixture.remote), "clone", fixture.remote, approved)
	writeFixtureBytes(t, approved, baselinePath, approvedBaseline)
	runIntegratedGit(t, approved, "add", "-f", baselinePath)
	runIntegratedGit(t, approved, "-c", "user.name=Hive Setup", "-c", "user.email=hive-setup@example.test", "commit", "-m", "approve replacement baseline")
	runIntegratedGit(t, approved, "push", "origin", "main")
	approvedHead := strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", "refs/heads/main"))

	fixture.mu.Lock()
	fixture.setupBaseSHA = preinstallSHA
	fixture.setupHead = setupHead
	fixture.setupOpen, fixture.setupMerged, fixture.setupBranchExists = false, true, true
	fixture.managedPathPresent = true
	fixture.baseSHA = approvedHead
	fixture.cleanupFiles = sortedUniquePaths(append(fixture.cleanupFiles, baselinePath))
	fixture.mu.Unlock()

	store, err := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(originalBaseline)
	config.SetupHeadSHA = setupHead
	config.SetupBaselineInitialCandidates = []SetupBaselineCandidate{{
		Path: baselinePath, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(originalBaseline)),
	}}
	config.SetupBaselineInitialDigest, err = setupBaselineCandidateDigest(config.SetupBaselineInitialCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}

	prepared, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, GitHub: fixture.client,
	})
	if err != nil {
		t.Fatal(err)
	}
	restoredOID := strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", prepared.CommitSHA+":"+baselinePath))
	restored, err := readSetupBaselineBlob(context.Background(), fixture.remote, restoredOID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, originalBaseline) {
		t.Fatalf("cleanup restored baseline hash %x, want %x", sha256.Sum256(restored), sha256.Sum256(originalBaseline))
	}
	fixture.merge(t, prepared.CommitSHA)
	result, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.CleanupVerified || !result.StateDeleted {
		t.Fatalf("repository-baseline-restoring uninstall result = %+v", result)
	}
}

func TestUninstallFinalizationPreservesStateUntilCleanupBranchRetires(t *testing.T) {
	fixture := newUninstallFixture(t)
	defer fixture.server.Close()
	prepared, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, GitHub: fixture.client})
	if err != nil {
		t.Fatal(err)
	}
	fixture.merge(t, prepared.CommitSHA)
	fixture.mu.Lock()
	fixture.deleteFailures = 1
	fixture.mu.Unlock()

	result, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client,
	})
	if err == nil || !strings.Contains(err.Error(), "retire exact uninstall cleanup branch") || result.StateDeleted {
		t.Fatalf("failed cleanup branch retirement result=%+v err=%v", result, err)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.stateDir, "integrated", "config.json")); statErr != nil {
		t.Fatalf("cleanup branch retirement failure deleted state: %v", statErr)
	}
	if _, refErr := integratedGitOutputError(fixture.remote, "rev-parse", "refs/heads/"+prepared.Branch); refErr != nil {
		t.Fatalf("cleanup branch retirement failure did not preserve the managed ref: %v", refErr)
	}

	finalized, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client,
	})
	if err != nil || !finalized.StateDeleted {
		t.Fatalf("cleanup branch retirement retry result=%+v err=%v", finalized, err)
	}
	if _, refErr := integratedGitOutputError(fixture.remote, "rev-parse", "refs/heads/"+prepared.Branch); refErr == nil {
		t.Fatalf("cleanup branch retirement retry left managed ref %s", prepared.Branch)
	}
}

func TestInterruptedUninstallFinalizationRecoversOnlyExactOwnedIntent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if err := RestoreInterruptedUninstallFinalization(missing, "owner/repo"); err == nil {
		t.Fatal("missing interrupted state unexpectedly recovered")
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("failed recovery created state: %v", err)
	}

	fixture := newUninstallFixtureWithManagedSetup(t, false)
	defer fixture.server.Close()
	prepared, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client,
	})
	if err != nil || !prepared.FinalizationPending {
		t.Fatalf("prepare uninstall result=%+v err=%v", prepared, err)
	}
	store, err := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := markStateDeletionFinalizing(store); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.stateDir, "integrated", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("config was not sealed before recovery: %v", err)
	}
	if err := RestoreInterruptedUninstallFinalization(fixture.stateDir, "other/repo"); err == nil || !strings.Contains(err.Error(), "does not match repository") {
		t.Fatalf("mismatched repository recovery err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.stateDir, "integrated", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("rejected recovery restored config: %v", err)
	}
	if err := RestoreInterruptedUninstallFinalization(fixture.stateDir, "owner/repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err != nil {
		t.Fatalf("recovered config cannot be loaded: %v", err)
	}
	audit, err := os.ReadFile(filepath.Join(fixture.stateDir, "integrated", "audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), "uninstall_finalization_recovered") {
		t.Fatalf("recovery audit missing: err=%v\n%s", err, audit)
	}
}

func TestUninstallRetiresExactUnmergedSetupReviewIdempotently(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	config := Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		StateDir: stateDir, SetupBranch: "hive/setup-123", SetupHeadSHA: head,
		SetupPRNumber: 9, SetupPRURL: "https://example.test/owner/repo/pull/9",
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	intent := UninstallIntent{
		PreviousSetupBranch: "hive/setup-123", PreviousSetupPRNumber: 9,
		PreviousSetupPRURL: "https://example.test/owner/repo/pull/9",
	}
	open, branchExists := true, true
	closeCalls, deleteCalls := 0, 0
	repository := map[string]any{"id": 123, "full_name": "owner/repo"}
	pullJSON := func() map[string]any {
		state := "closed"
		if open {
			state = "open"
		}
		return map[string]any{
			"number": 9, "html_url": intent.PreviousSetupPRURL, "state": state, "merged": false,
			"title":         "Install Hive + Visual Hive production automation",
			"body":          "<!-- hive-setup: owner/repo -->\nsetup",
			"changed_files": 1,
			"head":          map[string]any{"ref": intent.PreviousSetupBranch, "sha": head, "repo": repository},
			"base":          map[string]any{"ref": "main", "sha": strings.Repeat("b", 40), "repo": repository},
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			writeUninstallJSON(t, writer, repository)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/9" && strings.Contains(request.Header.Get("Accept"), "diff"):
			writer.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(writer, "exact setup diff\n")
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/9":
			writeUninstallJSON(t, writer, pullJSON())
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/9/files":
			writeUninstallJSON(t, writer, []map[string]any{{"filename": "visual-hive.config.yaml", "status": "added"}})
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/pulls/9":
			closeCalls++
			open = false
			writeUninstallJSON(t, writer, pullJSON())
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/setup-123":
			if !branchExists {
				http.Error(writer, `{"message":"Not Found"}`, http.StatusNotFound)
				return
			}
			writeUninstallJSON(t, writer, map[string]any{"ref": "refs/heads/hive/setup-123", "object": map[string]any{"sha": head, "type": "commit"}})
		case request.Method == http.MethodDelete && request.URL.Path == "/repos/owner/repo/git/refs/heads/hive/setup-123":
			deleteCalls++
			branchExists = false
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, fmt.Sprintf(`{"message":"unexpected %s %s"}`, request.Method, request.URL.Path), http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if err := retirePreviousSetupReviewForUninstall(context.Background(), client, store, config, intent); err != nil {
		t.Fatal(err)
	}
	if open || branchExists || closeCalls != 1 || deleteCalls != 1 {
		t.Fatalf("setup retirement incomplete: open=%t branch=%t closes=%d deletes=%d", open, branchExists, closeCalls, deleteCalls)
	}
	if err := retirePreviousSetupReviewForUninstall(context.Background(), client, store, config, intent); err != nil {
		t.Fatal(err)
	}
	if closeCalls != 1 || deleteCalls != 1 {
		t.Fatalf("idempotent setup retirement repeated mutations: closes=%d deletes=%d", closeCalls, deleteCalls)
	}
	audit, err := os.ReadFile(filepath.Join(stateDir, "integrated", "audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), "authorize_uninstall_close_setup_pr") || !strings.Contains(string(audit), "uninstall_setup_review_retired") {
		t.Fatalf("setup retirement audit missing: err=%v\n%s", err, audit)
	}
}

func TestUninstallFinalizationPreservesStateForManagedPathOrPendingOutbox(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *uninstallFixture)
		want    string
	}{
		{name: "managed path restored", want: "still exists", prepare: func(_ *testing.T, fixture *uninstallFixture) {
			fixture.mu.Lock()
			fixture.managedPathPresent = true
			fixture.mu.Unlock()
		}},
		{name: "pending issue mutation", want: "outbox", prepare: func(t *testing.T, fixture *uninstallFixture) {
			dir := filepath.Join(fixture.stateDir, "visual-hive")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			state := visualhive.LifecycleState{
				SchemaVersion: visualhive.LifecycleSchema, UpdatedAt: time.Now().UTC(), Findings: map[string]*visualhive.FindingLifecycle{}, ReplayKeys: map[string]string{},
				Outbox: []*visualhive.OutboxEntry{{ID: "pending-close", Action: visualhive.OutboxCloseIssue, Repository: "owner/repo", RepositoryFingerprint: "finding", CreatedAt: time.Now().UTC()}},
			}
			data, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newUninstallFixture(t)
			defer fixture.server.Close()
			prepared, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, GitHub: fixture.client})
			if err != nil {
				t.Fatal(err)
			}
			fixture.merge(t, prepared.CommitSHA)
			test.prepare(t, fixture)
			result, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client})
			if err == nil || !strings.Contains(err.Error(), test.want) || result.StateDeleted {
				t.Fatalf("unsafe finalization result=%+v err=%v", result, err)
			}
			if _, statErr := os.Stat(filepath.Join(fixture.stateDir, "integrated", "config.json")); statErr != nil {
				t.Fatalf("unsafe finalization deleted state: %v", statErr)
			}
		})
	}
}

func TestUninstallPreparationDrainsActiveIssueOpenRepairAndExactBranch(t *testing.T) {
	fixture := newUninstallFixture(t)
	defer fixture.server.Close()
	fingerprint := strings.Repeat("e", 64)
	fixture.mu.Lock()
	fixture.issueOpen, fixture.repairOpen, fixture.repairBranchExists = true, true, true
	fixture.repairFingerprint, fixture.repairHead = fingerprint, fixture.baseSHA
	fixture.mu.Unlock()
	runIntegratedGit(t, fixture.remote, "update-ref", "refs/heads/hive/repair-proof-a1", fixture.baseSHA)
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Fingerprint: fingerprint, Status: visualhive.StatusPROpen,
		IssueNumber: 7, IssueURL: "https://example.test/issues/7", Branch: "hive/repair-proof-a1", RepairCommitSHA: fixture.baseSHA,
		PRNumber: 23, PRURL: "https://example.test/pull/23", FirstSeenAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
	}
	pending := []*visualhive.OutboxEntry{{ID: "pending-update", Action: visualhive.OutboxUpdateIssue, Repository: "owner/repo", RepositoryFingerprint: fingerprint, CreatedAt: time.Now().UTC()}}
	writeUninstallLifecycle(t, fixture.stateDir, map[string]*visualhive.FindingLifecycle{fingerprint: &finding}, pending)
	repairStore, err := repair.NewStore(filepath.Join(fixture.stateDir, "repair"))
	if err != nil {
		t.Fatal(err)
	}
	attempt := repair.Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1, AttemptCounted: true, LifecycleStarted: true,
		Branch: finding.Branch, Worktree: "managed-worktree", Stage: repair.StagePROpen, Provider: "test", CommitSHA: finding.RepairCommitSHA,
		PRNumber: finding.PRNumber, PRURL: finding.PRURL, LifecyclePROpen: true, ChangedFiles: []string{"src/proof.test.ts"}, StartedAt: time.Now().UTC(),
	}
	if err := repairStore.Put(attempt); err != nil {
		t.Fatal(err)
	}

	result, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client})
	if err != nil || !result.FinalizationPending || result.PRNumber != 17 {
		t.Fatalf("active drain uninstall result=%+v err=%v", result, err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(filepath.Join(fixture.stateDir, "visual-hive"))
	if err != nil {
		t.Fatal(err)
	}
	drained, exists := lifecycle.Finding(fingerprint)
	if !exists || drained.Status != visualhive.StatusCancelled || drained.ClosedAt == nil || len(lifecycle.PendingOutbox()) != 0 {
		t.Fatalf("drained lifecycle=%+v exists=%t pending=%+v", drained, exists, lifecycle.PendingOutbox())
	}
	repairStore, err = repair.NewStore(filepath.Join(fixture.stateDir, "repair"))
	if err != nil {
		t.Fatal(err)
	}
	storedAttempt, exists := repairStore.Get(fingerprint)
	if !exists || storedAttempt.Stage != repair.StageCancelled {
		t.Fatalf("drained repair=%+v exists=%t", storedAttempt, exists)
	}
	fixture.mu.Lock()
	issueOpen, repairOpen, branchExists := fixture.issueOpen, fixture.repairOpen, fixture.repairBranchExists
	fixture.mu.Unlock()
	if issueOpen || repairOpen || branchExists {
		t.Fatalf("remote drain incomplete: issue_open=%t repair_open=%t branch_exists=%t", issueOpen, repairOpen, branchExists)
	}
	audit, err := os.ReadFile(filepath.Join(fixture.stateDir, "integrated", "audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), "authorize_uninstall_close_issue") || !strings.Contains(string(audit), "authorize_uninstall_close_repair_pr") || !strings.Contains(string(audit), "authorize_uninstall_delete_repair_branch") {
		t.Fatalf("exact drain audit missing: err=%v\n%s", err, audit)
	}
}

func TestUninstallPreparationAcceptsAbsentNoChangeRepairBranch(t *testing.T) {
	fixture := newUninstallFixture(t)
	defer fixture.server.Close()
	fingerprint := strings.Repeat("d", 64)
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Fingerprint: fingerprint,
		Status: visualhive.StatusRepairRunning, Branch: "hive/repair-proof-a1",
		FirstSeenAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
	}
	writeUninstallLifecycle(t, fixture.stateDir, map[string]*visualhive.FindingLifecycle{fingerprint: &finding}, nil)
	repairStore, err := repair.NewStore(filepath.Join(fixture.stateDir, "repair"))
	if err != nil {
		t.Fatal(err)
	}
	attempt := repair.Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1, AttemptCounted: true, LifecycleStarted: true,
		Branch: finding.Branch, Worktree: "managed-worktree", Stage: repair.StageNoChange, Provider: "test", StartedAt: time.Now().UTC(),
	}
	if err := repairStore.Put(attempt); err != nil {
		t.Fatal(err)
	}

	result, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client,
	})
	if err != nil || !result.FinalizationPending {
		t.Fatalf("absent no-change repair uninstall result=%+v err=%v", result, err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(filepath.Join(fixture.stateDir, "visual-hive"))
	if err != nil {
		t.Fatal(err)
	}
	drained, exists := lifecycle.Finding(fingerprint)
	if !exists || drained.Status != visualhive.StatusCancelled {
		t.Fatalf("drained lifecycle=%+v exists=%t", drained, exists)
	}
	repairStore, err = repair.NewStore(filepath.Join(fixture.stateDir, "repair"))
	if err != nil {
		t.Fatal(err)
	}
	storedAttempt, exists := repairStore.Get(fingerprint)
	if !exists || storedAttempt.Stage != repair.StageCancelled {
		t.Fatalf("drained repair=%+v exists=%t", storedAttempt, exists)
	}
	audit, err := os.ReadFile(filepath.Join(fixture.stateDir, "integrated", "audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), "authorize_uninstall_absent_repair_branch") {
		t.Fatalf("absent no-change repair audit missing: err=%v\n%s", err, audit)
	}
}

func TestUninstallPreparationRejectsExistingUnboundNoChangeRepairBranch(t *testing.T) {
	fixture := newUninstallFixture(t)
	defer fixture.server.Close()
	fingerprint := strings.Repeat("c", 64)
	fixture.mu.Lock()
	fixture.repairBranchExists = true
	fixture.repairHead = fixture.baseSHA
	fixture.mu.Unlock()
	runIntegratedGit(t, fixture.remote, "update-ref", "refs/heads/hive/repair-proof-a1", fixture.baseSHA)
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Fingerprint: fingerprint,
		Status: visualhive.StatusRepairRunning, Branch: "hive/repair-proof-a1",
		FirstSeenAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
	}
	writeUninstallLifecycle(t, fixture.stateDir, map[string]*visualhive.FindingLifecycle{fingerprint: &finding}, nil)
	repairStore, err := repair.NewStore(filepath.Join(fixture.stateDir, "repair"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repairStore.Put(repair.Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1, AttemptCounted: true, LifecycleStarted: true,
		Branch: finding.Branch, Worktree: "managed-worktree", Stage: repair.StageNoChange, Provider: "test", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	result, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client,
	})
	if err == nil || !strings.Contains(err.Error(), "incomplete repair branch binding") {
		t.Fatalf("existing unbound no-change repair result=%+v err=%v", result, err)
	}
	fixture.mu.Lock()
	branchExists := fixture.repairBranchExists
	fixture.mu.Unlock()
	if !branchExists {
		t.Fatal("unbound remote repair branch was mutated")
	}
}

func TestUninstallPreparationAuthorizesAbsentUnpublishedFailedRepairBranch(t *testing.T) {
	fixture := newUninstallFixture(t)
	defer fixture.server.Close()
	fingerprint := strings.Repeat("b", 64)
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Fingerprint: fingerprint,
		Status:      visualhive.StatusDetected,
		FirstSeenAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
	}
	writeUninstallLifecycle(t, fixture.stateDir, map[string]*visualhive.FindingLifecycle{fingerprint: &finding}, nil)
	repairStore, err := repair.NewStore(filepath.Join(fixture.stateDir, "repair"))
	if err != nil {
		t.Fatal(err)
	}
	attempt := repair.Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1,
		Branch: "hive/repair-proof-a1", Worktree: "managed-worktree",
		Stage: repair.StageFailed, ResumeStage: repair.StagePrepared, Provider: "test",
		LastFailureClass: repair.FailureInfrastructure, StartedAt: time.Now().UTC(),
	}
	if err := repairStore.Put(attempt); err != nil {
		t.Fatal(err)
	}

	result, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client,
	})
	if err != nil || !result.FinalizationPending {
		t.Fatalf("absent unpublished failed repair uninstall result=%+v err=%v", result, err)
	}
	repairStore, err = repair.NewStore(filepath.Join(fixture.stateDir, "repair"))
	if err != nil {
		t.Fatal(err)
	}
	storedAttempt, exists := repairStore.Get(fingerprint)
	if !exists || storedAttempt.Stage != repair.StageCancelled {
		t.Fatalf("drained repair=%+v exists=%t", storedAttempt, exists)
	}
	audit, err := os.ReadFile(filepath.Join(fixture.stateDir, "integrated", "audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), "authorize_uninstall_absent_repair_branch") {
		t.Fatalf("absent failed repair audit missing: err=%v\n%s", err, audit)
	}
}

func TestUninstallPreparationRejectsExistingUnpublishedFailedRepairBranch(t *testing.T) {
	fixture := newUninstallFixture(t)
	defer fixture.server.Close()
	fingerprint := strings.Repeat("a", 64)
	fixture.mu.Lock()
	fixture.repairBranchExists = true
	fixture.repairHead = fixture.baseSHA
	fixture.mu.Unlock()
	runIntegratedGit(t, fixture.remote, "update-ref", "refs/heads/hive/repair-proof-a1", fixture.baseSHA)
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Fingerprint: fingerprint,
		Status:      visualhive.StatusDetected,
		FirstSeenAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
	}
	writeUninstallLifecycle(t, fixture.stateDir, map[string]*visualhive.FindingLifecycle{fingerprint: &finding}, nil)
	repairStore, err := repair.NewStore(filepath.Join(fixture.stateDir, "repair"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repairStore.Put(repair.Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1,
		Branch: "hive/repair-proof-a1", Worktree: "managed-worktree",
		Stage: repair.StageFailed, ResumeStage: repair.StagePrepared, Provider: "test",
		LastFailureClass: repair.FailureInfrastructure, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	result, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client,
	})
	if err == nil || !strings.Contains(err.Error(), "incomplete repair branch binding") {
		t.Fatalf("existing unpublished failed repair result=%+v err=%v", result, err)
	}
	fixture.mu.Lock()
	branchExists := fixture.repairBranchExists
	fixture.mu.Unlock()
	if !branchExists {
		t.Fatal("unbound remote repair branch was mutated")
	}
}

func TestUninstallPreparationClosesNormalHiveLifecycleBead(t *testing.T) {
	fixture := newUninstallFixture(t)
	defer fixture.server.Close()

	ordinaryBeadsDir := filepath.Join(t.TempDir(), "ordinary-hive", "beads", "quality")
	ordinaryStore, err := beads.NewSharedStore(ordinaryBeadsDir)
	if err != nil {
		t.Fatal(err)
	}
	bead, err := ordinaryStore.Create("Verified Visual Hive finding", beads.TypeBug, beads.PriorityHigh, "quality", "visual-hive:proof")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("f", 64)
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Fingerprint: fingerprint,
		BeadID: bead.ID, Status: visualhive.StatusDetected, FirstSeenAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
	}
	writeUninstallLifecycle(t, fixture.stateDir, map[string]*visualhive.FindingLifecycle{fingerprint: &finding}, nil)

	result, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, LifecycleBeadsDirs: []string{ordinaryBeadsDir}, GitHub: fixture.client,
	})
	if err != nil || !result.FinalizationPending {
		t.Fatalf("normal Hive lifecycle bead drain result=%+v err=%v", result, err)
	}
	reloaded, err := beads.NewSharedStore(ordinaryBeadsDir)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := reloaded.Get(bead.ID)
	if err != nil || closed.Status != beads.StatusClosed {
		t.Fatalf("ordinary Hive lifecycle bead was not closed: bead=%+v err=%v", closed, err)
	}
}

func TestAlreadyCleanUninstallAcceptsLaterStillCleanDefaultHead(t *testing.T) {
	fixture := newUninstallFixtureWithManagedSetup(t, false)
	defer fixture.server.Close()
	prepared, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client})
	if err != nil || !prepared.FinalizationPending || prepared.PRNumber != 0 {
		t.Fatalf("already-clean preparation=%+v err=%v", prepared, err)
	}
	advance := filepath.Join(filepath.Dir(fixture.remote), "advance")
	runIntegratedGit(t, filepath.Dir(fixture.remote), "clone", fixture.remote, advance)
	writeFixture(t, advance, "README.md", "unrelated clean descendant\n")
	runIntegratedGit(t, advance, "add", "README.md")
	runIntegratedGit(t, advance, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "unrelated")
	runIntegratedGit(t, advance, "push", "origin", "main")

	finalized, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client})
	if err != nil || !finalized.StateDeleted {
		t.Fatalf("still-clean descendant finalization=%+v err=%v", finalized, err)
	}
}

func TestUninstallRetiresExactUnmergedSetupWithoutRestoringStalePreimages(t *testing.T) {
	fixture := newUninstallFixtureWithManagedSetup(t, false)
	defer fixture.server.Close()
	seedStaleUnmergedSetupPreimages(t, fixture)
	originalMain := strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", "refs/heads/main"))
	prepared, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, GitHub: fixture.client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.FinalizationPending || prepared.PRNumber != 0 || !strings.EqualFold(prepared.CommitSHA, originalMain) {
		t.Fatalf("unmerged setup retirement preparation=%+v", prepared)
	}
	if current := strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", "refs/heads/main")); current != originalMain {
		t.Fatalf("preparation changed default branch from %s to %s", originalMain, current)
	}
	fixture.mu.Lock()
	setupOpen, closeCalls, deleteCalls := fixture.setupOpen, fixture.setupCloseCalls, fixture.setupDeleteCalls
	fixture.mu.Unlock()
	if !setupOpen || closeCalls != 0 || deleteCalls != 0 {
		t.Fatalf("preparation retired setup too early: open=%t close=%d delete=%d", setupOpen, closeCalls, deleteCalls)
	}
	finalized, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !finalized.StateDeleted || finalized.FinalizationPending || !finalized.CleanupVerified {
		t.Fatalf("unmerged setup retirement finalization=%+v", finalized)
	}
	fixture.mu.Lock()
	setupOpen, closeCalls, deleteCalls = fixture.setupOpen, fixture.setupCloseCalls, fixture.setupDeleteCalls
	fixture.mu.Unlock()
	if setupOpen || closeCalls != 1 || deleteCalls != 1 {
		t.Fatalf("exact setup identity was not retired once: open=%t close=%d delete=%d", setupOpen, closeCalls, deleteCalls)
	}
	if current := strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", "refs/heads/main")); current != originalMain {
		t.Fatalf("finalization changed default branch from %s to %s", originalMain, current)
	}
}

func TestUninstallRejectsStalePreimageRecoveryForMergedSetupProposal(t *testing.T) {
	fixture := newUninstallFixtureWithManagedSetup(t, false)
	defer fixture.server.Close()
	seedStaleUnmergedSetupPreimages(t, fixture)
	fixture.mu.Lock()
	fixture.setupOpen, fixture.setupMerged = false, true
	fixture.mu.Unlock()
	result, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUninstall, StateDir: fixture.stateDir, GitHub: fixture.client,
	})
	if err == nil || !strings.Contains(err.Error(), "not the exact unmerged Hive-owned pull request") || result.StateDeleted {
		t.Fatalf("merged setup proposal bypassed stale preimage protection: result=%+v err=%v", result, err)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.stateDir, "integrated", "config.json")); statErr != nil {
		t.Fatalf("rejected recovery discarded state: %v", statErr)
	}
}

func TestUninstallCancelRecoversStrictBaseAndDescendantHeadDriftThenRestarts(t *testing.T) {
	fixture := newUninstallFixture(t)
	defer fixture.server.Close()
	prepared, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, GitHub: fixture.client})
	if err != nil {
		t.Fatal(err)
	}
	store, _ := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	firstIntent, exists, err := store.LoadUninstallIntent()
	if err != nil || !exists {
		t.Fatalf("first uninstall intent missing: exists=%t err=%v", exists, err)
	}
	fixture.advanceUninstallBase(t)
	driftedHead := fixture.advanceUninstallHead(t, prepared.Branch)
	if _, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, DeleteState: true, GitHub: fixture.client}); err == nil || !strings.Contains(err.Error(), "--cancel") {
		t.Fatalf("strict uninstall drift did not return the supported recovery command: %v", err)
	}
	cancelled, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, Cancel: true, GitHub: fixture.client})
	if err != nil || !cancelled.Cancelled || cancelled.FinalizationPending {
		t.Fatalf("drifted uninstall cancellation=%+v err=%v", cancelled, err)
	}
	if _, err := integratedGitOutputError(fixture.remote, "rev-parse", "refs/heads/"+prepared.Branch); err == nil {
		t.Fatalf("descendant cleanup ref %s was not retired", driftedHead)
	}
	config, err := store.Load()
	if err != nil || !config.Paused || config.SetupBranch != "hive/setup-123" || config.SetupPRNumber != 9 || config.SetupPRURL != "https://example.test/owner/repo/pull/9" {
		t.Fatalf("cancelled uninstall did not restore prior setup identity while remaining paused: config=%+v err=%v", config, err)
	}
	restarted, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, GitHub: fixture.client})
	if err != nil || !restarted.FinalizationPending || restarted.PRNumber != 17 {
		t.Fatalf("cancelled uninstall could not restart: result=%+v err=%v", restarted, err)
	}
	secondIntent, exists, err := store.LoadUninstallIntent()
	if err != nil || !exists || secondIntent.Marker == firstIntent.Marker {
		t.Fatalf("restarted uninstall did not receive fresh exact PR identity: first=%q second=%q exists=%t err=%v", firstIntent.Marker, secondIntent.Marker, exists, err)
	}
}

func TestUninstallCancelRetriesAfterPRCloseBranchDeleteFailure(t *testing.T) {
	fixture := newUninstallFixture(t)
	defer fixture.server.Close()
	if _, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, GitHub: fixture.client}); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.deleteFailures = 1
	fixture.mu.Unlock()
	if _, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, Cancel: true, GitHub: fixture.client}); err == nil || !strings.Contains(err.Error(), "retire exact") {
		t.Fatalf("injected uninstall branch retirement failure was not preserved: %v", err)
	}
	fixture.mu.Lock()
	closed := fixture.cleanupClosed
	fixture.mu.Unlock()
	store, _ := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	if _, exists, err := store.LoadUninstallIntent(); !closed || err != nil || !exists {
		t.Fatalf("partial uninstall cancellation was not resumable: pr_closed=%t intent_exists=%t err=%v", closed, exists, err)
	}
	result, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, Cancel: true, GitHub: fixture.client})
	if err != nil || !result.Cancelled {
		t.Fatalf("partial uninstall cancellation retry=%+v err=%v", result, err)
	}
}

func TestUninstallCancelRejectsWrongMarkerBranchPRAndMergedState(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *uninstallFixture)
	}{
		{name: "marker", mutate: func(_ *testing.T, fixture *uninstallFixture) { fixture.body = "marker removed" }},
		{name: "branch", mutate: func(_ *testing.T, fixture *uninstallFixture) { fixture.cleanupBranch = "hive/not-the-owned-uninstall" }},
		{name: "recorded pr", mutate: func(t *testing.T, fixture *uninstallFixture) {
			store, _ := NewStore(filepath.Join(fixture.stateDir, "integrated"))
			intent, _, _ := store.LoadUninstallIntent()
			intent.PRNumber, intent.PRURL = 99, "https://example.test/owner/repo/pull/99"
			if err := store.SaveUninstallIntent(intent); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "merged", mutate: func(t *testing.T, fixture *uninstallFixture) {
			store, _ := NewStore(filepath.Join(fixture.stateDir, "integrated"))
			intent, _, _ := store.LoadUninstallIntent()
			fixture.merge(t, intent.CleanupCommitSHA)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newUninstallFixture(t)
			defer fixture.server.Close()
			if _, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, GitHub: fixture.client}); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, fixture)
			if _, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, Cancel: true, GitHub: fixture.client}); err == nil {
				t.Fatal("mismatched or merged uninstall identity was cancellable")
			}
			store, _ := NewStore(filepath.Join(fixture.stateDir, "integrated"))
			if _, exists, err := store.LoadUninstallIntent(); err != nil || !exists {
				t.Fatalf("failed cancellation discarded evidence: exists=%t err=%v", exists, err)
			}
		})
	}
}

func TestUninstallProtectionDriftCanBeReconciledByExactContextRemoval(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", Coverage: CoverageComprehensive,
		Automation: AutomationAutoMerge, Provider: "codex", ACMMLevel: 6, VisualHive: true,
		VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40), VisualHiveConfigDigest: strings.Repeat("c", 64),
	}
	binding, err := activationBindingDigest(config)
	if err != nil {
		t.Fatal(err)
	}
	activation := ProtectionActivationState{
		SchemaVersion: ProtectionActivationSchema, Repository: config.Repository, RepositoryID: config.RepositoryID, DefaultBranch: config.DefaultBranch,
		BindingDigest: binding, CheckContext: visualHiveProductionCheckContext, RequiredContext: visualHivePRCheckContext, CheckAppID: 42,
		WorkflowRunID: 11, WorkflowName: visualHiveProductionWorkflowName, WorkflowPath: visualHiveProductionWorkflowPath,
		WorkflowEvent: visualHiveProductionWorkflowEvent, WorkflowHeadSHA: strings.Repeat("d", 40), CheckRunID: 12, SeedCheckRunID: 13,
		ProtectionCreated: true, ActivatedAt: time.Now().UTC(),
	}
	if err := store.SaveProtectionActivation(activation); err != nil {
		t.Fatal(err)
	}
	visualRequired, deletes := true, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main/protection":
			writeUninstallJSON(t, writer, driftProtectionJSON(visualRequired))
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/rules/branches/main":
			http.Error(writer, `{"message":"Not Found"}`, http.StatusNotFound)
		case request.Method == http.MethodDelete:
			deletes++
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if err := reconcileUninstallProtection(context.Background(), client, store, config); err == nil || !strings.Contains(err.Error(), "remove only the exact") {
		t.Fatalf("drifted protection was not actionable: %v", err)
	}
	visualRequired = false
	if err := reconcileUninstallProtection(context.Background(), client, store, config); err != nil {
		t.Fatalf("exact context removal did not reconcile: %v", err)
	}
	if deletes != 0 {
		t.Fatalf("repository-owned drift was deleted as a whole: deletes=%d", deletes)
	}
}

func TestCancelledRepairIsTerminalOnlyAfterOwnedRefAuthorityIsCleared(t *testing.T) {
	attempt := repair.Attempt{Stage: repair.StageCancelled}
	if repairAttemptPending(attempt) {
		t.Fatal("fully retired cancelled repair remained pending")
	}
	attempt.CandidateGuardRef = "refs/hive/repair-sealed-trees/candidate/owned"
	if !repairAttemptPending(attempt) {
		t.Fatal("cancelled repair with lingering owned ref was treated as terminal")
	}
}

type uninstallFixture struct {
	stateDir string
	remote   string
	server   *httptest.Server
	client   *hivegithub.Client
	files    []string

	mu                 sync.Mutex
	title              string
	body               string
	created            bool
	merged             bool
	mergeSHA           string
	diff               string
	baseSHA            string
	managedPathPresent bool
	issueOpen          bool
	repairOpen         bool
	repairBranchExists bool
	repairFingerprint  string
	repairHead         string
	statusContext      string
	statusTarget       string
	statusFailures     int
	cleanupClosed      bool
	cleanupBranch      string
	cleanupHead        string
	deleteFailures     int
	setupHead          string
	setupOpen          bool
	setupMerged        bool
	setupBranchExists  bool
	setupCloseCalls    int
	setupDeleteCalls   int
	setupBaseSHA       string
	cleanupFiles       []string
}

func newUninstallFixture(t *testing.T) *uninstallFixture {
	return newUninstallFixtureWithManagedSetup(t, true)
}

func seedStaleUnmergedSetupPreimages(t *testing.T, fixture *uninstallFixture) {
	t.Helper()
	store, err := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	config.ManagedPreimagesVersion = managedPreimagesVersion
	config.ManagedPathPreimages = make(map[string]ManagedPathPreimage, len(fixture.files))
	for _, relative := range fixture.files {
		content := []byte("stale preimage for " + relative + "\n")
		digest := sha256.Sum256(content)
		config.ManagedPathPreimages[relative] = ManagedPathPreimage{
			Existed: true, GitMode: "100644", Content: content, ContentSHA256: hex.EncodeToString(digest[:]),
		}
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
}

func newUninstallFixtureWithManagedSetup(t *testing.T, seedManagedSetup bool) *uninstallFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &uninstallFixture{
		stateDir: filepath.Join(root, "state"), remote: filepath.Join(root, "remote.git"),
		files: sortedUniquePaths(managedSetupFiles(true)), diff: "exact cleanup diff\n", managedPathPresent: seedManagedSetup,
	}
	fixture.cleanupFiles = append([]string(nil), fixture.files...)
	bindTestRepositoryCloneURL(t, fixture.remote)
	runIntegratedGit(t, root, "init", "--bare", fixture.remote)
	seed := filepath.Join(root, "seed")
	runIntegratedGit(t, root, "init", "-b", "main", seed)
	writeFixture(t, seed, "README.md", "fixture\n")
	if seedManagedSetup {
		for _, relative := range fixture.files {
			writeFixture(t, seed, relative, "managed "+relative+"\n")
		}
	}
	runIntegratedGit(t, seed, "add", ".")
	runIntegratedGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "seed")
	runIntegratedGit(t, seed, "remote", "add", "origin", fixture.remote)
	runIntegratedGit(t, seed, "push", "-u", "origin", "main")
	runIntegratedGit(t, fixture.remote, "symbolic-ref", "HEAD", "refs/heads/main")
	fixture.baseSHA = strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", "refs/heads/main"))
	fixture.setupBaseSHA = fixture.baseSHA
	fixture.setupOpen, fixture.setupMerged, fixture.setupBranchExists = !seedManagedSetup, seedManagedSetup, true
	if seedManagedSetup {
		fixture.setupHead = fixture.baseSHA
		runIntegratedGit(t, fixture.remote, "update-ref", "refs/heads/hive/setup-123", fixture.setupHead)
	} else {
		setup := filepath.Join(root, "setup")
		runIntegratedGit(t, root, "clone", fixture.remote, setup)
		runIntegratedGit(t, setup, "switch", "-c", "hive/setup-123")
		for _, relative := range fixture.files {
			writeFixture(t, setup, relative, "managed "+relative+"\n")
		}
		runIntegratedGit(t, setup, "add", ".")
		runIntegratedGit(t, setup, "-c", "user.name=Hive Setup", "-c", "user.email=hive-setup@example.test", "commit", "-m", "setup")
		runIntegratedGit(t, setup, "push", "-u", "origin", "hive/setup-123")
		fixture.setupHead = strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", "refs/heads/hive/setup-123"))
	}
	checkout := filepath.Join(fixture.stateDir, "integrated", "checkouts", "owner--repo")
	if err := os.MkdirAll(filepath.Dir(checkout), 0o700); err != nil {
		t.Fatal(err)
	}
	runIntegratedGit(t, root, "clone", fixture.remote, checkout)
	if err := writeManagedCheckoutOwner(checkout, "owner/repo"); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", Coverage: CoverageComprehensive,
		Automation: AutomationRepairPR, Provider: "codex", ACMMLevel: 5, MaxActiveIssues: 5, MaxRepairAttempts: 3,
		VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40), CheckoutDir: checkout, StateDir: fixture.stateDir,
		SetupAuthorizationActorID: 55, SetupBranch: "hive/setup-123", SetupPRNumber: 9, SetupPRURL: "https://example.test/owner/repo/pull/9", SetupHeadSHA: fixture.setupHead,
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fixture.serve(t, writer, request)
	}))
	fixture.client = hivegithub.NewClientForTest(fixture.server.URL, "owner", []string{"repo"}, slog.Default())
	return fixture
}

func (fixture *uninstallFixture) merge(t *testing.T, cleanupSHA string) {
	t.Helper()
	runIntegratedGit(t, fixture.remote, "update-ref", "refs/heads/main", cleanupSHA)
	fixture.mu.Lock()
	fixture.merged, fixture.mergeSHA, fixture.managedPathPresent = true, cleanupSHA, false
	fixture.mu.Unlock()
}

func (fixture *uninstallFixture) mergeAsCommitNotInCheckout(t *testing.T, cleanupSHA string) {
	t.Helper()
	tree := strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", cleanupSHA+"^{tree}"))
	mergeSHA := strings.TrimSpace(integratedGitOutput(
		t, fixture.remote,
		"-c", "user.name=Test",
		"-c", "user.email=test@example.com",
		"commit-tree", tree,
		"-p", fixture.baseSHA,
		"-p", cleanupSHA,
		"-m", "merge uninstall cleanup",
	))
	runIntegratedGit(t, fixture.remote, "update-ref", "refs/heads/main", mergeSHA)
	fixture.mu.Lock()
	fixture.merged, fixture.mergeSHA, fixture.managedPathPresent = true, mergeSHA, false
	fixture.mu.Unlock()
}

func (fixture *uninstallFixture) advanceUninstallBase(t *testing.T) {
	t.Helper()
	clone := filepath.Join(filepath.Dir(fixture.remote), "uninstall-base-advance")
	runIntegratedGit(t, filepath.Dir(fixture.remote), "clone", fixture.remote, clone)
	writeFixture(t, clone, "uninstall-base-drift.txt", "base advanced\n")
	runIntegratedGit(t, clone, "add", "uninstall-base-drift.txt")
	runIntegratedGit(t, clone, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "advance uninstall base")
	runIntegratedGit(t, clone, "push", "origin", "main")
	fixture.mu.Lock()
	fixture.baseSHA = strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", "refs/heads/main"))
	fixture.mu.Unlock()
}

func (fixture *uninstallFixture) advanceUninstallHead(t *testing.T, branch string) string {
	t.Helper()
	clone := filepath.Join(filepath.Dir(fixture.remote), "uninstall-head-advance")
	runIntegratedGit(t, filepath.Dir(fixture.remote), "clone", fixture.remote, clone)
	runIntegratedGit(t, clone, "switch", "-c", branch, "origin/"+branch)
	writeFixture(t, clone, "uninstall-head-drift.txt", "updated cleanup branch\n")
	runIntegratedGit(t, clone, "add", "uninstall-head-drift.txt")
	runIntegratedGit(t, clone, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "update uninstall branch")
	runIntegratedGit(t, clone, "push", "origin", branch)
	head := strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", "refs/heads/"+branch))
	fixture.mu.Lock()
	fixture.cleanupHead = head
	fixture.mu.Unlock()
	return head
}

func (fixture *uninstallFixture) serve(t *testing.T, writer http.ResponseWriter, request *http.Request) {
	t.Helper()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	path := request.URL.Path
	writer.Header().Set("Content-Type", "application/json")
	switch {
	case request.Method == http.MethodGet && path == "/repos/owner/repo":
		writeUninstallJSON(t, writer, map[string]any{"id": 123, "full_name": "owner/repo", "default_branch": "main"})
	case request.Method == http.MethodGet && path == "/user":
		writeUninstallJSON(t, writer, map[string]any{"id": 55, "login": "hive-bot", "type": "User"})
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/repos/owner/repo/commits/") && strings.HasSuffix(path, "/statuses"):
		if fixture.statusContext == "" {
			writeUninstallJSON(t, writer, []any{})
			return
		}
		writeUninstallJSON(t, writer, []any{fixture.setupStatusJSON()})
	case request.Method == http.MethodPost && strings.HasPrefix(path, "/repos/owner/repo/statuses/"):
		if fixture.statusFailures > 0 {
			fixture.statusFailures--
			http.Error(writer, `{"message":"injected status failure"}`, http.StatusInternalServerError)
			return
		}
		var input struct {
			Context   string `json:"context"`
			TargetURL string `json:"target_url"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		fixture.statusContext, fixture.statusTarget = input.Context, input.TargetURL
		writer.WriteHeader(http.StatusCreated)
		writeUninstallJSON(t, writer, fixture.setupStatusJSON())
	case request.Method == http.MethodGet && path == "/repos/owner/repo/issues/7":
		writeUninstallJSON(t, writer, fixture.issueJSON())
	case request.Method == http.MethodPatch && path == "/repos/owner/repo/issues/7":
		fixture.issueOpen = false
		writeUninstallJSON(t, writer, fixture.issueJSON())
	case request.Method == http.MethodGet && path == "/repos/owner/repo/pulls":
		if fixture.repairOpen && request.URL.Query().Get("state") == "open" {
			writeUninstallJSON(t, writer, []any{fixture.repairPullJSON()})
			return
		}
		if request.URL.Query().Get("state") == "all" {
			pulls := []any{fixture.setupPullJSON()}
			if fixture.created {
				pulls = append(pulls, fixture.pullJSON(t))
			}
			writeUninstallJSON(t, writer, pulls)
			return
		}
		writeUninstallJSON(t, writer, []any{})
	case request.Method == http.MethodPost && path == "/repos/owner/repo/pulls":
		var input struct {
			Title string `json:"title"`
			Body  string `json:"body"`
			Head  string `json:"head"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		fixture.title, fixture.body, fixture.created, fixture.cleanupClosed, fixture.cleanupBranch = input.Title, input.Body, true, false, input.Head
		writer.WriteHeader(http.StatusCreated)
		writeUninstallJSON(t, writer, fixture.pullJSON(t))
	case request.Method == http.MethodPatch && path == "/repos/owner/repo/pulls/17":
		fixture.cleanupClosed = true
		writeUninstallJSON(t, writer, fixture.pullJSON(t))
	case request.Method == http.MethodGet && path == "/repos/owner/repo/pulls/17/files":
		files := make([]map[string]any, 0, len(fixture.cleanupFiles))
		for _, relative := range fixture.cleanupFiles {
			files = append(files, map[string]any{"filename": relative, "status": "removed"})
		}
		writeUninstallJSON(t, writer, files)
	case request.Method == http.MethodGet && path == "/repos/owner/repo/pulls/17" && strings.Contains(request.Header.Get("Accept"), "diff"):
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(writer, fixture.diff)
	case request.Method == http.MethodGet && path == "/repos/owner/repo/pulls/17":
		writeUninstallJSON(t, writer, fixture.pullJSON(t))
	case request.Method == http.MethodGet && path == "/repos/owner/repo/pulls/9/files":
		files := make([]map[string]any, 0, len(fixture.files))
		for _, relative := range fixture.files {
			files = append(files, map[string]any{"filename": relative, "status": "added"})
		}
		writeUninstallJSON(t, writer, files)
	case request.Method == http.MethodGet && path == "/repos/owner/repo/pulls/9" && strings.Contains(request.Header.Get("Accept"), "diff"):
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(writer, "exact setup diff\n")
	case request.Method == http.MethodGet && path == "/repos/owner/repo/pulls/9":
		writeUninstallJSON(t, writer, fixture.setupPullJSON())
	case request.Method == http.MethodPatch && path == "/repos/owner/repo/pulls/9":
		fixture.setupCloseCalls++
		fixture.setupOpen = false
		writeUninstallJSON(t, writer, fixture.setupPullJSON())
	case request.Method == http.MethodGet && path == "/repos/owner/repo/pulls/23":
		writeUninstallJSON(t, writer, fixture.repairPullJSON())
	case request.Method == http.MethodPatch && path == "/repos/owner/repo/pulls/23":
		fixture.repairOpen = false
		writeUninstallJSON(t, writer, fixture.repairPullJSON())
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/repos/owner/repo/git/ref/heads/hive/uninstall-"):
		branch := strings.TrimPrefix(path, "/repos/owner/repo/git/ref/heads/")
		head, err := integratedGitOutputError(fixture.remote, "rev-parse", "refs/heads/"+branch)
		if err != nil {
			http.Error(writer, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		writeUninstallJSON(t, writer, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"sha": strings.TrimSpace(head), "type": "commit"}})
	case request.Method == http.MethodDelete && strings.HasPrefix(path, "/repos/owner/repo/git/refs/heads/hive/uninstall-"):
		if fixture.deleteFailures > 0 {
			fixture.deleteFailures--
			http.Error(writer, `{"message":"injected delete failure"}`, http.StatusInternalServerError)
			return
		}
		branch := strings.TrimPrefix(path, "/repos/owner/repo/git/refs/heads/")
		runIntegratedGit(t, fixture.remote, "update-ref", "-d", "refs/heads/"+branch)
		writer.WriteHeader(http.StatusNoContent)
	case request.Method == http.MethodGet && path == "/repos/owner/repo/git/ref/heads/hive/setup-123":
		if !fixture.setupBranchExists {
			http.Error(writer, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		writeUninstallJSON(t, writer, map[string]any{"ref": "refs/heads/hive/setup-123", "object": map[string]any{"sha": fixture.setupHead, "type": "commit"}})
	case request.Method == http.MethodDelete && path == "/repos/owner/repo/git/refs/heads/hive/setup-123":
		fixture.setupDeleteCalls++
		fixture.setupBranchExists = false
		runIntegratedGit(t, fixture.remote, "update-ref", "-d", "refs/heads/hive/setup-123")
		writer.WriteHeader(http.StatusNoContent)
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/repos/owner/repo/compare/"):
		pair := strings.TrimPrefix(path, "/repos/owner/repo/compare/")
		base, head, ok := strings.Cut(pair, "...")
		if !ok {
			http.Error(writer, `{"message":"bad comparison"}`, http.StatusBadRequest)
			return
		}
		status, behind := "diverged", 1
		if strings.EqualFold(base, head) {
			status, behind = "identical", 0
		} else if _, err := integratedGitOutputError(fixture.remote, "merge-base", "--is-ancestor", base, head); err == nil {
			status, behind = "ahead", 0
		}
		writeUninstallJSON(t, writer, map[string]any{"status": status, "behind_by": behind})
	case request.Method == http.MethodGet && strings.Contains(path, "heads/hive/repair-proof-a1"):
		if !fixture.repairBranchExists {
			http.Error(writer, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		writeUninstallJSON(t, writer, map[string]any{"ref": "refs/heads/hive/repair-proof-a1", "object": map[string]any{"sha": fixture.repairHead, "type": "commit"}})
	case request.Method == http.MethodDelete && strings.Contains(path, "heads/hive/repair-proof-a1"):
		if fixture.repairBranchExists {
			fixture.repairBranchExists = false
			runIntegratedGit(t, fixture.remote, "update-ref", "-d", "refs/heads/hive/repair-proof-a1")
		}
		writer.WriteHeader(http.StatusNoContent)
	case request.Method == http.MethodGet && path == "/repos/owner/repo/branches/main":
		head := strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", "refs/heads/main"))
		writeUninstallJSON(t, writer, map[string]any{"name": "main", "commit": map[string]any{"sha": head}})
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/repos/owner/repo/contents/"):
		if fixture.managedPathPresent {
			writeUninstallJSON(t, writer, map[string]any{"type": "file", "path": fixture.files[0], "content": "bWFuYWdlZA==", "encoding": "base64"})
			return
		}
		http.Error(writer, `{"message":"Not Found"}`, http.StatusNotFound)
	case request.Method == http.MethodGet && path == "/repos/owner/repo/branches/main/protection":
		http.Error(writer, `{"message":"Not Found"}`, http.StatusNotFound)
	case request.Method == http.MethodGet && path == "/repos/owner/repo/rules/branches/main":
		http.Error(writer, `{"message":"Not Found"}`, http.StatusNotFound)
	default:
		http.Error(writer, fmt.Sprintf(`{"message":"unexpected %s %s"}`, request.Method, path), http.StatusNotFound)
	}
}

func (fixture *uninstallFixture) setupStatusJSON() map[string]any {
	return map[string]any{
		"id": 901, "state": "success", "context": fixture.statusContext, "target_url": fixture.statusTarget,
		"creator": map[string]any{"id": 55, "login": "hive-bot", "type": "User"},
	}
}

func (fixture *uninstallFixture) issueJSON() map[string]any {
	state := "closed"
	if fixture.issueOpen {
		state = "open"
	}
	marker := fmt.Sprintf("<!-- hive-visual-fingerprint: %s -->", fixture.repairFingerprint)
	return map[string]any{
		"number": 7, "html_url": "https://example.test/issues/7", "state": state, "title": "Hive finding", "body": marker + "\nactive finding",
		"user": map[string]any{"id": 55, "login": "hive-bot"},
	}
}

func (fixture *uninstallFixture) repairPullJSON() map[string]any {
	state := "closed"
	if fixture.repairOpen {
		state = "open"
	}
	repository := map[string]any{"id": 123, "full_name": "owner/repo"}
	return map[string]any{
		"number": 23, "html_url": "https://example.test/pull/23", "state": state, "merged": false,
		"title": "Hive repair", "body": fmt.Sprintf("<!-- hive-repair: %s -->\nrepair", fixture.repairFingerprint),
		"head": map[string]any{"ref": "hive/repair-proof-a1", "sha": fixture.repairHead, "repo": repository},
		"base": map[string]any{"ref": "main", "sha": fixture.baseSHA, "repo": repository},
	}
}

func (fixture *uninstallFixture) setupPullJSON() map[string]any {
	state := "closed"
	if fixture.setupOpen {
		state = "open"
	}
	repository := map[string]any{"id": 123, "full_name": "owner/repo"}
	return map[string]any{
		"number": 9, "html_url": "https://example.test/owner/repo/pull/9", "state": state, "merged": fixture.setupMerged,
		"merge_commit_sha": fixture.baseSHA, "title": "Install Hive + Visual Hive production automation",
		"body": "<!-- hive-setup: owner/repo -->\nsetup", "changed_files": len(fixture.files),
		"head": map[string]any{"ref": "hive/setup-123", "sha": fixture.setupHead, "repo": repository},
		"base": map[string]any{"ref": "main", "sha": fixture.setupBaseSHA, "repo": repository},
	}
}

func (fixture *uninstallFixture) pullJSON(t *testing.T) map[string]any {
	t.Helper()
	branch := fixture.cleanupBranch
	if branch == "" {
		branch = "hive/uninstall-123"
	}
	head := fixture.cleanupHead
	if current, err := integratedGitOutputError(fixture.remote, "rev-parse", "refs/heads/"+branch); err == nil {
		head = strings.TrimSpace(current)
		fixture.cleanupHead = head
	}
	state := "open"
	if fixture.merged || fixture.cleanupClosed {
		state = "closed"
	}
	repository := map[string]any{"id": 123, "full_name": "owner/repo"}
	return map[string]any{
		"number": 17, "html_url": "https://example.test/owner/repo/pull/17", "state": state, "merged": fixture.merged,
		"merge_commit_sha": fixture.mergeSHA, "title": fixture.title, "body": fixture.body, "changed_files": len(fixture.cleanupFiles),
		"head": map[string]any{"ref": branch, "sha": head, "repo": repository},
		"base": map[string]any{"ref": "main", "sha": fixture.baseSHA, "repo": repository},
	}
}

func writeUninstallJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func writeUninstallLifecycle(t *testing.T, stateDir string, findings map[string]*visualhive.FindingLifecycle, outbox []*visualhive.OutboxEntry) {
	t.Helper()
	dir := filepath.Join(stateDir, "visual-hive")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := visualhive.LifecycleState{SchemaVersion: visualhive.LifecycleSchema, UpdatedAt: time.Now().UTC(), Findings: findings, ReplayKeys: map[string]string{}, Outbox: outbox}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func driftProtectionJSON(visualRequired bool) map[string]any {
	checks := []any{map[string]any{"context": "repository-owned", "app_id": 99}}
	if visualRequired {
		checks = append(checks, map[string]any{"context": "visual-hive", "app_id": 42})
	}
	return map[string]any{
		"required_status_checks":        map[string]any{"strict": true, "checks": checks},
		"required_pull_request_reviews": nil, "enforce_admins": map[string]any{"enabled": true}, "restrictions": nil,
		"required_linear_history": map[string]any{"enabled": true}, "allow_force_pushes": map[string]any{"enabled": false},
		"allow_deletions": map[string]any{"enabled": false}, "required_conversation_resolution": map[string]any{"enabled": true},
	}
}

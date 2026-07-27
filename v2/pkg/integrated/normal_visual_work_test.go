package integrated

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

func TestNormalVisualSetupBaselineRecoversMissingCheckpointAndFailsClosed(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("b", 40)
	config := Config{
		SchemaVersion: ConfigSchema, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: stateDir,
		SetupBranch: "hive/setup-123", SetupPRNumber: 7, SetupPRURL: "https://example.test/owner/repo/pull/7", SetupHeadSHA: strings.Repeat("a", 40),
		SetupAuthorizationActorID: 42, SetupBaselineRequired: true, VisualHiveConfigDigest: strings.Repeat("c", 64), SetupBaselineContractDigest: strings.Repeat("d", 64), SetupBaselineInitialDigest: emptySetupBaselineCandidateDigest(t),
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	dispatches := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main" {
			_, _ = io.WriteString(writer, `{"name":"main","commit":{"sha":"`+head+`"}}`)
			return
		}
		if request.Method == http.MethodPost {
			dispatches++
		}
		http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())

	err = reconcileNormalVisualSetupBaselineBeforeProduction(context.Background(), store, config, client)
	if err == nil || !errors.Is(err, ErrSetupBaselineLifecycleHold) || !strings.Contains(err.Error(), "recovered missing required setup baseline checkpoint") || dispatches != 0 {
		t.Fatalf("normal production did not stop at recovered setup baseline: dispatches=%d err=%v", dispatches, err)
	}
	intent, exists, loadErr := store.LoadSetupBaselineIntent()
	if loadErr != nil || !exists || intent.Phase != SetupBaselinePending || intent.SetupHeadSHA != config.SetupHeadSHA {
		t.Fatalf("normal gate did not durably recover the exact pending intent: exists=%t intent=%+v err=%v", exists, intent, loadErr)
	}
	if err := requireNormalVisualSetupBaselineProductionReady(store, config); !errors.Is(err, ErrNormalVisualSetupBaselinePending) {
		t.Fatalf("defense-in-depth gate accepted recovered pending baseline: %v", err)
	}
}

func TestNormalVisualSetupBaselineHeldReplayIsIdempotentAndNeverDispatches(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	intent, _, _ := mergedSetupBaselineIntentFixture(t, SetupBaselinePROpen)
	config := normalVisualSetupBaselineConfig(intent, stateDir)
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSetupBaselineIntent(intent); err != nil {
		t.Fatal(err)
	}
	dispatches := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main" {
			_, _ = io.WriteString(writer, `{"name":"main","commit":{"sha":"`+intent.CaptureHeadSHA+`"}}`)
			return
		}
		if request.Method == http.MethodPost {
			dispatches++
		}
		http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())

	before, _, err := store.LoadSetupBaselineIntent()
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		err := reconcileNormalVisualSetupBaselineBeforeProduction(context.Background(), store, config, client)
		if err == nil || !errors.Is(err, ErrSetupBaselineLifecycleHold) || !strings.Contains(err.Error(), "requires exact approval") {
			t.Fatalf("held replay %d escaped setup baseline gate: %v", attempt+1, err)
		}
		held, holdErr := NormalVisualWorkRequiresSetupQuiescence(stateDir)
		if holdErr != nil || !held {
			t.Fatalf("held replay %d did not release the approval lease: held=%t err=%v", attempt+1, held, holdErr)
		}
	}
	after, _, err := store.LoadSetupBaselineIntent()
	if err != nil {
		t.Fatal(err)
	}
	if dispatches != 0 || !reflect.DeepEqual(before, after) {
		t.Fatalf("held restart changed state or dispatched work: dispatches=%d before=%+v after=%+v", dispatches, before, after)
	}
}

func TestNormalVisualSetupBaselinePendingDispatchesOnlyCaptureOperation(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	intent, _, _ := mergedSetupBaselineIntentFixture(t, SetupBaselinePending)
	config := normalVisualSetupBaselineConfig(intent, stateDir)
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSetupBaselineIntent(intent); err != nil {
		t.Fatal(err)
	}
	var operations []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main":
			_, _ = io.WriteString(writer, `{"name":"main","commit":{"sha":"`+intent.SetupHeadSHA+`"}}`)
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/dispatches":
			var body struct {
				Inputs map[string]interface{} `json:"inputs"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode dispatch body: %v", err)
			}
			operation, _ := body.Inputs["hive_operation"].(string)
			operations = append(operations, operation)
			http.Error(writer, `{"message":"fixture rejection"}`, http.StatusUnprocessableEntity)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())

	err = reconcileNormalVisualSetupBaselineBeforeProduction(context.Background(), store, config, client)
	if err == nil || len(operations) != 1 || operations[0] != setupBaselineWorkflowOperation {
		t.Fatalf("pending baseline dispatched wrong operation: operations=%v err=%v", operations, err)
	}
	for _, operation := range operations {
		if operation == "production" {
			t.Fatalf("pending baseline reached ordinary production dispatch: %v", operations)
		}
	}
}

func TestNormalVisualSetupBaselinePreservesPrematureProductionIntent(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	intent, _, _ := mergedSetupBaselineIntentFixture(t, SetupBaselinePending)
	config := normalVisualSetupBaselineConfig(intent, stateDir)
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSetupBaselineIntent(intent); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	production, err := newWorkflowDispatchIntentForOperation(config, "hive-visual-hive.yml", config.DefaultBranch, "production", strings.Repeat("f", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	production.DispatchAttemptedAt, production.DispatchAcknowledgedAt = now, now
	production.RunID, production.RunURL, production.MatchedAt = 91, "https://example.test/runs/91", now
	if err := store.SaveWorkflowDispatchIntent(production); err != nil {
		t.Fatal(err)
	}
	beforeBaseline, _, err := store.LoadSetupBaselineIntent()
	if err != nil {
		t.Fatal(err)
	}
	beforeDispatch, _, err := store.LoadWorkflowDispatchIntent()
	if err != nil {
		t.Fatal(err)
	}
	dispatches := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main" {
			_, _ = io.WriteString(writer, `{"name":"main","commit":{"sha":"`+intent.SetupHeadSHA+`"}}`)
			return
		}
		if request.Method == http.MethodPost {
			dispatches++
		}
		http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())

	err = reconcileNormalVisualSetupBaselineBeforeProduction(context.Background(), store, config, client)
	if err == nil || !strings.Contains(err.Error(), "different or non-retired workflow dispatch") || dispatches != 0 {
		t.Fatalf("premature production intent did not fail closed: dispatches=%d err=%v", dispatches, err)
	}
	afterBaseline, _, baselineErr := store.LoadSetupBaselineIntent()
	afterDispatch, exists, dispatchErr := store.LoadWorkflowDispatchIntent()
	if baselineErr != nil || dispatchErr != nil || !exists || !reflect.DeepEqual(beforeBaseline, afterBaseline) || !reflect.DeepEqual(beforeDispatch, afterDispatch) {
		t.Fatalf("premature production intent was changed: baseline_before=%+v baseline_after=%+v dispatch_before=%+v dispatch_after=%+v baseline_err=%v dispatch_err=%v", beforeBaseline, afterBaseline, beforeDispatch, afterDispatch, baselineErr, dispatchErr)
	}
}

func TestNormalVisualSetupBaselineProductionVerifiedPermitsCadence(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	intent, _, _ := mergedSetupBaselineIntentFixture(t, SetupBaselineProductionVerified)
	config := normalVisualSetupBaselineConfig(intent, stateDir)
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSetupBaselineIntent(intent); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main" {
			_, _ = io.WriteString(writer, `{"name":"main","commit":{"sha":"`+intent.MergeSHA+`"}}`)
			return
		}
		http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())

	if err := reconcileNormalVisualSetupBaselineBeforeProduction(context.Background(), store, config, client); err != nil {
		t.Fatalf("production-verified normal cadence remained blocked: %v", err)
	}
	if err := requireNormalVisualSetupBaselineProductionReady(store, config); err != nil {
		t.Fatalf("defense-in-depth gate rejected production-verified state: %v", err)
	}
	if held, err := NormalVisualWorkRequiresSetupQuiescence(stateDir); err != nil || held {
		t.Fatalf("production-verified cadence remained quiesced: held=%t err=%v", held, err)
	}
}

func TestNormalVisualSetupBaselineRebindQuiescesWithoutChangingState(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := newPendingSetupBaselineProductionFixture(t, stateDir)
	if err := store.SaveSetupBaselineRebindIntent(fixture.rebind); err != nil {
		t.Fatal(err)
	}
	before, _, err := store.LoadSetupBaselineRebindIntent()
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		held, err := NormalVisualWorkRequiresSetupQuiescence(stateDir)
		if err != nil || !held {
			t.Fatalf("rebind probe %d did not quiesce: held=%t err=%v", attempt+1, held, err)
		}
	}
	after, _, err := store.LoadSetupBaselineRebindIntent()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("read-only rebind quiescence changed durable state: before=%+v after=%+v err=%v", before, after, err)
	}
}

func normalVisualSetupBaselineConfig(intent SetupBaselineIntent, stateDir string) Config {
	return Config{
		SchemaVersion: ConfigSchema, Repository: intent.Repository, RepositoryID: intent.RepositoryID, DefaultBranch: intent.DefaultBranch, StateDir: stateDir,
		ExecutionMode: ExecutionLocal, Automation: AutomationRepairPR, VisualHive: true, SetupBaselineRequired: true,
		SetupAuthorizationActorID: intent.AuthorizerID, SetupPRNumber: intent.SetupPRNumber, SetupPRURL: intent.SetupPRURL, SetupHeadSHA: intent.SetupHeadSHA,
		VisualHiveConfigDigest: intent.VisualHiveConfigDigest, SetupBaselineContractDigest: intent.ScreenshotContractDigest,
		SetupBaselineInitialDigest: intent.InitialBaselineDigest, SetupBaselineInitialCandidates: append([]SetupBaselineCandidate(nil), intent.InitialBaselineCandidates...),
	}
}

func TestSynchronizeNormalVisualWorkBaseFetchesExactHeadWithoutMovingCheckout(t *testing.T) {
	const repository = "owner/repo"
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	runIntegratedGit(t, root, "init", "--bare", remote)
	runIntegratedGit(t, root, "init", "-b", "main", seed)
	writeFixture(t, seed, "README.md", "initial\n")
	runIntegratedGit(t, seed, "add", "README.md")
	runIntegratedGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	runIntegratedGit(t, seed, "remote", "add", "origin", remote)
	runIntegratedGit(t, seed, "push", "-u", "origin", "main")
	runIntegratedGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	bindTestRepositoryCloneURL(t, remote)

	checkout := filepath.Join(root, "state", "integrated", "checkout")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if branch, err := ensureCheckout(ctx, repository, checkout); err != nil || branch != "main" {
		t.Fatalf("create managed checkout = %q, %v", branch, err)
	}
	initialHead := strings.TrimSpace(integratedGitOutput(t, checkout, "rev-parse", "HEAD"))

	writeFixture(t, seed, "defect.txt", "prepared defect\n")
	runIntegratedGit(t, seed, "add", "defect.txt")
	runIntegratedGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "prepared defect")
	runIntegratedGit(t, seed, "push", "origin", "main")
	verifiedHead := strings.TrimSpace(integratedGitOutput(t, seed, "rev-parse", "HEAD"))
	if initialHead == verifiedHead {
		t.Fatal("fixture did not advance the remote default branch")
	}
	if _, err := integratedGitOutputError(checkout, "cat-file", "-e", verifiedHead+"^{commit}"); err == nil {
		t.Fatal("managed checkout unexpectedly had the verified head before synchronization")
	}

	config := Config{Repository: repository, DefaultBranch: "main", CheckoutDir: checkout}
	tree, err := synchronizeNormalVisualWorkBase(ctx, config, verifiedHead)
	if err != nil {
		t.Fatal(err)
	}
	wantTree := strings.TrimSpace(integratedGitOutput(t, seed, "rev-parse", verifiedHead+"^{tree}"))
	if tree != wantTree {
		t.Fatalf("verified tree = %s, want %s", tree, wantTree)
	}
	if fetched := strings.TrimSpace(integratedGitOutput(t, checkout, "rev-parse", "refs/remotes/origin/main")); fetched != verifiedHead {
		t.Fatalf("fetched default head = %s, want %s", fetched, verifiedHead)
	}
	if current := strings.TrimSpace(integratedGitOutput(t, checkout, "rev-parse", "HEAD")); current != initialHead {
		t.Fatalf("synchronization moved checkout HEAD from %s to %s", initialHead, current)
	}
	if status := strings.TrimSpace(integratedGitOutput(t, checkout, "status", "--short")); status != "" {
		t.Fatalf("synchronization changed the checkout worktree: %q", status)
	}
	if _, err := synchronizeNormalVisualWorkBase(ctx, config, initialHead); err == nil || !strings.Contains(err.Error(), "does not match verified workflow head") {
		t.Fatalf("stale verified head did not fail closed: %v", err)
	}
}

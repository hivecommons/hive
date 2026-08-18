package integrated

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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

	hivegithub "github.com/kubestellar/hive/pkg/github"
)

func TestAuthorizerTransferKeepsOldAuthorityUntilExactMergedTargetAndRecoversWithNewActor(t *testing.T) {
	fixture := newAuthorizerTransferFixture(t)
	defer fixture.server.Close()
	options := AuthorizerTransferOptions{StateDir: fixture.stateDir, NewAuthorizer: "next-operator", Reason: "planned maintainer handoff", GitHub: fixture.client}

	pending, err := RunAuthorizerTransfer(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !pending.FinalizationPending || pending.Completed || pending.PRNumber != 11 || pending.SetupAuthorizationCreatorID != 456 || pending.SetupAuthorizationStatusID != 901 || pending.NextCommand == "" || pending.CancelCommand == "" {
		t.Fatalf("pending transfer=%+v", pending)
	}
	store, err := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.SetupAuthorizationActorID != 456 {
		t.Fatalf("open transfer changed durable authority early: %+v", config)
	}
	intent, exists, err := store.LoadAuthorizerTransferIntent()
	if err != nil || !exists || intent.Phase != authorizerTransferAuthorized || intent.OldAuthorizerID != 456 || intent.NewAuthorizerID != 789 || intent.AuthorizationCreator != 456 {
		t.Fatalf("durable transfer intent=%+v exists=%t err=%v", intent, exists, err)
	}
	if _, err := RunOnce(context.Background(), RunOptions{StateDir: fixture.stateDir, Timeout: time.Minute, GitHub: fixture.client}); err == nil || !strings.Contains(err.Error(), "setup authorizer transfer") || !strings.Contains(err.Error(), "is pending") {
		t.Fatalf("production run was not blocked by transfer intent: %v", err)
	}
	if _, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUpgrade, StateDir: fixture.stateDir, VisualHiveRef: strings.Repeat("b", 40), GitHub: fixture.client}); err == nil || !strings.Contains(err.Error(), "authorizer transfer") {
		t.Fatalf("competing management operation was not blocked: %v", err)
	}

	fixture.merge(t, pending.CommitSHA)
	fixture.setCurrentUser(789, "next-operator")
	completed, err := RunAuthorizerTransfer(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.Completed || completed.FinalizationPending || completed.CurrentDefaultHead != pending.CommitSHA || completed.NewAuthorizerID != 789 {
		t.Fatalf("completed transfer=%+v", completed)
	}
	config, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.SetupAuthorizationActorID != 789 || config.SetupBranch != pending.Branch || config.SetupPRNumber != pending.PRNumber {
		t.Fatalf("durable authority did not switch exactly after merge: %+v", config)
	}
	if _, exists, err := store.LoadAuthorizerTransferIntent(); err != nil || exists {
		t.Fatalf("completed transfer intent remains: exists=%t err=%v", exists, err)
	}
	audit, err := os.ReadFile(filepath.Join(fixture.stateDir, "integrated", "audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), "setup_authorizer_transfer_prepared") || !strings.Contains(string(audit), "setup_authorizer_transfer_authorized") || !strings.Contains(string(audit), "setup_authorizer_transfer_completed") {
		t.Fatalf("transfer audit is incomplete: err=%v\n%s", err, audit)
	}
}

func TestAuthorizerTransferCancelRequiresOldActorAndExactUnmergedResources(t *testing.T) {
	fixture := newAuthorizerTransferFixture(t)
	defer fixture.server.Close()
	options := AuthorizerTransferOptions{StateDir: fixture.stateDir, NewAuthorizer: "next-operator", Reason: "wrong planned handoff", GitHub: fixture.client}
	pending, err := RunAuthorizerTransfer(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setCurrentUser(789, "next-operator")
	if _, err := RunAuthorizerTransfer(context.Background(), AuthorizerTransferOptions{StateDir: fixture.stateDir, Cancel: true, GitHub: fixture.client}); err == nil || !strings.Contains(err.Error(), "old authorizer") {
		t.Fatalf("new actor cancelled pending transfer: %v", err)
	}
	fixture.setCurrentUser(456, "setup-operator")
	cancelled, err := RunAuthorizerTransfer(context.Background(), AuthorizerTransferOptions{StateDir: fixture.stateDir, Cancel: true, GitHub: fixture.client})
	if err != nil {
		t.Fatal(err)
	}
	if !cancelled.Cancelled || cancelled.FinalizationPending {
		t.Fatalf("cancel result=%+v", cancelled)
	}
	store, _ := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	config, err := store.Load()
	if err != nil || config.SetupAuthorizationActorID != 456 {
		t.Fatalf("cancel changed old authority: config=%+v err=%v", config, err)
	}
	if _, exists, err := store.LoadAuthorizerTransferIntent(); err != nil || exists {
		t.Fatalf("cancel did not remove durable intent: exists=%t err=%v", exists, err)
	}
	if _, err := integratedGitOutputError(fixture.remote, "rev-parse", "refs/heads/"+pending.Branch); err == nil {
		t.Fatal("cancel left the exact transfer branch behind")
	}
	fixture.mu.Lock()
	state := fixture.prState
	fixture.mu.Unlock()
	if state != "closed" {
		t.Fatalf("cancel did not close exact open PR: state=%s", state)
	}
}

func TestAuthorizerTransferCancelRefusesMergedPull(t *testing.T) {
	fixture := newAuthorizerTransferFixture(t)
	defer fixture.server.Close()
	options := AuthorizerTransferOptions{StateDir: fixture.stateDir, NewAuthorizer: "next-operator", Reason: "merged handoff", GitHub: fixture.client}
	pending, err := RunAuthorizerTransfer(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	fixture.merge(t, pending.CommitSHA)
	if _, err := RunAuthorizerTransfer(context.Background(), AuthorizerTransferOptions{StateDir: fixture.stateDir, Cancel: true, GitHub: fixture.client}); err == nil || !strings.Contains(err.Error(), "already merged") {
		t.Fatalf("merged transfer was cancellable: %v", err)
	}
	store, _ := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	if _, exists, err := store.LoadAuthorizerTransferIntent(); err != nil || !exists {
		t.Fatalf("merged cancel failure discarded intent: exists=%t err=%v", exists, err)
	}
}

func TestAuthorizerTransferCancelRecoversStrictBaseAndDescendantHeadDriftThenRestarts(t *testing.T) {
	fixture := newAuthorizerTransferFixture(t)
	defer fixture.server.Close()
	options := AuthorizerTransferOptions{StateDir: fixture.stateDir, NewAuthorizer: "next-operator", Reason: "restartable handoff", GitHub: fixture.client}
	pending, err := RunAuthorizerTransfer(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	firstIntent, exists, err := store.LoadAuthorizerTransferIntent()
	if err != nil || !exists {
		t.Fatalf("first transfer intent missing: exists=%t err=%v", exists, err)
	}
	fixture.advanceBase(t)
	driftedHead := fixture.advanceTransferHead(t, pending.Branch)
	if _, err := RunAuthorizerTransfer(context.Background(), options); err == nil || !strings.Contains(err.Error(), "--cancel") {
		t.Fatalf("strict transfer drift did not return the supported recovery command: %v", err)
	}

	cancelled, err := RunAuthorizerTransfer(context.Background(), AuthorizerTransferOptions{StateDir: fixture.stateDir, Cancel: true, GitHub: fixture.client})
	if err != nil || !cancelled.Cancelled {
		t.Fatalf("drifted transfer cancellation=%+v err=%v", cancelled, err)
	}
	if _, err := integratedGitOutputError(fixture.remote, "rev-parse", "refs/heads/"+pending.Branch); err == nil {
		t.Fatalf("descendant drifted transfer ref %s was not retired", driftedHead)
	}

	restarted, err := RunAuthorizerTransfer(context.Background(), options)
	if err != nil || !restarted.FinalizationPending || restarted.PRNumber != 11 {
		t.Fatalf("cancelled transfer could not restart: result=%+v err=%v", restarted, err)
	}
	secondIntent, exists, err := store.LoadAuthorizerTransferIntent()
	if err != nil || !exists || secondIntent.Marker == firstIntent.Marker {
		t.Fatalf("restarted transfer did not receive a fresh exact PR identity: first=%q second=%q exists=%t err=%v", firstIntent.Marker, secondIntent.Marker, exists, err)
	}
}

func TestAuthorizerTransferCancelRetriesAfterPRCloseBranchDeleteFailure(t *testing.T) {
	fixture := newAuthorizerTransferFixture(t)
	defer fixture.server.Close()
	options := AuthorizerTransferOptions{StateDir: fixture.stateDir, NewAuthorizer: "next-operator", Reason: "retryable cancellation", GitHub: fixture.client}
	if _, err := RunAuthorizerTransfer(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.deleteFailures = 1
	fixture.mu.Unlock()
	if _, err := RunAuthorizerTransfer(context.Background(), AuthorizerTransferOptions{StateDir: fixture.stateDir, Cancel: true, GitHub: fixture.client}); err == nil || !strings.Contains(err.Error(), "retire exact") {
		t.Fatalf("injected branch retirement failure was not preserved: %v", err)
	}
	fixture.mu.Lock()
	closed := fixture.prState == "closed"
	fixture.mu.Unlock()
	store, _ := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	if _, exists, err := store.LoadAuthorizerTransferIntent(); !closed || err != nil || !exists {
		t.Fatalf("partial cancellation was not resumable: pr_closed=%t intent_exists=%t err=%v", closed, exists, err)
	}
	result, err := RunAuthorizerTransfer(context.Background(), AuthorizerTransferOptions{StateDir: fixture.stateDir, Cancel: true, GitHub: fixture.client})
	if err != nil || !result.Cancelled {
		t.Fatalf("partial cancellation retry=%+v err=%v", result, err)
	}
}

func TestAuthorizerTransferCancelRejectsWrongDurableOrRemoteIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *authorizerTransferFixture)
	}{
		{name: "marker", mutate: func(_ *testing.T, fixture *authorizerTransferFixture) { fixture.prBody = "marker removed" }},
		{name: "branch", mutate: func(_ *testing.T, fixture *authorizerTransferFixture) { fixture.prHead = "hive/not-the-owned-transfer" }},
		{name: "non-descendant head", mutate: func(t *testing.T, fixture *authorizerTransferFixture) {
			base := strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", "refs/heads/main"))
			runIntegratedGit(t, fixture.remote, "update-ref", "refs/heads/"+fixture.prHead, base)
			fixture.prHeadSHA = base
		}},
		{name: "recorded pr", mutate: func(t *testing.T, fixture *authorizerTransferFixture) {
			store, _ := NewStore(filepath.Join(fixture.stateDir, "integrated"))
			intent, _, _ := store.LoadAuthorizerTransferIntent()
			intent.PRNumber, intent.PRURL = 99, "https://example.test/owner/repo/pull/99"
			if err := store.SaveAuthorizerTransferIntent(intent); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAuthorizerTransferFixture(t)
			defer fixture.server.Close()
			if _, err := RunAuthorizerTransfer(context.Background(), AuthorizerTransferOptions{StateDir: fixture.stateDir, NewAuthorizer: "next-operator", Reason: "identity rejection", GitHub: fixture.client}); err != nil {
				t.Fatal(err)
			}
			fixture.mu.Lock()
			test.mutate(t, fixture)
			fixture.mu.Unlock()
			if _, err := RunAuthorizerTransfer(context.Background(), AuthorizerTransferOptions{StateDir: fixture.stateDir, Cancel: true, GitHub: fixture.client}); err == nil {
				t.Fatal("mismatched transfer identity was cancellable")
			}
			store, _ := NewStore(filepath.Join(fixture.stateDir, "integrated"))
			intent, exists, err := store.LoadAuthorizerTransferIntent()
			if err != nil || !exists {
				t.Fatalf("failed cancellation discarded evidence: exists=%t err=%v", exists, err)
			}
			if _, err := integratedGitOutputError(fixture.remote, "rev-parse", "refs/heads/"+intent.Branch); err != nil {
				t.Fatalf("failed cancellation deleted an unproven branch: %v", err)
			}
		})
	}
}

func TestAuthorizerTransferWorkflowUsesDedicatedOldActorAuthorizedOperation(t *testing.T) {
	workflow := pullRequestWorkflow(Config{RepositoryID: "123", DefaultBranch: "main", SetupBranch: "hive/setup-123", SetupAuthorizationActorID: 456, VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40)})
	for _, required := range []string{"HIVE_EXPECTED_AUTHORIZER_TRANSFER_REF: hive/authorizer-transfer-123", `operation = "authorizer-transfer"`, `HIVE_AUTHORIZER_ID: "456"`} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("transfer workflow omitted %q", required)
		}
	}
	transferCandidate := pullRequestWorkflow(Config{RepositoryID: "123", DefaultBranch: "main", SetupBranch: "hive/authorizer-transfer-123", SetupAuthorizationActorID: 789, SetupAuthorizationPreviousActorID: 456, VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40)})
	if !strings.Contains(transferCandidate, `HIVE_AUTHORIZER_ID: "789"`) || !strings.Contains(transferCandidate, `HIVE_TRANSFER_AUTHORIZER_ID: "456"`) {
		t.Fatal("transfer candidate did not keep the old actor as its dedicated transfer authorizer")
	}
}

func TestAuthorizerTransferIntentSeparatesHumanActorFromAppStatusWriter(t *testing.T) {
	marker, err := newAuthorizerTransferMarker("owner/repo", 456, 789)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	intent := AuthorizerTransferIntent{
		SchemaVersion: authorizerTransferIntentSchema, Phase: authorizerTransferAuthorized,
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", VisualHive: true, ExecutionMode: ExecutionLocal,
		SourceConfigDigest: strings.Repeat("a", 64), OldAuthorizerID: 456, OldAuthorizerLogin: "old-owner",
		NewAuthorizerID: 789, NewAuthorizerLogin: "new-owner", WriterID: 202, WriterLogin: "hive-test[bot]", WriterType: "Bot",
		Reason: "rotate accountable owner", Branch: managedOperationBranch(string(OperationAuthorizerTransfer), "123"),
		BaseSHA: strings.Repeat("b", 40), HeadSHA: strings.Repeat("c", 40), Marker: marker,
		ChangedFiles: []string{".hive/integrated.json"}, PRNumber: 11, PRURL: "https://github.test/owner/repo/pull/11",
		PullDiffDigest: strings.Repeat("d", 64), SetupDiffDigest: strings.Repeat("e", 64),
		AuthorizationContext: SetupAuthorizationContextPrefix + strings.Repeat("f", 64), AuthorizationStatus: 901, AuthorizationCreator: 202,
		PreparedAt: now, PullRecordedAt: now, AuthorizedAt: now,
	}
	if err := validateAuthorizerTransferIntent(intent); err != nil {
		t.Fatalf("App-written transfer authorization was rejected: %v", err)
	}
	intent.AuthorizationCreator = intent.OldAuthorizerID
	if err := validateAuthorizerTransferIntent(intent); err == nil {
		t.Fatal("human actor was accepted as the App-written status creator")
	}
}

type authorizerTransferFixture struct {
	t              *testing.T
	stateDir       string
	remote         string
	server         *httptest.Server
	client         *hivegithub.Client
	mu             sync.Mutex
	currentID      int64
	current        string
	prCreated      bool
	prState        string
	prMerged       bool
	prMergeSHA     string
	prBaseSHA      string
	prTitle        string
	prBody         string
	prHead         string
	status         map[string]any
	prHeadSHA      string
	deleteFailures int
}

func newAuthorizerTransferFixture(t *testing.T) *authorizerTransferFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &authorizerTransferFixture{t: t, stateDir: filepath.Join(root, "state"), remote: filepath.Join(root, "remote.git"), currentID: 456, current: "setup-operator", prState: "open"}
	bindTestRepositoryCloneURL(t, fixture.remote)
	runIntegratedGit(t, root, "init", "--bare", fixture.remote)
	seed := filepath.Join(root, "seed")
	runIntegratedGit(t, root, "init", "-b", "main", seed)
	writeFixture(t, seed, "README.md", "authorizer transfer fixture\n")
	writeFixture(t, seed, "package.json", "{\"scripts\":{\"test\":\"node --test\"}}\n")
	visualConfig := "project:\n  name: transfer-fixture\n  setupProfile: complex-app\ntargets: []\nintegrations:\n  hive:\n    enabled: true\n"
	writeFixture(t, seed, "visual-hive.config.yaml", visualConfig)
	writeFixture(t, seed, "docs/visual-hive.md", "# Visual Hive\n\nTransfer fixture.\n")
	visualDigest := sha256.Sum256([]byte(normalizeManagedText(visualConfig)))
	checkout := filepath.Join(fixture.stateDir, "integrated", "checkouts", "owner--repo")
	config := Config{
		SchemaVersion: ConfigSchema, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		Coverage: CoverageComprehensive, Automation: AutomationAdvisory, Provider: "codex", ProviderCommand: os.Args[0],
		ExecutionMode: ExecutionLocal,
		ACMMLevel:     2, MaxActiveIssues: 5, MaxRepairAttempts: 3, VisualHive: true,
		VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40), VisualHiveConfigDigest: hex.EncodeToString(visualDigest[:]),
		TestCommands: [][]string{{"node", "--test"}}, AllowedRepairPaths: []string{"**/*.test.*"},
		CheckoutDir: checkout, StateDir: fixture.stateDir, SetupBranch: "hive/setup-123", SetupAuthorizationActorID: 456,
	}
	inspection := RepositoryInspection{DefaultBranch: "main", RepositoryID: "123", Languages: []string{"TypeScript/JavaScript"}, TestCommands: config.TestCommands}
	if err := writeManagedFiles(seed, config, inspection); err != nil {
		t.Fatal(err)
	}
	runIntegratedGit(t, seed, "add", ".")
	runIntegratedGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "seed managed setup")
	runIntegratedGit(t, seed, "remote", "add", "origin", fixture.remote)
	runIntegratedGit(t, seed, "push", "-u", "origin", "main")
	runIntegratedGit(t, fixture.remote, "symbolic-ref", "HEAD", "refs/heads/main")
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
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	fixture.client = hivegithub.NewClientForTest(fixture.server.URL, "owner", []string{"repo"}, slog.Default())
	return fixture
}

func (fixture *authorizerTransferFixture) setCurrentUser(id int64, login string) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.currentID, fixture.current = id, login
}

func (fixture *authorizerTransferFixture) merge(t *testing.T, head string) {
	t.Helper()
	runIntegratedGit(t, fixture.remote, "update-ref", "refs/heads/main", head)
	fixture.mu.Lock()
	fixture.prState, fixture.prMerged, fixture.prMergeSHA = "closed", true, head
	fixture.mu.Unlock()
}

func (fixture *authorizerTransferFixture) advanceBase(t *testing.T) {
	t.Helper()
	clone := filepath.Join(filepath.Dir(fixture.remote), "transfer-base-advance")
	runIntegratedGit(t, filepath.Dir(fixture.remote), "clone", fixture.remote, clone)
	writeFixture(t, clone, "base-drift.txt", "base advanced\n")
	runIntegratedGit(t, clone, "add", "base-drift.txt")
	runIntegratedGit(t, clone, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "advance base")
	runIntegratedGit(t, clone, "push", "origin", "main")
	fixture.mu.Lock()
	fixture.prBaseSHA = strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", "refs/heads/main"))
	fixture.mu.Unlock()
}

func (fixture *authorizerTransferFixture) advanceTransferHead(t *testing.T, branch string) string {
	t.Helper()
	clone := filepath.Join(filepath.Dir(fixture.remote), "transfer-head-advance")
	runIntegratedGit(t, filepath.Dir(fixture.remote), "clone", fixture.remote, clone)
	runIntegratedGit(t, clone, "switch", "-c", branch, "origin/"+branch)
	writeFixture(t, clone, "head-drift.txt", "updated branch\n")
	runIntegratedGit(t, clone, "add", "head-drift.txt")
	runIntegratedGit(t, clone, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "update transfer branch")
	runIntegratedGit(t, clone, "push", "origin", branch)
	head := strings.TrimSpace(integratedGitOutput(t, fixture.remote, "rev-parse", "refs/heads/"+branch))
	fixture.mu.Lock()
	fixture.prHeadSHA = head
	fixture.mu.Unlock()
	return head
}

func (fixture *authorizerTransferFixture) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	path := request.URL.Path
	switch {
	case request.Method == http.MethodGet && path == "/user":
		writeAuthorizerTransferJSON(fixture.t, writer, map[string]any{"id": fixture.currentID, "login": fixture.current, "type": "User"})
	case request.Method == http.MethodGet && path == "/users/next-operator":
		writeAuthorizerTransferJSON(fixture.t, writer, map[string]any{"id": 789, "login": "next-operator", "type": "User"})
	case request.Method == http.MethodGet && path == "/repos/owner/repo":
		writeAuthorizerTransferJSON(fixture.t, writer, map[string]any{"id": 123, "full_name": "owner/repo", "default_branch": "main"})
	case request.Method == http.MethodGet && path == "/repos/owner/repo/branches/main":
		head := strings.TrimSpace(integratedGitOutput(fixture.t, fixture.remote, "rev-parse", "refs/heads/main"))
		writeAuthorizerTransferJSON(fixture.t, writer, map[string]any{"name": "main", "commit": map[string]any{"sha": head}})
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/repos/owner/repo/contents/"):
		relative := strings.TrimPrefix(path, "/repos/owner/repo/contents/")
		ref := request.URL.Query().Get("ref")
		if ref == "" {
			ref = "main"
		}
		content, err := integratedGitOutputError(fixture.remote, "show", ref+":"+relative)
		if err != nil {
			http.Error(writer, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		writeAuthorizerTransferJSON(fixture.t, writer, map[string]any{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content))})
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/repos/owner/repo/git/ref/heads/"):
		branch := strings.TrimPrefix(path, "/repos/owner/repo/git/ref/heads/")
		head, err := integratedGitOutputError(fixture.remote, "rev-parse", "refs/heads/"+branch)
		if err != nil {
			http.Error(writer, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		writeAuthorizerTransferJSON(fixture.t, writer, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"type": "commit", "sha": strings.TrimSpace(head)}})
	case request.Method == http.MethodDelete && strings.HasPrefix(path, "/repos/owner/repo/git/refs/heads/"):
		if fixture.deleteFailures > 0 {
			fixture.deleteFailures--
			http.Error(writer, `{"message":"injected delete failure"}`, http.StatusInternalServerError)
			return
		}
		branch := strings.TrimPrefix(path, "/repos/owner/repo/git/refs/heads/")
		runIntegratedGit(fixture.t, fixture.remote, "update-ref", "-d", "refs/heads/"+branch)
		writer.WriteHeader(http.StatusNoContent)
	case request.Method == http.MethodGet && path == "/repos/owner/repo/pulls":
		state := request.URL.Query().Get("state")
		if !fixture.prCreated || (state == "open" && fixture.prState != "open") {
			writeAuthorizerTransferJSON(fixture.t, writer, []any{})
			return
		}
		writeAuthorizerTransferJSON(fixture.t, writer, []any{fixture.pullJSON()})
	case request.Method == http.MethodPost && path == "/repos/owner/repo/pulls":
		var input struct {
			Title string `json:"title"`
			Body  string `json:"body"`
			Head  string `json:"head"`
			Base  string `json:"base"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			fixture.t.Fatal(err)
		}
		fixture.prCreated, fixture.prState = true, "open"
		fixture.prTitle, fixture.prBody, fixture.prHead = input.Title, input.Body, input.Head
		fixture.prBaseSHA = strings.TrimSpace(integratedGitOutput(fixture.t, fixture.remote, "rev-parse", "refs/heads/"+input.Base))
		writer.WriteHeader(http.StatusCreated)
		writeAuthorizerTransferJSON(fixture.t, writer, fixture.pullJSON())
	case request.Method == http.MethodPatch && path == "/repos/owner/repo/pulls/11":
		var input struct {
			State string `json:"state"`
		}
		_ = json.NewDecoder(request.Body).Decode(&input)
		if input.State != "" {
			fixture.prState = input.State
		}
		writeAuthorizerTransferJSON(fixture.t, writer, fixture.pullJSON())
	case request.Method == http.MethodGet && path == "/repos/owner/repo/pulls/11/files":
		head := strings.TrimSpace(integratedGitOutput(fixture.t, fixture.remote, "rev-parse", "refs/heads/"+fixture.prHead))
		changed := strings.Fields(strings.TrimSpace(integratedGitOutput(fixture.t, fixture.remote, "diff", "--name-only", fixture.prBaseSHA, head)))
		files := make([]map[string]any, 0, len(changed))
		for _, relative := range changed {
			files = append(files, map[string]any{"filename": relative, "status": "modified"})
		}
		writeAuthorizerTransferJSON(fixture.t, writer, files)
	case request.Method == http.MethodGet && path == "/repos/owner/repo/pulls/11" && strings.Contains(request.Header.Get("Accept"), "diff"):
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(writer, "exact authorizer transfer diff\n")
	case request.Method == http.MethodGet && path == "/repos/owner/repo/pulls/11":
		writeAuthorizerTransferJSON(fixture.t, writer, fixture.pullJSON())
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
		writeAuthorizerTransferJSON(fixture.t, writer, map[string]any{"status": status, "behind_by": behind})
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/repos/owner/repo/commits/") && strings.HasSuffix(path, "/statuses"):
		if fixture.status == nil {
			writeAuthorizerTransferJSON(fixture.t, writer, []any{})
			return
		}
		writeAuthorizerTransferJSON(fixture.t, writer, []any{fixture.status})
	case request.Method == http.MethodPost && strings.HasPrefix(path, "/repos/owner/repo/statuses/"):
		var input struct {
			State     string `json:"state"`
			Context   string `json:"context"`
			TargetURL string `json:"target_url"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			fixture.t.Fatal(err)
		}
		fixture.status = map[string]any{"id": 901, "state": input.State, "context": input.Context, "target_url": input.TargetURL, "creator": map[string]any{"id": 456, "login": "setup-operator", "type": "User"}}
		writer.WriteHeader(http.StatusCreated)
		writeAuthorizerTransferJSON(fixture.t, writer, fixture.status)
	default:
		http.Error(writer, fmt.Sprintf(`{"message":"unexpected %s %s"}`, request.Method, path), http.StatusNotFound)
	}
}

func (fixture *authorizerTransferFixture) pullJSON() map[string]any {
	head := fixture.prHeadSHA
	if fixture.prHead != "" {
		if current, err := integratedGitOutputError(fixture.remote, "rev-parse", "refs/heads/"+fixture.prHead); err == nil {
			head = strings.TrimSpace(current)
			fixture.prHeadSHA = head
		}
	}
	changedFiles := []string(nil)
	if fixture.prBaseSHA != "" && head != "" {
		if output, err := integratedGitOutputError(fixture.remote, "diff", "--name-only", fixture.prBaseSHA, head); err == nil {
			changedFiles = strings.Fields(strings.TrimSpace(output))
		}
	}
	repository := map[string]any{"id": 123, "full_name": "owner/repo"}
	return map[string]any{
		"number": 11, "html_url": "https://example.test/owner/repo/pull/11", "state": fixture.prState, "merged": fixture.prMerged, "merge_commit_sha": fixture.prMergeSHA,
		"title": fixture.prTitle, "body": fixture.prBody, "changed_files": len(changedFiles),
		"head": map[string]any{"ref": fixture.prHead, "sha": head, "repo": repository},
		"base": map[string]any{"ref": "main", "sha": fixture.prBaseSHA, "repo": repository},
	}
}

func writeAuthorizerTransferJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func integratedGitOutputError(repository string, args ...string) (string, error) {
	return setupAcceptanceGitOutput(repository, args...)
}

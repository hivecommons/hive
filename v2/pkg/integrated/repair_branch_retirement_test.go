package integrated

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestRepairRetirementStateRejectsTamperingAndUnknownFields(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Repository: "owner/repo", RepositoryID: "123"}
	finding := retirementFinding(visualhive.StatusMerged)
	entry, err := newRepairRetirement(config, finding, finding.PRNumber, 0, finding.Branch, finding.RepairCommitSHA, finding.MergeSHA, repairRetirementPurposeMerge, repairRetirementDeleteBranch)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRepairBranchRetirements(RepairBranchRetirementState{MigrationCompleted: true, Entries: []RepairBranchRetirement{entry}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir(), repairRetirementFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"repair_attempts": 2`, `"repair_attempts": 3`, 1)
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadRepairBranchRetirements(); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("tampered retirement was accepted: %v", err)
	}

	if err := store.SaveRepairBranchRetirements(RepairBranchRetirementState{MigrationCompleted: true, Entries: []RepairBranchRetirement{entry}}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	unknown := strings.Replace(string(data), `"schema_version":`, `"unknown":true,"schema_version":`, 1)
	if err := os.WriteFile(path, []byte(unknown), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadRepairBranchRetirements(); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown retirement state was accepted: %v", err)
	}
}

func TestRepairRetirementEnqueueAuditFailureDoesNotAdvanceOutbox(t *testing.T) {
	_, store, lifecycle, config := newRetirementFixture(t, retirementFinding(visualhive.StatusMerged))
	finding, _ := lifecycle.Finding("finding")
	auditPath := filepath.Join(store.Dir(), "audit.jsonl")
	if err := os.Mkdir(auditPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := enqueueMergedRepairRetirement(store, config, finding, finding.MergeSHA); err == nil {
		t.Fatal("audit write failure advanced repair retirement enqueue")
	}
	if _, exists, err := store.LoadRepairBranchRetirements(); err != nil || exists {
		t.Fatalf("audit failure left unaudited retirement state: exists=%t err=%v", exists, err)
	}
	if err := os.Remove(auditPath); err != nil {
		t.Fatal(err)
	}
	if err := enqueueMergedRepairRetirement(store, config, finding, finding.MergeSHA); err != nil {
		t.Fatalf("retry after restored audit sink failed: %v", err)
	}
	if state, exists, err := store.LoadRepairBranchRetirements(); err != nil || !exists || len(state.Entries) != 1 {
		t.Fatalf("audited retry did not create retirement: state=%+v exists=%t err=%v", state, exists, err)
	}
}

func TestMergedRepairRetirementPersistsDeleteFailureAndRetries(t *testing.T) {
	head := strings.Repeat("a", 40)
	deleteCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/repair-proof":
			_, _ = fmt.Fprintf(writer, `{"ref":"refs/heads/hive/repair-proof","object":{"sha":%q,"type":"commit"}}`, head)
		case request.Method == http.MethodDelete && request.URL.Path == "/repos/owner/repo/git/refs/heads/hive/repair-proof":
			deleteCalls++
			if deleteCalls == 1 {
				http.Error(writer, "transient", http.StatusInternalServerError)
				return
			}
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	stateDir, store, lifecycle, config := newRetirementFixture(t, retirementFinding(visualhive.StatusMerged))
	finding, _ := lifecycle.Finding("finding")
	if err := enqueueMergedRepairRetirement(store, config, finding, finding.MergeSHA); err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	err := reconcileRepairRetirements(context.Background(), store, config, lifecycle, client, retirementPolicy(), false)
	if err == nil || !strings.Contains(err.Error(), "delete Hive branch") {
		t.Fatalf("transient deletion did not block reconciliation: %v", err)
	}
	state, _, loadErr := store.LoadRepairBranchRetirements()
	if loadErr != nil || len(state.Entries) != 1 || state.Entries[0].LastError == "" || state.Entries[0].Attempts != 1 {
		t.Fatalf("failed retirement was not durable: state=%+v err=%v", state, loadErr)
	}
	if err := reconcileRepairRetirements(context.Background(), store, config, lifecycle, client, retirementPolicy(), false); err != nil {
		t.Fatal(err)
	}
	state, _, _ = store.LoadRepairBranchRetirements()
	if deleteCalls != 2 || len(state.Entries) != 0 {
		t.Fatalf("durable retirement did not retry exactly once: deletes=%d state=%+v root=%s", deleteCalls, state, stateDir)
	}
}

func TestRepairRetirementMissingBranchSucceedsButMovedHeadFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		observed    string
		wantError   bool
		wantPending int
	}{
		{name: "missing", status: http.StatusNotFound, wantPending: 0},
		{name: "moved", status: http.StatusOK, observed: strings.Repeat("f", 40), wantError: true, wantPending: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, store, lifecycle, config := newRetirementFixture(t, retirementFinding(visualhive.StatusMerged))
			finding, _ := lifecycle.Finding("finding")
			if err := enqueueMergedRepairRetirement(store, config, finding, finding.MergeSHA); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if test.status == http.StatusNotFound {
					http.Error(writer, "missing", http.StatusNotFound)
					return
				}
				_, _ = fmt.Fprintf(writer, `{"ref":"refs/heads/hive/repair-proof","object":{"sha":%q,"type":"commit"}}`, test.observed)
			}))
			defer server.Close()
			client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
			err := reconcileRepairRetirements(context.Background(), store, config, lifecycle, client, retirementPolicy(), false)
			if (err != nil) != test.wantError {
				t.Fatalf("reconcile error=%v, want error=%t", err, test.wantError)
			}
			state, _, _ := store.LoadRepairBranchRetirements()
			if len(state.Entries) != test.wantPending {
				t.Fatalf("pending=%d, want %d: %+v", len(state.Entries), test.wantPending, state)
			}
		})
	}
}

func TestSupersededRepairRetirementRetriesWithoutOpenPRRediscovery(t *testing.T) {
	oldHead := strings.Repeat("a", 40)
	closed, pullReads, deleteCalls, listCalls := false, 0, 0, 0
	marker := "<!-- hive-repair: finding -->"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			listCalls++
			_, _ = io.WriteString(writer, `[]`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/11":
			pullReads++
			state := "open"
			if closed {
				state = "closed"
			}
			_, _ = fmt.Fprintf(writer, `{"number":11,"state":%q,"body":%q,"head":{"ref":"hive/repair-old","sha":%q,"repo":{"id":123,"full_name":"owner/repo"}},"base":{"ref":"main","repo":{"id":123,"full_name":"owner/repo"}}}`, state, marker, oldHead)
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/pulls/11":
			closed = true
			_, _ = io.WriteString(writer, `{"number":11,"state":"closed"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/git/ref/heads/hive/repair-old":
			_, _ = fmt.Fprintf(writer, `{"ref":"refs/heads/hive/repair-old","object":{"sha":%q,"type":"commit"}}`, oldHead)
		case request.Method == http.MethodDelete && request.URL.Path == "/repos/owner/repo/git/refs/heads/hive/repair-old":
			deleteCalls++
			if deleteCalls == 1 {
				http.Error(writer, "retry deletion", http.StatusServiceUnavailable)
				return
			}
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	finding := retirementFinding(visualhive.StatusReady)
	finding.PRNumber, finding.Branch, finding.RepairCommitSHA = 12, "hive/repair-current", strings.Repeat("b", 40)
	_, store, lifecycle, config := newRetirementFixture(t, finding)
	if err := enqueueSupersededRepairRetirement(store, config, finding, 11, "hive/repair-old", oldHead); err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if err := reconcileRepairRetirements(context.Background(), store, config, lifecycle, client, retirementPolicy(), false); err == nil {
		t.Fatal("transient superseded branch deletion did not remain pending")
	}
	state, _, _ := store.LoadRepairBranchRetirements()
	if !closed || pullReads != 2 || len(state.Entries) != 1 || state.Entries[0].Phase != repairRetirementDeleteBranch {
		t.Fatalf("close checkpoint was not durable: closed=%t reads=%d state=%+v", closed, pullReads, state)
	}
	if err := reconcileRepairRetirements(context.Background(), store, config, lifecycle, client, retirementPolicy(), false); err != nil {
		t.Fatal(err)
	}
	if pullReads != 2 || listCalls != 0 || deleteCalls != 2 {
		t.Fatalf("retry rediscovered or reclosed superseded PR: reads=%d lists=%d deletes=%d", pullReads, listCalls, deleteCalls)
	}
}

func TestLegacyTerminalRepairMigrationRunsOnce(t *testing.T) {
	_, store, lifecycle, config := newRetirementFixture(t, retirementFinding(visualhive.StatusIssueClosed))
	if err := prepareRepairRetirements(store, config, lifecycle); err != nil {
		t.Fatal(err)
	}
	state, exists, err := store.LoadRepairBranchRetirements()
	if err != nil || !exists || !state.MigrationCompleted || len(state.Entries) != 1 {
		t.Fatalf("legacy retirement migration=%+v exists=%t err=%v", state, exists, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "missing", http.StatusNotFound)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if err := reconcileRepairRetirements(context.Background(), store, config, lifecycle, client, retirementPolicy(), false); err != nil {
		t.Fatal(err)
	}
	if err := prepareRepairRetirements(store, config, lifecycle); err != nil {
		t.Fatal(err)
	}
	state, _, _ = store.LoadRepairBranchRetirements()
	if len(state.Entries) != 0 {
		t.Fatalf("completed legacy migration re-enqueued terminal finding: %+v", state)
	}
}

func TestUninstallRetiresPendingRepairBranchWhileAutomationPaused(t *testing.T) {
	_, store, lifecycle, config := newRetirementFixture(t, retirementFinding(visualhive.StatusMerged))
	config.Paused = true
	finding, _ := lifecycle.Finding("finding")
	if err := enqueueMergedRepairRetirement(store, config, finding, finding.MergeSHA); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "missing", http.StatusNotFound)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if err := reconcileRepairRetirements(context.Background(), store, config, lifecycle, client, integratedPolicy(config), true); err != nil {
		t.Fatalf("authenticated uninstall could not drain exact retirement while paused: %v", err)
	}
	if pending, err := pendingRepairRetirementCount(store); err != nil || pending != 0 {
		t.Fatalf("paused uninstall left retirement pending=%d err=%v", pending, err)
	}
}

func newRetirementFixture(t *testing.T, finding visualhive.FindingLifecycle) (string, *Store, *visualhive.LifecycleStore, Config) {
	t.Helper()
	stateDir := t.TempDir()
	lifecycleDir := filepath.Join(stateDir, "visual-hive")
	if err := os.MkdirAll(lifecycleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := visualhive.LifecycleState{SchemaVersion: visualhive.LifecycleSchema, UpdatedAt: time.Now().UTC(),
		Findings: map[string]*visualhive.FindingLifecycle{finding.RepositoryFingerprint: &finding}, ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lifecycleDir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(lifecycleDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	return stateDir, store, lifecycle, Config{Repository: "owner/repo", RepositoryID: "123", Automation: AutomationAutoMerge, ACMMLevel: 6, MaxRepairAttempts: 5}
}

func retirementFinding(status visualhive.LifecycleStatus) visualhive.FindingLifecycle {
	return visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: "finding", Status: status, IssueNumber: 10,
		PRNumber: 11, Branch: "hive/repair-proof", RepairCommitSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("c", 40),
		RepairAttempts: 2, OwningAgentHint: "quality", FirstSeenAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
	}
}

func retirementPolicy() automation.Policy {
	return automation.Policy{ACMMLevel: 6, Mode: automation.ModeAutoMerge, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 5}
}

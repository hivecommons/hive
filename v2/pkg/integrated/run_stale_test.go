package integrated

import (
	"context"
	"errors"
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
	"github.com/kubestellar/hive/v2/pkg/beads"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestRequireLiveDefaultWorkflowHeadRejectsMainAdvance(t *testing.T) {
	workflowHead := strings.Repeat("a", 40)
	currentHead := workflowHead
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/owner/repo/branches/main" {
			http.NotFound(writer, request)
			return
		}
		_, _ = io.WriteString(writer, `{"name":"main","commit":{"sha":"`+currentHead+`"}}`)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "", nil, slog.Default())
	config := Config{Repository: "owner/repo", DefaultBranch: "main"}
	if got, err := requireLiveDefaultWorkflowHead(context.Background(), client, config, workflowHead); err != nil || got != workflowHead {
		t.Fatalf("exact current workflow was rejected: head=%s err=%v", got, err)
	}
	currentHead = strings.Repeat("b", 40)
	_, err := requireLiveDefaultWorkflowHead(context.Background(), client, config, workflowHead)
	var stale *staleWorkflowHeadError
	if !errors.As(err, &stale) || stale.WorkflowHead != workflowHead || stale.CurrentHead != currentHead {
		t.Fatalf("advanced main was not classified as stale: stale=%+v err=%v", stale, err)
	}
}

func TestDiscardStaleWorkflowDispatchConsumesOnlyExactIntent(t *testing.T) {
	stateDir := t.TempDir()
	config := Config{SchemaVersion: ConfigSchema, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: stateDir}
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	intent, err := newWorkflowDispatchIntent(config, "hive-visual-hive.yml", "main")
	if err != nil {
		t.Fatal(err)
	}
	intent.DispatchAttemptedAt = time.Now().UTC()
	intent.DispatchAcknowledgedAt = intent.DispatchAttemptedAt
	intent.RunID = 77
	intent.RunURL = "https://github.test/owner/repo/actions/runs/77"
	intent.MatchedAt = intent.DispatchAttemptedAt
	if err := store.SaveWorkflowDispatchIntent(intent); err != nil {
		t.Fatal(err)
	}
	workflow := WorkflowRunEvidence{CorrelationID: intent.CorrelationID, RunID: intent.RunID, HeadSHA: strings.Repeat("a", 40)}
	stale := &staleWorkflowHeadError{WorkflowHead: workflow.HeadSHA, CurrentHead: strings.Repeat("b", 40)}
	if err := discardStaleWorkflowDispatch(stateDir, config, workflow, stale); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.LoadWorkflowDispatchIntent(); err != nil || exists {
		t.Fatalf("stale exact intent remains: exists=%t err=%v", exists, err)
	}
	audit, err := os.ReadFile(filepath.Join(stateDir, "integrated", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"discard_stale_workflow_evidence", workflow.HeadSHA, stale.CurrentHead, `"allowed":false`} {
		if !strings.Contains(string(audit), expected) {
			t.Fatalf("stale discard audit is missing %q: %s", expected, audit)
		}
	}
}

func TestMainAdvanceAfterApplyDefersCloseAndNextBundleRecovers(t *testing.T) {
	stateDir := t.TempDir()
	lifecycle, err := visualhive.NewLifecycleStore(filepath.Join(stateDir, "visual-hive"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore, err := beads.NewStore(filepath.Join(stateDir, "beads"))
	if err != nil {
		t.Fatal(err)
	}
	oldHead := strings.Repeat("a", 40)
	newHead := strings.Repeat("b", 40)
	config := Config{Repository: "owner/repo", DefaultBranch: "main"}
	oldPresent := staleRaceBundle("old-present", oldHead, "present")
	if _, err := lifecycle.ApplyBundle(oldPresent, beadStore, visualhive.ApplyLifecycleOptions{TargetRef: "main", CurrentTargetCommitSHA: oldHead}); err != nil {
		t.Fatal(err)
	}
	fingerprint := oldPresent.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(fingerprint, 17, "https://github.test/owner/repo/issues/17"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range lifecycle.PendingOutbox() {
		if err := lifecycle.MarkOutboxAttempt(entry.ID, nil); err != nil {
			t.Fatal(err)
		}
	}

	// The old-head absence is valid at its first binding and is durably applied.
	// Main advances in the injected window before the outbox is allowed to close.
	oldAbsent := staleRaceBundle("old-absent", oldHead, "absent")
	oldAbsent.Manifest.Observations = nil
	if result, err := lifecycle.ApplyBundle(oldAbsent, beadStore, visualhive.ApplyLifecycleOptions{TargetRef: "main", CurrentTargetCommitSHA: oldHead}); err != nil || result.Resolved != 1 {
		t.Fatalf("old evidence was not applied before fault injection: result=%+v err=%v", result, err)
	}
	currentHead := newHead
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/owner/repo/branches/main" {
			http.NotFound(writer, request)
			return
		}
		_, _ = io.WriteString(writer, `{"name":"main","commit":{"sha":"`+currentHead+`"}}`)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "", nil, slog.Default())
	issues := &staleRaceIssueClient{}
	policy := automation.Policy{ACMMLevel: 4, Mode: automation.ModeIssues, AllowedRepositories: []string{"owner/repo"}}
	_, err = processWorkflowOutboxAtCurrentHead(context.Background(), config, WorkflowRunEvidence{HeadSHA: oldHead}, lifecycle, beadStore, client, issues, policy)
	var stale *staleWorkflowHeadError
	if !errors.As(err, &stale) {
		t.Fatalf("post-apply main advance did not stop the outbox: %v", err)
	}
	if len(issues.states) != 0 {
		t.Fatalf("stale evidence mutated the issue: states=%v", issues.states)
	}
	if finding, _ := lifecycle.Finding(fingerprint); finding.Status != visualhive.StatusResolved || len(lifecycle.PendingOutbox()) != 1 {
		t.Fatalf("fault window was not durably recoverable: finding=%+v pending=%+v", finding, lifecycle.PendingOutbox())
	}

	// The exact new-head bundle sees the finding again. It restores the open
	// lifecycle, supersedes the stale close by bundle digest, and reopens/updates
	// the issue without ever sending a close mutation.
	currentPresent := staleRaceBundle("new-present", newHead, "present")
	if result, err := lifecycle.ApplyBundle(currentPresent, beadStore, visualhive.ApplyLifecycleOptions{TargetRef: "main", CurrentTargetCommitSHA: newHead}); err != nil || result.Reopened != 1 {
		t.Fatalf("current evidence did not repair temporary resolution: result=%+v err=%v", result, err)
	}
	outbox, err := processWorkflowOutboxAtCurrentHead(context.Background(), config, WorkflowRunEvidence{HeadSHA: newHead}, lifecycle, beadStore, client, issues, policy)
	if err != nil {
		t.Fatal(err)
	}
	if outbox.StaleSkipped != 1 || outbox.Succeeded != 1 || outbox.Failed != 0 {
		t.Fatalf("current evidence did not supersede stale close cleanly: %+v", outbox)
	}
	if len(issues.states) != 1 || issues.states[0] != "open" {
		t.Fatalf("recovery sent an unsafe issue mutation: states=%v", issues.states)
	}
	if finding, _ := lifecycle.Finding(fingerprint); finding.Status != visualhive.StatusIssueOpen || len(lifecycle.PendingOutbox()) != 0 {
		t.Fatalf("recovered lifecycle is not open and drained: finding=%+v pending=%+v", finding, lifecycle.PendingOutbox())
	}
}

type staleRaceIssueClient struct {
	states []string
}

func (c *staleRaceIssueClient) ResolveLifecycleIssueWriter(_ context.Context, _, _ string, _ int) (string, int64, error) {
	return "hive-writer", 42, nil
}

func (c *staleRaceIssueClient) UpsertLifecycleIssueOwned(_ context.Context, _, _, _ string, _ int64, _, _ string, _ []string) (int, string, bool, error) {
	return 17, "https://github.test/owner/repo/issues/17", false, nil
}

func (c *staleRaceIssueClient) UpdateLifecycleIssueOwned(_ context.Context, _ string, number int, _, _ string, _ int64, _, _ string, state string, _ []string) (int, string, error) {
	c.states = append(c.states, state)
	return number, "https://github.test/owner/repo/issues/17", nil
}

func (c *staleRaceIssueClient) MigrateLifecycleIssueMarkerOwned(_ context.Context, _ string, number int, _, _, _ string, _ int64, _, _ string, state string, _ []string) (int, string, error) {
	c.states = append(c.states, state)
	return number, "https://github.test/owner/repo/issues/17", nil
}

func staleRaceBundle(id, head, state string) *visualhive.ValidatedBundle {
	now := time.Now().UTC()
	observation := visualhive.Observation{
		Fingerprint: "visual-regression", RepositoryFingerprint: strings.Repeat("f", 64), State: state,
		IssueKind: "visual_regression", Severity: "high", OwningAgentHint: "quality",
		Title: "Visual regression", Body: "The page changed unexpectedly.", Labels: []string{"visual-hive"},
		AffectedContracts: []string{"route:/"}, ValidationCommand: "npm test", ObservedAt: now.Format(time.RFC3339Nano), FirstSeenAt: now.Add(-time.Hour).Format(time.RFC3339Nano),
	}
	return &visualhive.ValidatedBundle{Manifest: visualhive.Manifest{
		BundleID: id, OverallDigest: strings.Repeat(string(id[0]), 64),
		Source:       visualhive.Source{Repository: "owner/repo", RepositoryID: "123", Ref: "refs/heads/main", CommitSHA: head, WorkflowRunID: id},
		Scan:         visualhive.Scan{Scope: "full", AuthoritativeForResolution: true, EvaluatedContracts: []string{"route:/"}},
		Observations: []visualhive.Observation{observation}, ReplayProtection: visualhive.ReplayProtection{Key: id},
	}, Validation: visualhive.Validation{Status: "passed", Trusted: true, Authoritative: true}}
}

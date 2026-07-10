package visualhive

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/automation"
)

type fakeLifecycleIssueClient struct {
	upserts int
	updates int
	state   string
	labels  []string
}

func (client *fakeLifecycleIssueClient) UpsertLifecycleIssue(_ context.Context, _, _, _, _ string, labels []string) (int, string, bool, error) {
	client.upserts++
	client.state = "open"
	client.labels = append([]string(nil), labels...)
	return 17, "https://github.test/owner/repo/issues/17", client.upserts == 1, nil
}

func (client *fakeLifecycleIssueClient) UpdateLifecycleIssue(_ context.Context, _ string, number int, _, _ string, state string, labels []string) (int, string, error) {
	client.updates++
	client.state = state
	client.labels = append([]string(nil), labels...)
	return number, "https://github.test/owner/repo/issues/17", nil
}

func TestProcessOutboxUsesACMMAndClosesOnlyResolvedFinding(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-present", "present", "main", true))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}

	client := &fakeLifecycleIssueClient{}
	denied := ProcessOutbox(context.Background(), lifecycle, beadStore, automation.Policy{
		ACMMLevel: 2, Mode: automation.ModeIssues, AllowedRepositories: []string{"owner/repo"},
	}, client)
	if denied.Denied != 1 || client.upserts != 0 {
		t.Fatalf("ACMM L2 write was not denied: result=%+v calls=%d", denied, client.upserts)
	}
	allowed := ProcessOutbox(context.Background(), lifecycle, beadStore, automation.Policy{
		ACMMLevel: 4, Mode: automation.ModeIssues, AllowedRepositories: []string{"owner/repo"},
	}, client)
	if allowed.Succeeded != 1 || client.upserts != 1 || client.state != "open" || !containsLabel(client.labels, "hive/active") {
		t.Fatalf("issue open failed: result=%+v client=%+v", allowed, client)
	}

	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkRepairStarted(fingerprint, "hive/repair"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPROpen(fingerprint, "repair-sha", 18, "https://github.test/owner/repo/pull/18"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkChecks(fingerprint, "repair-sha", true); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkMerged(fingerprint, present.Manifest.Source.CommitSHA); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPostMergeVerifying(fingerprint, "run-19", "https://github.test/owner/repo/actions/19"); err != nil {
		t.Fatal(err)
	}
	absent := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "absent"), "bundle-absent", "absent", "refs/heads/main", true))
	if _, err := lifecycle.ApplyBundle(absent, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	closed := ProcessOutbox(context.Background(), lifecycle, beadStore, automation.Policy{
		ACMMLevel: 4, Mode: automation.ModeIssues, AllowedRepositories: []string{"owner/repo"},
	}, client)
	if closed.Succeeded != 1 || client.state != "closed" || !containsLabel(client.labels, "hive/resolved") || containsLabel(client.labels, "hive/active") {
		t.Fatalf("resolved issue close failed: result=%+v client=%+v", closed, client)
	}
	finding, _ := lifecycle.Finding(fingerprint)
	if finding.Status != StatusIssueClosed {
		t.Fatalf("finding state was not closed: %+v", finding)
	}
}

func TestProcessOutboxSkipsSupersededClose(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-1", "present", "main", true))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(fingerprint, 17, "https://github.test/owner/repo/issues/17"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range lifecycle.PendingOutbox() {
		_ = lifecycle.MarkOutboxAttempt(entry.ID, nil)
	}
	absent := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "absent"), "bundle-2", "absent", "main", true))
	if _, err := lifecycle.ApplyBundle(absent, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	recurrence := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "recurrence"), "bundle-3", "present", "main", true))
	if _, err := lifecycle.ApplyBundle(recurrence, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	client := &fakeLifecycleIssueClient{}
	result := ProcessOutbox(context.Background(), lifecycle, beadStore, automation.Policy{ACMMLevel: 4, Mode: automation.ModeIssues, AllowedRepositories: []string{"owner/repo"}}, client)
	if result.StaleSkipped != 1 || result.Succeeded != 1 || client.state != "open" {
		t.Fatalf("superseded close was not skipped: result=%+v client=%+v", result, client)
	}
}

func containsLabel(labels []string, expected string) bool {
	for _, label := range labels {
		if label == expected {
			return true
		}
	}
	return false
}

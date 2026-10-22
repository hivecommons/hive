package github

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/fleetreport"
)

// The watcher feeds issue_request_stuck once a request has failed
// issueRequestStuckAttempts times OR has been retrying for
// issueRequestStuckAge, and clears it when the retry state is dropped.
func TestIssueRequestStuckObservedAfterAttemptsAndClearedOnDrop(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	tr := fleetreport.NewRuntimeTracker(clock)
	fleetreport.SetDefaultTrackerForTest(t, tr)
	c := &Client{}
	path := filepath.Join(t.TempDir(), "placeholder-1.json")
	boom := errors.New("repository owner/repo is a placeholder")

	for i := 1; i < issueRequestStuckAttempts; i++ {
		c.issueNoteFailure(path, "issue", clock, boom)
		c.observeIssueRequestStuck(path, "owner/repo", "issue", boom, now)
		now = now.Add(time.Minute)
	}
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("below %d attempts produced evidence: %+v", issueRequestStuckAttempts, ev)
	}
	c.issueNoteFailure(path, "issue", clock, boom)
	c.observeIssueRequestStuck(path, "owner/repo", "issue", boom, now)
	ev := tr.Evidence()
	if len(ev) != 1 || ev[0].Class != fleetreport.ClassIssueRequestStuck || ev[0].Agent != "placeholder-1.json" || ev[0].Repo != "owner/repo" {
		t.Fatalf("evidence = %+v", ev)
	}
	if ev[0].Diagnostics == "" || ev[0].Message == "" {
		t.Fatalf("evidence lacks message/diagnostics: %+v", ev[0])
	}
	c.issueClearRetry(path)
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("cleared request still reported: %+v", ev)
	}
	if cleared := tr.TakeCleared(); len(cleared) != 1 || cleared[0].Key != "placeholder-1.json" {
		t.Fatalf("TakeCleared = %+v", cleared)
	}
}

func TestIssueRequestStuckObservedAfterAge(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	tr := fleetreport.NewRuntimeTracker(clock)
	fleetreport.SetDefaultTrackerForTest(t, tr)
	c := &Client{}
	path := filepath.Join(t.TempDir(), "old.json")
	boom := errors.New("502")
	c.issueNoteFailure(path, "comment", clock, boom)
	c.observeIssueRequestStuck(path, "owner/repo", "comment", boom, now)
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("single fresh failure produced evidence: %+v", ev)
	}
	now = now.Add(issueRequestStuckAge)
	c.issueNoteFailure(path, "comment", clock, boom)
	c.observeIssueRequestStuck(path, "owner/repo", "comment", boom, now)
	if ev := tr.Evidence(); len(ev) != 1 || ev[0].Class != fleetreport.ClassIssueRequestStuck {
		t.Fatalf("aged request not reported: %+v", ev)
	}
}

// No retry state (the request was never noted as failed) is a no-op, and
// clearing an untracked path never panics or records a clear.
func TestIssueRequestStuckNoopWithoutRetryState(t *testing.T) {
	tr := fleetreport.NewRuntimeTracker(nil)
	fleetreport.SetDefaultTrackerForTest(t, tr)
	c := &Client{}
	c.observeIssueRequestStuck("/nowhere/x.json", "owner/repo", "issue", nil, time.Now())
	c.issueClearRetry("/nowhere/x.json")
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("evidence = %+v", ev)
	}
	if cleared := tr.TakeCleared(); len(cleared) != 0 {
		t.Fatalf("cleared = %+v", cleared)
	}
}

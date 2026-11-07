package github

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFailPRRequestReportsMissingHeadOnce(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		wantSpecific bool
		wantGeneric  bool
	}{
		{"missing head", &errMissingHead{repo: "o/r", head: "branch", repoVisible: true}, true, false},
		{"other failure", errors.New("temporary failure"), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			c := NewClientForTest("http://127.0.0.1:0", "o", nil, slog.New(slog.NewTextHandler(&logs, nil)))
			path := filepath.Join(t.TempDir(), "request.json")
			c.failPRRequest(path, PRRequest{Repo: "o/r", Head: "branch"}, tc.err, time.Now)
			got := logs.String()
			if strings.Contains(got, "head branch is not on the remote") != tc.wantSpecific || strings.Contains(got, "request failed, will retry with backoff") != tc.wantGeneric {
				t.Fatalf("unexpected logs: %s", got)
			}
		})
	}
}

// #16969: one unpushed branch retried every tick must be ONE fleet-feed
// observation, not one per retry. The first sighting is ERROR and names the
// ref checked and the push outcome seen; retries stay below the feed's WARN
// floor; the eventual success is logged so the story closes.
func TestFailPRRequestMissingHeadErrorIsOncePerRequest(t *testing.T) {
	pushBranchRequestDirForTest = t.TempDir()
	defer func() { pushBranchRequestDirForTest = "" }()

	var logs bytes.Buffer
	c := NewClientForTest("http://127.0.0.1:0", "o", nil, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	path := filepath.Join(t.TempDir(), "request.json")
	req := PRRequest{Repo: "o/r", Head: "docs/runbook", Agent: "operations"}
	err := &errMissingHead{repo: "o/r", head: "docs/runbook", repoVisible: true}

	now := time.Now()
	for i := 0; i < 3; i++ {
		c.failPRRequest(path, req, err, func() time.Time { return now.Add(time.Duration(i) * time.Minute) })
	}
	got := logs.String()
	if n := strings.Count(got, "level=ERROR msg=\"pr-request watcher: head branch is not on the remote\""); n != 1 {
		t.Fatalf("missing head logged at ERROR %d times across 3 retries, want exactly 1:\n%s", n, got)
	}
	if n := strings.Count(got, "head branch still not on the remote, retrying"); n != 2 {
		t.Fatalf("retries logged %d times, want 2:\n%s", n, got)
	}
	if strings.Contains(got, "did not authenticate") || strings.Contains(got, "agent's push") {
		t.Fatalf("message asserts a cause the watcher did not observe:\n%s", got)
	}
	for _, want := range []string{
		"checked=\"refs/heads/docs/runbook on github.com/o/r\"",
		"push_outcome=\"" + pushRelayOutcomeNone + "\"",
		"attempt=1", "attempt=3", "agent=operations",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %s:\n%s", want, got)
		}
	}
	if c.prRetries.attempts(path) != 3 {
		t.Fatalf("attempts = %d, want 3", c.prRetries.attempts(path))
	}
}

func TestPushRelayOutcomeReadsTheRelayDropBox(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := pushRelayOutcome(dir, "docs/x"); got != pushRelayOutcomeNone {
		t.Fatalf("empty drop box: %q", got)
	}
	write("operations-1.result.json", `{"ok":false,"branch":"docs/x","attempts":2,"error":"git push failed: remote: Permission denied","at":"2026-11-07T05:00:00Z"}`)
	if got := pushRelayOutcome(dir, "docs/x"); !strings.Contains(got, "FAILED after 2 attempt(s)") || !strings.Contains(got, "Permission denied") {
		t.Fatalf("failed relay push not reported: %q", got)
	}
	// A newer success for the same branch wins over the older failure.
	write("operations-2.result.json", `{"ok":true,"branch":"docs/x","sha":"abc123","attempts":1,"at":"2026-11-07T05:05:00Z"}`)
	if got := pushRelayOutcome(dir, "docs/x"); !strings.Contains(got, "pushed docs/x as abc123") {
		t.Fatalf("newest relay result not preferred: %q", got)
	}
	// Another branch's result never speaks for this one.
	if got := pushRelayOutcome(dir, "docs/other"); got != pushRelayOutcomeNone {
		t.Fatalf("unrelated branch matched: %q", got)
	}
	// A request still queued is reported as such, ahead of any old result.
	write("operations-3.json", `{"repo":"o/r","branch":"docs/x","dir":"/data/agents/operations/r"}`)
	if got := pushRelayOutcome(dir, "refs/heads/docs/x"); !strings.Contains(got, "still queued") {
		t.Fatalf("queued relay request not reported: %q", got)
	}
}

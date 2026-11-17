package dashboard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestHiveStatusBrief pins hivecommons/hive#18590 finding 9: the meeting
// attendee answers hive-state questions from the dashboard's in-memory
// status, the same counts the Overview shows.
func TestHiveStatusBrief(t *testing.T) {
	s := &Server{}
	if _, err := s.HiveStatusBrief(context.Background()); !errors.Is(err, errHiveStatusUnavailable) {
		t.Fatalf("no snapshot: err = %v", err)
	}
	st := chatEvalStatus(time.Now())
	st.OpenItems.Buckets.Actionable = FrontendOpenItemKinds{Issues: 7, PRs: 3}
	st.OpenItems.Buckets.Held = FrontendOpenItemKinds{Issues: 4, PRs: 1}
	st.OpenItems.Buckets.Blocked = FrontendOpenItemKinds{Issues: 2}
	st.Agents = append(st.Agents, FrontendAgent{Name: "coder", State: "running", Busy: "working", Doing: "fixing #13990"})
	s.status = st
	got, err := s.HiveStatusBrief(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Open now: 40 issues and 12 PRs.",
		"Actionable: 7 issues, 3 PRs. Held: 4 issues, 1 PRs. Blocked: 2 issues, 0 PRs.",
		"Agents: 17, 1 paused, 1 blocked.",
		"Working on: coder (fixing #13990).",
		"Blocked: reviewer-2.",
		"Recently finished: scanner: opened PR hivecommons/hive#14001",
		"Open PRs: ",
		"14002 fix: other (CI failure)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief lacks %q:\n%s", want, got)
		}
	}
	if len(got) > hiveStatusMaxChars {
		t.Fatalf("brief is %d bytes", len(got))
	}

	st.StaleSince = "2026-11-16T10:00:00Z"
	st.OpenItems.IssuesSyncing = true
	got, _ = s.HiveStatusBrief(context.Background())
	if !strings.Contains(got, "at least this many") || !strings.Contains(got, "last-good figures from 2026-11-16T10:00:00Z") {
		t.Fatalf("stale and syncing are said: %s", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.HiveStatusBrief(ctx); err == nil {
		t.Fatal("a cancelled context returns its error")
	}
}

func TestHiveStatusListCaps(t *testing.T) {
	var b strings.Builder
	hiveStatusList(&b, "L", []string{"1", "2", "3", "4", "5", "6", "7"})
	if got := b.String(); got != "L: 1; 2; 3; 4; 5 (and 2 more)." {
		t.Fatalf("got %q", got)
	}
	b.Reset()
	hiveStatusList(&b, "L", nil)
	if b.Len() != 0 {
		t.Fatal("an empty list writes nothing")
	}
}

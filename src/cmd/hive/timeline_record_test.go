package main

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/timeline"
)

// fakeLifecycleRecorder is a minimal lifecycleRecorder backed by a real
// timeline.Store, so the recording helpers can be exercised without standing up
// a full dashboard.Server.
type fakeLifecycleRecorder struct {
	store *timeline.Store
}

func (f *fakeLifecycleRecorder) LifecycleTimeline() *timeline.Store { return f.store }

func TestRecordEnumeratedIssues(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{
			Items: []github.Issue{
				{Repo: "org/repo", Number: 1},
				{Repo: "org/repo", Number: 2},
			},
		},
	}

	recordEnumeratedIssues(context.Background(), rec, actionable)

	journeys := rec.store.Journeys(0)
	if len(journeys) != 2 {
		t.Fatalf("recorded %d journeys, want 2", len(journeys))
	}
	for _, j := range journeys {
		if j.Current != timeline.KindEnumerated {
			t.Fatalf("journey %s current = %q, want %q", j.Ref, j.Current, timeline.KindEnumerated)
		}
	}

	// Re-enumeration (every eval cycle) refreshes, never floods (#5656).
	recordEnumeratedIssues(context.Background(), rec, actionable)
	if got := len(rec.store.Journeys(0)); got != 2 {
		t.Fatalf("re-enumeration made %d journeys, want still 2", got)
	}
	j, _ := rec.store.Journey("org/repo#1")
	if st := j.Stages[timeline.KindEnumerated]; st == nil || st.Count != 2 {
		t.Fatalf("re-enumeration should bump Count to 2, got %+v", st)
	}
}

func TestRecordEnumeratedIssues_BoundedPerCycle(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	n := maxTimelineEnumeratePerCycle + 25
	items := make([]github.Issue, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, github.Issue{Repo: "org/repo", Number: i + 1})
	}
	actionable := &github.ActionableResult{Issues: github.IssueResult{Items: items}}

	recordEnumeratedIssues(context.Background(), rec, actionable)

	if got := len(rec.store.Journeys(0)); got != maxTimelineEnumeratePerCycle {
		t.Fatalf("recorded %d journeys, want cap of %d", got, maxTimelineEnumeratePerCycle)
	}
}

// TestRecordKickAgentScopedHasNoJourney pins the journey-store contract: a
// kick with no issue refs has no work-item identity, so nothing lands (the
// tracing span still fires; only the journey view drops it).
func TestRecordKickAgentScopedHasNoJourney(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}

	recordKick(context.Background(), rec, "quality")

	if got := len(rec.store.Journeys(0)); got != 0 {
		t.Fatalf("agent-scoped kick made %d journeys, want 0", got)
	}
}

func TestRecordKickIssueScoped(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}

	recordKick(context.Background(), rec, "scanner", "org/repo#1", "org/repo#2")

	journeys := rec.store.Journeys(0)
	if len(journeys) != 2 {
		t.Fatalf("recorded %d journeys, want 2", len(journeys))
	}
	for _, j := range journeys {
		if j.Current != timeline.KindKicked || j.Agent != "scanner" {
			t.Fatalf("bad kick journey: %+v", j)
		}
	}
	// Repeat kicks accumulate Count on the same journey (retro reads it).
	recordKick(context.Background(), rec, "scanner", "org/repo#1")
	j, _ := rec.store.Journey("org/repo#1")
	if st := j.Stages[timeline.KindKicked]; st == nil || st.Count != 2 {
		t.Fatalf("kick count = %+v, want Count 2", st)
	}
}

// TestRecordHelpers_NilSafe confirms the guards: nil recorder, nil actionable,
// and a recorder returning a nil store must all be no-ops (never panic).
func TestRecordHelpers_NilSafe(t *testing.T) {
	// nil recorder
	recordEnumeratedIssues(context.Background(), nil, &github.ActionableResult{})
	recordKick(context.Background(), nil, "x")
	recordPROpened(nil, "org", "a", "repo", 1, "u")
	recordBlocked(context.Background(), nil, "org", "repo", 1, 3, nil)
	recordLifecycleFromAudit(nil, "org", "pr_merged", "repo=r, number=1", "")

	// nil actionable
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	recordEnumeratedIssues(context.Background(), rec, nil)
	if got := len(rec.store.Journeys(0)); got != 0 {
		t.Fatalf("nil actionable recorded %d journeys, want 0", got)
	}

	// recorder that returns a nil store
	nilStore := &fakeLifecycleRecorder{store: nil}
	recordEnumeratedIssues(context.Background(), nilStore, &github.ActionableResult{
		Issues: github.IssueResult{Items: []github.Issue{{Repo: "r", Number: 1}}},
	})
	recordKick(context.Background(), nilStore, "x")
	recordPROpened(nilStore, "org", "a", "repo", 1, "u")
	recordBlocked(context.Background(), nilStore, "org", "repo", 1, 3, nil)
	recordLifecycleFromAudit(nilStore, "org", "pr_merged", "repo=r, number=1", "")
}

func TestIssueRef(t *testing.T) {
	if got := issueRef("org/repo", 42); got != "org/repo#42" {
		t.Fatalf("issueRef = %q, want org/repo#42", got)
	}
	if got := issueRef("", 42); got != "" {
		t.Fatalf("issueRef(empty repo) = %q, want empty", got)
	}
}

// TestTimelineRef pins the identity rule: full "org/repo" sources normalize to
// the short "repo#number" key the enumeration producer uses, so all stages of
// one item share ONE journey.
func TestTimelineRef(t *testing.T) {
	cases := []struct {
		org, repo string
		number    int
		want      string
	}{
		{"hivecommons", "hivecommons/hive", 7, "hive#7"},
		{"hivecommons", "hive", 7, "hive#7"},
		{"hivecommons", "otherorg/tool", 7, "otherorg/tool#7"},
		{"", "hive", 7, "hive#7"},
		{"hivecommons", "", 7, ""},
	}
	for _, c := range cases {
		if got := timelineRef(c.org, c.repo, c.number); got != c.want {
			t.Fatalf("timelineRef(%q,%q,%d) = %q, want %q", c.org, c.repo, c.number, got, c.want)
		}
	}
}

func TestParseAuditDetail(t *testing.T) {
	kv := parseAuditDetail("repo=acme/widgets, number=7, method=squash, sha=abc123, agent=quality")
	if kv["repo"] != "acme/widgets" || kv["number"] != "7" || kv["method"] != "squash" {
		t.Fatalf("parsed = %v", kv)
	}
	// Defensive: junk segments are skipped, not fatal.
	kv = parseAuditDetail("no-equals-here, repo=r")
	if kv["repo"] != "r" || len(kv) != 1 {
		t.Fatalf("parsed = %v, want only repo", kv)
	}
}

// TestRecordLifecycleFromAudit_PRMerged is the merged-stage wire: every
// pr_merged attribution audit entry (automerge sweep, MergePR from the
// dashboard queue / merge watcher) must land as a merged journey stage.
func TestRecordLifecycleFromAudit_PRMerged(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}

	recordLifecycleFromAudit(rec, "acme",
		github.AuditActionPRMerged,
		"repo=acme/widgets, number=41, method=squash, sha=abc123",
		"governor")

	j, ok := rec.store.Journey("widgets#41")
	if !ok {
		t.Fatalf("merged journey missing; journeys = %+v", rec.store.Journeys(0))
	}
	if j.Current != timeline.KindMerged {
		t.Fatalf("current = %s, want merged", j.Current)
	}
	st := j.Stages[timeline.KindMerged]
	if st == nil || st.Attrs["sha"] != "abc123" || st.Attrs["method"] != "squash" {
		t.Fatalf("merged stage lost evidence: %+v", st)
	}
}

// TestRecordLifecycleFromAudit_AgentPRCreated is the pr_opened audit wire.
func TestRecordLifecycleFromAudit_AgentPRCreated(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}

	recordLifecycleFromAudit(rec, "acme",
		github.AuditActionAgentPRCreated,
		"repo=widgets, number=99, author=hive-agent, url=https://github.com/acme/widgets/pull/99, reused=false",
		"quality")

	j, ok := rec.store.Journey("widgets#99")
	if !ok || j.Current != timeline.KindPROpened {
		t.Fatalf("pr_opened journey = %+v ok=%v", j, ok)
	}
	if j.Agent != "quality" {
		t.Fatalf("agent = %q, want quality", j.Agent)
	}
}

// TestRecordLifecycleFromAudit_IgnoresOtherActionsAndBadDetails: only the two
// lifecycle-bearing actions map, and unattributable details are skipped.
func TestRecordLifecycleFromAudit_IgnoresOtherActionsAndBadDetails(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	recordLifecycleFromAudit(rec, "acme", "pr_reviewed", "repo=widgets, number=5", "")
	recordLifecycleFromAudit(rec, "acme", github.AuditActionPRMerged, "no usable pairs", "")
	recordLifecycleFromAudit(rec, "acme", github.AuditActionPRMerged, "repo=widgets, number=zero", "")
	if got := len(rec.store.Journeys(0)); got != 0 {
		t.Fatalf("journeys = %d, want 0", got)
	}
}

// TestRecordPROpened is the typed PR-opened hook wire.
func TestRecordPROpened(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}

	recordPROpened(rec, "acme", "scanner", "acme/widgets", 12, "https://github.com/acme/widgets/pull/12")

	j, ok := rec.store.Journey("widgets#12")
	if !ok || j.Current != timeline.KindPROpened || j.Agent != "scanner" {
		t.Fatalf("journey = %+v ok=%v", j, ok)
	}
	// The audit-sink bridge reporting the same creation dedupes into the
	// same stage instead of a second row.
	recordLifecycleFromAudit(rec, "acme",
		github.AuditActionAgentPRCreated, "repo=acme/widgets, number=12", "scanner")
	if got := len(rec.store.Journeys(0)); got != 1 {
		t.Fatalf("double wire made %d journeys, want 1", got)
	}
}

// TestRecordBlocked is the escalation wire's recorder.
func TestRecordBlocked(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}

	recordBlocked(context.Background(), rec, "acme", "acme/widgets", 7, 3, []string{"test", "lint"})

	j, ok := rec.store.Journey("widgets#7")
	if !ok || j.Current != timeline.KindBlocked {
		t.Fatalf("journey = %+v ok=%v", j, ok)
	}
	st := j.Stages[timeline.KindBlocked]
	if st == nil || st.Attrs["fix_attempts"] != "3" || st.Attrs["failing_checks"] != "test,lint" {
		t.Fatalf("blocked stage lost evidence: %+v", st)
	}
}

// TestRunEscalationSweepRecordsBlockedJourney wires the whole path: a PR
// crossing the escalation threshold must produce a KindBlocked lifecycle
// stage, not just the comment/label (#5656 — "blocked" had no real producer).
func TestRunEscalationSweepRecordsBlockedJourney(t *testing.T) {
	newTestEscalationStore(t)
	cfg := escalationTestConfig()
	cfg.Escalation.Threshold = 2
	w := &fakeIssueWriter{}
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	ctx := context.Background()

	runEscalationSweep(ctx, cfg, w,
		actionableWith(redPR("widgets", 7, "hive-agent", "sha-1")), nil, rec, discardLogger())
	if got := len(rec.store.Journeys(0)); got != 0 {
		t.Fatalf("below threshold recorded %d journeys, want 0", got)
	}

	runEscalationSweep(ctx, cfg, w,
		actionableWith(redPR("widgets", 7, "hive-agent", "sha-2")), nil, rec, discardLogger())
	j, ok := rec.store.Journey("widgets#7")
	if !ok || j.Current != timeline.KindBlocked {
		t.Fatalf("escalation did not record blocked journey: %+v ok=%v", j, ok)
	}
	if st := j.Stages[timeline.KindBlocked]; st == nil || st.Attrs["fix_attempts"] != "2" {
		t.Fatalf("blocked stage evidence = %+v", j.Stages[timeline.KindBlocked])
	}
}

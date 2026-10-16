package main

import (
	"context"
	"testing"
	"time"

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
	recordLifecycleFromAudit(nil, "org", github.AuditRecord{Action: "pr_merged", Detail: "repo=r, number=1"})

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
	recordLifecycleFromAudit(nilStore, "org", github.AuditRecord{Action: "pr_merged", Detail: "repo=r, number=1"})
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

	recordLifecycleFromAudit(rec, "acme", github.AuditRecord{
		Action: github.AuditActionPRMerged,
		Detail: "repo=acme/widgets, number=41, method=squash, sha=abc123",
		Agent:  "governor",
	})

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

	recordLifecycleFromAudit(rec, "acme", github.AuditRecord{
		Action: github.AuditActionAgentPRCreated,
		Detail: "repo=widgets, number=99, author=hive-agent, url=https://github.com/acme/widgets/pull/99, reused=false",
		Agent:  "quality",
	})

	j, ok := rec.store.Journey("widgets#99")
	if !ok || j.Current != timeline.KindPROpened {
		t.Fatalf("pr_opened journey = %+v ok=%v", j, ok)
	}
	if j.Agent != "quality" {
		t.Fatalf("agent = %q, want quality", j.Agent)
	}
}

// TestRecordLifecycleFromAudit_PrefersTypedFields (#9587): the typed
// Repo/Target audit fields name the work item, so a record whose detail has no
// repo=/number= pairs still journeys, and typed fields win over stale pairs.
// Evidence attrs are still read from detail.
func TestRecordLifecycleFromAudit_PrefersTypedFields(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}

	recordLifecycleFromAudit(rec, "acme", github.AuditRecord{
		Action: github.AuditActionPRMerged,
		Detail: "method=squash, sha=def456",
		Agent:  "governor",
		Repo:   "acme/widgets",
		Target: 43,
	})
	j, ok := rec.store.Journey("widgets#43")
	if !ok || j.Current != timeline.KindMerged {
		t.Fatalf("typed-only merged journey = %+v ok=%v", j, ok)
	}
	if st := j.Stages[timeline.KindMerged]; st == nil || st.Attrs["sha"] != "def456" {
		t.Fatalf("merged stage lost detail evidence: %+v", st)
	}

	recordLifecycleFromAudit(rec, "acme", github.AuditRecord{
		Action: github.AuditActionAgentPRCreated,
		Detail: "repo=gadgets, number=1",
		Agent:  "quality",
		Repo:   "widgets",
		Target: 44,
	})
	if _, ok := rec.store.Journey("widgets#44"); !ok {
		t.Fatalf("typed target not used; journeys = %+v", rec.store.Journeys(0))
	}
	if _, ok := rec.store.Journey("gadgets#1"); ok {
		t.Fatal("detail pairs overrode the typed target")
	}
}

// TestRecordLifecycleFromAudit_IgnoresOtherActionsAndBadDetails: only the two
// lifecycle-bearing actions map, and unattributable details are skipped.
func TestRecordLifecycleFromAudit_IgnoresOtherActionsAndBadDetails(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	recordLifecycleFromAudit(rec, "acme", github.AuditRecord{Action: "pr_reviewed", Detail: "repo=widgets, number=5", Repo: "widgets", Target: 5})
	recordLifecycleFromAudit(rec, "acme", github.AuditRecord{Action: github.AuditActionPRMerged, Detail: "no usable pairs"})
	recordLifecycleFromAudit(rec, "acme", github.AuditRecord{Action: github.AuditActionPRMerged, Detail: "repo=widgets, number=zero"})
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
	recordLifecycleFromAudit(rec, "acme", github.AuditRecord{
		Action: github.AuditActionAgentPRCreated,
		Detail: "repo=acme/widgets, number=12",
		Agent:  "scanner",
		Repo:   "acme/widgets",
		Target: 12,
	})
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

// TestRecordStarted is the agent-start hook's recorder: the hive/in-progress
// claim becomes the "started" stage so kicked→started→pr_opened spans exist.
func TestRecordStarted(t *testing.T) {
	recordStarted(nil, "acme", "scanner", "acme/widgets", 3, "claim")
	recordStarted(&fakeLifecycleRecorder{store: nil}, "acme", "scanner", "acme/widgets", 3, "claim")

	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	recordStarted(rec, "acme", "scanner", "acme/widgets", 3, "claim")
	j, ok := rec.store.Journey("widgets#3")
	if !ok || j.Current != timeline.KindStarted || j.Agent != "scanner" {
		t.Fatalf("journey = %+v ok=%v", j, ok)
	}
	recordStarted(rec, "acme", "scanner", "", 3, "claim")
	if got := len(rec.store.Journeys(0)); got != 1 {
		t.Fatalf("empty repo recorded; journeys = %d", got)
	}
}

// A comment/label on a PR's own number must not record started on the PR journey.
func TestRecordStarted_SkipsPRJourney(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	recordPROpened(rec, "acme", "scanner", "acme/widgets", 41, "https://x/41")
	recordStarted(rec, "acme", "scanner", "acme/widgets", 41, "comment")
	j, ok := rec.store.Journey("widgets#41")
	if !ok || j.Stages[timeline.KindStarted] != nil {
		t.Fatalf("PR journey recorded started: %+v ok=%v", j, ok)
	}
}

// Delivery SLA wiring: the issue journeys a PR fixes get pr_opened (with the
// PR's ref) and later merged, so start→PR and PR→merge spans close per issue.
func TestRecordPROpenedForIssues_ThenMergeFoldsOntoIssues(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	recordStarted(rec, "acme", "bee", "acme/widgets", 7, "claim")
	recordPROpenedForIssues(rec, "acme", "bee", "acme/widgets", 41, "https://x/41", []int{7, 8, 0, 41})

	for _, ref := range []string{"widgets#7", "widgets#8"} {
		j, ok := rec.store.Journey(ref)
		if !ok {
			t.Fatalf("%s missing", ref)
		}
		st := j.Stages[timeline.KindPROpened]
		if st == nil || st.Attrs[timeline.AttrPRRef] != "widgets#41" || st.Agent != "bee" {
			t.Fatalf("%s pr_opened = %+v", ref, st)
		}
	}
	if _, ok := rec.store.Journey("widgets#41"); ok {
		t.Fatal("the PR's own number must be skipped (recordPROpened owns it)")
	}
	if _, ok := rec.store.Journey("widgets#0"); ok {
		t.Fatal("zero issue numbers must be skipped")
	}

	recordLifecycleFromAudit(rec, "acme", github.AuditRecord{
		Action: github.AuditActionPRMerged, Repo: "acme/widgets", Target: 41, Agent: "governor",
		Detail: "repo=acme/widgets, number=41, method=squash, sha=abc123",
	})
	for _, ref := range []string{"widgets#41", "widgets#7", "widgets#8"} {
		j, _ := rec.store.Journey(ref)
		st := j.Stages[timeline.KindMerged]
		if st == nil || st.Attrs["sha"] != "abc123" {
			t.Fatalf("%s merged = %+v", ref, st)
		}
	}
	samples := timeline.DeriveSLASamples(rec.store.Journeys(0))
	spans := map[string]int{}
	for _, s := range samples {
		spans[s.Ref+":"+string(s.Span)]++
	}
	for _, want := range []string{"widgets#7:start_to_pr", "widgets#7:pr_to_merge", "widgets#8:pr_to_merge"} {
		if spans[want] != 1 {
			t.Errorf("missing sample %s in %v", want, spans)
		}
	}
	if spans["widgets#41:pr_to_merge"] != 0 {
		t.Error("PR-keyed journey must not produce issue SLA samples")
	}
}

func TestRecordObservedMerge_UsesObservedInstant(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	recordPROpenedForIssues(rec, "acme", "bee", "acme/widgets", 41, "", []int{7})
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	recordObservedMerge(rec, "acme", "acme/widgets", 41, at, "alice", "human")

	for _, ref := range []string{"widgets#41", "widgets#7"} {
		j, ok := rec.store.Journey(ref)
		if !ok {
			t.Fatalf("%s missing", ref)
		}
		st := j.Stages[timeline.KindMerged]
		if st == nil || st.FirstAt != at.UnixMilli() || st.Attrs["actor"] != "alice" || st.Attrs["source"] != "observed" {
			t.Fatalf("%s merged = %+v", ref, st)
		}
	}
	recordObservedMerge(nil, "acme", "acme/widgets", 41, at, "", "")
	recordPROpenedForIssues(&fakeLifecycleRecorder{}, "acme", "bee", "acme/widgets", 41, "", []int{7})
}

func TestRecordEnumeratedIssues_RetainsCreatedAt(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	created := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	recordEnumeratedIssues(context.Background(), rec, &github.ActionableResult{
		Issues: github.IssueResult{Items: []github.Issue{{Repo: "widgets", Number: 1, CreatedAt: created}, {Repo: "widgets", Number: 2}}},
	})
	j, _ := rec.store.Journey("widgets#1")
	if got := j.Stages[timeline.KindEnumerated].Attrs[timeline.AttrIssueCreatedAt]; got != "2026-10-01T09:30:00Z" {
		t.Fatalf("created_at = %q", got)
	}
	j2, _ := rec.store.Journey("widgets#2")
	if j2.Stages[timeline.KindEnumerated].Attrs[timeline.AttrIssueCreatedAt] != "" {
		t.Fatal("zero CreatedAt must not be recorded")
	}
}

// #12007: a merged PR is folded onto the issue journeys it fixes with the
// PR's real created_at / merged_at, re-running is a no-op, and the issue
// stops being reported as stale in-progress.
func TestBackfillMergedPROnIssues_TimestampsIdempotencyAndStale(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	now := time.Now().UTC().Truncate(time.Millisecond)
	started := now.Add(-20 * time.Hour)
	created := now.Add(-19 * time.Hour)
	merged := now.Add(-18 * time.Hour)
	rec.store.Record(timeline.Event{IssueRef: "widgets#7", Kind: timeline.KindStarted, Agent: "bee", At: started.UnixMilli()})

	ttl := 4 * time.Hour
	if stale := timeline.DeriveSLAStale(rec.store.Journeys(0), ttl, now); len(stale) != 1 || stale[0].Number != 7 {
		t.Fatalf("precondition: stale = %+v, want widgets#7", stale)
	}

	obs := github.PRTerminalObservation{
		Repo: "acme/widgets", Number: 41, State: github.PRStateMerged,
		ObservedAt: merged, CreatedAt: created, URL: "https://x/41", Actor: "alice",
		Issues: []int{7, 9, 41, 0},
	}
	if got := backfillMergedPROnIssues(rec, "acme", obs); got != 1 {
		t.Fatalf("updated = %d, want 1 (only the retained widgets#7)", got)
	}
	j, _ := rec.store.Journey("widgets#7")
	op, mg := j.Stages[timeline.KindPROpened], j.Stages[timeline.KindMerged]
	if op == nil || op.FirstAt != created.UnixMilli() || op.Attrs[timeline.AttrPRRef] != "widgets#41" {
		t.Fatalf("pr_opened = %+v, want PR created_at and pr_ref widgets#41", op)
	}
	if mg == nil || mg.FirstAt != merged.UnixMilli() || mg.Attrs["source"] != "backfill" || mg.Attrs[timeline.AttrPRRef] != "widgets#41" {
		t.Fatalf("merged = %+v, want PR merged_at", mg)
	}
	if _, ok := rec.store.Journey("widgets#9"); ok {
		t.Fatal("backfill must not create journeys for issues the store never tracked")
	}
	if _, ok := rec.store.Journey("widgets#41"); ok {
		t.Fatal("the PR's own number must be skipped")
	}

	if got := backfillMergedPROnIssues(rec, "acme", obs); got != 0 {
		t.Fatalf("re-run updated = %d, want 0", got)
	}
	j2, _ := rec.store.Journey("widgets#7")
	if j2.Stages[timeline.KindPROpened].Count != 1 || j2.Stages[timeline.KindMerged].Count != 1 {
		t.Fatalf("re-run touched stages: %+v", j2.Stages)
	}

	if stale := timeline.DeriveSLAStale(rec.store.Journeys(0), ttl, now); len(stale) != 0 {
		t.Fatalf("stale after backfill = %+v, want none", stale)
	}
	samples := map[string]float64{}
	for _, s := range timeline.DeriveSLASamples(rec.store.Journeys(0)) {
		samples[s.Ref+":"+string(s.Span)] = s.Hours
	}
	if h, ok := samples["widgets#7:start_to_pr"]; !ok || h != 1 {
		t.Errorf("start_to_pr = %v (present %v), want 1h from the PR's created_at; samples %v", h, ok, samples)
	}
}

// An existing pr_opened (possibly naming another PR) is kept; a PR with no
// created_at still records merged so the issue leaves stale in-progress.
func TestBackfillMergedPROnIssues_KeepsExistingPROpenedAndGuards(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	recordStarted(rec, "acme", "bee", "acme/widgets", 7, "claim")
	recordPROpenedForIssues(rec, "acme", "bee", "acme/widgets", 40, "", []int{7})
	merged := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)

	obs := github.PRTerminalObservation{Repo: "acme/widgets", Number: 41, State: github.PRStateMerged, ObservedAt: merged, Issues: []int{7}}
	if got := backfillMergedPROnIssues(rec, "acme", obs); got != 1 {
		t.Fatalf("updated = %d, want 1", got)
	}
	j, _ := rec.store.Journey("widgets#7")
	if st := j.Stages[timeline.KindPROpened]; st.Attrs[timeline.AttrPRRef] != "widgets#40" || st.Count != 1 {
		t.Fatalf("existing pr_opened rewritten: %+v", st)
	}
	if st := j.Stages[timeline.KindMerged]; st == nil || st.FirstAt != merged.UnixMilli() {
		t.Fatalf("merged = %+v", st)
	}

	for _, bad := range []github.PRTerminalObservation{
		{Repo: "acme/widgets", Number: 42, State: github.PRStateClosed, ObservedAt: merged, Issues: []int{7}},
		{Repo: "acme/widgets", Number: 42, State: github.PRStateMerged, Issues: []int{7}},
		{Repo: "acme/widgets", Number: 42, State: github.PRStateMerged, ObservedAt: merged},
	} {
		if got := backfillMergedPROnIssues(rec, "acme", bad); got != 0 {
			t.Errorf("backfill(%+v) = %d, want 0", bad, got)
		}
	}
	if got := backfillMergedPROnIssues(nil, "acme", obs); got != 0 {
		t.Error("nil recorder must be a no-op")
	}
	if got := backfillMergedPROnIssues(&fakeLifecycleRecorder{}, "acme", obs); got != 0 {
		t.Error("nil store must be a no-op")
	}
}

type fakeMergedPRLister struct {
	since time.Time
	prs   []github.PRTerminalObservation
	err   error
}

func (f *fakeMergedPRLister) MergedPRsSince(_ context.Context, since time.Time) ([]github.PRTerminalObservation, error) {
	f.since = since
	return f.prs, f.err
}

func TestBackfillRecentMergedPRs_LooksBackSevenDaysAndAppliesPartialResults(t *testing.T) {
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	recordStarted(rec, "acme", "bee", "acme/widgets", 7, "claim")
	now := time.Now().UTC()
	lister := &fakeMergedPRLister{
		prs: []github.PRTerminalObservation{{
			Repo: "acme/widgets", Number: 41, State: github.PRStateMerged,
			ObservedAt: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour), Issues: []int{7},
		}},
		err: context.DeadlineExceeded,
	}
	if got := backfillRecentMergedPRs(context.Background(), lister, rec, "acme", now, nil); got != 1 {
		t.Fatalf("updated = %d, want 1", got)
	}
	if want := now.Add(-github.MergedPRBackfillWindow); !lister.since.Equal(want) {
		t.Errorf("since = %v, want %v", lister.since, want)
	}
	if got := backfillRecentMergedPRs(context.Background(), lister, rec, "acme", now, nil); got != 0 {
		t.Fatalf("second run updated = %d, want 0", got)
	}
	if got := backfillRecentMergedPRs(context.Background(), nil, rec, "acme", now, nil); got != 0 {
		t.Error("nil lister must be a no-op")
	}
}

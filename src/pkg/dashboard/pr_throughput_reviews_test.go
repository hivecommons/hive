package dashboard

import (
	"path/filepath"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// ── #16116: other people's reviews in the Reviewed row ──────────────────────

// reviewEntries are the audit entries one merged PR's settle scan produces:
// two hive reviews (agent_pr_reviewed) plus the reviews the hive did not
// write, observed from GitHub at their own submitted_at.
func reviewEntries() []AuditEntry {
	return []AuditEntry{
		{Timestamp: "2026-10-31T09:10:00Z", User: "system", Action: ghpkg.AuditActionPRReviewed, Detail: "repo=o/r, number=5, state=approved", Agent: "reviewer", Repo: "o/r", Target: 5},
		{Timestamp: "2026-10-31T09:20:00Z", User: "system", Action: ghpkg.AuditActionPRReviewed, Detail: "repo=o/r, number=6, state=commented", Agent: "reviewer", Repo: "o/r", Target: 6},
		{Timestamp: "2026-10-31T09:30:00Z", User: "system", Action: ghpkg.AuditActionPRReviewObserved, Detail: "repo=o/r, number=5, review_id=101, actor=alice, actor_class=human", Agent: "governor", Repo: "o/r", Target: 5},
		{Timestamp: "2026-10-31T10:05:00Z", User: "system", Action: ghpkg.AuditActionPRReviewObserved, Detail: "repo=o/r, number=5, review_id=102, actor=bob, actor_class=human", Agent: "governor", Repo: "o/r", Target: 5},
		{Timestamp: "2026-10-31T10:15:00Z", User: "system", Action: ghpkg.AuditActionPRReviewObserved, Detail: "repo=o/r, number=6, review_id=103, actor=renovate[bot], actor_class=other_automation", Agent: "governor", Repo: "o/r", Target: 6},
	}
}

// observedReviewEntries is the part of reviewEntries a later settle scan
// re-observes: the three reviews the hive did not write.
func observedReviewEntries() []AuditEntry {
	return reviewEntries()[2:]
}

func logEntries(a *AuditLog, entries []AuditEntry) {
	for _, e := range entries {
		a.LogRecordAt(e.Timestamp, e.User, e.Action, e.Detail, e.Agent, e.Repo, e.Target)
	}
}

func assertReviewedActors(t *testing.T, label string, m PRThroughputActorMatrix, hive, human, other int) {
	t.Helper()
	got := m[prThroughputKindPR][prThroughputRoleReviewed]
	if got[prThroughputActorHive] != hive || got[prThroughputActorHuman] != human || got[prThroughputActorOtherAutomation] != other {
		t.Errorf("%s: by_actor.pr.reviewed = %v, want hive=%d human=%d other=%d", label, got, hive, human, other)
	}
}

func TestPRThroughputAttributesObservedReview(t *testing.T) {
	for _, tc := range []struct {
		detail string
		actor  string
	}{
		{"repo=o/r, number=5, review_id=1, actor=alice, actor_class=human", prThroughputActorHuman},
		{"repo=o/r, number=5, review_id=2, actor=renovate[bot], actor_class=other_automation", prThroughputActorOtherAutomation},
		{"repo=o/r, number=5, review_id=3", prThroughputActorUnknown},
	} {
		kind, role, actor, ok := prThroughputActorAttribution(AuditEntry{Action: ghpkg.AuditActionPRReviewObserved, Detail: tc.detail})
		if !ok || kind != prThroughputKindPR || role != prThroughputRoleReviewed || actor != tc.actor {
			t.Errorf("attribution(%q) = %s/%s/%s/%v, want pr/reviewed/%s", tc.detail, kind, role, actor, ok, tc.actor)
		}
	}
}

func TestPRThroughputCountsObservedReviewsByActor(t *testing.T) {
	a := &AuditLog{}
	logEntries(a, reviewEntries())

	got := a.PRThroughputCounters()
	assertReviewedActors(t, "all repos", got.ByActor, 2, 2, 1)
	assertReviewedActors(t, "repo o/r", got.forRepo("o/r").ByActor, 2, 2, 1)
	// Bucketed at submitted_at, not at the scan that observed it.
	if n := got.ActorBuckets["2026-10-31T09:00:00Z"][prThroughputKindPR][prThroughputRoleReviewed][prThroughputActorHuman]; n != 1 {
		t.Errorf("09:00 bucket human reviews = %d, want 1 (alice at 09:30)", n)
	}
	if n := got.ActorBuckets["2026-10-31T10:00:00Z"][prThroughputKindPR][prThroughputRoleReviewed][prThroughputActorHuman]; n != 1 {
		t.Errorf("10:00 bucket human reviews = %d, want 1 (bob at 10:05)", n)
	}
	// A review is not a PR opened, merged or closed.
	if got.Opened != 0 || got.Merged != 0 || got.Closed != 0 {
		t.Errorf("totals = opened %d merged %d closed %d, want all 0", got.Opened, got.Merged, got.Closed)
	}
}

// Every settle scan inside the window re-observes the same reviews; each
// review_id counts once, and the repeat is not written to the audit trail.
func TestPRThroughputObservedReviewCountsOnceAcrossRescans(t *testing.T) {
	a := &AuditLog{}
	logEntries(a, reviewEntries())
	logEntries(a, observedReviewEntries())

	got := a.PRThroughputCounters()
	assertReviewedActors(t, "all repos", got.ByActor, 2, 2, 1)
	assertReviewedActors(t, "repo o/r", got.forRepo("o/r").ByActor, 2, 2, 1)
	observed := 0
	for _, e := range a.RecentWithPrefixSince(time.Time{}, "") {
		if e.Action == ghpkg.AuditActionPRReviewObserved {
			observed++
		}
	}
	if observed != 3 {
		t.Errorf("pr_review_observed audit entries = %d, want 3 (a re-observed review is not re-recorded)", observed)
	}
}

// Restarting (counters file reloaded) or reseeding from the audit files must
// not change the counts.
func TestPRThroughputObservedReviewsSurviveRestartAndReseed(t *testing.T) {
	dir := t.TempDir()
	countersPath := filepath.Join(dir, "pr-throughput-counters.json")
	auditPath := filepath.Join(dir, "audit.jsonl")

	a := loadedPRCounterLog(t, countersPath, auditPath)
	logEntries(a, reviewEntries())
	b := loadedPRCounterLog(t, countersPath, auditPath)
	logEntries(b, observedReviewEntries()) // a scan after the restart
	assertReviewedActors(t, "after restart", b.PRThroughputCounters().ByActor, 2, 2, 1)

	// Reseed: no counters file, the audit history (including a duplicated
	// review) is the only source.
	seedDir := t.TempDir()
	seedAudit := writeAuditFixture(t, seedDir, append(reviewEntries(), observedReviewEntries()[0]))
	seeded := loadedPRCounterLog(t, filepath.Join(seedDir, "pr-throughput-counters.json"), seedAudit).PRThroughputCounters()
	assertReviewedActors(t, "reseeded", seeded.ByActor, 2, 2, 1)
}

// The windowed views compute from audit entries, not the counters; a
// duplicated review_id there counts once too.
func TestPRThroughputWindowCountsObservedReviewOnce(t *testing.T) {
	entries := append(reviewEntries(), observedReviewEntries()[0])
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	since := now.Add(-24 * time.Hour)

	window := buildPRThroughputWindow(entries, now, 24, "", prThroughputRoleReviewed)
	assertReviewedActors(t, "24h window", window.ByActor, 2, 2, 1)

	totals := prThroughputActorTotals(entries, since, now, "", prThroughputRoleReviewed)
	if totals.Hive != 2 || totals.Human != 2 || totals.Other != 1 {
		t.Errorf("actor totals = %+v, want hive=2 human=2 other=1", totals)
	}
	var human int
	for _, p := range prThroughputActorSeries(entries, since, now, time.Hour, "", prThroughputRoleReviewed) {
		human += p.Human
	}
	if human != 2 {
		t.Errorf("series human reviews = %d, want 2", human)
	}
}

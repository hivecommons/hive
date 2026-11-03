package dashboard

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hivestate"
	"github.com/hivecommons/hive/pkg/hub/spoke"
)

func TestBuildHeartbeatEvidence(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-48 * time.Hour)
	tooOld := now.Add(-91 * 24 * time.Hour)

	// PR 1: approved at high confidence, merged, then reverted → false approve.
	reverted := evSummaryBundle("a/b", 1, evidence.AuthorAgent, recent, true, []evidence.Verdict{
		{Perspective: "security", Model: "claude-sonnet-5", Verdict: "approve", Confidence: 0.95},
	}, 0, true)
	reverted.Author.Model = "gpt-5.4"
	reverted.RevertedBy = []string{"x"}
	// An older bundle for PR 1 is superseded by the newer one above.
	superseded := evSummaryBundle("a/b", 1, evidence.AuthorAgent, recent.Add(-time.Hour), false, []evidence.Verdict{
		{Perspective: "security", Model: "grok-4.5", Verdict: "approve", Confidence: 0.1},
	}, 0, true)

	// PR 2: blocked, merged anyway → false block; approve verdict is fine.
	blocked := evSummaryBundle("a/b", 2, evidence.AuthorHuman, recent, true, []evidence.Verdict{
		{Perspective: "quality", Model: "gpt-5.5", Verdict: "changes_requested", Confidence: 0.4},
		{Perspective: "security", Model: "", Verdict: "approve", Confidence: 1.0},
	}, 0, true)

	// PR 3: followed up after merge, rejected; reject on a merged PR is a false block.
	followed := evSummaryBundle("c/d", 3, evidence.AuthorBot, recent, true, []evidence.Verdict{
		{Perspective: "security", Model: "claude-opus-5", Verdict: "reject", Confidence: 0.0},
	}, 0, true)
	followed.FollowedUpBy = []string{"y"}

	// PR 4: open, approved — no outcome yet, not bad.
	open := evSummaryBundle("c/d", 4, evidence.AuthorAgent, recent, false, []evidence.Verdict{
		{Perspective: "security", Model: "claude-sonnet-5", Verdict: "approve", Confidence: 0.6},
	}, 0, true)

	// Shadow gates (#12370): the review gate would have held PR 1 (reverted)
	// and passed PR 2 (merged); intent would have blocked PR 4 (open). PR 3
	// carries a shadow block with no decision and must not count.
	yesHeld, noHeld := true, false
	reverted.Shadow = &evidence.Shadow{ReviewGate: &evidence.ShadowGate{WouldHaveHeld: &yesHeld}}
	blocked.Shadow = &evidence.Shadow{ReviewGate: &evidence.ShadowGate{WouldHaveHeld: &noHeld}}
	open.Shadow = &evidence.Shadow{Intent: &evidence.ShadowGate{WouldHaveBlocked: &yesHeld}}
	followed.Shadow = &evidence.Shadow{}

	// Outside the 90-day window.
	old := evSummaryBundle("c/d", 5, evidence.AuthorAgent, tooOld, true, []evidence.Verdict{
		{Perspective: "security", Model: "mistral-large", Verdict: "approve", Confidence: 0.9},
	}, 0, true)
	old.RevertedBy = []string{"z"}

	yes := true
	got := buildHeartbeatEvidence([]*evidence.Bundle{superseded, reverted, blocked, followed, open, old, nil}, now, &yes)

	if got.Verdicts90d != 5 || got.Merged90d != 3 || got.Reverted14d90d != 2 {
		t.Errorf("counts = verdicts %d merged %d reverted %d, want 5/3/2", got.Verdicts90d, got.Merged90d, got.Reverted14d90d)
	}
	for kind, want := range map[string][2]int{"human": {1, 0}, "agent": {1, 1}, "bot": {1, 0}} {
		b := got.ByAuthorKind[kind]
		if b.N != want[0] || b.Bad != want[1] {
			t.Errorf("author kind %s = %+v, want %v (follow-ups excluded)", kind, b, want)
		}
	}
	if got.FalseApprove90d != 1 || got.FalseBlock90d != 2 {
		t.Errorf("false approve/block = %d/%d, want 1/2", got.FalseApprove90d, got.FalseBlock90d)
	}
	if got.AutomergeSelfAuthored == nil || !*got.AutomergeSelfAuthored {
		t.Errorf("automerge_self_authored = %v, want true", got.AutomergeSelfAuthored)
	}
	if want := []string{"claude", "gpt"}; !reflect.DeepEqual(got.ReviewerFamilies, want) {
		t.Errorf("reviewer families = %v, want %v", got.ReviewerFamilies, want)
	}
	if want := []string{"gpt"}; !reflect.DeepEqual(got.AuthorFamilies, want) {
		t.Errorf("author families = %v, want %v", got.AuthorFamilies, want)
	}
	// Buckets: 0.95→5 bad, 0.4→2 bad, 1.0→5, 0.0→0 bad, 0.6→3.
	want := [6][2]int{{1, 1}, {0, 0}, {1, 1}, {1, 0}, {0, 0}, {2, 1}}
	for i, b := range got.Calibration {
		if b.N != want[i][0] || b.Bad != want[i][1] {
			t.Errorf("calibration[%d] = %+v, want n=%d bad=%d", i, b, want[i][0], want[i][1])
		}
	}

	// Fleet-panel aggregates (#12370). Agent: PR 1 (merged, reverted) + PR 4
	// (open); human: PR 2 merged; bot: PR 3 merged and followed up.
	wantOutcomes := map[string]spoke.HeartbeatEvidenceOutcome{
		evidence.AuthorAgent: {N: 2, Merged: 1, Reverted: 1},
		evidence.AuthorHuman: {N: 1, Merged: 1},
		evidence.AuthorBot:   {N: 1, Merged: 1, Reverted: 1},
	}
	if !reflect.DeepEqual(got.OutcomesByAuthorKind, wantOutcomes) {
		t.Errorf("outcomes_by_author_kind = %+v, want %+v", got.OutcomesByAuthorKind, wantOutcomes)
	}
	wantShadow := &spoke.HeartbeatEvidenceShadow{
		ReviewGate: spoke.HeartbeatEvidenceShadowGate{
			Held:   spoke.HeartbeatEvidenceOutcome{N: 1, Merged: 1, Reverted: 1},
			Passed: spoke.HeartbeatEvidenceOutcome{N: 1, Merged: 1},
		},
		Intent: spoke.HeartbeatEvidenceShadowGate{Held: spoke.HeartbeatEvidenceOutcome{N: 1}},
	}
	if !reflect.DeepEqual(got.ShadowGate, wantShadow) {
		t.Errorf("shadow_gate = %+v, want %+v", got.ShadowGate, wantShadow)
	}
	// buildHeartbeatEvidence never reads the proxy counter file.
	if got.WriteCounters != nil {
		t.Errorf("write_counters = %v, want nil from the bundle aggregation", got.WriteCounters)
	}
}

// TestBuildHeartbeatEvidenceOmitsUnreportedPanels: with no shadow block and
// an unknown author kind, the optional blocks stay nil so the hub reads them
// as "not reported" and an old-spoke heartbeat shape is unchanged.
func TestBuildHeartbeatEvidenceOmitsUnreportedPanels(t *testing.T) {
	now := time.Now().UTC()
	b := evSummaryBundle("a/b", 1, "martian", now.Add(-time.Hour), true, nil, 0, true)
	got := buildHeartbeatEvidence([]*evidence.Bundle{b}, now, nil)
	if got.OutcomesByAuthorKind != nil || got.ShadowGate != nil || got.WriteCounters != nil {
		t.Fatalf("optional blocks must be nil: %+v", got)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"outcomes_by_author_kind", "write_counters", "shadow_gate"} {
		if strings.Contains(string(raw), `"`+k+`"`) {
			t.Errorf("json carries %q although nothing was reported: %s", k, raw)
		}
	}
}

func TestHeartbeatWriteCounters(t *testing.T) {
	if got := heartbeatWriteCounters(nil); got != nil {
		t.Fatalf("nil totals = %v, want nil", got)
	}
	got := heartbeatWriteCounters(map[string]EvidenceWriteCount{"guarded": {Attempted: 12, Blocked: 3}, "sandbox": {Attempted: 2}})
	want := map[string]spoke.HeartbeatEvidenceWriteCount{"guarded": {Attempted: 12, Blocked: 3}, "sandbox": {Attempted: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("write counters = %+v, want %+v", got, want)
	}
}

func TestBuildHeartbeatEvidenceEmpty(t *testing.T) {
	got := buildHeartbeatEvidence(nil, time.Now(), nil)
	if got == nil || got.Verdicts90d != 0 || got.AutomergeSelfAuthored != nil || got.ReviewerFamilies != nil {
		t.Fatalf("empty = %+v, want zero non-nil block", got)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"verdicts_90d", "false_approve_90d", "false_block_90d", "merged_90d", "reverted_14d_90d", "calibration"} {
		if _, ok := m[k]; !ok {
			t.Errorf("json missing %q: %s", k, raw)
		}
	}
	if cal, _ := m["calibration"].([]any); len(cal) != 6 {
		t.Errorf("calibration len = %d, want 6", len(cal))
	}
}

func TestEvidenceCalibrationBucket(t *testing.T) {
	for _, tc := range []struct {
		c    float64
		want int
	}{{-1, 0}, {0, 0}, {0.16, 0}, {0.17, 1}, {0.5, 3}, {0.99, 5}, {1, 5}, {2, 5}, {math.NaN(), 0}} {
		if got := evidenceCalibrationBucket(tc.c); got != tc.want {
			t.Errorf("bucket(%v) = %d, want %d", tc.c, got, tc.want)
		}
	}
}

func TestEvidenceHeartbeat(t *testing.T) {
	dir := t.TempDir()
	prev := ghpkg.ReviewEvidenceRoot
	ghpkg.ReviewEvidenceRoot = filepath.Join(dir, "evidence")
	t.Cleanup(func() { ghpkg.ReviewEvidenceRoot = prev })

	// A nil server has no cache and no refresher: nil, so the hub keeps what
	// it has rather than reading a false zero.
	if got := (*Server)(nil).EvidenceHeartbeat(); got != nil {
		t.Fatalf("nil server = %+v, want nil", got)
	}

	b := evSummaryBundle("a/b", 7, evidence.AuthorAgent, time.Now().UTC().Add(-time.Hour), true, []evidence.Verdict{
		{Perspective: "security", Model: "claude-sonnet-5", Verdict: "approve", Confidence: 0.9},
	}, 0, true)
	path := filepath.Join(ghpkg.ReviewEvidenceRoot, "a", "b", "7", "abc1234.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := evidence.Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	// Proxy write counters (#12370) ride along from the durable counter file.
	prevCounters := writeCountersPath
	writeCountersPath = filepath.Join(dir, "write-counters.json")
	t.Cleanup(func() { writeCountersPath = prevCounters })
	if err := os.WriteFile(writeCountersPath, []byte(`{"totals":{"guarded":{"attempted":7,"blocked":2}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{}}
	// The bundle walk never runs on the beat path: the first call only
	// schedules the background refresh and reports nil ("keep what you have").
	if got := s.EvidenceHeartbeat(); got != nil {
		t.Fatalf("first call = %+v, want nil while the refresh is in flight", got)
	}
	waitEvidenceHeartbeatRefresh(t, s)
	got := s.EvidenceHeartbeat()
	if got == nil || got.Verdicts90d != 1 || got.Merged90d != 1 {
		t.Fatalf("got %+v, want 1 verdict / 1 merged", got)
	}
	if got.OutcomesByAuthorKind[evidence.AuthorAgent] != (spoke.HeartbeatEvidenceOutcome{N: 1, Merged: 1}) {
		t.Errorf("outcomes_by_author_kind = %+v, want one merged agent PR", got.OutcomesByAuthorKind)
	}
	if got.WriteCounters["guarded"] != (spoke.HeartbeatEvidenceWriteCount{Attempted: 7, Blocked: 2}) {
		t.Errorf("write_counters = %+v, want guarded 7/2", got.WriteCounters)
	}
	want := s.deps.Config.AutoMerge.SelfAuthoredAutoMergeAllowed(s.deps.Config.ACMMLevel)
	if got.AutomergeSelfAuthored == nil || *got.AutomergeSelfAuthored != want {
		t.Errorf("automerge_self_authored = %v, want %v", got.AutomergeSelfAuthored, want)
	}
	if got.HiveState != nil {
		t.Errorf("hive_state without a Nous series = %+v, want nil", got.HiveState)
	}

	// With a recorded snapshot the block carries the hive state trends (#12339).
	snapDir := t.TempDir()
	s.deps.Nous = &NousState{SnapshotDir: snapDir, Status: map[string]interface{}{}}
	if err := s.deps.Nous.HiveState().Record(hivestate.Snapshot{QueueIssues: 6, QueuePRs: 2}); err != nil {
		t.Fatal(err)
	}
	got = s.EvidenceHeartbeat()
	if got == nil || got.HiveState == nil || got.HiveState.Last24h.Samples != 1 || got.HiveState.Last24h.QueueIssues.Median != 6 {
		t.Fatalf("hive_state = %+v, want one 24h sample with median queue_issues 6", got.HiveState)
	}
}

// A refresh can run after its caller has restored the package default. It
// must still read the root captured for that beat (#15169).
func TestRefreshEvidenceHeartbeatUsesCapturedRoot(t *testing.T) {
	root := t.TempDir()
	b := evSummaryBundle("a/b", 7, evidence.AuthorAgent, time.Now().UTC().Add(-time.Hour), true, nil, 0, true)
	if err := evidence.Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bundle.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	prev := ghpkg.ReviewEvidenceRoot
	ghpkg.ReviewEvidenceRoot = t.TempDir()
	t.Cleanup(func() { ghpkg.ReviewEvidenceRoot = prev })
	s := newTestServer()
	done := make(chan struct{})
	go func() {
		s.refreshEvidenceHeartbeat(root)
		close(done)
	}()
	<-done
	got := s.EvidenceHeartbeat()
	if got == nil || got.Merged90d != 1 {
		t.Fatalf("got %+v, want one merged PR from the captured root", got)
	}
}

func TestNousStateHiveState(t *testing.T) {
	var nilNous *NousState
	if nilNous.HiveState() != nil {
		t.Fatal("nil NousState must have no series")
	}
	if (&NousState{}).HiveState() != nil {
		t.Fatal("NousState without SnapshotDir must have no series")
	}
	ns := &NousState{SnapshotDir: t.TempDir()}
	if st := ns.HiveState(); st == nil || st != ns.HiveState() {
		t.Fatal("HiveState must return one stable store")
	}
}

// TestBuildHeartbeatEvidenceWindows pins the trailing 24h / 7d / 30d blocks
// the hub window selector reads (RFC #12062): each window re-counts the
// bundles created inside it and leaves the lifetime fields empty.
func TestBuildHeartbeatEvidenceWindows(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	mk := func(n int, age time.Duration) *evidence.Bundle {
		return evSummaryBundle("a/b", n, evidence.AuthorAgent, now.Add(-age), true, []evidence.Verdict{
			{Perspective: "security", Model: "claude-sonnet-5", Verdict: "approve", Confidence: 0.9},
		}, 0, true)
	}
	yes := true
	got := buildHeartbeatEvidence([]*evidence.Bundle{mk(1, 2*time.Hour), mk(2, 3*24*time.Hour), mk(3, 20*24*time.Hour), mk(4, 60*24*time.Hour)}, now, &yes)
	if got.Merged90d != 4 || got.Verdicts90d != 4 {
		t.Fatalf("90d = %+v", got)
	}
	want := map[string]int{"24h": 1, "7d": 2, "30d": 3}
	if len(got.Windows) != len(want) {
		t.Fatalf("windows = %v", got.Windows)
	}
	for key, n := range want {
		w := got.Windows[key]
		if w == nil || w.Merged90d != n || w.Verdicts90d != n || len(w.OutcomesByAuthorKind) != 1 || w.OutcomesByAuthorKind["agent"].N != n {
			t.Errorf("%s = %+v, want %d PRs", key, w, n)
		}
		if w != nil && (w.Windows != nil || w.AutomergeSelfAuthored != nil || w.WriteCounters != nil) {
			t.Errorf("%s carries lifetime fields: %+v", key, w)
		}
	}
	if _, ok := spoke.HeartbeatEvidenceWindowKeys["7d"]; !ok || spoke.HeartbeatEvidenceWindowKeys["30d"] != 720 {
		t.Errorf("window keys = %v", spoke.HeartbeatEvidenceWindowKeys)
	}
}

// TestHeartbeatEvidenceMatchesSummaryFold pins that the heartbeat evidence
// block folds a spoke's live + backfilled bundles per PR exactly as
// GET /api/evidence/summary does (#12516): the same PRs land in the same
// window by PR open time, with the live verdicts and the forge outcome.
// Before the shared fold the heartbeat took the newest (live) bundle only,
// so all three PRs below counted as open in the 24h window.
func TestHeartbeatEvidenceMatchesSummaryFold(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	backfilled := func(b *evidence.Bundle, state string) *evidence.Bundle {
		b.Backfilled = true
		b.Backfill = &evidence.Backfill{Source: "github-api", At: now.Add(-30 * time.Minute), State: state}
		return b
	}
	approve := []evidence.Verdict{{Perspective: "security", Model: "claude-sonnet-5", Verdict: "approve", Confidence: 0.95}}

	// PR 1 (agent): opened 6 days ago, merged by a human on the forge;
	// reviewed live 2h ago.
	opened1 := now.Add(-6 * 24 * time.Hour)
	bf1 := backfilled(evSummaryBundle("a/b", 1, evidence.AuthorAgent, opened1, true, nil, 0, false), evidence.BackfillStateMerged)
	bf1.Merge.At, bf1.Merge.OpenedAt = opened1.Add(48*time.Hour), opened1
	live1 := evSummaryBundle("a/b", 1, evidence.AuthorAgent, now.Add(-2*time.Hour), false, approve, 0, true)
	// PR 2 (human): opened 20 days ago, closed without merge; reviewed 1h ago.
	bf2 := backfilled(evSummaryBundle("a/b", 2, evidence.AuthorHuman, now.Add(-20*24*time.Hour), false, nil, 0, false), evidence.BackfillStateClosed)
	live2 := evSummaryBundle("a/b", 2, evidence.AuthorHuman, now.Add(-time.Hour), false, []evidence.Verdict{
		{Perspective: "quality", Model: "gpt-5.5", Verdict: "changes_requested", Confidence: 0.4},
	}, 0, true)
	// PR 3 (bot): opened 60 days ago, merged and reverted; reviewed 3h ago.
	bf3 := backfilled(evSummaryBundle("a/b", 3, evidence.AuthorBot, now.Add(-60*24*time.Hour), true, nil, 0, false), evidence.BackfillStateMerged)
	bf3.RevertedBy = []string{"a/b#9@x"}
	live3 := evSummaryBundle("a/b", 3, evidence.AuthorBot, now.Add(-3*time.Hour), false, approve, 0, true)
	bundles := []*evidence.Bundle{live1, bf1, live2, bf2, live3, bf3}

	got := buildHeartbeatEvidence(bundles, now, nil)
	windows := map[string]*spoke.HeartbeatEvidence{"90d": got}
	hours := map[string]int{"90d": int(evidenceHeartbeatWindow / time.Hour)}
	for key, h := range spoke.HeartbeatEvidenceWindowKeys {
		windows[key], hours[key] = got.Windows[key], h
	}
	wantN := map[string]int{"24h": 0, "7d": 1, "30d": 2, "90d": 3}
	for key, hb := range windows {
		sum := buildEvidenceSummary(bundles, now, hours[key], "", "")
		if sum.N != wantN[key] {
			t.Fatalf("%s: summary n = %d, want %d", key, sum.N, wantN[key])
		}
		n := 0
		for _, kind := range []string{evidence.AuthorHuman, evidence.AuthorAgent, evidence.AuthorBot} {
			h, s := hb.OutcomesByAuthorKind[kind], sum.Outcomes[kind]
			if h.N != s.N || h.Merged != s.Merged {
				t.Errorf("%s %s: heartbeat n/merged = %d/%d, summary = %d/%d", key, kind, h.N, h.Merged, s.N, s.Merged)
			}
			n += h.N
		}
		if n != sum.N {
			t.Errorf("%s: heartbeat counts %d PRs, summary %d", key, n, sum.N)
		}
	}
	// 90 days: PR 1 merged (approval is not a false approve), PR 2 closed
	// (changes requested is not a false block), PR 3 merged and reverted
	// (its live approval is a false approve).
	if got.Verdicts90d != 3 || got.Merged90d != 2 || got.Reverted14d90d != 1 || got.FalseApprove90d != 1 || got.FalseBlock90d != 0 {
		t.Errorf("90d = verdicts %d merged %d reverted %d false approve %d false block %d, want 3/2/1/1/0",
			got.Verdicts90d, got.Merged90d, got.Reverted14d90d, got.FalseApprove90d, got.FalseBlock90d)
	}
}

// waitEvidenceHeartbeatRefresh blocks until the refresh EvidenceHeartbeat
// scheduled has stored its result. No sleeping: the cache exposes the
// completion channel.
func waitEvidenceHeartbeatRefresh(t *testing.T, s *Server) {
	t.Helper()
	done := s.evidenceHB.refreshDone()
	if done == nil {
		t.Fatal("no evidence refresh was scheduled")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("evidence refresh did not finish")
	}
}

// TestEvidenceHeartbeatCacheRefreshesOffTheBeatPath pins the behaviour that
// fixes the hosted hive's permanent MINIMAL beats: the walk runs at most once
// per evidenceHeartbeatRefreshInterval in the background, a fresh cache is
// served without re-walking, a stale cache triggers exactly one refresh, and
// an unreadable root keeps the previous block.
func TestEvidenceHeartbeatCacheRefreshesOffTheBeatPath(t *testing.T) {
	var walks atomic.Int32
	var c evidenceHeartbeatCache
	// refresh stands in for the bundle walk; at is the fake clock it stores
	// as refreshedAt so staleness is judged against the test's own time.
	refresh := func(block *spoke.HeartbeatEvidence, at time.Time) func() {
		return func() {
			walks.Add(1)
			c.store(block, at)
		}
	}
	now := time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)

	// Empty cache: schedules a refresh, returns nil.
	if got := c.snapshot(now, refresh(&spoke.HeartbeatEvidence{Merged90d: 1}, now)); got != nil {
		t.Fatalf("empty cache = %+v, want nil", got)
	}
	<-c.refreshDone()
	if walks.Load() != 1 {
		t.Fatalf("walks = %d, want 1", walks.Load())
	}
	// Fresh cache: served as-is, no second walk, no in-flight refresh.
	soon := now.Add(evidenceHeartbeatRefreshInterval / 2)
	if got := c.snapshot(soon, refresh(nil, soon)); got == nil || got.Merged90d != 1 {
		t.Fatalf("fresh cache = %+v, want merged=1", got)
	}
	if c.isRefreshing() || walks.Load() != 1 {
		t.Fatalf("fresh cache must not refresh: walks=%d refreshing=%v", walks.Load(), c.isRefreshing())
	}
	// Stale cache: previous block is served immediately while ONE refresh runs.
	later := now.Add(evidenceHeartbeatRefreshInterval)
	if got := c.snapshot(later, refresh(&spoke.HeartbeatEvidence{Merged90d: 2}, later)); got == nil || got.Merged90d != 1 {
		t.Fatalf("stale cache = %+v, want previous merged=1 while refreshing", got)
	}
	<-c.refreshDone()
	if got := c.snapshot(later, refresh(nil, later)); got == nil || got.Merged90d != 2 {
		t.Fatalf("after refresh = %+v, want merged=2", got)
	}
	if walks.Load() != 2 {
		t.Fatalf("walks = %d, want 2", walks.Load())
	}
	// A failed refresh (nil block) keeps the previous block and still advances
	// refreshedAt, so a broken root is retried per interval, not per beat.
	much := later.Add(evidenceHeartbeatRefreshInterval)
	c.snapshot(much, refresh(nil, much))
	<-c.refreshDone()
	if got := c.snapshot(much, refresh(nil, much)); got == nil || got.Merged90d != 2 {
		t.Fatalf("after failed refresh = %+v, want previous merged=2", got)
	}
	if c.isRefreshing() || walks.Load() != 3 {
		t.Fatalf("failed refresh must not re-run within the interval: walks=%d", walks.Load())
	}
}

// TestEvidenceHeartbeatCacheSingleFlight: a second snapshot while a refresh
// is in flight must not start another walk.
func TestEvidenceHeartbeatCacheSingleFlight(t *testing.T) {
	var c evidenceHeartbeatCache
	started := make(chan struct{})
	release := make(chan struct{})
	var walks atomic.Int32
	refresh := func() {
		walks.Add(1)
		close(started)
		<-release
		c.store(&spoke.HeartbeatEvidence{Merged90d: 1}, time.Now())
	}
	now := time.Now()
	c.snapshot(now, refresh)
	<-started
	if got := c.snapshot(now, refresh); got != nil {
		t.Fatalf("snapshot during refresh = %+v, want nil", got)
	}
	close(release)
	<-c.refreshDone()
	if walks.Load() != 1 {
		t.Fatalf("walks = %d, want 1 (single flight)", walks.Load())
	}
}

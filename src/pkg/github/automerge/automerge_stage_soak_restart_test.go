package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// soakRestartStage is a trust-blocked contributor PR (the lane refuses it
// for its tier, before its own soak step) whose head went green at
// greenAt, seen by a freshly started process: the soak tracker is empty,
// as after every pod restart or upgrade. check-runs answers *greenAt as the
// forge's completed_at for the head; reads counts those calls.
type soakRestartStage struct {
	e     *Engine
	api   *stageAPI
	store *stagelabels.Store
	clock *contributorLaneClock
	reads *int
}

func newSoakRestartStage(t *testing.T, greenAt *time.Time) (*soakRestartStage, func()) {
	t.Helper()
	api, closeAPI := seededStageAPI(t)
	reads := 0
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			return false
		}
		switch r.URL.Path {
		case "/repos/acme/widget/commits/sha/check-runs":
			reads++
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []map[string]any{
				{"name": "build", "status": "completed", "conclusion": "success", "completed_at": greenAt.Format(time.RFC3339)},
			}})
			return true
		case "/repos/acme/widget/commits/sha/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "statuses": []any{}})
			return true
		}
		return false
	}
	clock := &contributorLaneClock{at: time.Date(2026, 11, 20, 12, 18, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	e, store := humanStageEngine(t, api.url, clock, verdict, 30*time.Minute)
	return &soakRestartStage{e: e, api: api, store: store, clock: clock, reads: &reads}, closeAPI
}

func (s *soakRestartStage) checkRunReads() int {
	s.api.mu.Lock()
	defer s.api.mu.Unlock()
	return *s.reads
}

func (s *soakRestartStage) sweep(t *testing.T, labels ...string) {
	t.Helper()
	s.api.reset()
	s.e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", stagePR(9, labels...), "untrusted-author-role", false, nil)
	s.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(9, labels...), contributorLaneReasonNoLinkedIssue, false, nil)
	s.e.noteStageRole("acme", "widget", 9, stagelabels.RoleUntrusted)
	if _, err := s.e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A head the forge says went green two hours ago, past its 30m soak, is
// still 090 on the first pass after a restart: the human route seeds the
// empty tracker from the check's completed_at, as the lane does for its own
// soak (hivecommons/hive#12726), instead of soaking it again from now.
// Before, every restart moved such PRs from 090 to 060 together for one
// full window (projectbluefin/utah-packages#1222, server#700, 2026-11-20).
func TestHumanRouteSoakSurvivesRestart(t *testing.T) {
	greenAt := time.Date(2026, 11, 20, 10, 18, 0, 0, time.UTC)
	s, closeAPI := newSoakRestartStage(t, &greenAt)
	defer closeAPI()

	s.sweep(t, "role/untrusted", "stage/090-ready-for-human-merge")
	if got := stageOf(t, s.store, 9); got != stagelabels.ReadyForHumanMerge {
		t.Fatalf("after restart: stage = %v, want 090 (soaked two hours ago)", got)
	}
	if slices.Contains(s.api.removed, "stage/090-ready-for-human-merge") {
		t.Fatalf("restart dropped the 090 label: removed=%v added=%v", s.api.removed, s.api.added)
	}
	seeded := greenAt
	if first, ok := s.e.contributorSoak.FirstGreen("acme", "widget", 9, "sha"); !ok || !first.Equal(seeded) {
		t.Fatalf("first green = %v ok=%v, want seeded from completed_at", first, ok)
	}
	// The seed is read once per head; later passes keep the tracker's clock
	// even when the forge's answer moves (a re-run check).
	greenAt = s.clock.at.Add(-time.Minute)
	s.clock.at = s.clock.at.Add(humanChecksTTL)
	s.sweep(t, "role/untrusted", "stage/090-ready-for-human-merge")
	if first, _ := s.e.contributorSoak.FirstGreen("acme", "widget", 9, "sha"); !first.Equal(seeded) {
		t.Fatalf("first green moved to %v on a later pass, want %v kept", first, seeded)
	}
	if got := s.checkRunReads(); got != 1 {
		t.Fatalf("check-runs read %d times over two passes, want once per head", got)
	}
	if got := stageOf(t, s.store, 9); got != stagelabels.ReadyForHumanMerge {
		t.Fatalf("second pass: stage = %v, want 090", got)
	}
}

// The seed only moves the clock back to when the head really went green:
// a head green for 10 minutes still soaks (060) until its 30m are up, and a
// completed_at in the future (clock skew) is ignored, so the soak runs from
// now.
func TestHumanRouteSoakSeedKeepsAnOpenSoak(t *testing.T) {
	for name, greenAt := range map[string]time.Time{
		"green 10m ago": time.Date(2026, 11, 20, 12, 8, 0, 0, time.UTC),
		"future":        time.Date(2026, 11, 20, 13, 18, 0, 0, time.UTC),
	} {
		t.Run(name, func(t *testing.T) {
			s, closeAPI := newSoakRestartStage(t, &greenAt)
			defer closeAPI()
			s.sweep(t, "role/untrusted")
			if got := stageOf(t, s.store, 9); got != stagelabels.Reviewed {
				t.Fatalf("stage = %v, want 060 while the soak runs", got)
			}
			want := greenAt
			if greenAt.After(s.clock.at) {
				want = s.clock.at
			}
			if first, ok := s.e.contributorSoak.FirstGreen("acme", "widget", 9, "sha"); !ok || !first.Equal(want) {
				t.Fatalf("first green = %v ok=%v, want %v", first, ok, want)
			}
		})
	}
}

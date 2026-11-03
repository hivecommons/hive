package dashboard

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/timeline"
)

// Delivery SLA on /api/status: the fleet block, per-repo blocks matched by
// org-less name, stale in-progress flags and the targets the UI colours
// against — all derived from the lifecycle timeline, no GitHub traffic.
func TestStatusWithOverviewBands_AttachesSLA(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{SLA: config.SLAConfig{Targets: config.SLATargets{PRToMergeH: 12}}}}
	now := time.Date(2026, 10, 14, 18, 0, 0, 0, time.UTC)
	resetSLAForTest(t)
	store := s.LifecycleTimeline()
	started := now.Add(-6 * time.Hour)
	store.Record(timeline.Event{IssueRef: "widgets#7", Kind: timeline.KindStarted, At: started.UnixMilli(), Agent: "bee"})
	store.Record(timeline.Event{IssueRef: "widgets#7", Kind: timeline.KindPROpened, At: started.Add(2 * time.Hour).UnixMilli(),
		Attrs: map[string]string{timeline.AttrPRRef: "widgets#41"}})
	store.Record(timeline.Event{IssueRef: "widgets#7", Kind: timeline.KindMerged, At: started.Add(5 * time.Hour).UnixMilli()})
	// Started 3 days ago with no PR: past any sane claim TTL → stale.
	store.Record(timeline.Event{IssueRef: "widgets#8", Kind: timeline.KindStarted, At: now.Add(-72 * time.Hour).UnixMilli(), Agent: "wasp"})

	status := &StatusPayload{Repos: []FrontendRepo{{Name: "widgets", Full: "acme/widgets"}, {Name: "quiet", Full: "acme/quiet"}}}
	out := s.statusWithOverviewBands(status, now)
	if status.SLA != nil || status.Repos[0].SLA != nil {
		t.Fatal("cached status must not be mutated")
	}
	if out.SLA == nil {
		t.Fatal("fleet sla missing")
	}
	if out.SLA.Targets.StartToPRH != 4 || out.SLA.Targets.PRToMergeH != 12 || out.SLA.Targets.OpenToMergeH != 72 {
		t.Fatalf("targets = %+v", out.SLA.Targets)
	}
	w7 := out.SLA.Windows["7d"]
	if w7.StartToPR.N != 1 || w7.StartToPR.P50 != 2 || w7.PRToMerge.P50 != 3 {
		t.Fatalf("fleet 7d = %+v", w7)
	}
	if out.SLA.StaleCount != 1 || len(out.SLA.StaleInProgress) != 1 || out.SLA.StaleInProgress[0].Number != 8 || out.SLA.ClaimTTLS <= 0 {
		t.Fatalf("stale = %+v ttl=%d", out.SLA.StaleInProgress, out.SLA.ClaimTTLS)
	}
	if len(out.SLA.History) != timeline.SLAHistoryDays || out.SLA.History[timeline.SLAHistoryDays-1].NMerged != 1 {
		t.Fatalf("fleet history = %d days, today=%+v", len(out.SLA.History), out.SLA.History[len(out.SLA.History)-1])
	}
	widgets, quiet := out.Repos[0].SLA, out.Repos[1].SLA
	if widgets == nil || widgets.Windows["30d"].StartToPR.N != 1 || widgets.MergedLast7d != 1 {
		t.Fatalf("widgets sla = %+v", widgets)
	}
	if quiet == nil || quiet.Windows["7d"].StartToPR.N != 0 || len(quiet.StaleInProgress) != 0 {
		t.Fatalf("quiet repo must get an empty report: %+v", quiet)
	}
	raw, _ := json.Marshal(out)
	for _, key := range []string{`"sla":{`, `"stale_in_progress":[`, `"targets":{`, `"start_to_pr_h":4`, `"history":[`} {
		if !bytesContain(raw, key) {
			t.Errorf("payload missing %s", key)
		}
	}
}

func bytesContain(b []byte, s string) bool {
	return len(b) >= len(s) && stringIndex(string(b), s) >= 0
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestEnableSLAHistoryPersistence_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sla-history.json")
	s := newTestServer()
	resetSLAForTest(t)
	s.EnableSLAHistoryPersistence(path)
	now := time.Date(2026, 10, 14, 18, 0, 0, 0, time.UTC)
	s.SLATracker().History.Update(map[string]map[string]*timeline.SLADay{"widgets": {"2026-10-14": {Day: "2026-10-14", NPR: 2}}}, now)
	if err := s.SLATracker().History.Flush(); err != nil {
		t.Fatal(err)
	}
	fresh := timeline.NewSLAHistory()
	if err := fresh.EnablePersistence(path, nil); err != nil {
		t.Fatal(err)
	}
	if got := fresh.Series("widgets", now); got[len(got)-1].NPR != 2 {
		t.Fatalf("history not persisted: %+v", got[len(got)-1])
	}
	// Missing directory is a warning, not a crash.
	s.EnableSLAHistoryPersistence(filepath.Join(t.TempDir(), "missing", "x.json"))
}

func TestSLAConfigEndpoints(t *testing.T) {
	s := newFullServer(t)
	s.authToken = convergence4263Token

	w := convergenceRequest(s, http.MethodGet, "/api/config/sla", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Effective config.SLATargets `json:"effective_targets"`
		Defaults  config.SLATargets `json:"defaults"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Effective.StartToPRH != 4 || resp.Effective.PRToMergeH != 24 || resp.Effective.OpenToMergeH != 72 || resp.Defaults != resp.Effective {
		t.Fatalf("defaults = %+v / %+v", resp.Effective, resp.Defaults)
	}

	if w := convergenceRequest(s, http.MethodPut, "/api/config/sla", `{"targets":{"start_to_pr_h":-3}}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("negative target = %d, want 400", w.Code)
	}
	if s.deps.Config.SLA.Targets.StartToPRH != 0 {
		t.Fatal("rejected PUT mutated config")
	}
	w = convergenceRequest(s, http.MethodPut, "/api/config/sla", `{"targets":{"start_to_pr_h":2,"pr_to_merge_h":0,"open_to_merge_h":48}}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d body=%s", w.Code, w.Body.String())
	}
	if got := s.deps.Config.SLA.EffectiveTargets(); got.StartToPRH != 2 || got.PRToMergeH != 24 || got.OpenToMergeH != 48 {
		t.Fatalf("effective after PUT = %+v", got)
	}
	if w := convergenceRequest(s, http.MethodPut, "/api/config/sla", `{"targets":{"start_to_pr_h":1}}`, false); w.Code == http.StatusOK {
		t.Fatal("non-owner PUT must be rejected")
	}
	// The governor config GET carries the section for the Settings tab.
	w = convergenceRequest(s, http.MethodGet, "/api/config/governor", "", true)
	if w.Code != http.StatusOK || !bytesContain(w.Body.Bytes(), `"sla":{`) {
		t.Fatalf("governor config lacks sla: %d %s", w.Code, w.Body.String()[:min(200, len(w.Body.String()))])
	}
}

// resetSLAForTest isolates the process-wide timeline store and SLA tracker.
func resetSLAForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		lifecycleTimelineOnce = sync.Once{}
		lifecycleStore = nil
		slaTrackerOnce = sync.Once{}
		slaTracker = nil
	}
	reset()
	t.Cleanup(reset)
}

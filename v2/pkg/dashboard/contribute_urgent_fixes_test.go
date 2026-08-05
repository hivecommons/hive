package dashboard

import (
	"net/http"
	"strings"
	"testing"
)

// ── Two urgent live-hive fixes: empty Ready-queue (label-filter binding) and the
//    perpetually-empty Live Activity rail. ──────────────────────────────────────

// TestLabelFilterBindsCanonicalField proves the admission filter tag list binds to
// the CANONICAL contribute_deny_<kind> field in BOTH modes — the only field the
// backend filter reads. Binding an allow-mode filter elsewhere admitted nothing
// (empty queue). Also proves the policy DISPLAY reads deny_labels in both modes.
func TestLabelFilterBindsCanonicalField(t *testing.T) {
	body := renderContributePage(t)

	if !strings.Contains(body, "function adminFilterListKey(kind){return 'contribute_deny_'+kind;}") {
		t.Error("admin filter does not bind to the canonical contribute_deny_<kind> field")
	}
	for _, kind := range []string{"titles", "authors", "labels"} {
		if !strings.Contains(body, "renderAdminFilter('admin-filter-"+kind+"','") {
			t.Errorf("filter for kind %q not rendered", kind)
		}
		if !strings.Contains(body, "','"+kind+"');") {
			t.Errorf("filter %q not bound by kind (per-mode canonical binding)", kind)
		}
	}
	// The policy display reads deny_labels in BOTH modes (not allow_labels in allow-mode).
	if strings.Contains(body, "p.labels_mode==='allow'?p.allow_labels:p.deny_labels") {
		t.Error("policy display still reads allow_labels in allow-mode (empty on the live hive)")
	}
	if !strings.Contains(body, "['Label filter',esc(p.labels_mode||'deny')+': '+list(p.deny_labels)]") {
		t.Error("policy display does not read the canonical deny_labels list")
	}
	// The save patch clears the stale allow_labels remnant.
	if !strings.Contains(body, "contribute_allow_labels:[]") {
		t.Error("save patch does not clear the stale allow_labels field")
	}
}

// TestLabelFilterAllowModeAdmitsMatching proves the end-to-end behavior: an Allow-mode
// label filter with `clanker-queue` admits ONLY clanker-queue-labeled issues (the live
// hive's intended config), and an EMPTY allow-list admits ALL (the footgun-safe
// default the backend already implements).
func TestLabelFilterAllowModeAdmitsMatching(t *testing.T) {
	s, deps := apiServer(t)

	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{{
		Name: "acme/repo", Full: "acme/repo",
		ActionableIssues: []any{
			map[string]any{"number": float64(1), "title": "queued", "labels": []any{"clanker-queue"}},
			map[string]any{"number": float64(2), "title": "other", "labels": []any{"docs"}},
		},
	}}}
	s.statusMu.Unlock()

	// Empty allow-list = admit all (footgun-safe).
	deps.Config.Hub.ContributeLabelsMode = "allow"
	deps.Config.Hub.ContributeDenyLabels = nil
	if q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit); len(q) != 2 {
		t.Fatalf("allow-mode + empty list should admit ALL, got %d", len(q))
	}

	// Allow + clanker-queue admits only #1.
	deps.Config.Hub.ContributeDenyLabels = []string{"clanker-queue"}
	q := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	if len(q) != 1 || q[0].Number != 1 {
		t.Fatalf("allow-mode clanker-queue should admit ONLY #1, got %+v", q)
	}
}

// TestLabelFilterSaveRoundTripsToDenyField proves the UI Save PUTs the list into the
// CANONICAL deny_labels field (backend-read) and clears the stale allow_labels — so
// Allow + a label actually persists and admits.
func TestLabelFilterSaveRoundTripsToDenyField(t *testing.T) {
	s, deps := apiServer(t)
	payload := map[string]any{
		"contribute_labels_mode":  "allow",
		"contribute_deny_labels":  []string{"clanker-queue"},
		"contribute_allow_labels": []string{},
	}
	rec := doPut(s, "/api/config/governor/hub", payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT governor/hub: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if deps.Config.Hub.ContributeLabelsMode != "allow" {
		t.Errorf("labels mode = %q, want allow", deps.Config.Hub.ContributeLabelsMode)
	}
	if len(deps.Config.Hub.ContributeDenyLabels) != 1 || deps.Config.Hub.ContributeDenyLabels[0] != "clanker-queue" {
		t.Errorf("canonical deny_labels = %v, want [clanker-queue]", deps.Config.Hub.ContributeDenyLabels)
	}
	if len(deps.Config.Hub.ContributeAllowLabels) != 0 {
		t.Errorf("stale allow_labels should be cleared, got %v", deps.Config.Hub.ContributeAllowLabels)
	}
}

// TestLiveActivityRailSeedsFromActivityPoll proves the rail is seeded + kept current
// from the RELIABLE /api/contribute/activity endpoint (the source Onboarding uses),
// not solely from the SSE stream which returns nothing on hosted spokes — so the rail
// shows the backlog even when SSE never delivers, deduping cross-source events.
func TestLiveActivityRailSeedsFromActivityPoll(t *testing.T) {
	body := renderContributePage(t)

	// ccStart seeds/polls the rail from the activity endpoint.
	if !strings.Contains(body, "ccPollActivity()") {
		t.Error("ccStart does not seed/poll the rail from /api/contribute/activity")
	}
	pollStart := strings.Index(body, "function ccPollActivity(){")
	if pollStart < 0 {
		t.Fatal("ccPollActivity missing")
	}
	pollBody := body[pollStart : pollStart+strings.Index(body[pollStart:], "\n}")]
	if !strings.Contains(pollBody, "/api/contribute/activity") {
		t.Error("ccPollActivity does not fetch the reliable activity endpoint")
	}
	// The rail rebuild narrates via ccNarrate + renders via ccRenderLog (format parity).
	rb := body[strings.Index(body, "function ccRebuildLogFromActivity(){"):]
	rb = rb[:strings.Index(rb, "\n}")]
	if !strings.Contains(rb, "ccNarrate") || !strings.Contains(rb, "ccRenderLog()") {
		t.Error("rail rebuild does not use ccNarrate/ccRenderLog")
	}
	// Dedupe key covers timestamp+username+action+task.
	keyBody := body[strings.Index(body, "function ccActivityKey(e){"):]
	keyBody = keyBody[:strings.Index(keyBody, "\n}")]
	for _, want := range []string{"e.timestamp", "e.username", "e.action", "e.task"} {
		if !strings.Contains(keyBody, want) {
			t.Errorf("ccActivityKey does not dedupe on %q", want)
		}
	}
	// SSE routes through the SAME shared store (dedupe against poll).
	if !strings.Contains(body, "ccSSEDelivered=true") || !strings.Contains(body, "if(ccIngestActivity(e)){") {
		t.Error("SSE onActivity does not route through the shared deduped store")
	}
	// The empty state stays reachable only for genuinely-zero activity.
	if !strings.Contains(body, "if(!ccLogLines.length){el.innerHTML='<div class=\"ops-empty\">Watching the hive&hellip;</div>';return;}") {
		t.Error("the genuine empty-state render was lost")
	}
	// The live pill reflects real connectivity: polling when SSE hasn't delivered.
	if !strings.Contains(body, "if(!ccSSEDelivered)ccSetLive('poll');") {
		t.Error("live pill does not reflect the polling fallback state")
	}
}

// TestLiveActivityRailBackendServesData proves /api/contribute/activity (the rail's
// reliable source) returns the recorded activity — the data the rail seeds from.
func TestLiveActivityRailBackendServesData(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/contribute/activity")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET activity: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "activity") {
		t.Errorf("activity endpoint response missing the activity array: %s", rec.Body.String())
	}
}

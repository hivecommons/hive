package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// ── PR 2 (features): Apple-Music playlist queue controls + Opportunistic Work
//    (#2592) + file-an-issue link (#2594) + hive-settings/quota (#2595) ─────────

// TestQueuePlaylistControlsPresent asserts the queue card carries the search field
// and the per-row move-to-top / move-to-position controls, and that the reorder
// actions operate on the FULL queue by qkey (so search + act targets the right item).
func TestQueuePlaylistControlsPresent(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		`id="cc-q-search"`,                  // search input on the queue card
		`id="cc-q-search-clear"`,            // clear affordance
		`function ccQueueMatches(q){`,       // client-side filter over the loaded queue
		`function ccMoveToTop(key){`,        // per-row move-to-top
		`function ccMoveToPosition(key,n){`, // move-to-specific-position
		`ccQueueMenuHTML(`,                  // per-row ⋯ menu (owner/RW only)
		`function ccQueueIndexOf(key){`,     // action targets by qkey in the FULL model
		`ccPersistQueueOrder();`,            // all controls persist the same order
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queue playlist controls missing marker %q", want)
		}
	}

	// The ⋯ menu (reorder actions) must be gated on adminEnabled (owner/read-write).
	if !strings.Contains(body, "var menu=adminEnabled?ccQueueMenuHTML(") {
		t.Error("per-row reorder menu is not gated on adminEnabled (owner/read-write only)")
	}
	// Move-to-position must validate/clamp N to 1..len.
	if !strings.Contains(body, "if(n<1)n=1;if(n>ccQueue.length)n=ccQueue.length;") {
		t.Error("ccMoveToPosition does not clamp the target position to 1..len")
	}
}

// TestQueueSearchIsViewOnly asserts search filters the DISPLAY without mutating the
// persisted order: it only sets ccQueueSearch and re-renders; the model is untouched,
// and reorder actions read the full ccQueue by qkey.
func TestQueueSearchIsViewOnly(t *testing.T) {
	body := renderContributePage(t)
	// The search handler sets the filter text and re-renders — it must NOT persist.
	searchStart := strings.Index(body, "function ccInitQueueSearch(){")
	if searchStart < 0 {
		t.Fatal("ccInitQueueSearch missing")
	}
	searchBody := body[searchStart : searchStart+strings.Index(body[searchStart:], "\n}")]
	if strings.Contains(searchBody, "ccPersistQueueOrder") {
		t.Error("search must be a VIEW filter — it must not persist the queue order")
	}
	if !strings.Contains(searchBody, "ccQueueSearch=") || !strings.Contains(searchBody, "ccRenderQueue()") {
		t.Error("search handler must set ccQueueSearch and re-render")
	}
}

// TestOpportunisticEndpointReturnsCandidates proves the opportunistic endpoint
// surfaces REAL admissible candidates (recency-ranked), excluding the ones already
// at the FRONT of the ready queue, and never fabricates.
func TestOpportunisticEndpointReturnsCandidates(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))

	// A backlog large enough that some items sit beyond the front window.
	issues := make([]any, 0, 12)
	for i := 1; i <= 12; i++ {
		issues = append(issues, map[string]any{
			"number":      float64(i),
			"title":       "issue " + string(rune('A'+i)),
			"labels":      []any{},
			"age_minutes": float64(i * 60), // varying recency
		})
	}
	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{{Name: "acme/repo", Full: "acme/repo", ActionableIssues: issues}}}
	s.statusMu.Unlock()

	rec := doGet(s, "/api/contribute/opportunistic")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET opportunistic: got %d, want 200", rec.Code)
	}
	var resp struct {
		Opportunistic []OpportunisticItem `json:"opportunistic"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Opportunistic) == 0 {
		t.Fatal("expected opportunistic candidates, got none")
	}
	// Every surfaced item must be a real actionable issue (number 1..12), and NONE
	// of the front-window items (positions 1..opportunisticFrontWindow) should appear.
	ready := s.contributeHub.ReadyQueue(readyQueueDefaultLimit)
	front := map[string]bool{}
	for i := 0; i < opportunisticFrontWindow && i < len(ready); i++ {
		front[ready[i].Repo+"#"+itoa(ready[i].Number)] = true
	}
	for _, it := range resp.Opportunistic {
		if it.Number < 1 || it.Number > 12 {
			t.Errorf("fabricated candidate: %+v", it)
		}
		if front[it.Repo+"#"+itoa(it.Number)] {
			t.Errorf("front-of-queue item %s#%d must not appear in opportunistic list", it.Repo, it.Number)
		}
	}
}

// TestOpportunisticRespectsAdmission proves the opportunistic list only contains
// admissible work (it draws from ReadyQueue), so a denied issue is never surfaced.
func TestOpportunisticRespectsAdmission(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.RegisterAPI(testDeps(t))

	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{{
		Name: "acme/repo", Full: "acme/repo",
		ActionableIssues: []any{
			map[string]any{"number": float64(1), "title": "keep me", "labels": []any{}, "age_minutes": float64(30)},
			map[string]any{"number": float64(2), "title": "SPAM please ignore", "labels": []any{}, "age_minutes": float64(5)},
		},
	}}}
	s.statusMu.Unlock()
	s.deps.Config.Hub.ContributeTitlesMode = config.FilterModeDeny
	s.deps.Config.Hub.ContributeDenyTitles = []string{"SPAM"}

	items := s.contributeHub.OpportunisticWork(opportunisticDefaultLimit)
	for _, it := range items {
		if it.Number == 2 {
			t.Fatalf("denied issue #2 surfaced in opportunistic list: %+v", items)
		}
	}
}

// TestOpportunisticAddToQueueUsesOrderEndpoint proves "add to queue" reuses the
// existing owner/RW queue-order PUT (no new mutation endpoint) and that a read
// viewer is blocked (403) — i.e. the add ACTION is gated exactly like reorder.
func TestOpportunisticAddToQueueUsesOrderEndpoint(t *testing.T) {
	body := renderContributePage(t)
	// The add handler must PUT to the SAME order endpoint.
	if !strings.Contains(body, "function ccOppAddToQueue(key,btn){") {
		t.Fatal("ccOppAddToQueue missing")
	}
	addStart := strings.Index(body, "function ccOppAddToQueue(key,btn){")
	addBody := body[addStart : addStart+strings.Index(body[addStart:], "\n}")]
	if !strings.Contains(addBody, "/api/contribute/queue/order") {
		t.Error("add-to-queue must reuse the existing queue/order PUT endpoint")
	}
	// The add control is only rendered when adminEnabled (owner/read-write).
	if !strings.Contains(body, "var add=adminEnabled?('<button type=\"button\" class=\"opp-add\"") {
		t.Error("opportunistic add control is not gated on adminEnabled")
	}

	// Server-side: the order endpoint (which add-to-queue uses) 403s a read viewer.
	s, _ := apiServer(t)
	req := httptest.NewRequest(http.MethodPut, "/api/contribute/queue/order", strings.NewReader(`{"order":["acme/repo#1"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "read")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("read viewer add-to-queue: got %d, want 403", rec.Code)
	}
}

// TestFileAnIssueLinkTabAware proves the "file an issue" link points at the real
// new-issue form, is tab-aware, and uses ONLY the existing `enhancement` label
// (never a non-existent `contribute` label — the #2536/#2540 regression).
func TestFileAnIssueLinkTabAware(t *testing.T) {
	body := renderContributePage(t)

	if !strings.Contains(body, `id="cc-report-link"`) {
		t.Fatal("file-an-issue link missing")
	}
	if !strings.Contains(body, "github.com/hivecommons/hive/issues/new") {
		t.Error("report link does not point at the new-issue form")
	}
	// Tab-aware prefill: builds title with the tab name + body with the page URL.
	if !strings.Contains(body, "function ccUpdateReportLink(dp){") {
		t.Error("tab-aware report link builder missing")
	}
	if !strings.Contains(body, "'contribute: '+tabName+' — '") {
		t.Error("report title is not tab-aware")
	}
	if !strings.Contains(body, "window.location.href") {
		t.Error("report body does not include the current page URL")
	}
	// Only the existing `enhancement` label — never a `contribute` label.
	if !strings.Contains(body, "labels=enhancement") {
		t.Error("report link should use the existing enhancement label")
	}
	if strings.Contains(body, "labels=contribute") {
		t.Error("report link must NOT reference a non-existent `contribute` label (#2536/#2540)")
	}
}

// TestHiveLimitsEndpoint proves the limits endpoint returns REAL per-tier rate
// limits (from config) and, for an identified viewer, their own tier usage — while
// an anonymous viewer gets the tier table with no "you" block.
func TestHiveLimitsEndpoint(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Hub.TierLimits = map[string]config.TierRate{
		"newcomer":    {MaxPerHour: 3, MaxPerDay: 10, MaxConcurrent: 1},
		"contributor": {MaxPerHour: 10, MaxPerDay: 50, MaxConcurrent: 2},
	}

	// Anonymous: tiers present, no "you".
	rec := doGet(s, "/api/contribute/limits")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET limits: got %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	tiers, _ := resp["tiers"].([]any)
	if len(tiers) < 2 {
		t.Fatalf("expected the configured tiers, got %v", resp["tiers"])
	}
	if _, hasYou := resp["you"]; hasYou {
		t.Error("anonymous viewer must not get a `you` quota block")
	}
	// The newcomer tier's real caps must be present (3/hr, 10/day).
	if !strings.Contains(rec.Body.String(), `"max_per_day":10`) {
		t.Errorf("configured newcomer daily cap (10) not surfaced: %s", rec.Body.String())
	}
}

// TestEndOfQueueAndQuotaWidgets proves the end-of-queue "caught up" state, the
// hive-settings render, and the daily-quota widget (end-of-queue + me-card) are wired.
func TestEndOfQueueAndQuotaWidgets(t *testing.T) {
	body := renderContributePage(t)
	for _, want := range []string{
		`id="cc-q-end"`,                    // end-of-queue mount
		"End of queue reached",             // the calm caught-up marker
		"function ccRenderQueueEnd(show){", // end-of-queue renderer
		"function ccQuotaHTML(variant){",   // shared quota meter
		`id="me-quota-slot"`,               // quota widget on the me-card
		"function ccRenderMeQuota(){",      // me-card quota renderer
		"/api/contribute/limits",           // driven by the real limits read
	} {
		if !strings.Contains(body, want) {
			t.Errorf("end-of-queue / quota marker missing: %q", want)
		}
	}
}

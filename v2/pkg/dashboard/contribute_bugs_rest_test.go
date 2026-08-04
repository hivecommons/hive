package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── PR-B (bugs, rest): My-work Done filter, me-card tier-badge overlap, highlight-me
//    row, Governor-Hub↔Management mirror parity, #2601 bot-filter. ───────────────

// TestMyWorkDoneFilterFromCompletedActivity proves "Done" renders from the completed
// activity events (not the in-flight-only work array), and All/Active still filter
// the in-flight list.
func TestMyWorkDoneFilterFromCompletedActivity(t *testing.T) {
	body := renderContributePage(t)

	if !strings.Contains(body, "if(currentFilter==='done'){shown=(typeof ccCompletedWorkItems==='function')?ccCompletedWorkItems(30):[];}") {
		t.Error(`"Done" filter does not source from completed activity (ccCompletedWorkItems)`)
	}
	if !strings.Contains(body, "function ccCompletedWorkItems(cap){") {
		t.Fatal("ccCompletedWorkItems missing")
	}
	fn := body[strings.Index(body, "function ccCompletedWorkItems(cap){"):]
	fn = fn[:strings.Index(fn, "\n}")]
	if !strings.Contains(fn, "e.action!=='completed'") {
		t.Error("ccCompletedWorkItems does not derive rows from completed events")
	}
	if !strings.Contains(body, "else{shown=list.filter(workMatchesFilter);}") {
		t.Error("All/Active/Review must still filter the in-flight work array")
	}
}

// TestMeCardTierBadgeNoOverlap proves the me-card tier badge lives in its own layout
// slot (.me-id__tier) beside the identity, NOT absolutely positioned over the avatar.
func TestMeCardTierBadgeNoOverlap(t *testing.T) {
	body := renderContributePage(t)

	if !strings.Contains(body, `'<div class="me-id__tier">'+tierBadge(p.trust_tier,'tier-hero')+'</div>'`) {
		t.Error("tier badge is not in its own .me-id__tier layout slot")
	}
	if strings.Contains(body, `onerror="this.style.visibility=\'hidden\'">'+tierBadge(p.trust_tier,'tier-hero')+'</div>'`) {
		t.Error("tier badge is still emitted INSIDE the medallion (overlap regression)")
	}
	if strings.Contains(body, ".me-medallion .tier-badge{position:absolute") {
		t.Error("the absolute .me-medallion .tier-badge overlay rule still present (overlap)")
	}
	if !strings.Contains(body, ".me-id__tier{margin:") {
		t.Error(".me-id__tier layout slot styling missing")
	}
}

// TestLeaderboardHighlightsOwnRow proves the viewer's own row gets a subtle
// .lb-row--me highlight + "you" chip, keyed on the resolved username; anonymous
// viewers highlight nothing.
func TestLeaderboardHighlightsOwnRow(t *testing.T) {
	body := renderContributePage(t)

	if !strings.Contains(body, "var ccMeUsername=''") {
		t.Error("ccMeUsername (viewer identity for self-highlight) missing")
	}
	if !strings.Contains(body, "var isMe=(!e.is_agent&&ccMeUsername&&uname&&uname.toLowerCase()===ccMeUsername.toLowerCase());") {
		t.Error("own-row match logic missing / not case-insensitive / not human-only")
	}
	if !strings.Contains(body, "'<div class=\"lb-row'+(isMe?' lb-row--me':'')+'\">'") {
		t.Error("lb-row does not apply the .lb-row--me highlight class")
	}
	if !strings.Contains(body, `<span class="lb-you">you</span>`) {
		t.Error("own-row `you` chip missing")
	}
	if !strings.Contains(body, "ccMeUsername=u;") {
		t.Error("viewer username is not captured for the self-highlight")
	}
	if !strings.Contains(body, ".lb-row--me{background:rgba(31,111,235,.09);box-shadow:inset 3px 0 0 0 #1f6feb}") {
		t.Error("subtle .lb-row--me highlight CSS missing")
	}
}

// TestLeaderboardExcludesInternalAgents proves the Rankings render CONTRIBUTORS ONLY
// (#2601): internal agents are filtered out, and any is_agent entry reaching render
// is dropped. Intro copy reflects contributors-only.
func TestLeaderboardExcludesInternalAgents(t *testing.T) {
	body := renderContributePage(t)

	if !strings.Contains(body, "renderLeaderboard(contribs);") {
		t.Error("loadLeaderboard still passes agents into the Rankings render")
	}
	if !strings.Contains(body, "function renderLeaderboard(contribs){") {
		t.Error("renderLeaderboard was not narrowed to contributors-only")
	}
	if !strings.Contains(body, "contribs=(contribs||[]).filter(function(e){return !e.is_agent;});") {
		t.Error("renderLeaderboard does not defensively exclude is_agent entries")
	}
	if strings.Contains(body, "for(i=0;i<agents.length;i++){rank++;html+=lbRow(agents[i],rank);}") {
		t.Error("the internal-agents ranking loop is still present (#2601 regression)")
	}
	if strings.Contains(body, "Agents run by this hive and human contributors who donate compute both appear here") {
		t.Error("leaderboard intro still claims internal agents appear in the standings")
	}
}

// TestMeCardRankAmongContributorsOnly proves the Me-card rank/total count
// contributors only, not contributors+agents (#2601).
func TestMeCardRankAmongContributorsOnly(t *testing.T) {
	setupContributeEnv(t)
	s, _ := apiServer(t)

	if err := saveContributorProfile(&ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "contributor", TasksCompleted: 20}); err != nil {
		t.Fatal(err)
	}
	if err := saveContributorProfile(&ContributorProfile{GitHubUsername: "bob", ContributorID: "c-bob", TrustTier: "newcomer", TasksCompleted: 5}); err != nil {
		t.Fatal(err)
	}

	rec := doGet(s, "/api/leaderboard/contributor/alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET contributor profile: got %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if total, _ := resp["total"].(float64); int(total) != 2 {
		t.Errorf("me-card total = %v, want 2 (contributors only)", resp["total"])
	}
	if rank, _ := resp["rank"].(float64); int(rank) != 1 {
		t.Errorf("alice contributor-rank = %v, want 1", resp["rank"])
	}
}

// TestManagementMirrorsReposAndTierLimits proves the Management tab mirrors the
// Governor Hub "Repos for Contribute" toggles + "Tier access & rate limits" rows,
// writing through the existing PUT /api/config/governor/hub (no new endpoint).
func TestManagementMirrorsReposAndTierLimits(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		`id="admin-repos"`,
		"function renderAdminRepos(){",
		"Repos for Contribute",
		`id="admin-tiers"`,
		"function renderAdminTierLimits(){",
		"Tier access &amp; rate limits",
		"data-tier-field=\"max_per_hour\"",
		"data-tier-field=\"max_per_day\"",
		"data-tier-field=\"max_concurrent\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Management mirror marker missing: %q", want)
		}
	}
	for _, want := range []string{
		"disabled_repos:adminHub.disabled_repos||[]",
		"disabled_tiers:adminHub.disabled_tiers||[]",
		"tier_limits:adminHub.tier_limits||{}",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("save patch missing mirror field: %q", want)
		}
	}
}

// TestGovernorHubRoundTripsReposAndTiers proves the config GET exposes available_repos
// and the PUT persists disabled_repos + tier_limits, and a read viewer is blocked.
func TestGovernorHubRoundTripsReposAndTiers(t *testing.T) {
	s, deps := apiServer(t)

	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{
		{Name: "acme/one", Full: "acme/one"},
		{Name: "acme/two", Full: "acme/two"},
	}}
	s.statusMu.Unlock()

	rec := doGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "available_repos") || !strings.Contains(rec.Body.String(), "acme/one") {
		t.Errorf("config GET does not expose available_repos: %s", rec.Body.String())
	}

	payload := map[string]any{
		"disabled_repos": []string{"acme/two"},
		"tier_limits": map[string]any{
			"newcomer": map[string]any{"max_per_hour": 3, "max_per_day": 10, "max_concurrent": 1},
		},
		"disabled_tiers": []string{"advisor"},
	}
	rec = doPut(s, "/api/config/governor/hub", payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT governor/hub: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if len(deps.Config.Hub.DisabledRepos) != 1 || deps.Config.Hub.DisabledRepos[0] != "acme/two" {
		t.Errorf("disabled_repos not persisted: %v", deps.Config.Hub.DisabledRepos)
	}
	if tr, ok := deps.Config.Hub.TierLimits["newcomer"]; !ok || tr.MaxPerHour != 3 || tr.MaxPerDay != 10 {
		t.Errorf("tier_limits not persisted: %+v", deps.Config.Hub.TierLimits)
	}
	if len(deps.Config.Hub.DisabledTiers) != 1 {
		t.Errorf("disabled_tiers not persisted: %v", deps.Config.Hub.DisabledTiers)
	}

	// Read viewer is blocked on the governor PUT (via the full handler chain).
	var b strings.Builder
	_ = json.NewEncoder(&b).Encode(payload)
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/hub", strings.NewReader(b.String()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "read")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("read viewer governor PUT: got %d, want 403", rr.Code)
	}
}

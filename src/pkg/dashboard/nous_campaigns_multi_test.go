package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/nous"
)

func doPatchOwner(s *Server, path string, body interface{}) *httptest.ResponseRecorder {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, path, &b)
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

func doPatchNoOwner(s *Server, path string, body interface{}) *httptest.ResponseRecorder {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, path, &b)
	req.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(rec, req)
	return rec
}

func nousDecodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func campaignScope(repos ...string) map[string]interface{} {
	return map[string]interface{}{"repos": repos}
}

// TestNousCampaignsRunConcurrentlyWithScopeGuard covers hivecommons/hive#12532
// end to end: the migrated history campaign is the first (hive-wide) card,
// a second experiment is refused with 409 until the first is narrowed or
// paused, disjoint scopes run side by side with their own 0 / 672
// baselines, and pause/resume/archive go through PATCH.
func TestNousCampaignsRunConcurrentlyWithScopeGuard(t *testing.T) {
	const history = 8
	s, deps, _ := nousCampaignServer(t, history)
	now := time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)
	nousNow = func() time.Time { return now }
	t.Cleanup(func() { nousNow = time.Now })

	// Migration: campaign 1 (hive-wide, active) is the only card.
	rec := doGet(s, "/api/nous/campaigns")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	list := nousDecodeMap(t, rec)
	cards, _ := list["campaigns"].([]interface{})
	if len(cards) != 1 || list["target"] != float64(NousBaselineTarget) || list["targetExplainer"] != NousBaselineTargetExplainer {
		t.Fatalf("initial list = %v", list)
	}
	first := cards[0].(map[string]interface{})
	if first["id"] != float64(1) || first["status"] != "active" || first["scope"].(map[string]interface{})["hiveWide"] != true || first["snapshots"] != float64(history) {
		t.Fatalf("migrated card = %v", first)
	}

	// Owner-only writes.
	if rec := doPostNoOwner(s, "/api/nous/campaigns", map[string]interface{}{"question": "x"}); rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-owner create = %d", rec.Code)
	}
	if rec := doPatchNoOwner(s, "/api/nous/campaigns/1", map[string]interface{}{"action": "pause"}); rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-owner patch = %d", rec.Code)
	}
	if rec := doPost(s, "/api/nous/campaigns", map[string]interface{}{"question": "  "}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty question = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doPostRaw(s, "/api/nous/campaigns", "{nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}

	// Confound guard: campaign 1 is hive-wide, so anything else conflicts.
	rec = doPost(s, "/api/nous/campaigns", map[string]interface{}{"question": "Cut CI time", "scope": campaignScope("acme/api")})
	if rec.Code != http.StatusConflict {
		t.Fatalf("create next to hive-wide = %d: %s", rec.Code, rec.Body.String())
	}
	conflict := nousDecodeMap(t, rec)
	if conflict["code"] != "scope_conflict" || conflict["conflict"].(map[string]interface{})["id"] != float64(1) || !strings.Contains(conflict["error"].(string), "confound") {
		t.Fatalf("409 body = %v", conflict)
	}

	// Narrow campaign 1 to acme/web via PATCH edit, then the create works.
	rec = doPatchOwner(s, "/api/nous/campaigns/1", map[string]interface{}{"scope": campaignScope("acme/web"), "hypothesis": "fewer flakes"})
	if rec.Code != http.StatusOK {
		t.Fatalf("narrow = %d: %s", rec.Code, rec.Body.String())
	}
	if c := nousDecodeMap(t, rec)["campaign"].(map[string]interface{}); c["hypothesis"] != "fewer flakes" || c["scope"].(map[string]interface{})["hiveWide"] != false {
		t.Fatalf("narrowed card = %v", c)
	}
	rec = doPost(s, "/api/nous/campaigns", map[string]interface{}{"question": "Cut CI time", "hypothesis": "shard tests", "scope": map[string]interface{}{"repos": []string{"acme/api"}, "intervention": "ci", "tags": []string{"ci"}}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	created := nousDecodeMap(t, rec)["campaign"].(map[string]interface{})
	if created["id"] != float64(2) || created["status"] != "active" || created["snapshots"] != float64(0) || created["percent"] != float64(0) || created["scope"].(map[string]interface{})["intervention"] != "ci" {
		t.Fatalf("created card = %v", created)
	}

	// Both run at once with independent baselines; status lists both.
	st := nousStatusJSON(t, s)
	all, _ := st["campaigns"].([]interface{})
	if len(all) != 2 || all[0].(map[string]interface{})["snapshots"] != float64(history) || all[1].(map[string]interface{})["snapshots"] != float64(0) {
		t.Fatalf("status campaigns = %v", st["campaigns"])
	}
	if st["baselineTargetExplainer"] != NousBaselineTargetExplainer {
		t.Fatalf("baselineTargetExplainer = %v", st["baselineTargetExplainer"])
	}
	// The legacy single-campaign block is the oldest running campaign and
	// the old ledger row is attributed to it, not to the new campaign.
	if camp := nousBlock(t, st, "campaign"); camp["id"] != float64(1) || camp["experiments"] != float64(1) {
		t.Fatalf("legacy campaign block = %v", camp)
	}
	if all[1].(map[string]interface{})["experiments"] != float64(0) {
		t.Fatalf("new campaign must not inherit old experiments: %v", all[1])
	}
	exps, _ := st["experiments"].([]interface{})
	if len(exps) != 1 || exps[0].(map[string]interface{})["campaignId"] != float64(1) {
		t.Fatalf("experiments = %v", st["experiments"])
	}

	// Overlapping scope is refused; the shared repo is named.
	rec = doPost(s, "/api/nous/campaigns", map[string]interface{}{"question": "Third", "scope": campaignScope("ACME/API", "acme/docs")})
	if rec.Code != http.StatusConflict {
		t.Fatalf("overlap = %d: %s", rec.Code, rec.Body.String())
	}
	if conflict = nousDecodeMap(t, rec); conflict["conflict"].(map[string]interface{})["id"] != float64(2) || conflict["shared"].([]interface{})[0] != "ACME/API" {
		t.Fatalf("overlap 409 = %v", conflict)
	}

	// Pausing #2 frees acme/api; resuming it then conflicts with #3.
	rec = doPatchOwner(s, "/api/nous/campaigns/2", map[string]interface{}{"action": "pause"})
	if rec.Code != http.StatusOK || nousDecodeMap(t, rec)["campaign"].(map[string]interface{})["status"] != "paused" {
		t.Fatalf("pause = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doPost(s, "/api/nous/campaigns", map[string]interface{}{"question": "Third", "scope": campaignScope("acme/api")})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create after pause = %d: %s", rec.Code, rec.Body.String())
	}
	if rec = doPatchOwner(s, "/api/nous/campaigns/2", map[string]interface{}{"action": "resume"}); rec.Code != http.StatusConflict {
		t.Fatalf("resume into taken scope = %d: %s", rec.Code, rec.Body.String())
	}

	// Archive #3 (frozen outcome), then #2 resumes.
	now = now.Add(2 * time.Hour)
	rec = doPatchOwner(s, "/api/nous/campaigns/3", map[string]interface{}{"action": "archive"})
	if rec.Code != http.StatusOK {
		t.Fatalf("archive = %d: %s", rec.Code, rec.Body.String())
	}
	if c := nousDecodeMap(t, rec)["campaign"].(map[string]interface{}); c["status"] != "archived" || c["endedAt"] != now.Format(time.RFC3339) {
		t.Fatalf("archived card = %v", c)
	}
	if rec = doPatchOwner(s, "/api/nous/campaigns/2", map[string]interface{}{"action": "resume"}); rec.Code != http.StatusOK {
		t.Fatalf("resume after archive = %d: %s", rec.Code, rec.Body.String())
	}

	// Error paths.
	if rec = doPatchOwner(s, "/api/nous/campaigns/3", map[string]interface{}{"action": "pause"}); rec.Code != http.StatusConflict {
		t.Fatalf("pause archived = %d", rec.Code)
	}
	if rec = doPatchOwner(s, "/api/nous/campaigns/99", map[string]interface{}{"action": "pause"}); rec.Code != http.StatusNotFound {
		t.Fatalf("pause unknown = %d", rec.Code)
	}
	if rec = doPatchOwner(s, "/api/nous/campaigns/abc", map[string]interface{}{"action": "pause"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d", rec.Code)
	}
	if rec = doPatchOwner(s, "/api/nous/campaigns/1", map[string]interface{}{"action": "explode"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad action = %d", rec.Code)
	}
	if rec = doPatchOwner(s, "/api/nous/campaigns/1", map[string]interface{}{}); rec.Code != http.StatusOK {
		t.Fatalf("empty patch = %d: %s", rec.Code, rec.Body.String())
	}

	// Legacy goals PUT still swaps only the oldest running campaign (#1 →
	// #4, same scope) and leaves #2 running.
	rec = doPut(s, "/api/nous/config/goals", map[string]string{"repoGoal": "Brand new question"})
	if rec.Code != http.StatusOK || nousDecodeMap(t, rec)["newCampaign"] != true {
		t.Fatalf("goals = %d: %s", rec.Code, rec.Body.String())
	}
	st = nousStatusJSON(t, s)
	all, _ = st["campaigns"].([]interface{})
	statuses := map[float64]string{}
	for _, c := range all {
		m := c.(map[string]interface{})
		statuses[m["id"].(float64)] = m["status"].(string)
	}
	if statuses[1] != "archived" || statuses[2] != "active" || statuses[3] != "archived" || statuses[4] != "active" {
		t.Fatalf("statuses after goals = %v", statuses)
	}

	// Without a snapshot directory campaigns are disabled.
	_ = deps
	bare, bareDeps := apiServerWithNous(t)
	bareDeps.Nous.SnapshotDir = ""
	if rec := doGet(bare, "/api/nous/campaigns"); rec.Code != http.StatusConflict {
		t.Fatalf("list without store = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestNousCampaignsNotConfigured(t *testing.T) {
	s, _ := apiServer(t)
	s.deps.Nous = nil
	if rec := doGet(s, "/api/nous/campaigns"); rec.Code != http.StatusNotFound {
		t.Fatalf("list = %d", rec.Code)
	}
	if rec := doPost(s, "/api/nous/campaigns", map[string]interface{}{"question": "q"}); rec.Code != http.StatusNotFound {
		t.Fatalf("create = %d", rec.Code)
	}
	if rec := doPatchOwner(s, "/api/nous/campaigns/1", map[string]interface{}{"action": "pause"}); rec.Code != http.StatusNotFound {
		t.Fatalf("patch = %d", rec.Code)
	}
}

func TestNousCampaignForAssignsRowsByWindowAndRepo(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	open := []nous.Campaign{
		{ID: 1, StartedAt: t0, Scope: nous.Scope{Repos: []string{"acme/web"}}},
		{ID: 2, StartedAt: t0.Add(24 * time.Hour), Scope: nous.Scope{Repos: []string{"acme/api"}}},
		{ID: 3, StartedAt: t0.Add(48 * time.Hour)},
	}
	cases := []struct {
		row  nousExperimentProgress
		want int
	}{
		{nousExperimentProgress{StartedAt: t0.Add(time.Hour).Format(time.RFC3339), Repo: "acme/web"}, 1},
		{nousExperimentProgress{StartedAt: t0.Add(30 * time.Hour).Format(time.RFC3339), Repo: "ACME/API"}, 2},
		{nousExperimentProgress{StartedAt: t0.Add(30 * time.Hour).Format(time.RFC3339), Repo: "acme/other"}, 0},
		{nousExperimentProgress{StartedAt: t0.Add(72 * time.Hour).Format(time.RFC3339), Repo: "acme/other"}, 3},
		{nousExperimentProgress{StartedAt: t0.Add(72 * time.Hour).Format(time.RFC3339)}, 1},
		{nousExperimentProgress{}, 1},
		{nousExperimentProgress{StartedAt: t0.Add(-time.Hour).Format(time.RFC3339)}, 0},
	}
	for i, c := range cases {
		if got := nousCampaignFor(c.row, open); got != c.want {
			t.Errorf("case %d: campaign = %d, want %d", i, got, c.want)
		}
	}
}

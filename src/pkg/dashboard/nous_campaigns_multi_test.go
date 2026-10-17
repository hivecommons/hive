package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
// paused, narrowing the scope is a material edit that must be acknowledged
// with restart: true (the old version is archived, the new one starts at
// 0 / 672 as v2), disjoint scopes run side by side with their own baselines,
// and pause/resume/archive go through PATCH.
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

	// Narrowing campaign 1 to acme/web is a scope change: without
	// restart: true the edit is refused and nothing moves.
	rec = doPatchOwner(s, "/api/nous/campaigns/1", map[string]interface{}{"scope": campaignScope("acme/web"), "hypothesis": "fewer flakes"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("narrow without restart = %d: %s", rec.Code, rec.Body.String())
	}
	if body := nousDecodeMap(t, rec); body["code"] != "restart_required" || !strings.Contains(body["error"].(string), "restarts this experiment") || body["campaign"].(map[string]interface{})["id"] != float64(1) {
		t.Fatalf("restart_required body = %v", body)
	}
	if c, _ := deps.Nous.Campaigns().Get(1); c.Scope.HiveWide() != true || c.Hypothesis != "" {
		t.Fatalf("refused edit must leave campaign 1 untouched: %+v", c)
	}
	// Acknowledged: #1 is archived as v1 with its frozen outcome and #2 is
	// created as v2 with the edited fields and a fresh baseline.
	rec = doPatchOwner(s, "/api/nous/campaigns/1", map[string]interface{}{"question": "Fewer flaky tests on web", "scope": campaignScope("acme/web"), "hypothesis": "fewer flakes", "restart": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("narrow with restart = %d: %s", rec.Code, rec.Body.String())
	}
	restarted := nousDecodeMap(t, rec)
	if restarted["status"] != "restarted" {
		t.Fatalf("restart status = %v", restarted["status"])
	}
	if c := restarted["campaign"].(map[string]interface{}); c["id"] != float64(2) || c["version"] != float64(2) || c["restartedFrom"] != float64(1) || c["question"] != "Fewer flaky tests on web" || c["hypothesis"] != "fewer flakes" || c["scope"].(map[string]interface{})["hiveWide"] != false || c["snapshots"] != float64(0) || c["status"] != "active" {
		t.Fatalf("restarted card = %v", c)
	}
	if a := restarted["archived"].(map[string]interface{}); a["id"] != float64(1) || a["status"] != "archived" || a["version"] != float64(1) || a["snapshots"] != float64(history) || a["experiments"] != float64(1) {
		t.Fatalf("archived v1 = %v", a)
	}
	rec = doPost(s, "/api/nous/campaigns", map[string]interface{}{"question": "Cut CI time", "hypothesis": "shard tests", "scope": map[string]interface{}{"repos": []string{"acme/api"}, "intervention": "ci", "tags": []string{"ci"}}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	created := nousDecodeMap(t, rec)["campaign"].(map[string]interface{})
	if created["id"] != float64(3) || created["status"] != "active" || created["snapshots"] != float64(0) || created["percent"] != float64(0) || created["scope"].(map[string]interface{})["intervention"] != "ci" || created["version"] != float64(1) {
		t.Fatalf("created card = %v", created)
	}

	// Both run at once with independent baselines; status lists all three
	// (archived v1 keeps the history count).
	st := nousStatusJSON(t, s)
	all, _ := st["campaigns"].([]interface{})
	if len(all) != 3 || all[0].(map[string]interface{})["snapshots"] != float64(history) || all[1].(map[string]interface{})["snapshots"] != float64(0) || all[2].(map[string]interface{})["snapshots"] != float64(0) {
		t.Fatalf("status campaigns = %v", st["campaigns"])
	}
	if st["baselineTargetExplainer"] != NousBaselineTargetExplainer {
		t.Fatalf("baselineTargetExplainer = %v", st["baselineTargetExplainer"])
	}
	// The legacy single-campaign block is the oldest running campaign (#2)
	// and the old ledger row stays with archived v1, not with any new one.
	if camp := nousBlock(t, st, "campaign"); camp["id"] != float64(2) || camp["experiments"] != float64(0) {
		t.Fatalf("legacy campaign block = %v", camp)
	}
	if all[2].(map[string]interface{})["experiments"] != float64(0) {
		t.Fatalf("new campaign must not inherit old experiments: %v", all[2])
	}
	// The old ledger row predates every open campaign, so it is dropped
	// from the live experiment rows (it stays in archived v1's count).
	if exps, _ := st["experiments"].([]interface{}); len(exps) != 0 {
		t.Fatalf("experiments = %v", st["experiments"])
	}
	// A hypothesis-only edit is saved in place: same id, baseline kept.
	rec = doPatchOwner(s, "/api/nous/campaigns/3", map[string]interface{}{"hypothesis": "shard the slowest package", "scope": map[string]interface{}{"repos": []string{"ACME/API"}, "tags": []string{"ci", "speed"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("in-place edit = %d: %s", rec.Code, rec.Body.String())
	}
	if body := nousDecodeMap(t, rec); body["status"] != "updated" || body["campaign"].(map[string]interface{})["id"] != float64(3) || body["campaign"].(map[string]interface{})["hypothesis"] != "shard the slowest package" || body["campaign"].(map[string]interface{})["version"] != float64(1) {
		t.Fatalf("in-place edit body = %v", body)
	}

	// Overlapping scope is refused; the shared repo is named.
	rec = doPost(s, "/api/nous/campaigns", map[string]interface{}{"question": "Third", "scope": campaignScope("ACME/API", "acme/docs")})
	if rec.Code != http.StatusConflict {
		t.Fatalf("overlap = %d: %s", rec.Code, rec.Body.String())
	}
	if conflict = nousDecodeMap(t, rec); conflict["conflict"].(map[string]interface{})["id"] != float64(3) || conflict["shared"].([]interface{})[0] != "ACME/API" {
		t.Fatalf("overlap 409 = %v", conflict)
	}
	// A restart whose new scope collides with a running experiment is
	// refused with scope_conflict and leaves the store untouched.
	rec = doPatchOwner(s, "/api/nous/campaigns/2", map[string]interface{}{"scope": campaignScope("acme/api"), "restart": true})
	if rec.Code != http.StatusConflict || nousDecodeMap(t, rec)["code"] != "scope_conflict" {
		t.Fatalf("restart into taken scope = %d: %s", rec.Code, rec.Body.String())
	}
	if c, ok := deps.Nous.Campaigns().Get(2); !ok || !c.Running() || len(deps.Nous.Campaigns().All()) != 3 {
		t.Fatalf("refused restart must leave #2 running and create nothing: %+v", c)
	}

	// Pausing #3 frees acme/api; resuming it then conflicts with #4.
	rec = doPatchOwner(s, "/api/nous/campaigns/3", map[string]interface{}{"action": "pause"})
	if rec.Code != http.StatusOK || nousDecodeMap(t, rec)["campaign"].(map[string]interface{})["status"] != "paused" {
		t.Fatalf("pause = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doPost(s, "/api/nous/campaigns", map[string]interface{}{"question": "Third", "scope": campaignScope("acme/api")})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create after pause = %d: %s", rec.Code, rec.Body.String())
	}
	if rec = doPatchOwner(s, "/api/nous/campaigns/3", map[string]interface{}{"action": "resume"}); rec.Code != http.StatusConflict {
		t.Fatalf("resume into taken scope = %d: %s", rec.Code, rec.Body.String())
	}

	// Archive #4 (frozen outcome), then #3 resumes.
	now = now.Add(2 * time.Hour)
	rec = doPatchOwner(s, "/api/nous/campaigns/4", map[string]interface{}{"action": "archive"})
	if rec.Code != http.StatusOK {
		t.Fatalf("archive = %d: %s", rec.Code, rec.Body.String())
	}
	if c := nousDecodeMap(t, rec)["campaign"].(map[string]interface{}); c["status"] != "archived" || c["endedAt"] != now.Format(time.RFC3339) {
		t.Fatalf("archived card = %v", c)
	}
	if rec = doPatchOwner(s, "/api/nous/campaigns/3", map[string]interface{}{"action": "resume"}); rec.Code != http.StatusOK {
		t.Fatalf("resume after archive = %d: %s", rec.Code, rec.Body.String())
	}

	// Error paths.
	if rec = doPatchOwner(s, "/api/nous/campaigns/4", map[string]interface{}{"action": "pause"}); rec.Code != http.StatusConflict {
		t.Fatalf("pause archived = %d", rec.Code)
	}
	if rec = doPatchOwner(s, "/api/nous/campaigns/4", map[string]interface{}{"question": "edited after archive", "restart": true}); rec.Code != http.StatusConflict {
		t.Fatalf("edit archived = %d: %s", rec.Code, rec.Body.String())
	}
	if rec = doPatchOwner(s, "/api/nous/campaigns/99", map[string]interface{}{"action": "pause"}); rec.Code != http.StatusNotFound {
		t.Fatalf("pause unknown = %d", rec.Code)
	}
	if rec = doPatchOwner(s, "/api/nous/campaigns/abc", map[string]interface{}{"action": "pause"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d", rec.Code)
	}
	if rec = doPatchOwner(s, "/api/nous/campaigns/2", map[string]interface{}{"action": "explode"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad action = %d", rec.Code)
	}
	if rec = doPatchOwner(s, "/api/nous/campaigns/2", map[string]interface{}{}); rec.Code != http.StatusOK {
		t.Fatalf("empty patch = %d: %s", rec.Code, rec.Body.String())
	}

	// Legacy goals PUT (no longer called by the dashboard) still swaps only
	// the oldest running campaign (#2 → #5, same scope) and leaves #3
	// running.
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
	if statuses[1] != "archived" || statuses[2] != "archived" || statuses[3] != "active" || statuses[4] != "archived" || statuses[5] != "active" {
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

// A campaign card must always say where it is and what happens next
// (#12532, kellya's "no defined experiments yet?" report): a question with a
// complete baseline is "ready" and, in observe mode, carries the single
// Generate proposals action; after the operator presses it the status
// reports the live mode (not the boot-time one) and the card waits on the
// experiment loop without asking anything more of the operator.
func TestNousCampaignStageLifecycle(t *testing.T) {
	unit := nousCampaignView{ID: 7, Status: nous.StatusActive, Snapshots: 3, Target: NousBaselineTarget}
	nousCampaignStage(&unit, "observe", nil, false)
	if unit.Stage != NousStageBaseline || unit.StageLabel != "collecting baseline 3 / 672" || unit.NextAction != "" {
		t.Fatalf("baseline stage = %+v", unit)
	}
	unit.Complete = true
	nousCampaignStage(&unit, "observe", nil, false)
	if unit.Stage != NousStageReady || unit.NextAction != NousActionGenerateProposals || !strings.Contains(unit.StageDetail, "Observe mode") {
		t.Fatalf("ready/observe stage = %+v", unit)
	}
	nousCampaignStage(&unit, "suggest", nil, false)
	if unit.Stage != NousStageReady || unit.NextAction != "" || !strings.Contains(unit.StageDetail, "Suggest mode") {
		t.Fatalf("ready/suggest stage = %+v", unit)
	}
	// #12541: with the experiment loop known idle, the waiting ready stage
	// says so instead of promising proposals; observe mode still offers
	// Generate proposals, and a proposal from the loop wins over idleness.
	nousCampaignStage(&unit, "suggest", nil, true)
	if unit.Stage != NousStageReady || unit.StageLabel != "ready — the experiment loop is not running on this hive" || !unit.ExperimentLoopIdle || unit.NextAction != "" || !strings.Contains(unit.StageDetail, "designs no proposals itself") {
		t.Fatalf("ready/suggest/idle stage = %+v", unit)
	}
	nousCampaignStage(&unit, "observe", nil, true)
	if unit.NextAction != NousActionGenerateProposals || unit.ExperimentLoopIdle {
		t.Fatalf("ready/observe/idle stage = %+v", unit)
	}
	rows := []nousExperimentProgress{{CampaignID: 7, Status: "pending"}}
	nousCampaignStage(&unit, "suggest", rows, true)
	if unit.Stage != NousStageProposed || unit.ExperimentLoopIdle {
		t.Fatalf("proposed/idle stage = %+v", unit)
	}
	nousCampaignStage(&unit, "suggest", rows, false)
	if unit.Stage != NousStageProposed || unit.StageLabel != "proposals awaiting your review" {
		t.Fatalf("proposed stage = %+v", unit)
	}
	rows = []nousExperimentProgress{{CampaignID: 7, Status: "active", Phase: "experiment"}, {CampaignID: 7, Status: "pending"}}
	nousCampaignStage(&unit, "suggest", rows, false)
	if unit.Stage != NousStageRunning {
		t.Fatalf("running beats proposed: %+v", unit)
	}
	rows = []nousExperimentProgress{{CampaignID: 7, Status: "judged", Phase: "judged"}, {CampaignID: 8, Status: "active"}}
	nousCampaignStage(&unit, "suggest", rows, false)
	if unit.Stage != NousStageConcluded || !strings.Contains(unit.StageDetail, "1 experiment(s) judged") {
		t.Fatalf("concluded ignores other campaigns' rows: %+v", unit)
	}
	unit.Status = nous.StatusPaused
	nousCampaignStage(&unit, "observe", rows, false)
	if unit.Stage != NousStagePaused || unit.NextAction != "" {
		t.Fatalf("paused stage = %+v", unit)
	}
	unit.Status = nous.StatusArchived
	nousCampaignStage(&unit, "observe", nil, false)
	if unit.Stage != NousStageConcluded || unit.StageLabel != "concluded" {
		t.Fatalf("archived stage = %+v", unit)
	}
	nousCampaignStage(nil, "observe", nil, false)

	// End to end: the screenshot scenario — a question, baseline past 672,
	// zero principles, lab booted in observe.
	s, deps, _ := nousCampaignServer(t, NousBaselineTarget+200)
	deps.Nous.Mode = "observe"
	deps.Nous.Status["mode"] = "observe"
	deps.Nous.Principles = nil
	deps.Nous.Ledger = nil
	status := nousStatusJSON(t, s)
	cards, _ := status["campaigns"].([]interface{})
	if len(cards) != 1 {
		t.Fatalf("campaigns = %v", status["campaigns"])
	}
	card := cards[0].(map[string]interface{})
	if card["stage"] != NousStageReady || card["nextAction"] != NousActionGenerateProposals || card["snapshots"].(float64) < float64(NousBaselineTarget) || card["complete"] != true {
		t.Fatalf("ready card = %v", card)
	}
	if status["principlesExplainer"] != NousPrinciplesExplainer || status["principleCount"] != nil && status["principleCount"] != float64(0) {
		t.Fatalf("principles explainer missing: %v %v", status["principlesExplainer"], status["principleCount"])
	}
	// Generate proposals = PUT mode suggest; the status must report it live.
	if rec := doPutOwner(s, "/api/nous/mode", map[string]interface{}{"mode": "suggest"}); rec.Code != http.StatusOK {
		t.Fatalf("set mode = %d: %s", rec.Code, rec.Body.String())
	}
	status = nousStatusJSON(t, s)
	if status["mode"] != "suggest" {
		t.Fatalf("status.mode still %v after PUT /api/nous/mode (stale boot value)", status["mode"])
	}
	card = status["campaigns"].([]interface{})[0].(map[string]interface{})
	if card["stage"] != NousStageReady || card["nextAction"] != nil || !strings.Contains(card["stageDetail"].(string), "experiment loop") {
		t.Fatalf("card after switching to suggest = %v", card)
	}
}

// TestNousExperimentLoopIdle pins hivecommons/hive#12541: the hive designs
// no proposals itself, so GET /api/nous/status reports whether the external
// experiment loop has written under the governor directory recently, and a
// ready card in suggest mode says the loop is not running instead of
// waiting forever.
func TestNousExperimentLoopIdle(t *testing.T) {
	s, deps, _ := nousCampaignServer(t, NousBaselineTarget+10)
	deps.Nous.Mode = "suggest"
	deps.Nous.Ledger = nil
	govDir := t.TempDir()
	deps.Nous.GovernorDir = govDir

	readyCard := func(status map[string]interface{}) map[string]interface{} {
		t.Helper()
		cards, _ := status["campaigns"].([]interface{})
		if len(cards) != 1 {
			t.Fatalf("campaigns = %v", status["campaigns"])
		}
		return cards[0].(map[string]interface{})
	}

	// Nothing ever written: loop not running, card says so.
	status := nousStatusJSON(t, s)
	loop := nousBlock(t, status, "experimentLoop")
	if loop["running"] != false || loop["dir"] != govDir || loop["lastWriteAt"] != nil || loop["idleAfterSeconds"].(float64) <= 0 {
		t.Fatalf("experimentLoop with empty dir = %v", loop)
	}
	card := readyCard(status)
	if card["stage"] != NousStageReady || card["stageLabel"] != "ready — the experiment loop is not running on this hive" || card["experimentLoopIdle"] != true {
		t.Fatalf("idle card = %v", card)
	}

	// A stale write (older than NousLoopIdleCycles cycles) is still idle.
	ledger := filepath.Join(govDir, "ledger.json")
	if err := os.WriteFile(ledger, []byte(`{"iterations":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(ledger, old, old); err != nil {
		t.Fatal(err)
	}
	if got := deps.Nous.ExperimentLoopLastWrite(); got.Sub(old.UTC()).Abs() > time.Second {
		t.Fatalf("ExperimentLoopLastWrite = %v, want %v", got, old)
	}
	status = nousStatusJSON(t, s)
	if loop = nousBlock(t, status, "experimentLoop"); loop["running"] != false || loop["lastWriteAt"] == nil {
		t.Fatalf("experimentLoop with stale write = %v", loop)
	}
	if card = readyCard(status); card["experimentLoopIdle"] != true {
		t.Fatalf("card with stale write = %v", card)
	}

	// A fresh write: running, the card waits honestly on the loop.
	now := time.Now()
	if err := os.Chtimes(ledger, now, now); err != nil {
		t.Fatal(err)
	}
	status = nousStatusJSON(t, s)
	if loop = nousBlock(t, status, "experimentLoop"); loop["running"] != true {
		t.Fatalf("experimentLoop with fresh write = %v", loop)
	}
	card = readyCard(status)
	if card["stageLabel"] != "ready — generating proposals" || card["experimentLoopIdle"] != nil {
		t.Fatalf("card with running loop = %v", card)
	}

	// Without a governor directory the loop cannot be observed: no block,
	// no idle claim.
	deps.Nous.GovernorDir = ""
	status = nousStatusJSON(t, s)
	if _, ok := status["experimentLoop"]; ok {
		t.Fatalf("experimentLoop reported without a governor dir: %v", status["experimentLoop"])
	}
	if card = readyCard(status); card["experimentLoopIdle"] != nil {
		t.Fatalf("card claims idle loop without a governor dir: %v", card)
	}
	if got := (*NousState)(nil).ExperimentLoopLastWrite(); !got.IsZero() {
		t.Fatalf("nil state last write = %v", got)
	}
}

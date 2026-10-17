package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hivestate"
)

// writeNousSnapshots writes n raw hive-state snapshots into dir, one per
// minute starting at start.
func writeNousSnapshots(t *testing.T, dir string, start time.Time, n int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		data, _ := json.Marshal(hivestate.Snapshot{Timestamp: at.Format(time.RFC3339), QueueIssues: i})
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", at.UnixMilli())), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func nousCampaignServer(t *testing.T, snapshots int) (*Server, *Dependencies, string) {
	t.Helper()
	s, deps := apiServerWithNous(t)
	root := t.TempDir()
	snapDir := filepath.Join(root, "snapshots")
	first := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	writeNousSnapshots(t, snapDir, first, snapshots)
	deps.Nous.SnapshotDir = snapDir
	deps.Nous.Config = map[string]interface{}{}
	deps.Nous.Status = map[string]interface{}{"snapshotCount": snapshots, "snapshots": snapshots}
	deps.Nous.Ledger = []map[string]interface{}{{
		"id": "exp-old", "type": "completed", "verdict": "improved",
		"started_at": first.Add(10 * time.Minute).Format(time.RFC3339),
	}}
	return s, deps, root
}

func nousStatusJSON(t *testing.T, s *Server) map[string]interface{} {
	t.Helper()
	rec := doGet(s, "/api/nous/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func nousBlock(t *testing.T, status map[string]interface{}, key string) map[string]interface{} {
	t.Helper()
	m, ok := status[key].(map[string]interface{})
	if !ok {
		t.Fatalf("%s block missing: %v", key, status[key])
	}
	return m
}

// TestNousNewResearchQuestionRestartsBaseline is the kellya report (#12501):
// a hive with 2688 snapshots of history must show 0 / 672 right after a new
// research question, while the history stays visible as "observation".
func TestNousNewResearchQuestionRestartsBaseline(t *testing.T) {
	const history = 40
	s, deps, root := nousCampaignServer(t, history)
	now := time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)
	nousNow = func() time.Time { return now }
	t.Cleanup(func() { nousNow = time.Now })

	// Migration: existing history is campaign 1, baseline counts all of it.
	st := nousStatusJSON(t, s)
	camp := nousBlock(t, st, "campaign")
	base := nousBlock(t, st, "baseline")
	if camp["id"] != float64(1) || camp["status"] != "active" || base["snapshots"] != float64(history) || camp["snapshots"] != float64(history) {
		t.Fatalf("migrated campaign=%v baseline=%v", camp, base)
	}
	if _, err := os.Stat(filepath.Join(root, "campaigns.json")); err != nil {
		t.Fatalf("campaign file not created next to snapshots: %v", err)
	}
	if exps, _ := st["experiments"].([]interface{}); len(exps) != 1 {
		t.Fatalf("campaign 1 experiments = %v", st["experiments"])
	}

	// A research question arrives on the questionless history campaign.
	rec := doPut(s, "/api/nous/config/goals", map[string]string{"repoGoal": "Can we cut CI time by 20%?"})
	if rec.Code != http.StatusOK {
		t.Fatalf("put goals = %d: %s", rec.Code, rec.Body.String())
	}
	var put map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &put)
	if put["newCampaign"] != true {
		t.Fatalf("goals response = %v, want newCampaign true", put)
	}

	st = nousStatusJSON(t, s)
	camp = nousBlock(t, st, "campaign")
	base = nousBlock(t, st, "baseline")
	obs := nousBlock(t, st, "observation")
	if camp["id"] != float64(2) || camp["question"] != "Can we cut CI time by 20%?" || camp["startedAt"] != now.Format(time.RFC3339) {
		t.Fatalf("active campaign = %v", camp)
	}
	if base["snapshots"] != float64(0) || base["percent"] != float64(0) || base["complete"] != false || base["target"] != float64(NousBaselineTarget) {
		t.Fatalf("baseline after new question = %v, want 0 / %d", base, NousBaselineTarget)
	}
	if st["baseline_pct"] != float64(0) || st["snapshotCount"] != float64(history) {
		t.Fatalf("baseline_pct=%v snapshotCount=%v", st["baseline_pct"], st["snapshotCount"])
	}
	if obs["snapshots"] != float64(history) || obs["since"] == "" {
		t.Fatalf("observation = %v", obs)
	}
	if st["researchQuestion"] != "Can we cut CI time by 20%?" {
		t.Fatalf("researchQuestion = %v", st["researchQuestion"])
	}
	// The old experiment belongs to the archived campaign.
	if exps, _ := st["experiments"].([]interface{}); len(exps) != 0 {
		t.Fatalf("campaign 2 experiments = %v, want none", exps)
	}
	// status.campaigns lists every campaign in ID order (#12532): the
	// archived #1 and the running #2.
	all, _ := st["campaigns"].([]interface{})
	if len(all) != 2 || all[1].(map[string]interface{})["status"] != "active" {
		t.Fatalf("campaigns = %v", st["campaigns"])
	}
	prev := all[0].(map[string]interface{})
	if prev["id"] != float64(1) || prev["status"] != "archived" || prev["snapshots"] != float64(history) || prev["experiments"] != float64(1) || prev["endedAt"] == "" {
		t.Fatalf("archived campaign 1 = %v", prev)
	}
	if results, _ := prev["results"].([]interface{}); len(results) != 1 {
		t.Fatalf("archived results = %v", prev["results"])
	}

	// Snapshots recorded after the new question count toward its baseline.
	writeNousSnapshots(t, deps.Nous.SnapshotDir, now.Add(time.Minute), 3)
	st = nousStatusJSON(t, s)
	if base = nousBlock(t, st, "baseline"); base["snapshots"] != float64(3) {
		t.Fatalf("baseline after 3 new snapshots = %v", base)
	}

	// Re-saving the same question (case/whitespace differences) is not a
	// new experiment — the edited text is kept in place; clearing it is
	// not a new experiment either.
	for _, same := range []string{"  can we cut  CI time by 20%?  ", ""} {
		rec = doPut(s, "/api/nous/config/goals", map[string]string{"repoGoal": same})
		if rec.Code != http.StatusOK {
			t.Fatalf("put goals(%q) = %d", same, rec.Code)
		}
		if c, _ := deps.Nous.ActiveCampaign(); c.ID != 2 {
			t.Fatalf("question %q rotated the campaign to %d", same, c.ID)
		}
	}

	// Explicit new experiment with the same question: archive and restart.
	now = now.Add(time.Hour)
	rec = doPost(s, "/api/nous/campaign/start", map[string]string{})
	if rec.Code != http.StatusOK {
		t.Fatalf("campaign start = %d: %s", rec.Code, rec.Body.String())
	}
	st = nousStatusJSON(t, s)
	camp = nousBlock(t, st, "campaign")
	if camp["id"] != float64(3) || camp["question"] != "can we cut CI time by 20%?" || camp["snapshots"] != float64(0) {
		t.Fatalf("campaign after explicit start = %v", camp)
	}
	if all, _ = st["campaigns"].([]interface{}); len(all) != 3 || all[1].(map[string]interface{})["status"] != "archived" || all[2].(map[string]interface{})["id"] != float64(3) {
		t.Fatalf("campaigns after explicit start = %v", st["campaigns"])
	}

	// Config GET exposes the active campaign and the persisted question.
	deps.Nous.Config = map[string]interface{}{}
	rec = doGet(s, "/api/nous/config")
	var cfg map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &cfg)
	if ac, _ := cfg["activeCampaign"].(map[string]interface{}); ac["id"] != float64(3) {
		t.Fatalf("config activeCampaign = %v", cfg["activeCampaign"])
	}
	if nousResearchQuestion(cfg["goals"]) != "can we cut CI time by 20%?" {
		t.Fatalf("config goals fallback = %v", cfg["goals"])
	}
}

func TestNousFirstQuestionOnEmptyHiveDoesNotRotate(t *testing.T) {
	s, deps, _ := nousCampaignServer(t, 0)
	rec := doPut(s, "/api/nous/config/goals", map[string]string{"repoGoal": "first question"})
	if rec.Code != http.StatusOK {
		t.Fatalf("put goals = %d", rec.Code)
	}
	c, ok := deps.Nous.ActiveCampaign()
	if !ok || c.ID != 1 || c.Question != "first question" {
		t.Fatalf("campaign = %+v ok=%v, want question set on campaign 1", c, ok)
	}
	if deps.Nous.Campaigns().Archived() != nil {
		t.Fatal("nothing should be archived on an empty hive")
	}
}

func TestNousCampaignEndpointsWithoutStore(t *testing.T) {
	s, deps := apiServerWithNous(t)
	deps.Nous.SnapshotDir = ""
	// Goals still work; no campaign block is published.
	rec := doPut(s, "/api/nous/config/goals", map[string]string{"repoGoal": "q"})
	if rec.Code != http.StatusOK {
		t.Fatalf("put goals = %d", rec.Code)
	}
	st := nousStatusJSON(t, s)
	if _, ok := st["campaign"]; ok {
		t.Fatalf("campaign block published without a store: %v", st["campaign"])
	}
	if rec := doPost(s, "/api/nous/campaign/start", map[string]string{}); rec.Code != http.StatusConflict {
		t.Fatalf("campaign start without store = %d, want 409", rec.Code)
	}
	if rec := doPostNoOwner(s, "/api/nous/campaign/start", map[string]string{}); rec.Code != http.StatusForbidden {
		t.Fatalf("campaign start without owner = %d, want 403", rec.Code)
	}
	deps.Nous = nil
	if rec := doPost(s, "/api/nous/campaign/start", map[string]string{}); rec.Code != http.StatusNotFound {
		t.Fatalf("campaign start without nous = %d, want 404", rec.Code)
	}
	if rec := doPut(s, "/api/nous/config/goals", map[string]string{"repoGoal": "q"}); rec.Code != http.StatusNotFound {
		t.Fatalf("goals without nous = %d, want 404", rec.Code)
	}
}

func TestNousRefreshStatusScopesBaselinePctToCampaign(t *testing.T) {
	_, deps, _ := nousCampaignServer(t, 5)
	ns := deps.Nous
	ns.Phase = "collecting"
	ns.refreshStatus()
	if ns.Status["baseline_pct"] != NousBaselinePct(5) || ns.Status["snapshotCount"] != 5 || ns.Phase != "observing" {
		t.Fatalf("status before rotation = %v phase=%s", ns.Status, ns.Phase)
	}
	if _, err := ns.StartCampaign("fresh", nil); err != nil {
		t.Fatal(err)
	}
	ns.refreshStatus()
	if ns.Status["baseline_pct"] != float64(0) || ns.Status["snapshotCount"] != 5 {
		t.Fatalf("status after rotation = %v", ns.Status)
	}
}

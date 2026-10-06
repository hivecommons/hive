package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/mergelane"
)

func useMergeLaneFixtures(t *testing.T, lanes []mergelane.Record, rules github.BranchRulesResult) *int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fronts.json")
	if lanes != nil {
		data, err := json.Marshal(struct {
			Version int                `json:"version"`
			Lanes   []mergelane.Record `json:"lanes"`
		}{1, lanes})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	origPath, origRules := mergeLaneStorePath, mergeLaneBranchRules
	mergeLaneStorePath = path
	mergeLaneBranchRules = func(_ context.Context, _ *Server, _, _ string) github.BranchRulesResult {
		calls++
		return rules
	}
	t.Cleanup(func() { mergeLaneStorePath, mergeLaneBranchRules = origPath, origRules })
	return &calls
}

func getMergeLane(t *testing.T, srv *Server, repo string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/repos/merge-lane?repo="+repo, nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET merge-lane = %d %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %q: %v", w.Body.String(), err)
	}
	return out
}

// AC34: a hive-serialized repo with three eligible pull requests shows the
// front PR and its stage, the other two in order, the last leave-front reason
// and the enforcement state; not server-enforced carries the recommendation.
func TestRepoMergeLaneViewShowsFrontWaitingAndEnforcement(t *testing.T) {
	srv := newFullServer(t)
	if changed, err := srv.deps.Config.SetRepoMergeStrategyForRepoAndSave("testrepo", config.MergeStrategyHiveSerialized); !changed {
		t.Fatalf("setting hive-serialized: %v", err)
	}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	exitReason := "required check \"build\" finished as failure on head abc1234"
	holdReason := "waiting for required checks on head def5678: build (in_progress)"
	calls := useMergeLaneFixtures(t, []mergelane.Record{{
		Repo:   "testorg/testrepo",
		Branch: "main",
		Epoch:  4,
		Front: &mergelane.Front{
			PR: 11, Epoch: 4, Stage: mergelane.StageWaitingChecks,
			EnteredAt: now, DeadlineAt: now.Add(time.Hour), LastReason: holdReason,
		},
		Waiting: []mergelane.Waiter{
			{PR: 12, EligibleAt: now.Add(time.Minute), LastSeen: now},
			{PR: 13, EligibleAt: now.Add(2 * time.Minute), LastSeen: now},
		},
		LastExit: &mergelane.Exit{PR: 10, Reason: exitReason, At: now},
	}}, github.BranchRulesResult{Known: true, Required: map[string]bool{"build": true}, UpToDate: github.UpToDateHiveChecked, MergeQueueKnown: true})

	out := getMergeLane(t, srv, "testrepo")
	if out["merge_strategy"] != config.MergeStrategyHiveSerialized {
		t.Fatalf("merge_strategy = %v", out["merge_strategy"])
	}
	lanes, _ := out["lanes"].([]any)
	if len(lanes) != 1 {
		t.Fatalf("lanes = %v, want one lane", out["lanes"])
	}
	lane := lanes[0].(map[string]any)
	if lane["branch"] != "main" {
		t.Errorf("branch = %v", lane["branch"])
	}
	front, _ := lane["front"].(map[string]any)
	if front["pr"] != float64(11) || front["stage"] != mergelane.StageWaitingChecks || front["stage_label"] != "waiting for checks" || front["reason"] != holdReason {
		t.Errorf("front = %v", front)
	}
	waiting, _ := lane["waiting"].([]any)
	if len(waiting) != 2 {
		t.Fatalf("waiting = %v, want two", lane["waiting"])
	}
	for i, want := range []float64{12, 13} {
		w := waiting[i].(map[string]any)
		if w["pr"] != want || w["position"] != float64(i+1) || w["reason"] != mergelane.ReasonNotAtFront {
			t.Errorf("waiting[%d] = %v, want PR %v at position %d", i, w, want, i+1)
		}
	}
	last, _ := lane["last_exit"].(map[string]any)
	if last["pr"] != float64(10) || last["reason"] != exitReason {
		t.Errorf("last_exit = %v", last)
	}
	if lane["enforcement"] != string(github.UpToDateHiveChecked) || lane["enforcement_label"] != "Hive-checked" {
		t.Errorf("enforcement = %v / %v", lane["enforcement"], lane["enforcement_label"])
	}
	if lane["recommend_up_to_date_rule"] != true || !strings.Contains(lane["recommendation"].(string), "one CI run per merge") {
		t.Errorf("recommendation missing: %v / %v", lane["recommend_up_to_date_rule"], lane["recommendation"])
	}
	reasons, _ := json.Marshal(lane["reasons"])
	for _, want := range []string{holdReason, mergelane.ReasonNotAtFront, exitReason} {
		if !strings.Contains(string(reasons), want) {
			t.Errorf("reasons %s do not include %q", reasons, want)
		}
	}
	if *calls != 1 {
		t.Errorf("branch rules read %d times, want once per lane", *calls)
	}
}

func TestRepoMergeLaneViewServerEnforcedHasNoRecommendation(t *testing.T) {
	srv := newFullServer(t)
	if changed, err := srv.deps.Config.SetRepoMergeStrategyForRepoAndSave("testrepo", config.MergeStrategyHiveSerialized); !changed {
		t.Fatalf("setting hive-serialized: %v", err)
	}
	useMergeLaneFixtures(t, []mergelane.Record{{Repo: "testorg/testrepo", Branch: "main"}},
		github.BranchRulesResult{Known: true, Required: map[string]bool{"build": true}, UpToDate: github.UpToDateServerEnforced, MergeQueueKnown: true})

	lane := getMergeLane(t, srv, "testrepo")["lanes"].([]any)[0].(map[string]any)
	if lane["enforcement_label"] != "server-enforced" || lane["recommend_up_to_date_rule"] != false {
		t.Errorf("lane = %v, want server-enforced without a recommendation", lane)
	}
	if _, ok := lane["front"]; ok {
		t.Errorf("empty lane reports a front: %v", lane["front"])
	}
}

// A direct repo gets its strategy and no lane payload, and the view makes no
// GitHub call for it.
func TestRepoMergeLaneViewDirectRepoHasNoLane(t *testing.T) {
	srv := newFullServer(t)
	calls := useMergeLaneFixtures(t, []mergelane.Record{{
		Repo: "testorg/testrepo", Branch: "main",
		Front: &mergelane.Front{PR: 1, Stage: mergelane.StageValidating},
	}}, github.BranchRulesResult{UpToDate: github.UpToDateUnknown})

	out := getMergeLane(t, srv, "testrepo")
	if out["merge_strategy"] != config.MergeStrategyDirect {
		t.Fatalf("merge_strategy = %v, want direct", out["merge_strategy"])
	}
	if _, ok := out["lanes"]; ok {
		t.Errorf("direct repo carries a lane payload: %v", out)
	}
	if *calls != 0 {
		t.Errorf("direct repo read branch rules %d times, want none", *calls)
	}
}

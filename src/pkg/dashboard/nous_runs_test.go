package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestNousApproveAdmitsSpektacularCampaignRun(t *testing.T) {
	s, deps := runsTestServer(t)
	deps.Config.Runs.Spektacular.Enabled = true
	disableSpekHubExecutorForRelayTests(s)
	deps.Nous = &NousState{
		Status: map[string]interface{}{
			"pending": map[string]interface{}{
				"target":     "myorg/repo1#8737",
				"hypothesis": "Campaign should walk spec plan implement",
			},
		},
		Config: map[string]interface{}{
			"output": map[string]interface{}{"mode": nousOutputModeSpektacularRun},
		},
	}

	rec := doPost(s, "/api/nous/approve", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d body=%s", rec.Code, rec.Body.String())
	}
	stages, err := s.RunStageAccessor().PendingRunStages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 1 || stages[0].Repo != "myorg/repo1" || stages[0].Stage != StageSpec {
		t.Fatalf("pending stages = %+v", stages)
	}
	pending := deps.Nous.Status["pending"].(map[string]interface{})
	if pending["run_key"] != "myorg/repo1#8737" {
		t.Fatalf("pending run linkage = %+v", pending)
	}
	if run, ok := deps.Nous.Status["run"].(map[string]string); !ok || run["stage"] != StageSpec {
		t.Fatalf("status run linkage = %#v", deps.Nous.Status["run"])
	}
}

func TestNousApproveSpektacularCampaignRequiresTarget(t *testing.T) {
	s, deps := runsTestServer(t)
	deps.Config.Runs.Spektacular.Enabled = true
	disableSpekHubExecutorForRelayTests(s)
	deps.Nous = &NousState{
		Status: map[string]interface{}{"pending": map[string]interface{}{"hypothesis": "missing target"}},
		Config: map[string]interface{}{
			"output": map[string]interface{}{"mode": nousOutputModeSpektacularRun},
		},
	}

	rec := doPost(s, "/api/nous/approve", map[string]interface{}{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("approve: %d body=%s", rec.Code, rec.Body.String())
	}
	if got := pendingRunStages(t, s); got != 0 {
		t.Fatalf("pending stages = %d, want 0", got)
	}
}

func nousSpektacularRunTestServer(t *testing.T, pending map[string]interface{}) (*Server, *Dependencies) {
	t.Helper()
	s, deps := runsTestServer(t)
	deps.Config.Runs.Spektacular.Enabled = true
	disableSpekHubExecutorForRelayTests(s)
	deps.Nous = &NousState{
		Status: map[string]interface{}{"pending": pending},
		Config: map[string]interface{}{
			"output": map[string]interface{}{"mode": nousOutputModeSpektacularRun},
		},
	}
	return s, deps
}

func liveRunLeases(s *Server, now time.Time) int {
	s.contributeHub.leaseMu.Lock()
	defer s.contributeHub.leaseMu.Unlock()
	n := 0
	for _, l := range s.contributeHub.leases {
		if l != nil && l.stage != "" && now.Before(l.expiresAt) {
			n++
		}
	}
	return n
}

// A repeated approve of an admitted proposal reports its run instead of
// admitting it again, even once the admitted lease has expired
// (hivecommons/hive#10115).
func TestNousApproveSpektacularRunIsNotReadmitted(t *testing.T) {
	s, _ := nousSpektacularRunTestServer(t, map[string]interface{}{"target": "myorg/repo1#8737", "hypothesis": "once"})

	rec := doPost(s, "/api/nous/approve", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("first approve: %d body=%s", rec.Code, rec.Body.String())
	}
	s.contributeHub.leaseMu.Lock()
	for _, l := range s.contributeHub.leases {
		if l != nil {
			l.expiresAt = time.Now().Add(-time.Minute)
		}
	}
	s.contributeHub.leaseMu.Unlock()

	rec = doPost(s, "/api/nous/approve", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat approve: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string            `json:"status"`
		Run    map[string]string `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if resp.Status != "already_approved" || resp.Run["key"] != "myorg/repo1#8737" {
		t.Fatalf("repeat approve response = %+v", resp)
	}
	if got := liveRunLeases(s, time.Now()); got != 0 {
		t.Fatalf("live run leases after repeat approve = %d, want 0 (no re-admission)", got)
	}
}

// A bare pending repo is admitted under the org-qualified run key, as
// inception approve does (hivecommons/hive#10115).
func TestNousApproveSpektacularRunQualifiesBareRepo(t *testing.T) {
	s, deps := nousSpektacularRunTestServer(t, map[string]interface{}{"repo": "repo1", "number": 8737, "hypothesis": "bare"})
	deps.Config.Project.Org = "myorg"

	rec := doPost(s, "/api/nous/approve", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d body=%s", rec.Code, rec.Body.String())
	}
	stages, err := s.RunStageAccessor().PendingRunStages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 1 || stages[0].Repo != "myorg/repo1" {
		t.Fatalf("pending stages = %+v, want myorg/repo1", stages)
	}
	pending := deps.Nous.Status["pending"].(map[string]interface{})
	if pending["run_key"] != "myorg/repo1#8737" {
		t.Fatalf("pending run linkage = %+v", pending)
	}
}

// Server-side admission failures are not reported as client errors
// (hivecommons/hive#10115).
func TestNousApproveSpektacularRunRegistryUnavailableIs503(t *testing.T) {
	s, deps := nousSpektacularRunTestServer(t, map[string]interface{}{"target": "myorg/repo1#8737"})
	hub := s.contributeHub
	s.contributeHub = nil
	rec := doPost(s, "/api/nous/approve", map[string]interface{}{})
	s.contributeHub = hub
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("approve: %d body=%s, want 503", rec.Code, rec.Body.String())
	}
	pending := deps.Nous.Status["pending"].(map[string]interface{})
	if _, ok := pending["run_key"]; ok {
		t.Fatalf("failed approve linked a run: %+v", pending)
	}
}

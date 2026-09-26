package dashboard

import (
	"context"
	"net/http"
	"testing"
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

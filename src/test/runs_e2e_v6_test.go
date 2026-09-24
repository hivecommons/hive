//go:build integration

package test

import (
	"net/http"
	"testing"
	"time"
)

func TestRunsE2EV6Surfaces(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping v6 run surface e2e in short mode")
	}
	client := newAPIClient()
	if _, code, err := client.get("/api/version"); err != nil || code != http.StatusOK {
		t.Fatalf("hive not reachable at %s: %v (code=%d)", hiveURL, err, code)
	}
	features := runsE2EFeatureProbe(t, client)
	originalPlanCheckpoint, ok := boolField(features, "checkpointPlanEnabled")
	if !ok {
		skipUntil(t, "gap 4 of #8460", "governor feature payload does not expose checkpointPlanEnabled")
	}
	defer setPlanCheckpoint(t, client, originalPlanCheckpoint)
	setPlanCheckpoint(t, client, true)

	runKey := runsE2ETargetKey(t)
	triggerRunsTriage(t, client, runKey)

	plan, err := waitForRun(t, client, runKey, func(r runsE2ERun) bool {
		return r.Stage == "plan" && r.PlanEpicID != ""
	}, runsE2ETimeout())
	if err != nil {
		t.Fatalf("run %s did not reach plan with plan_epic_id: %v", runKey, err)
	}

	t.Run("runs_card_status_surface", func(t *testing.T) {
		if !waitForStatusRun(t, client, runKey, runsE2EPollInterval) {
			t.Fatalf("/api/status did not expose run %s within one runs SSE tick", runKey)
		}
	})

	t.Run("checkpoint_gate_policy_projection", func(t *testing.T) {
		blocked := getRun(t, client, runKey)
		if blocked.Stage == "plan" && blocked.WaitingOn != "human" {
			t.Fatalf("plan checkpoint enabled: waiting_on=%q, want human; run=%+v", blocked.WaitingOn, blocked)
		}
		setPlanCheckpoint(t, client, false)
		unblocked := getRun(t, client, runKey)
		if unblocked.Stage == "plan" && unblocked.WaitingOn == "human" {
			t.Fatalf("plan checkpoint disabled still blocks on human: %+v", unblocked)
		}
		setPlanCheckpoint(t, client, true)
	})

	t.Run("chat_approval_advances_same_lease", func(t *testing.T) {
		code, body, err := postJSON(client, "/api/chat", map[string]string{"query": "!runs approve " + runKey}, nil)
		if code == http.StatusServiceUnavailable {
			t.Skip("dashboard chat is not configured on this live hive")
		}
		if code == http.StatusForbidden || code == http.StatusUnauthorized {
			t.Skip("HIVE_TOKEN cannot post dashboard chat commands")
		}
		if err != nil || code != http.StatusOK {
			t.Fatalf("POST /api/chat !runs approve returned %d: %v %s", code, err, body)
		}
		implement, err := waitForRun(t, client, runKey, func(r runsE2ERun) bool {
			return r.Stage == "implement" && r.Gen == plan.Gen+1
		}, runsE2ETimeout())
		if err != nil {
			t.Fatalf("chat approval did not advance plan->implement: %v", err)
		}
		if implement.PlanEpicID != plan.PlanEpicID || implement.Assignee == "" {
			t.Fatalf("chat approval changed lease identity unexpectedly: before=%+v after=%+v", plan, implement)
		}
	})
}

func waitForStatusRun(t *testing.T, client *apiClient, key string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for attempt := 0; ; attempt++ {
		var status struct {
			Runs []runsE2ERun `json:"runs"`
		}
		code, body, err := getJSON(client, "/api/status", &status)
		if err != nil || code != http.StatusOK {
			t.Fatalf("GET /api/status returned %d: %v %s", code, err, body)
		}
		for _, run := range status.Runs {
			if run.Key == key {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		waitNextPoll(attempt)
	}
}

func setPlanCheckpoint(t *testing.T, client *apiClient, enabled bool) {
	t.Helper()
	payload := map[string]bool{"checkpointPlanEnabled": enabled}
	_, code, err := client.put("/api/config/governor/features", payload)
	if code == http.StatusForbidden || code == http.StatusUnauthorized {
		t.Skip("HIVE_TOKEN cannot update governor checkpoint policy")
	}
	if err != nil || code != http.StatusOK {
		t.Fatalf("set checkpointPlanEnabled=%v returned %d: %v", enabled, code, err)
	}
}

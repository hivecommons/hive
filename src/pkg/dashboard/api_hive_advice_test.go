package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hiveadvisor"
)

func TestHandleHiveAdviceFreezesEpoch(t *testing.T) {
	s := newTestServer()
	status := minimalPayload()
	status.Governor.Mode = "idle"
	status.Governor.Issues = 0
	status.Governor.PRs = 0
	status.Repos = []FrontendRepo{{Name: "one"}}
	s.UpdateStatus(status)

	req := httptest.NewRequest(http.MethodGet, "/api/hive-advice", nil)
	rr := httptest.NewRecorder()
	s.handleHiveAdvice(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var first hiveadvisor.Result
	if err := json.Unmarshal(rr.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Recommendations) == 0 || first.Recommendations[0].ID != "widen-hive-repos" {
		t.Fatalf("first recommendations = %#v", first.Recommendations)
	}

	status.Governor.Mode = "idle"
	status.ConfiguredAgents = []FrontendConfiguredAgent{{Name: "guide", Enabled: false}}
	second := s.AttachHiveAdvice(status, first.Epoch.Start.Add(time.Hour))
	if !second.Frozen {
		t.Fatal("expected same-mode request inside week to reuse frozen epoch")
	}
	if second.Recommendations[0].ID != first.Recommendations[0].ID {
		t.Fatalf("frozen top = %s, want %s", second.Recommendations[0].ID, first.Recommendations[0].ID)
	}
}

func TestAttachHiveAdviceInvalidatesOnModeChange(t *testing.T) {
	s := newTestServer()
	status := minimalPayload()
	status.Governor.Mode = "quiet"
	status.Repos = []FrontendRepo{{Name: "one"}}
	first := s.AttachHiveAdvice(status, time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))

	status.Governor.Mode = "surge"
	status.Governor.Issues = 20
	second := s.AttachHiveAdvice(status, first.Epoch.Start.Add(time.Hour))
	if second.Frozen {
		t.Fatal("mode change should invalidate the frozen epoch")
	}
	if second.Epoch.Mode != "SURGE" || second.Recommendations[0].ID != "throttle-pr-producing-lanes" {
		t.Fatalf("second = %#v", second)
	}
}

func TestBuildHiveAdvisorSignals(t *testing.T) {
	status := &StatusPayload{
		Governor: FrontendGovernor{Mode: "busy", Issues: 2, PRs: 5},
		Hold:     FrontendHold{Total: 1},
		Repos:    []FrontendRepo{{}, {}},
		ConfiguredAgents: []FrontendConfiguredAgent{
			{Name: "scanner", Enabled: true},
			{Name: "guide", Enabled: false},
		},
		Agents: []FrontendAgent{{Name: "scanner", Enabled: true, NoCadence: true}},
		Budget: FrontendBudget{PctUsed: 91, Exhausted: true},
	}
	got := buildHiveAdvisorSignals(status)
	if got.Mode != "busy" || got.QueueIssues != 2 || got.QueuePRs != 5 || got.HoldCount != 1 || got.RepoCount != 2 || got.DisabledAgentCount != 1 || got.NoCadenceAgentCount != 1 || got.BudgetUsedPct != 91 || !got.BudgetExhausted {
		t.Fatalf("signals = %#v", got)
	}
}

func TestStatusPayloadCarriesHiveAdvice(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{Agents: map[string]config.AgentConfig{}}}
	payload := minimalPayload()
	payload.Governor.Mode = "idle"
	payload.Governor.Issues = 0
	payload.Governor.PRs = 0
	payload.Repos = []FrontendRepo{{Name: "one"}}
	s.UpdateStatus(payload)
	if payload.HiveAdvice == nil || len(payload.HiveAdvice.Recommendations) == 0 {
		t.Fatalf("hive advice missing from status: %#v", payload.HiveAdvice)
	}
}

func TestHandleHiveAdviceNilStatusDoesNotFreezeStartup(t *testing.T) {
	s := newTestServer()
	s.hiveAdviceLoaded = true
	rr := httptest.NewRecorder()
	s.handleHiveAdvice(rr, httptest.NewRequest(http.MethodGet, "/api/hive-advice", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := s.CurrentHiveAdvice(); got != nil {
		t.Fatalf("nil status should not persist a startup epoch: %#v", got)
	}
}

func TestHandleHiveAdviceDoesNotMutatePublishedStatus(t *testing.T) {
	s := newTestServer()
	status := minimalPayload()
	status.Governor.Issues = 0
	status.Governor.PRs = 0
	status.Repos = []FrontendRepo{{Name: "one"}}
	computed := s.AttachHiveAdvice(status, time.Now().UTC())
	status.HiveAdvice = nil
	s.status = status
	rr := httptest.NewRecorder()
	s.handleHiveAdvice(rr, httptest.NewRequest(http.MethodGet, "/api/hive-advice", nil))
	if status.HiveAdvice != nil {
		t.Fatal("handler mutated the published status snapshot")
	}
	var got hiveadvisor.Result
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Epoch.Mode != computed.Epoch.Mode {
		t.Fatalf("handler did not serve persisted advice: %#v", got)
	}
}

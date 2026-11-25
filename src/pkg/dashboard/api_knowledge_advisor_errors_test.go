package dashboard

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/retro"
)

// hivecommons/hive#17054 evidence pass 2: the summary has to say why the
// deliver arm delivered nothing, not only that it errored.
func TestAdvisorSummaryErrorReasons(t *testing.T) {
	store := retro.NewAdvisorDecisionStore("")
	rec := func(kick, outcome, verdict, budget, class, reason string) {
		store.Record(retro.AdvisorDecisionRecord{Timestamp: "2026-11-20T10:00:00Z", Agent: "dev", Kick: kick, Mode: "rank", Outcome: outcome, Verdict: verdict, Budget: budget, ErrorClass: class, ErrorReason: reason})
	}
	rec("k1", "error", "error", "", "rejected", "HTTP 400: Authorization header is badly formatted")
	rec("k2", "held-out", "error", "", "rejected", "HTTP 400: Authorization header is badly formatted")
	rec("k3", "error", "error", "", "auth", "HTTP 401: Bad credentials")
	rec("k4", "over-budget", "over-budget", "error-agent-hour", "", "")
	rec("k5", "delivered", "pick", "", "", "")

	s := knowledgeAdvisorTestServer(t)
	t.Setenv("ADVISOR_SUMMARY_TEST_KEY", "")
	s.deps.Config.Knowledge = config.KnowledgeConfig{Advisor: true, AdvisorLLM: config.AdvisorLLMConfig{Mode: "rank", Endpoint: "https://llm.example.invalid", APIKeyEnv: "ADVISOR_SUMMARY_TEST_KEY"}}
	s.SetKnowledgeAdvisorSummary(store, nil)

	httpRec, _ := getAdvisorSummary(t, s, "/api/knowledge/advisor/summary", config.RoleRead)
	if httpRec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", httpRec.Code, httpRec.Body.String())
	}
	var got struct {
		Settings map[string]interface{} `json:"settings"`
		Counts   struct {
			ByVerdict map[string]int `json:"by_verdict"`
			ByBudget  map[string]int `json:"by_budget"`
		} `json:"counts"`
		Errors struct {
			ByClass    map[string]int           `json:"by_class"`
			TopReasons []advisorErrorReasonJSON `json:"top_reasons"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(httpRec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Errors.ByClass, map[string]int{"rejected": 2, "auth": 1}) {
		t.Errorf("by_class = %v", got.Errors.ByClass)
	}
	wantTop := []advisorErrorReasonJSON{
		{Class: "rejected", Reason: "HTTP 400: Authorization header is badly formatted", Count: 2},
		{Class: "auth", Reason: "HTTP 401: Bad credentials", Count: 1},
	}
	if !reflect.DeepEqual(got.Errors.TopReasons, wantTop) {
		t.Errorf("top_reasons = %+v, want %+v", got.Errors.TopReasons, wantTop)
	}
	if !reflect.DeepEqual(got.Counts.ByVerdict, map[string]int{"error": 3, "over-budget": 1, "pick": 1}) {
		t.Errorf("by_verdict = %v", got.Counts.ByVerdict)
	}
	if !reflect.DeepEqual(got.Counts.ByBudget, map[string]int{"error-agent-hour": 1}) {
		t.Errorf("by_budget = %v", got.Counts.ByBudget)
	}
	// No model resolves: the summary names the misconfiguration.
	problem, _ := got.Settings["problem"].(string)
	if !strings.Contains(problem, "no model") {
		t.Errorf("settings.problem = %v, want the missing model named", got.Settings["problem"])
	}
	body := httpRec.Body.String()
	for _, banned := range []string{"ADVISOR_SUMMARY_TEST_KEY", "llm.example.invalid", "\"dev\""} {
		if strings.Contains(body, banned) {
			t.Errorf("response leaks %q", banned)
		}
	}
}

func TestAdvisorSummaryEmptyErrorsBlock(t *testing.T) {
	s := knowledgeAdvisorTestServer(t)
	s.SetKnowledgeAdvisorSummary(retro.NewAdvisorDecisionStore(""), nil)
	httpRec, _ := getAdvisorSummary(t, s, "/api/knowledge/advisor/summary", config.RoleRead)
	body := httpRec.Body.String()
	if !strings.Contains(body, `"top_reasons":[]`) || !strings.Contains(body, `"by_class":{}`) {
		t.Fatalf("empty store must return an empty errors block, got %s", body)
	}
	if !strings.Contains(body, `"problem":null`) && strings.Contains(body, `"problem"`) {
		t.Fatalf("problem must be null when the advisor is off: %s", body)
	}
}

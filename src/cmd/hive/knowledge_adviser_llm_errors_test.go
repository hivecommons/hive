package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/keystore"
	"github.com/hivecommons/hive/pkg/retro"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// Refused calls (hivecommons/hive#17054 evidence pass 2): a week of deliver-arm
// decisions on hive's own hive was all error or over-budget, with no reason
// recorded anywhere and failed calls spending the per-agent hour.

const llmTestLeakedToken = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

// refuse makes the model answer status with body to every request.
func (f *llmFixture) refuse(status int, body string) {
	f.with(func() {
		f.handler = func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	})
}

func (f *llmFixture) budgetState(agent string) (events, used, errEvents, errUsed int) {
	f.d.budget.mu.Lock()
	defer f.d.budget.mu.Unlock()
	return len(f.d.budget.events[agent]), f.d.budget.used, len(f.d.budget.errEvents[agent]), f.d.budget.errUsed
}

func TestKnowledgeAdviserLLM_RefusedCallRecordsClassAndRedactedReason(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank})
	store := retro.NewAdvisorDecisionStore("")
	f.d.decisions = store
	f.refuse(http.StatusBadRequest, `{"error":{"message":"Authorization header is badly formatted: Bearer `+llmTestLeakedToken+`","code":"invalid_auth_header"}}`)

	if got := f.d.Advise("developer", llmTestScope); len(got) != 0 {
		t.Fatalf("refused call delivered %v", slugsOf(got))
	}
	d := f.lastDecision(t)
	if d["verdict"] != knowledgeVerdictError || d["error_class"] != knowledgeErrorClassRejected {
		t.Fatalf("decision = %v, want verdict error, class rejected", d)
	}
	reason, _ := d["error_reason"].(string)
	if !strings.HasPrefix(reason, "HTTP 400 invalid_auth_header: Authorization header is badly formatted") {
		t.Errorf("error_reason = %q", reason)
	}
	if strings.Contains(f.logs.String(), llmTestLeakedToken) {
		t.Errorf("a token echoed by the provider reached the log:\n%s", f.logs.String())
	}

	sum := store.Summarize(time.Time{})
	if sum.ByErrorClass[knowledgeErrorClassRejected] != 1 || sum.ByVerdict[knowledgeVerdictError] != 1 {
		t.Fatalf("summary by class %v by verdict %v", sum.ByErrorClass, sum.ByVerdict)
	}
	if len(sum.TopErrorReasons) != 1 || sum.TopErrorReasons[0].Reason != reason || sum.TopErrorReasons[0].Count != 1 {
		t.Fatalf("top reasons = %+v, want the logged reason once", sum.TopErrorReasons)
	}
	if strings.Contains(sum.TopErrorReasons[0].Reason, llmTestLeakedToken) {
		t.Errorf("token reached the decision store")
	}
}

func TestKnowledgeAdviserLLM_AuthStatusIsAuthClass(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank})
	f.refuse(http.StatusUnauthorized, `{"message":"Bad credentials"}`)
	f.d.Advise("developer", llmTestScope)
	if d := f.lastDecision(t); d["error_class"] != knowledgeErrorClassAuth || d["error_reason"] != "HTTP 401: Bad credentials" {
		t.Fatalf("decision = %v", d)
	}
}

func TestKnowledgeAdviserLLM_RefusedCallDoesNotConsumeCallBudget(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank, MaxCallsPerAgentHour: 2})
	f.refuse(http.StatusBadRequest, `{"error":{"message":"model not supported","code":"model_not_supported"}}`)
	f.d.Advise("developer", llmTestScope)
	if events, used, errEvents, errUsed := f.budgetState("developer"); events != 0 || used != 0 || errEvents != 1 || errUsed != 1 {
		t.Fatalf("after a refused call events=%d used=%d errEvents=%d errUsed=%d, want 0/0/1/1", events, used, errEvents, errUsed)
	}
	// The endpoint recovers: both of the hour's calls are still there.
	f.with(func() { f.handler = nil })
	f.d.Advise("developer", llmTestScope)
	f.d.Advise("developer", llmTestScope)
	if n := f.callCount(); n != 3 {
		t.Fatalf("calls = %d, want 3 (the refused one plus the hour's two)", n)
	}
	if d := f.lastDecision(t); d["outcome"] != knowledgeVerdictNull {
		t.Fatalf("second answered call = %v, want a real null, not over-budget", d)
	}
	// The call budget still holds.
	f.d.Advise("developer", llmTestScope)
	if d := f.lastDecision(t); d["verdict"] != knowledgeVerdictOverBudget || d["budget"] != "agent-hour" || f.callCount() != 3 {
		t.Fatalf("decision = %v calls = %d, want agent-hour over-budget without a call", d, f.callCount())
	}
}

func TestKnowledgeAdviserLLM_RefusedCallsStopAtErrorCeiling(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank})
	f.refuse(http.StatusBadRequest, `{"error":"bad request"}`)
	for i := 0; i < config.AdvisorLLMMaxErrorsPerAgentHourCap; i++ {
		f.d.Advise("developer", llmTestScope)
		f.clock = f.clock.Add(time.Minute)
	}
	if n := f.callCount(); n != config.AdvisorLLMMaxErrorsPerAgentHourCap {
		t.Fatalf("calls = %d, want %d", n, config.AdvisorLLMMaxErrorsPerAgentHourCap)
	}
	f.d.Advise("developer", llmTestScope)
	if n := f.callCount(); n != config.AdvisorLLMMaxErrorsPerAgentHourCap {
		t.Fatalf("a refused call past the error ceiling reached the endpoint (calls = %d)", n)
	}
	if d := f.lastDecision(t); d["verdict"] != knowledgeVerdictOverBudget || d["budget"] != knowledgeBudgetErrorAgentHour || d["outcome"] != knowledgeVerdictOverBudget {
		t.Fatalf("decision = %v, want over-budget on %s (fail closed)", d, knowledgeBudgetErrorAgentHour)
	}
	// The error window rolls like the call window.
	f.clock = f.clock.Add(time.Hour)
	f.d.Advise("developer", llmTestScope)
	if n := f.callCount(); n != config.AdvisorLLMMaxErrorsPerAgentHourCap+1 {
		t.Fatalf("after the hour rolled calls = %d", n)
	}
}

func TestKnowledgeAdviserLLMBudget_ErrorCeilingsHoldUnderOverride(t *testing.T) {
	clock := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	b := newKnowledgeAdviserLLMBudget(func() time.Time { return clock })
	// budget_override raised the call budget; the error ledger stays at the
	// hard caps.
	const overriddenHour, overriddenDay = 50, 5000
	for i := 0; i < config.AdvisorLLMMaxErrorsPerAgentHourCap; i++ {
		over, at := b.take("developer", overriddenHour, overriddenDay)
		if over != "" {
			t.Fatalf("take %d over = %q", i, over)
		}
		b.chargeError("developer", at)
		clock = clock.Add(time.Second)
	}
	if over, _ := b.take("developer", overriddenHour, overriddenDay); over != knowledgeBudgetErrorAgentHour {
		t.Fatalf("over = %q, want %s", over, knowledgeBudgetErrorAgentHour)
	}
	// Fleet day: spread across agents so the hour never trips.
	for i := 0; b.errUsed < config.AdvisorLLMMaxErrorsPerDayCap; i++ {
		agent := fmt.Sprintf("agent-%d", i)
		over, at := b.take(agent, overriddenHour, overriddenDay)
		if over != "" {
			t.Fatalf("take for %s over = %q at errUsed %d", agent, over, b.errUsed)
		}
		b.chargeError(agent, at)
	}
	if over, _ := b.take("fresh", overriddenHour, overriddenDay); over != knowledgeBudgetErrorFleetDay {
		t.Fatalf("over = %q, want %s", over, knowledgeBudgetErrorFleetDay)
	}
	if b.used != 0 {
		t.Fatalf("refused calls left %d on the call budget", b.used)
	}
	// A new UTC day clears the fleet error counter.
	clock = time.Date(2026, 11, 21, 0, 0, 1, 0, time.UTC)
	if over, _ := b.take("fresh", overriddenHour, overriddenDay); over != "" {
		t.Fatalf("new day over = %q", over)
	}
}

func TestKnowledgeAdviserLLMErrorCeiling(t *testing.T) {
	for _, tc := range []struct{ limit, hardCap, want int }{
		{0, 6, 6}, {-1, 6, 6}, {2, 6, 2}, {6, 6, 6}, {20, 6, 6},
	} {
		if got := knowledgeAdviserLLMErrorCeiling(tc.limit, tc.hardCap); got != tc.want {
			t.Errorf("ceiling(%d, %d) = %d, want %d", tc.limit, tc.hardCap, got, tc.want)
		}
	}
}

func TestKnowledgeAdviserLLM_SealedKeyIsNeverSent(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank})
	t.Setenv("HIVE_TEST_ADVISOR_KEY", keystore.Prefix+"v1:env:abc:Zm9v")
	f.d.Advise("developer", llmTestScope)
	if n := f.callCount(); n != 0 {
		t.Fatalf("calls = %d, want 0: an envelope is not a bearer token", n)
	}
	d := f.lastDecision(t)
	if d["verdict"] != knowledgeVerdictError || d["error_class"] != knowledgeErrorClassConfig {
		t.Fatalf("decision = %v", d)
	}
	if strings.Contains(f.logs.String(), "Zm9v") {
		t.Errorf("envelope text reached the log")
	}
}

func TestKnowledgeAdviserLLM_TransportFailureIsRefused(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank, MaxCallsPerAgentHour: 1})
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	f.cfg.Knowledge.AdvisorLLM.Endpoint = deadURL
	f.d.Advise("developer", llmTestScope)
	d := f.lastDecision(t)
	if d["verdict"] != knowledgeVerdictError || d["error_class"] != knowledgeErrorClassTransport {
		t.Fatalf("decision = %v", d)
	}
	if events, _, errEvents, _ := f.budgetState("developer"); events != 0 || errEvents != 1 {
		t.Fatalf("events=%d errEvents=%d, want the call moved to the error ledger", events, errEvents)
	}
}

func TestKnowledgeAdviserLLM_UndecodableOKStaysCharged(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank})
	f.refuse(http.StatusOK, `not json`)
	f.d.Advise("developer", llmTestScope)
	if d := f.lastDecision(t); d["error_class"] != knowledgeErrorClassDecode {
		t.Fatalf("decision = %v", d)
	}
	if events, used, errEvents, _ := f.budgetState("developer"); events != 1 || used != 1 || errEvents != 0 {
		t.Fatalf("events=%d used=%d errEvents=%d, want a 200 to stay on the call budget", events, used, errEvents)
	}
}

func TestKnowledgeAdviserLLMErrorReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"openai nested", 400, `{"error":{"message":"The model x is not supported","type":"invalid_request_error","code":"model_not_supported"}}`, "HTTP 400 model_not_supported: The model x is not supported"},
		{"type when no code", 404, `{"error":{"message":"no route","type":"not_found"}}`, "HTTP 404 not_found: no route"},
		{"string error", 400, `{"error":"bad request: missing header"}`, "HTTP 400: bad request: missing header"},
		{"top-level message", 403, `{"message":"Resource not accessible","code":403}`, "HTTP 403 403: Resource not accessible"},
		{"plain text", 502, "upstream\n  connect error", "HTTP 502: upstream connect error"},
		{"empty", 500, "", "HTTP 500"},
		{"envelope echoed", 400, `{"error":"bad token ` + keystore.Prefix + `v1:env:abc:Zm9v"}`, "HTTP 400: bad token [sealed]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := knowledgeAdviserLLMErrorReason(tc.status, []byte(tc.body)); got != tc.want {
				t.Errorf("reason = %q, want %q", got, tc.want)
			}
		})
	}
	long := knowledgeAdviserLLMErrorReason(400, []byte(`{"error":"`+strings.Repeat("x", 4*knowledgeAdviserLLMErrorReasonRunes)+`"}`))
	if n := len([]rune(long)); n > knowledgeAdviserLLMErrorReasonRunes+len("HTTP 400: ")+1 {
		t.Errorf("reason is %d runes, want it capped", n)
	}
}

// The over-budget fleet-day test above relies on the call budget; make sure a
// kick change does not reset the error ledger (it is per agent, not per kick).
func TestKnowledgeAdviserLLM_ErrorLedgerIsPerAgentNotPerKick(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank, MaxCallsPerAgentHour: 1})
	f.refuse(http.StatusBadRequest, `{}`)
	f.d.Advise("developer", llmTestScope)
	f.setKick("developer", scheduler.KickSummary{ID: "developer#2"})
	f.d.Advise("developer", llmTestScope)
	if d := f.lastDecision(t); d["budget"] != knowledgeBudgetErrorAgentHour {
		t.Fatalf("decision = %v, want the error ledger to carry across kicks", d)
	}
}

package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"
)

// #12089: the Experiment Lab header shows the research question for the
// active scope in place of the retired "OBSERVING · GOVERNOR" pill, so
// GET /api/nous/status must carry it — and omit it when none is set.
func TestNousStatusCarriesResearchQuestionForScope(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = &NousState{
		Mode:   "observe",
		Scope:  "governor",
		Status: map[string]interface{}{"status": "active", "mode": "observe", "scope": "governor"},
		Config: map[string]interface{}{},
	}

	readStatus := func() map[string]interface{} {
		t.Helper()
		rec := doOwnerGet(s, "/api/nous/status")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET status = %d: %s", rec.Code, rec.Body.String())
		}
		var got map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("status body: %v", err)
		}
		return got
	}

	if got := readStatus(); got[nousStatusKeyResearchQuestion] != nil {
		t.Fatalf("no goals set: researchQuestion must be absent, got %v", got[nousStatusKeyResearchQuestion])
	}

	rec := doPut(s, "/api/nous/config/goals", map[string]interface{}{
		"governorGoal": "  Can we cut MTTR 20% with shorter busy cadences?  ",
		"repoGoal":     "Does a flaky-test quarantine lower CI reruns?",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT goals = %d: %s", rec.Code, rec.Body.String())
	}
	if got := readStatus(); got[nousStatusKeyResearchQuestion] != "Can we cut MTTR 20% with shorter busy cadences?" {
		t.Fatalf("governor scope researchQuestion = %v", got[nousStatusKeyResearchQuestion])
	}

	if rec := doPut(s, "/api/nous/scope", map[string]interface{}{"scope": "repo"}); rec.Code != http.StatusOK {
		t.Fatalf("PUT scope = %d: %s", rec.Code, rec.Body.String())
	}
	if got := readStatus(); got[nousStatusKeyResearchQuestion] != "Does a flaky-test quarantine lower CI reruns?" {
		t.Fatalf("repo scope researchQuestion = %v", got[nousStatusKeyResearchQuestion])
	}

	// Whitespace-only goals count as unset.
	if rec := doPut(s, "/api/nous/config/goals", map[string]interface{}{"governorGoal": " ", "repoGoal": ""}); rec.Code != http.StatusOK {
		t.Fatalf("PUT goals = %d: %s", rec.Code, rec.Body.String())
	}
	if got := readStatus(); got[nousStatusKeyResearchQuestion] != nil {
		t.Fatalf("blank goals: researchQuestion must be absent, got %v", got[nousStatusKeyResearchQuestion])
	}
}

func TestNousResearchQuestionBothScopeAndLegacyKeys(t *testing.T) {
	ns := &NousState{Scope: "both", Config: map[string]interface{}{
		"goals": map[string]interface{}{"repo": "repo question"},
	}}
	if got := ns.researchQuestionLocked(); got != "repo question" {
		t.Fatalf("both scope with only a repo goal = %q", got)
	}
	ns.Config["goals"].(map[string]interface{})["governor"] = "governor question"
	if got := ns.researchQuestionLocked(); got != "governor question" {
		t.Fatalf("both scope prefers the governor goal, got %q", got)
	}
	ns.Config["goals"] = "not a map"
	if got := ns.researchQuestionLocked(); got != "" {
		t.Fatalf("malformed goals = %q, want empty", got)
	}
	var nilState *NousState
	if got := nilState.researchQuestionLocked(); got != "" {
		t.Fatalf("nil state = %q", got)
	}
}

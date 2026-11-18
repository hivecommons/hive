package dashboard

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeKickOutcomes is a canned KickOutcomeSource: the derivation, redaction
// and caps are pkg/kickoutcome's tests; here only the route is under test.
type fakeKickOutcomes struct {
	intents, refusals []string
}

func (f *fakeKickOutcomes) Intend(agent, kind, actor string) {
	f.intents = append(f.intents, agent+"|"+kind+"|"+actor)
}

func (f *fakeKickOutcomes) Refused(agent, kind, actor, code, reason string, _ time.Time) {
	f.refusals = append(f.refusals, agent+"|"+kind+"|"+actor+"|"+code)
}

func (f *fakeKickOutcomes) Latest() any {
	return map[string]any{"scanner": map[string]any{"kind": "stood_down", "summary": "stood down"}}
}

func (f *fakeKickOutcomes) Recent(agent string, _ int) (any, any) {
	return map[string]any{"kind": "stood_down", "summary": "stood down"},
		[]any{map[string]any{"kind": "stood_down", "summary": "stood down", "trigger_kind": "governor", "started_at": "x", "quote": "no push rights"}}
}

func kickOutcomeServer(t *testing.T) (*Server, *fakeKickOutcomes) {
	t.Helper()
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	deps := testDeps(t)
	f := &fakeKickOutcomes{}
	deps.KickOutcomes = f
	s.RegisterAPI(deps)
	return s, f
}

func kickOutcomeGet(s *Server, path, role string, verified bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Hive-User", "u")
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// A read-only session sees the one-line summary and nothing else: no quoted
// reason, no trigger, no times of the turn. An owner and a read-write session
// see the detail, with tokens redacted.
func TestKickOutcomesRoleGatedDetail(t *testing.T) {
	s, _ := kickOutcomeServer(t)

	for _, c := range []struct {
		name, role string
		verified   bool
		detail     bool
	}{{"read-only", "read", false, false}, {"proofless owner", "owner", false, false}, {"read-write", "read-write", false, true}, {"owner", "owner", true, true}} {
		rec := kickOutcomeGet(s, "/api/kick-outcomes?agent=scanner", c.role, c.verified)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
		body := rec.Body.String()
		hasQuote := strings.Contains(body, "no push rights")
		if hasQuote != c.detail {
			t.Fatalf("%s: quote visible=%v want %v: %s", c.name, hasQuote, c.detail, body)
		}
		if !c.detail && (strings.Contains(body, "trigger") || strings.Contains(body, "started_at") || strings.Contains(body, "kicks")) {
			t.Fatalf("%s: detail fields leaked to a read-only session: %s", c.name, body)
		}
		if !strings.Contains(body, "stood down") {
			t.Fatalf("%s: summary missing: %s", c.name, body)
		}
	}
}

func TestKickOutcomesLatestListHoldsSummariesOnly(t *testing.T) {
	s, _ := kickOutcomeServer(t)
	rec := kickOutcomeGet(s, "/api/kick-outcomes", "owner", true)
	var resp struct {
		Agents map[string]map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if v := resp.Agents["scanner"]; v["kind"] != "stood_down" || v["quote"] != nil {
		t.Fatalf("%v", v)
	}
}

func TestKickOutcomesUnavailableWithoutStore(t *testing.T) {
	s, _ := kickOutcomeServer(t)
	s.deps.KickOutcomes = nil
	if rec := kickOutcomeGet(s, "/api/kick-outcomes", "owner", true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d", rec.Code)
	}
}

// A refusal is recorded only with a documented reason code; the record never
// invents one.
func TestKickRefusedOnErrorUsesDocumentedCodes(t *testing.T) {
	s, f := kickOutcomeServer(t)
	s.kickRefusedOnError("a", kickTriggerDashboard, "alice", errors.New("agent a cannot be kicked: it is paused (by budget)"))
	s.kickRefusedOnError("a", kickTriggerDashboard, "alice", errors.New("something nobody documented"))
	s.kickRefusedOnError("a", kickTriggerDashboard, "alice", nil)
	if len(f.refusals) != 1 || f.refusals[0] != "a|dashboard|alice|"+kickRefusalPaused {
		t.Fatalf("%v", f.refusals)
	}
	s.kickIntend("a", kickTriggerWorkflowDispatch, "bob")
	if len(f.intents) != 1 {
		t.Fatalf("%v", f.intents)
	}
}

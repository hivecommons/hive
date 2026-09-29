package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/pkg/config"
)

func effortHub(t *testing.T, floor string, rejectUnknown bool) *ContributeWSHub {
	t.Helper()
	srv := newMinimalServer(t)
	srv.deps.Config.Hub.ContributeMinReasoningEffort = floor
	srv.deps.Config.Hub.ContributeRejectUnknownEffort = rejectUnknown
	hub := NewContributeWSHub(slog.Default(), nil)
	t.Cleanup(hub.Close)
	hub.server = srv
	return hub
}

func TestCheckEffortAllowedNoConfig(t *testing.T) {
	hub := NewContributeWSHub(slog.Default(), nil)
	t.Cleanup(hub.Close)
	if ok, floor := hub.checkEffortAllowed("codex", "minimal"); !ok || floor != "" {
		t.Errorf("no config should allow, got (%v,%q)", ok, floor)
	}
}

func TestCheckEffortAllowed(t *testing.T) {
	cases := []struct {
		name            string
		floor           string
		rejectUnknown   bool
		backend, effort string
		wantOK          bool
	}{
		{"no floor", "", true, "codex", "", true},
		{"meets floor", "medium", false, "codex", "high", true},
		{"below floor", "medium", false, "codex", "low", false},
		{"below floor even when lenient on unknown", "high", false, "claude", "medium", false},
		{"clamped floor for agy", "max", false, "agy", "high", true},
		{"empty effort lenient", "medium", false, "codex", "", true},
		{"empty effort strict", "medium", true, "codex", "", false},
		{"invalid-for-backend strict", "low", true, "agy", "minimal", false},
		{"invalid-for-backend lenient", "low", false, "agy", "minimal", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hub := effortHub(t, c.floor, c.rejectUnknown)
			ok, floor := hub.checkEffortAllowed(c.backend, c.effort)
			if ok != c.wantOK {
				t.Fatalf("checkEffortAllowed(%q,%q) = %v, want %v", c.backend, c.effort, ok, c.wantOK)
			}
			if !ok && floor != c.floor {
				t.Errorf("rejection should echo floor %q, got %q", c.floor, floor)
			}
			if ok && floor != "" {
				t.Errorf("admission should return empty floor, got %q", floor)
			}
		})
	}
}

func dialEffortAuth(t *testing.T, floor string, rejectUnknown bool, msg WSMessage) WSMessage {
	t.Helper()
	s, ts := setupWSTest(t)
	t.Cleanup(ts.Close)
	s.deps = &Dependencies{Config: &config.Config{}}
	s.deps.Config.Hub.ContributeMinReasoningEffort = floor
	s.deps.Config.Hub.ContributeRejectUnknownEffort = rejectUnknown
	token, _ := registerWSUser(t, s, "effort-floor-user")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	readMsg(t, conn) // challenge
	msg.Type = "auth_response"
	msg.RegistrationToken = token
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	return readMsg(t, conn)
}

func TestWSAuthRejectsEffortBelowFloor(t *testing.T) {
	got := dialEffortAuth(t, "high", false, WSMessage{CLIBackend: "codex", ReasoningEffort: "low"})
	if got.Type != "auth_failed" {
		t.Fatalf("expected auth_failed, got %s", got.Type)
	}
	if got.MinReasoningEffort != "high" {
		t.Errorf("auth_failed should echo floor, got %q", got.MinReasoningEffort)
	}
	if !strings.Contains(got.Reason, "below") {
		t.Errorf("reason should explain the floor, got %q", got.Reason)
	}
}

func TestWSAuthRejectsMissingEffortWhenStrict(t *testing.T) {
	got := dialEffortAuth(t, "medium", true, WSMessage{CLIBackend: "codex"})
	if got.Type != "auth_failed" || !strings.Contains(got.Reason, "No reasoning effort") {
		t.Fatalf("expected auth_failed for missing effort, got %s: %q", got.Type, got.Reason)
	}
}

func TestWSAuthRejectsUnrecognisedEffortWhenStrict(t *testing.T) {
	got := dialEffortAuth(t, "low", true, WSMessage{CLIBackend: "agy", ReasoningEffort: "minimal"})
	if got.Type != "auth_failed" || !strings.Contains(got.Reason, "not recognised") {
		t.Fatalf("expected auth_failed for unrecognised effort, got %s: %q", got.Type, got.Reason)
	}
}

func TestWSAuthAdmitsEffortAtFloor(t *testing.T) {
	got := dialEffortAuth(t, "high", false, WSMessage{CLIBackend: "claude", ReasoningEffort: "max"})
	if got.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s: %q", got.Type, got.Reason)
	}
}

func TestHandleGovernorHubReasoningEffortFloor(t *testing.T) {
	srv := newFullServer(t)
	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/api/governor/hub", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		markOwnerRequest(req)
		srv.handleGovernorHub(w, req)
		return w
	}

	if w := put(`{"contribute_min_reasoning_effort":" High ","contribute_reject_unknown_effort":true}`); w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.Hub.ContributeMinReasoningEffort; got != "high" {
		t.Errorf("floor = %q, want high", got)
	}
	if !srv.deps.Config.Hub.ContributeRejectUnknownEffort {
		t.Error("reject unknown effort should be true")
	}

	if w := put(`{"contribute_min_reasoning_effort":"turbo"}`); w.Code != 400 {
		t.Fatalf("invalid floor should 400, got %d", w.Code)
	}
	if got := srv.deps.Config.Hub.ContributeMinReasoningEffort; got != "high" {
		t.Errorf("rejected PUT must not change floor, got %q", got)
	}

	if w := put(`{"contribute_min_reasoning_effort":""}`); w.Code != 200 {
		t.Fatalf("clearing floor should 200, got %d", w.Code)
	}
	if got := srv.deps.Config.Hub.ContributeMinReasoningEffort; got != "" {
		t.Errorf("floor should be cleared, got %q", got)
	}
}

func TestGovernorConfigGetExposesReasoningEffortFloor(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.ContributeMinReasoningEffort = "medium"
	srv.deps.Config.Hub.ContributeRejectUnknownEffort = true
	req := httptest.NewRequest("GET", "/api/config/governor", nil)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleGovernorConfigGet(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body struct {
		Hub map[string]any `json:"hub"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Hub["contribute_min_reasoning_effort"] != "medium" {
		t.Errorf("floor = %v", body.Hub["contribute_min_reasoning_effort"])
	}
	if body.Hub["contribute_reject_unknown_effort"] != true {
		t.Errorf("reject unknown effort = %v", body.Hub["contribute_reject_unknown_effort"])
	}
	ladder, _ := body.Hub["contribute_reasoning_effort_ladder"].([]any)
	if len(ladder) != len(config.ReasoningEffortLadder) {
		t.Errorf("ladder = %v", body.Hub["contribute_reasoning_effort_ladder"])
	}
}

func TestContributeAdmissionPolicyExposesReasoningEffortFloor(t *testing.T) {
	srv := newMinimalServer(t)
	srv.deps.Config.Hub.ContributeMinReasoningEffort = "high"
	srv.deps.Config.Hub.ContributeRejectUnknownEffort = true
	p := srv.buildContributeAdmissionPolicy()
	if p.MinReasoningEffort != "high" || !p.RejectUnknownEffort {
		t.Errorf("policy = (%q,%v), want (high,true)", p.MinReasoningEffort, p.RejectUnknownEffort)
	}
	raw, _ := json.Marshal(p)
	if !strings.Contains(string(raw), `"min_reasoning_effort":"high"`) || !strings.Contains(string(raw), `"reject_unknown_effort":true`) {
		t.Errorf("policy JSON missing effort floor: %s", raw)
	}
}

func TestHubPanelRendersEffortFloorControl(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		`id="hub-min-reasoning-effort"`,
		`data-arg1="contribute_min_reasoning_effort"`,
		`data-key="contribute_reject_unknown_effort"`,
		"function contributeEffortFloorOptions(",
		"BACKEND_REASONING_EFFORTS[b]",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

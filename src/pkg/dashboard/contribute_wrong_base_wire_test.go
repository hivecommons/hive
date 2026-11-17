package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// projectbluefin/utah#1272: the real task_complete handler parks a wrong_base
// verdict whose target is not a vN branch with ONE note that quotes the
// agent's reason. The generic needs-decision question is not posted beside
// it, and an agent reason that reads like a repository confirmation gate does
// not re-book the park as idle.
func TestWrongBaseParkWireShape(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(`{"github_username":"Karibhou"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatalf("register response: %v", err)
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMsg(t, conn)
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "pi"})
	readMsg(t, conn)

	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{{
		Name: "widgets", Full: "acme/widgets",
		ActionableIssues: []any{noWorkIssue(1272, "Package refresh across the testing line", time.Now().Add(-24*time.Hour))},
	}}}
	s.statusMu.Unlock()

	hub := s.contributeHub
	type park struct {
		repo         string
		number       int
		marker, body string
	}
	parks := make(chan park, 2)
	hub.wrongBaseParker = func(_ context.Context, repo string, number int, marker, body string) error {
		parks <- park{repo, number, marker, body}
		return nil
	}
	questions := make(chan string, 2)
	hub.needsDecisionParkPoster = func(_ context.Context, _ string, _ int, _ time.Time, body string) error {
		questions <- body
		return nil
	}
	gate := newToolingReleaseFixture()
	hub.gateParkReleaser = gate.release

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.Number != 1272 {
		t.Fatalf("expected task_assign for #1272, got %+v", assign)
	}
	// The relay keeps only a vN target, so a "testing" answer arrives with no
	// target branch and the agent's words in the reason.
	conn.WriteJSON(WSMessage{
		Type: "task_complete", TaskID: assign.TaskID, Result: "completed",
		CompletionSignal: completionSignalVerdict,
		Verdict:          "wrong_base",
		VerdictReason:    "testing — " + gateParkReason,
	})
	select {
	case p := <-parks:
		if p.repo != "acme/widgets" || p.number != 1272 || p.marker != wrongBaseParkMarker(assign.TaskID) {
			t.Fatalf("park call = %+v", p)
		}
		for _, want := range []string{"@Karibhou's agent", "The agent said: “testing — Implementation is paused", "/hive decision"} {
			if !strings.Contains(p.body, want) {
				t.Errorf("park note missing %q: %s", want, p.body)
			}
		}
		if strings.Contains(p.body, "release branch was supplied") {
			t.Errorf("park note blames a missing release branch: %s", p.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a wrong_base verdict with no vN target must park with a note")
	}
	select {
	case q := <-questions:
		t.Fatalf("the wrong-base note is the park's question; a second one was posted: %s", q)
	case <-gate.released:
		t.Fatal("a wrong-base park must not be re-booked as a gate park")
	case <-time.After(300 * time.Millisecond):
	}
}

package dashboard

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
)

func clarifyChatServer(t *testing.T, phaseClarify bool) (*Server, *[]string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	eng := knowledge.NewInceptionEngine(t.TempDir(), nil, logger)
	if _, err := eng.Start("a todo app"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if phaseClarify {
		if err := eng.SetQuestions([]knowledge.Question{{ID: "language", Text: "Which language?"}}); err != nil {
			t.Fatalf("SetQuestions: %v", err)
		}
	}
	var submitted []string
	s.RegisterAPI(&Dependencies{
		Config:    &config.Config{},
		Inception: eng,
		DashboardChatSubmit: func(user, role, text string) (uint64, error) {
			submitted = append(submitted, role+":"+text)
			return 7, nil
		},
	})
	return s, &submitted
}

// #13215: while clarify has unanswered questions, an owner's plain reply goes
// to the chat spine (which submits it as the answer) instead of the fallback.
func TestHandleChatRoutesPlainReplyToSpineDuringClarify(t *testing.T) {
	s, submitted := clarifyChatServer(t, true)
	rec := doPostWithRole(s, "/api/chat", map[string]interface{}{"query": "Go, single static binary"}, config.RoleOwner)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/chat = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["accepted"] != true || body["status"] == "responder_unavailable" {
		t.Fatalf("plain reply not forwarded to spine: %v", body)
	}
	if len(*submitted) != 1 || (*submitted)[0] != "owner:Go, single static binary" {
		t.Fatalf("submitted = %v", *submitted)
	}
}

func TestHandleChatPlainReplyNotRoutedOutsideClarifyOrBelowOwner(t *testing.T) {
	s, submitted := clarifyChatServer(t, false)
	rec := doPostWithRole(s, "/api/chat", map[string]interface{}{"query": "Go, single static binary"}, config.RoleOwner)
	// v5's router answers an unrouted plain question with the responder-unavailable
	// usage reply instead of v6's "fallback" status; either way it must not reach the spine.
	if got := decodeJSON(t, rec)["status"]; got != "responder_unavailable" {
		t.Fatalf("capture phase: status = %v, want responder_unavailable", got)
	}

	s2, submitted2 := clarifyChatServer(t, true)
	rec = doPostWithRole(s2, "/api/chat", map[string]interface{}{"query": "Go, single static binary"}, config.RoleReadWrite)
	if got := decodeJSON(t, rec)["status"]; got != "responder_unavailable" {
		t.Fatalf("read-write caller: status = %v, want responder_unavailable", got)
	}
	if len(*submitted) != 0 || len(*submitted2) != 0 {
		t.Fatalf("unexpected submissions: %v %v", *submitted, *submitted2)
	}
}

func TestInceptionWatcherPhaseChangeHookFiresOnClarify(t *testing.T) {
	w, eng, _ := covFWatcher(t)
	fired := make(chan struct{}, 4)
	w.SetPhaseChangeHook(func() { fired <- struct{}{} })
	if _, err := eng.Start("a todo app"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	w.poll(w.ctx)
	waitHook := func(want bool) {
		t.Helper()
		select {
		case <-fired:
			if !want {
				t.Fatal("hook fired without a phase change")
			}
		case <-time.After(200 * time.Millisecond):
			if want {
				t.Fatal("hook did not fire on phase change")
			}
		}
	}
	waitHook(true)
	w.poll(w.ctx)
	waitHook(false)
	if err := eng.SetQuestions([]knowledge.Question{{ID: "q1", Text: "Which language?"}}); err != nil {
		t.Fatalf("SetQuestions: %v", err)
	}
	w.poll(w.ctx)
	waitHook(true)
}

func doPostWithRole(s *Server, path string, body interface{}, role string) *httptest.ResponseRecorder {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, &b)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", role)
	s.mux.ServeHTTP(rec, req)
	return rec
}

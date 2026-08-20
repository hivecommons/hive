package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// contribute_ws_progress_model_test.go — kubestellar/hive#4117: the relay
// re-detects the running model from the CLI's own session transcript and
// piggybacks optional model/reasoning_effort fields on periodic task_progress
// reports. The hub must update contributor.model / contributor.reasoningEffort
// (and the profile mirrors) when the fields are present, so a mid-session
// `/model` switch is reflected instead of staying stuck at the connect-time
// auth_response value — and must change NOTHING when the fields are absent
// (an older relay omits them).

// authWithModel registers a contributor and authenticates its WS connection,
// declaring the given connect-time model/effort in auth_response.
func authWithModel(t *testing.T, s *Server, ts *httptest.Server, username, model, effort string) (*websocket.Conn, map[string]string) {
	t.Helper()
	body := `{"github_username":"` + username + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatalf("register decode: %v", err)
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	readMsg(t, conn) // auth_challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude", Model: model, ReasoningEffort: effort})
	readMsg(t, conn) // auth_ok
	return conn, reg
}

// waitForModel polls the contributor connection until its model matches want
// (or the deadline passes), returning the final observed model/effort pair.
func waitForModel(h *ContributeWSHub, contributorID, want string, window time.Duration) (string, string) {
	deadline := time.Now().Add(window)
	var model, effort string
	for {
		h.mu.RLock()
		for _, c := range h.connections {
			c.mu.Lock()
			if c.profile != nil && c.profile.ContributorID == contributorID {
				model, effort = c.model, c.reasoningEffort
			}
			c.mu.Unlock()
		}
		h.mu.RUnlock()
		if model == want || time.Now().After(deadline) {
			return model, effort
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTaskProgressUpdatesModelMidSession proves the #4117 consume path: a
// task_progress carrying model/reasoning_effort for the connection's held task
// updates the connection record, so the mid-session value supersedes the
// connect-time one.
func TestTaskProgressUpdatesModelMidSession(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	conn, reg := authWithModel(t, s, ts, "modelswitcher", "claude-sonnet-5", "medium")
	defer conn.Close()

	holder := mustConn(t, s.contributeHub, reg["contributor_id"])
	if holder.model != "claude-sonnet-5" || holder.reasoningEffort != "medium" {
		t.Fatalf("connect-time model/effort not recorded: got %q/%q", holder.model, holder.reasoningEffort)
	}

	// The hub tracks a task on this connection (routine-progress branch, not
	// the lease-resume branch).
	holder.mu.Lock()
	holder.currentTask = &WSTaskAssign{TaskID: "ct-4117", Kind: "issue", Repo: "myorg/repo1", Number: 1}
	holder.currentTaskGen = 7
	holder.mu.Unlock()

	// Mid-session the relay's periodic tick detects a model switch and
	// piggybacks the new value on its routine progress report.
	conn.WriteJSON(WSMessage{Type: "task_progress", TaskID: "ct-4117", TaskGen: 7, Status: "working", Model: "claude-opus-5", ReasoningEffort: "high"})

	model, effort := waitForModel(s.contributeHub, reg["contributor_id"], "claude-opus-5", 2*time.Second)
	if model != "claude-opus-5" {
		t.Fatalf("task_progress model was not consumed: connection still reports %q (want claude-opus-5)", model)
	}
	if effort != "high" {
		t.Fatalf("task_progress reasoning_effort was not consumed: connection reports %q (want high)", effort)
	}
	holder.mu.Lock()
	profModel, profEffort := holder.profile.Model, holder.profile.ReasoningEffort
	holder.mu.Unlock()
	if profModel != "claude-opus-5" || profEffort != "high" {
		t.Fatalf("profile mirrors not updated: got %q/%q", profModel, profEffort)
	}
}

// TestTaskProgressWithoutModelChangesNothing pins the no-regression leg: an
// older relay (or a backend with no detectable transcript — codex, agy, goose,
// pi, aider, litellm) omits the fields, and the connect-time values must
// survive untouched. An empty string must never clobber a known model.
func TestTaskProgressWithoutModelChangesNothing(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	conn, reg := authWithModel(t, s, ts, "legacyrelay", "gpt-5.4", "low")
	defer conn.Close()

	holder := mustConn(t, s.contributeHub, reg["contributor_id"])
	holder.mu.Lock()
	holder.currentTask = &WSTaskAssign{TaskID: "ct-4117-b", Kind: "issue", Repo: "myorg/repo1", Number: 2}
	holder.currentTaskGen = 3
	holder.mu.Unlock()

	// Progress with NO model fields — today's relay shape for unsupported
	// backends and for every relay written before #4117.
	conn.WriteJSON(WSMessage{Type: "task_progress", TaskID: "ct-4117-b", TaskGen: 3, Status: "working", TmuxOutput: []string{"still working"}})

	// Wait until the progress was definitely consumed (tmuxOutput recorded),
	// then assert the model/effort are untouched.
	deadline := time.Now().Add(2 * time.Second)
	for {
		holder.mu.Lock()
		consumed := len(holder.tmuxOutput) == 1 && holder.tmuxOutput[0] == "still working"
		holder.mu.Unlock()
		if consumed || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	holder.mu.Lock()
	model, effort := holder.model, holder.reasoningEffort
	holder.mu.Unlock()
	if model != "gpt-5.4" || effort != "low" {
		t.Fatalf("a task_progress without model fields mutated model/effort: got %q/%q (want gpt-5.4/low)", model, effort)
	}
}

// TestTaskProgressStaleGenerationDoesNotUpdateModel: the #2568 Gate runs
// BEFORE the #4117 consume — a stale-generation straggler must not overwrite
// the current owner's model either.
func TestTaskProgressStaleGenerationDoesNotUpdateModel(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	conn, reg := authWithModel(t, s, ts, "staleworker", "claude-sonnet-5", "")
	defer conn.Close()

	holder := mustConn(t, s.contributeHub, reg["contributor_id"])
	holder.mu.Lock()
	holder.currentTask = &WSTaskAssign{TaskID: "ct-4117-c", Kind: "issue", Repo: "myorg/repo1", Number: 3}
	holder.currentTaskGen = 9
	holder.mu.Unlock()

	// A straggler echoing an OLDER generation carries a model — it must be
	// rejected wholesale by the Gate, model included.
	conn.WriteJSON(WSMessage{Type: "task_progress", TaskID: "ct-4117-c", TaskGen: 4, Status: "working", Model: "spoofed-model"})

	model, _ := waitForModel(s.contributeHub, reg["contributor_id"], "spoofed-model", 400*time.Millisecond)
	if model == "spoofed-model" {
		t.Fatalf("a stale-generation task_progress updated the model — the #2568 Gate must reject the whole message")
	}
	if model != "claude-sonnet-5" {
		t.Fatalf("model unexpectedly changed to %q", model)
	}
}

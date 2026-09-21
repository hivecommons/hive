package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/internal/testutil"
)

// contribute_ws_advisor_model_test.go — hivecommons/hive#7760: an omp
// contributor runs a primary model AND an advisor, and the relay now reports
// both (model/reasoning_effort for the primary, advisor_model /
// advisor_reasoning_effort for the reviewer) on auth_response and on every
// task_progress. The hub has to carry the advisor pair to the same places the
// primary already reaches — the connection, the profile, the fleet snapshot,
// the run rows, the activity rail and the PR trailer — and change nothing for a
// relay that sends neither field.

// authWithAdvisor registers a contributor and authenticates it declaring an omp
// primary plus advisor pair.
func authWithAdvisor(t *testing.T, s *Server, ts *httptest.Server, username string) (*websocket.Conn, map[string]string) {
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
	conn.WriteJSON(WSMessage{
		Type: "auth_response", RegistrationToken: reg["registration_token"],
		CLIBackend: "omp", Model: "openai-codex/gpt-5.6-terra", ReasoningEffort: "medium",
		AdvisorModel: "anthropic/claude-opus-5", AdvisorReasoningEffort: "high",
	})
	readMsg(t, conn) // auth_ok
	return conn, reg
}

func TestAuthResponseRecordsAdvisorPair(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	conn, reg := authWithAdvisor(t, s, ts, "ompadvisor")
	defer conn.Close()

	holder := mustConn(t, s.contributeHub, reg["contributor_id"])
	holder.mu.Lock()
	gotModel, gotEffort := holder.advisorModel, holder.advisorEffort
	profModel, profEffort := holder.profile.AdvisorModel, holder.profile.AdvisorEffort
	holder.mu.Unlock()
	if gotModel != "anthropic/claude-opus-5" || gotEffort != "high" {
		t.Fatalf("connection advisor pair = %q/%q, want anthropic/claude-opus-5/high", gotModel, gotEffort)
	}
	if profModel != "anthropic/claude-opus-5" || profEffort != "high" {
		t.Fatalf("profile advisor pair = %q/%q, want anthropic/claude-opus-5/high", profModel, profEffort)
	}

	// The fleet snapshot is where the operator sees it.
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/fleet", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	fleet := w.Body.String()
	for _, want := range []string{`"advisor_model":"anthropic/claude-opus-5"`, `"advisor_effort":"high"`, `"model":"openai-codex/gpt-5.6-terra"`, `"reasoning_effort":"medium"`} {
		if !strings.Contains(fleet, want) {
			t.Errorf("fleet snapshot lacks %s: %s", want, fleet)
		}
	}

	// And the activity rail's "joined" row carries it alongside the primary.
	// The hub appends the "joined" row AFTER sending auth_ok (contribute_ws.go:
	// handleAuth sends auth_ok, then calls addActivity), so an immediate check
	// races the server goroutine under CI load — poll instead.
	var joined ActivityEntry
	testutil.Eventually(t, 2*time.Second, func() bool {
		s.contributeHub.activityMu.Lock()
		defer s.contributeHub.activityMu.Unlock()
		for i := range s.contributeHub.activity {
			if s.contributeHub.activity[i].Username == "ompadvisor" && s.contributeHub.activity[i].Action == "joined" {
				joined = s.contributeHub.activity[i]
				return true
			}
		}
		return false
	}, "no joined activity row for ompadvisor")
	if joined.AdvisorModel != "anthropic/claude-opus-5" || joined.AdvisorEffort != "high" {
		t.Errorf("joined row advisor = %q/%q, want anthropic/claude-opus-5/high", joined.AdvisorModel, joined.AdvisorEffort)
	}
	if joined.Model != "openai-codex/gpt-5.6-terra" || joined.Effort != "medium" {
		t.Errorf("joined row primary = %q/%q — the advisor must not displace the primary", joined.Model, joined.Effort)
	}
}

func TestAuthResponseWithoutAdvisorLeavesEveryFieldEmpty(t *testing.T) {
	// A single-model backend, or an older relay, sends neither field: nothing
	// new appears anywhere — the fleet JSON for such a contributor is
	// byte-identical to before the fields existed.
	s, ts := setupWSTest(t)
	defer ts.Close()

	conn, reg := authWithModel(t, s, ts, "singlemodel", "claude-sonnet-5", "medium")
	defer conn.Close()

	holder := mustConn(t, s.contributeHub, reg["contributor_id"])
	holder.mu.Lock()
	adv := holder.advisorModel + holder.advisorEffort + holder.profile.AdvisorModel + holder.profile.AdvisorEffort
	holder.mu.Unlock()
	if adv != "" {
		t.Fatalf("a relay that declared no advisor must record none, got %q", adv)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/fleet", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), "advisor") {
		t.Fatalf("fleet snapshot must omit the advisor fields entirely when none was declared: %s", w.Body.String())
	}
}

func TestAuthResponseAdvisorIsSanitized(t *testing.T) {
	// Client text that is re-serialized into every fleet poll and lands in PR
	// trailers: HTML is stripped the way every other stored contributor string is.
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"tagadvisor"}`
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
	defer conn.Close()
	readMsg(t, conn)
	conn.WriteJSON(WSMessage{
		Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "omp",
		AdvisorModel: "<b>anthropic/claude-opus-5</b>", AdvisorReasoningEffort: " high<i></i> ",
	})
	readMsg(t, conn)
	holder := mustConn(t, s.contributeHub, reg["contributor_id"])
	holder.mu.Lock()
	gotModel, gotEffort := holder.advisorModel, holder.advisorEffort
	holder.mu.Unlock()
	if gotModel != "anthropic/claude-opus-5" || gotEffort != "high" {
		t.Fatalf("advisor pair not sanitized: %q/%q", gotModel, gotEffort)
	}
}

func TestTaskProgressRefreshesAdvisorPair(t *testing.T) {
	// The relay re-reads omp's own records every tick, so a changed advisor
	// reaches the hub the same way a mid-session /model switch does (#4117).
	s, ts := setupWSTest(t)
	defer ts.Close()

	conn, reg := authWithAdvisor(t, s, ts, "advisorswitch")
	defer conn.Close()

	holder := mustConn(t, s.contributeHub, reg["contributor_id"])
	holder.mu.Lock()
	holder.currentTask = &WSTaskAssign{TaskID: "ct-7760", Kind: "issue", Repo: "myorg/repo1", Number: 1}
	holder.currentTaskGen = 4
	holder.mu.Unlock()

	conn.WriteJSON(WSMessage{Type: "task_progress", TaskID: "ct-7760", TaskGen: 4, Status: "working",
		Model: "openai-codex/gpt-5.6-terra", ReasoningEffort: "medium",
		AdvisorModel: "anthropic/claude-sonnet-5", AdvisorReasoningEffort: "medium"})

	var gotModel, gotEffort string
	testutil.Eventually(t, 2*time.Second, func() bool {
		holder.mu.Lock()
		defer holder.mu.Unlock()
		gotModel, gotEffort = holder.advisorModel, holder.advisorEffort
		return gotModel == "anthropic/claude-sonnet-5"
	}, "task_progress advisor pair not consumed")
	if gotModel != "anthropic/claude-sonnet-5" || gotEffort != "medium" {
		t.Fatalf("task_progress advisor pair not consumed: %q/%q", gotModel, gotEffort)
	}
	holder.mu.Lock()
	profModel := holder.profile.AdvisorModel
	holder.mu.Unlock()
	if profModel != "anthropic/claude-sonnet-5" {
		t.Fatalf("profile advisor mirror not updated: %q", profModel)
	}

	// A later progress WITHOUT the fields (the shape of every relay before
	// #7760) must not clobber what is known.
	conn.WriteJSON(WSMessage{Type: "task_progress", TaskID: "ct-7760", TaskGen: 4, Status: "working", TmuxOutput: []string{"still working"}})
	testutil.Eventually(t, 2*time.Second, func() bool {
		holder.mu.Lock()
		defer holder.mu.Unlock()
		return len(holder.tmuxOutput) == 1 && holder.tmuxOutput[0] == "still working"
	}, "later progress without advisor fields was not consumed")
	holder.mu.Lock()
	gotModel, gotEffort = holder.advisorModel, holder.advisorEffort
	holder.mu.Unlock()
	if gotModel != "anthropic/claude-sonnet-5" || gotEffort != "medium" {
		t.Fatalf("a progress without advisor fields mutated the pair: %q/%q", gotModel, gotEffort)
	}
}

func TestAbandonedRunRowCarriesAdvisorPair(t *testing.T) {
	// Run rows are the durable record; the same pair the fleet shows has to
	// land there, on every outcome. The abandoned row is built by
	// appendAbandonedRun from the connection fields directly, so it is the
	// cheapest of the three to assert without a full task lifecycle.
	s, ts := setupWSTest(t)
	defer ts.Close()

	conn, reg := authWithAdvisor(t, s, ts, "advisorrun")
	defer conn.Close()

	holder := mustConn(t, s.contributeHub, reg["contributor_id"])
	task := &WSTaskAssign{TaskID: "ct-7760-run", Kind: "issue", Repo: "myorg/repo1", Number: 9}

	path := filepath.Join(t.TempDir(), "task_runs.jsonl")
	orig := taskRunLogPath
	taskRunLogPath = path
	t.Cleanup(func() { taskRunLogPath = orig })
	// v5 hubs resolve the run log per data root (taskRunLogFile); point that
	// at the scratch file too so the package-level override is honoured.
	origHubFile := s.contributeHub.taskRunLogFile
	s.contributeHub.taskRunLogFile = path
	t.Cleanup(func() { s.contributeHub.taskRunLogFile = origHubFile })
	s.contributeHub.appendAbandonedRun(holder, task, abandonCauseHandback, time.Now().Add(-time.Minute))

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read run log: %v", err)
	}
	var rec TaskRunRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatalf("decode run row: %v (%q)", err, string(data))
	}
	if rec.AdvisorModel != "anthropic/claude-opus-5" || rec.AdvisorEffort != "high" {
		t.Fatalf("run row advisor pair = %q/%q, want anthropic/claude-opus-5/high", rec.AdvisorModel, rec.AdvisorEffort)
	}
	if rec.Model != "openai-codex/gpt-5.6-terra" || rec.Effort != "medium" {
		t.Fatalf("run row primary = %q/%q — the advisor must not displace the primary", rec.Model, rec.Effort)
	}
}

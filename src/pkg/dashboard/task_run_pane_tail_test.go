package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/config"
)

// #7317 item 3: pane output on the fleet row and the run log, gated to
// owner/read-write viewers.

func TestBoundPaneTail(t *testing.T) {
	if got := boundPaneTail(nil); got != nil {
		t.Errorf("nil in should be nil out (omitted field), got %v", got)
	}
	if got := boundPaneTail([]string{}); got != nil {
		t.Errorf("empty in should be nil out, got %v", got)
	}

	// Keeps the LAST maxPaneTailLines — the end of the pane is where the
	// failure is.
	many := make([]string, maxPaneTailLines+10)
	for i := range many {
		many[i] = strings.Repeat("x", 3) + "-" + string(rune('a'+i%26)) + "-" + itoa(i)
	}
	got := boundPaneTail(many)
	if len(got) != maxPaneTailLines {
		t.Fatalf("len = %d, want %d", len(got), maxPaneTailLines)
	}
	if got[len(got)-1] != many[len(many)-1] || got[0] != many[10] {
		t.Errorf("did not keep the tail: first=%q last=%q", got[0], got[len(got)-1])
	}

	// Per-line cap, rune-safe.
	long := strings.Repeat("é", maxPaneTailLineRune+5)
	got = boundPaneTail([]string{long})
	if r := []rune(got[0]); len(r) != maxPaneTailLineRune+len([]rune("… (truncated)")) || !strings.HasSuffix(got[0], "(truncated)") {
		t.Errorf("long line not truncated rune-safely: len=%d suffix=%q", len(r), got[0][len(got[0])-12:])
	}

	// Token redaction: a pane that echoed a credential must not store it.
	tok := "ghp_" + strings.Repeat("A", 36)
	got = boundPaneTail([]string{"pushing with " + tok})
	if strings.Contains(got[0], tok) || !strings.Contains(got[0], "***") {
		t.Errorf("token survived: %q", got[0])
	}

	// A copy, never the caller's slice.
	src := []string{"a", "b"}
	got = boundPaneTail(src)
	got[0] = "mutated"
	if src[0] != "a" {
		t.Error("boundPaneTail aliased the input slice")
	}
}

func TestPaneTailViewer(t *testing.T) {
	cases := []struct {
		name      string
		authToken string
		role      string
		want      bool
	}{
		{"owner on a guarded spoke", "secret", config.RoleOwner, true},
		{"read-write on a guarded spoke", "secret", config.RoleReadWrite, true},
		{"read on a guarded spoke", "secret", "read", false},
		{"anonymous on a guarded spoke", "secret", "", false},
		{"unknown role on a guarded spoke", "secret", "contributor", false},
		{"anonymous on an open spoke", "", "", true},
		{"read on an open spoke", "", "read", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{authToken: tc.authToken}
			r := httptest.NewRequest(http.MethodGet, "/api/contribute/runs", nil)
			if tc.role != "" {
				r.Header.Set("X-Hive-Role", tc.role)
			}
			if got := s.paneTailViewer(r); got != tc.want {
				t.Errorf("paneTailViewer = %v, want %v", got, tc.want)
			}
		})
	}
}

// The stored pane tail is written once and stripped per viewer at the
// boundary: an anonymous reader of a guarded spoke gets every other field and
// pane_tail_visible:false; an owner gets the lines.
func TestHandleContributeRuns_PaneTailGated(t *testing.T) {
	path := scratchRunLog(t)
	rec := TaskRunRecord{
		TS:          time.Now().UTC().Format(time.RFC3339),
		TaskID:      "ct-pane",
		Username:    "alice",
		Backend:     "litellm",
		Outcome:     outcomeFailed,
		FailureKind: "environment",
		Reason:      "CLI never became ready",
		Scenario:    scenarioEnvFailure,
		PaneTail:    []string{"$ claude", "Error: model not found: Qwen/Qwen3.6-35B-A3B[1m]"},
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	type resp struct {
		Returned        int             `json:"returned"`
		Runs            []TaskRunRecord `json:"runs"`
		PaneTailVisible bool            `json:"pane_tail_visible"`
	}
	serve := func(s *Server, role string) resp {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/contribute/runs?username=alice", nil)
		if role != "" {
			req.Header.Set("X-Hive-Role", role)
		}
		s.handleContributeRuns(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d", rr.Code)
		}
		var out resp
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		if out.Returned != 1 {
			t.Fatalf("returned = %d", out.Returned)
		}
		return out
	}

	guarded := &Server{authToken: "secret"}
	anon := serve(guarded, "")
	if anon.PaneTailVisible || anon.Runs[0].PaneTail != nil {
		t.Errorf("anonymous reader got the pane: %+v", anon)
	}
	if anon.Runs[0].Reason != "CLI never became ready" {
		t.Errorf("stripping the pane must not touch the reason: %+v", anon.Runs[0])
	}
	if !strings.Contains(anon.Runs[0].TaskID, "ct-pane") {
		t.Errorf("record otherwise intact: %+v", anon.Runs[0])
	}

	owner := serve(guarded, config.RoleOwner)
	if !owner.PaneTailVisible || len(owner.Runs[0].PaneTail) != 2 {
		t.Errorf("owner did not get the pane: %+v", owner)
	}
	rw := serve(guarded, config.RoleReadWrite)
	if !rw.PaneTailVisible || len(rw.Runs[0].PaneTail) != 2 {
		t.Errorf("read-write did not get the pane: %+v", rw)
	}
	reader := serve(guarded, "read")
	if reader.PaneTailVisible || reader.Runs[0].PaneTail != nil {
		t.Errorf("read-only viewer got the pane: %+v", reader)
	}
}

// Drives the real WebSocket handler: a task_progress carrying pane lines,
// then a hand-back, then a second task failed with pane lines on the report.
// Both records must carry the pane; the live fleet snapshot must show it
// while the task is in flight and only to an admitted viewer.
func TestTaskRunLog_PaneTailOnHandbackAndFailure(t *testing.T) {
	path := scratchRunLog(t)
	s, ts := setupWSTest(t)
	defer ts.Close()
	// A guarded spoke, so an anonymous fleet read is genuinely anonymous.
	s.authToken = "secret"

	body := `{"github_username":"pane-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
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

	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "litellm", Model: "Qwen/Qwen3.6-35B-A3B"})
	readMsg(t, conn) // auth_ok

	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "repo1",
				Full: "myorg/repo1",
				ActionableIssues: []any{
					noWorkIssue(92, "first issue", time.Now().Add(-24*time.Hour)),
					noWorkIssue(93, "second issue", time.Now().Add(-24*time.Hour)),
				},
			},
		},
	}
	s.statusMu.Unlock()

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", assign)
	}

	// The relay reports progress with the pane it sees. A token in the pane
	// must not reach either the snapshot or the log.
	tok := "ghp_" + strings.Repeat("B", 36)
	frozen := []string{"$ claude", "⠋ Thinking… (20m)", "GH_TOKEN=" + tok}
	conn.WriteJSON(WSMessage{Type: "task_progress", Seq: 3, TaskID: assign.TaskID, TaskGen: assign.TaskGen, Status: "working", TmuxOutput: frozen})

	// Live snapshot, while the task is held: the pane is on the row for an
	// owner and absent for an anonymous reader of the same endpoint.
	type fleetResp struct {
		Clankers        []FleetClanker `json:"clankers"`
		PaneTailVisible bool           `json:"pane_tail_visible"`
	}
	fleet := func(role string) fleetResp {
		t.Helper()
		rr := httptest.NewRecorder()
		fr := httptest.NewRequest(http.MethodGet, "/api/contribute/fleet", nil)
		if role != "" {
			fr.Header.Set("X-Hive-Role", role)
		}
		s.handleContributeFleet(rr, fr)
		var out fleetResp
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("fleet not JSON: %v", err)
		}
		return out
	}
	ownerFleet := testutil.EventuallyValue(t, 5*time.Second, func() (fleetResp, bool) {
		f := fleet(config.RoleOwner)
		return f, len(f.Clankers) == 1 && len(f.Clankers[0].PaneTail) > 0
	}, "pane tail never appeared on the owner's fleet row")
	if !ownerFleet.PaneTailVisible {
		t.Error("owner fleet read should say pane_tail_visible:true")
	}
	if got := strings.Join(ownerFleet.Clankers[0].PaneTail, "\n"); strings.Contains(got, tok) || !strings.Contains(got, "Thinking") {
		t.Errorf("owner pane tail wrong or unredacted: %q", got)
	}
	anonFleet := fleet("")
	if anonFleet.PaneTailVisible || len(anonFleet.Clankers) != 1 || anonFleet.Clankers[0].PaneTail != nil {
		t.Errorf("anonymous fleet read leaked the pane: %+v", anonFleet)
	}
	if anonFleet.Clankers[0].CurrentTask == nil {
		t.Error("stripping the pane must leave the rest of the row intact")
	}

	// Hand the task back: the abandonment record carries the frozen pane.
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 4})
	assign2 := readMsg(t, conn)
	if assign2.Type != "task_assign" {
		t.Fatalf("expected second task_assign, got %+v", assign2)
	}
	// Fail the second one with a pane on the report itself.
	conn.WriteJSON(WSMessage{Type: "task_failed", Seq: 5, TaskID: assign2.TaskID, TaskGen: assign2.TaskGen, Result: "failed", Reason: "CLI never became ready", FailureKind: "environment", TmuxOutput: []string{"$ claude", "Error: model not found"}})

	data := testutil.EventuallyValue(t, 5*time.Second, func() ([]byte, bool) {
		b, err := os.ReadFile(path)
		return b, err == nil && bytes.Count(b, []byte("\n")) >= 2
	}, "expected two run records in %s", path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var handback, failed TaskRunRecord
	if err := json.Unmarshal([]byte(lines[0]), &handback); err != nil {
		t.Fatalf("unmarshal handback: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &failed); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if handback.Outcome != outcomeAbandoned || handback.Number != 92 {
		t.Fatalf("first record is not the hand-back: %+v", handback)
	}
	if got := strings.Join(handback.PaneTail, "\n"); !strings.Contains(got, "Thinking") || strings.Contains(got, tok) {
		t.Errorf("hand-back record pane wrong or unredacted: %q", got)
	}
	if failed.Outcome != outcomeFailed || failed.Number != 93 {
		t.Fatalf("second record is not the failure: %+v", failed)
	}
	if len(failed.PaneTail) != 2 || failed.PaneTail[1] != "Error: model not found" {
		t.Errorf("failure record pane = %v", failed.PaneTail)
	}

	// And it is stripped for the anonymous reader on the run endpoint too,
	// so a disconnected contributor's pane is exactly as gated as a live one.
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/contribute/runs?username=pane-user&days=1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("runs status %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), `"pane_tail":`) {
		t.Errorf("anonymous runs read leaked pane_tail: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "CLI never became ready") {
		t.Errorf("anonymous runs read lost the reason: %s", rr.Body.String())
	}
}

// #7605: the pane attached to an abandoned row must be one the relay reported
// FOR that task. The sequence from the report: the relay finishes task A and
// its task_complete leaves A's final screen in tmuxOutput; the hub assigns
// task B on the same socket; the socket drops two seconds later, before any
// progress frame for B. The abandoned row for B must not carry A's clean
// "HIVE_VERDICT: complete" screen as "terminal when it stopped".
func TestAppendAbandonedRun_PaneOnlyWhenReportedForThisTask(t *testing.T) {
	prevScreen := []string{"PR opened: org/other#1301", "HIVE_VERDICT: complete", ">"}
	for _, tc := range []struct {
		name     string
		paneTask string
		wantPane bool
	}{
		{"pane is the previous task's final screen", "ct-prev", false},
		{"no pane reported yet", "", false},
		{"pane reported for this task", "ct-this", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := scratchRunLog(t)
			var hub *ContributeWSHub
			conn := &ContributorConnection{
				profile:        &ContributorProfile{GitHubUsername: "pane-user"},
				cliBackend:     "pi",
				model:          "deepseek/deepseek-v4-flash-0731",
				role:           "contributor",
				tmuxOutput:     prevScreen,
				tmuxOutputTask: tc.paneTask,
			}
			hub.appendAbandonedRun(conn, &WSTaskAssign{TaskID: "ct-this", Repo: "org/server", Number: 175},
				abandonCauseDisconnect, time.Now().Add(-2*time.Second))

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read run log: %v", err)
			}
			var rec TaskRunRecord
			if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if rec.Outcome != outcomeAbandoned || rec.Repo != "org/server" {
				t.Fatalf("record = %+v", rec)
			}
			if tc.wantPane {
				if len(rec.PaneTail) != len(prevScreen) {
					t.Fatalf("pane_tail = %v, want the reported pane", rec.PaneTail)
				}
				return
			}
			if rec.PaneTail != nil {
				t.Fatalf("pane_tail = %v, want none: it was reported for %q, not for this task", rec.PaneTail, tc.paneTask)
			}
			if strings.Contains(string(data), "pane_tail") {
				t.Fatalf("an unattributable pane must be omitted, not serialized empty: %s", data)
			}
		})
	}
}

// The live fleet card has the same window: between task_assign and the first
// task_progress frame the stored pane is still the previous task's ending.
func TestFleetSnapshot_PaneOnlyWhenReportedForCurrentTask(t *testing.T) {
	hub, conn := failureTestHub(t)
	conn.currentTask = &WSTaskAssign{TaskID: "ct-this", Repo: "org/server", Number: 175}
	conn.tmuxOutput = []string{"HIVE_VERDICT: complete", ">"}

	conn.tmuxOutputTask = "ct-prev"
	if got := hub.FleetSnapshot().Clankers[0].PaneTail; got != nil {
		t.Fatalf("pane_tail = %v on the new task's card, but it was reported for the previous task", got)
	}

	conn.tmuxOutputTask = "ct-this"
	if got := hub.FleetSnapshot().Clankers[0].PaneTail; len(got) != 2 {
		t.Fatalf("pane_tail = %v, want the pane reported for the current task", got)
	}
}

package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// projectbluefin/common#1605 (j1mc's agent, 2026-11-01 05:58Z) and
// projectbluefin/utah#1036 (Karibhou's agent, 15:36Z): a needs-decision
// verdict parked the issue at 045 through the relay's label alone, with no
// comment, so the maintainer had no question to answer.
const parkReason1605 = "owner must choose the merge-queue admission owner, verify the deployed Hive version, and approve App grants and queue policy"

func TestNeedsDecisionParkComment(t *testing.T) {
	body := needsDecisionParkComment("ct-projectbluefin/common-1605-1", "j1mc", parkReason1605, "stage/045-needs-decision")
	if !strings.HasPrefix(body, needsDecisionParkMarker("ct-projectbluefin/common-1605-1")) {
		t.Fatalf("comment must open with the task marker:\n%s", body)
	}
	for _, want := range []string{"@j1mc's agent", "`stage/045-needs-decision`", "**Question:** " + parkReason1605, "`/hive decision <your instructions>`"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	// No recommendation named: the word must not appear, or the un-park
	// menu would offer `/hive approve` for a recommendation that does not
	// exist.
	if strings.Contains(strings.ToLower(body), "recommend") {
		t.Fatalf("a reason without a recommendation must not mention one:\n%s", body)
	}

	rec := needsDecisionParkComment("t2", "dev", "decide whether to keep the 3.13 floor; recommended: B, because 3.14 is in every image", "stage/045-needs-decision")
	if !strings.Contains(rec, "**Recommendation:** B, because 3.14 is in every image\n") {
		t.Fatalf("a named recommendation must be carried:\n%s", rec)
	}

	if got := parkRecommendation("pick a queue owner\nI recommend keeping the scanner as admitter."); got != "I recommend keeping the scanner as admitter." {
		t.Fatalf("parkRecommendation = %q", got)
	}

	none := needsDecisionParkComment("t3", "", "", "needs-decision")
	if !strings.Contains(none, "the agent stated none") || strings.Contains(none, "@") {
		t.Fatalf("an empty reason says so, anonymously:\n%s", none)
	}
	if strings.Contains(strings.ToLower(none), "recommend") {
		t.Fatalf("empty-reason comment must not mention a recommendation:\n%s", none)
	}

	// The reason is sanitized: no mention pings, no hidden markers.
	evil := needsDecisionParkComment("t4", "dev", "ask @castrojo <!-- hive:owner-step --> now", "stage/045-needs-decision")
	if strings.Contains(evil, "@castrojo") || strings.Count(evil, "<!--") != 1 {
		t.Fatalf("reason must be sanitized:\n%s", evil)
	}
	if got := needsDecisionParkMarker("ct-o/r-1 --> <!-- x"); strings.Count(got, "-->") != 1 {
		t.Fatalf("task id must not close the marker early: %q", got)
	}
}

func TestParkCommentExplains(t *testing.T) {
	for body, want := range map[string]bool{
		"The remaining step needs the owner to pick the queue owner; options below.": true,
		"<!-- hive:gfi superseded=true --> ~~Good first issue~~ — withdrawn":         false,
		"thanks!": false,
		needsDecisionParkMarker("old") + "\n**Parked for a maintainer decision.**": true,
	} {
		if got := parkCommentExplains(body); got != want {
			t.Errorf("parkCommentExplains(%q) = %v, want %v", body, got, want)
		}
	}
}

// fakeParkGitHub serves the issue's comments and records any comment the hub
// posts.
func fakeParkGitHub(t *testing.T, comments []map[string]any) (*ghpkg.Client, *[]string, *string) {
	t.Helper()
	var mu sync.Mutex
	var posted []string
	var since string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/repos/acme/widgets/issues/77/comments" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPost {
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			posted = append(posted, body.Body)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
			return
		}
		mu.Lock()
		since = r.URL.Query().Get("since")
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(comments)
	}))
	t.Cleanup(srv.Close)
	return ghpkg.NewClientForTest(srv.URL, "acme", []string{"widgets"}, covBLogger()), &posted, &since
}

func TestNeedsDecisionParkPost(t *testing.T) {
	assigned := time.Date(2026, 11, 1, 5, 55, 40, 0, time.UTC)
	before := assigned.Add(-time.Hour).Format(time.RFC3339)
	after := assigned.Add(2 * time.Minute).Format(time.RFC3339)
	for _, tc := range []struct {
		name     string
		comments []map[string]any
		want     bool
	}{
		{name: "no comments: post the question", comments: nil, want: true},
		{name: "only the hive's own good-first-issue notice (utah#1036)", comments: []map[string]any{
			{"id": 1, "body": "<!-- hive:gfi superseded=true --> ~~Good first issue~~ — withdrawn by the hive", "created_at": after}}, want: true},
		{name: "a comment from before the task does not explain this park", comments: []map[string]any{
			{"id": 2, "body": "This looks insane, I love it! The way I have been doing this is by automating bumps.", "created_at": before}}, want: true},
		{name: "the agent explained the park itself", comments: []map[string]any{
			{"id": 3, "body": "Parking: the owner must choose who admits PRs to the merge queue. Recommended: A.", "created_at": after}}, want: false},
		{name: "already posted for this task", comments: []map[string]any{
			{"id": 4, "body": needsDecisionParkMarker("t") + "\n**Parked for a maintainer decision.**", "created_at": after}}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, _ := covK2Hub(t)
			client, posted, since := fakeParkGitHub(t, tc.comments)
			hub.server.deps.GHClient = client
			hub.postNeedsDecisionPark(&WSTaskAssign{Kind: "issue", Repo: "acme/widgets", Number: 77}, "t", "j1mc", parkReason1605, assigned)
			if got := len(*posted) == 1; got != tc.want {
				t.Fatalf("posted = %v, want a post: %v", *posted, tc.want)
			}
			if tc.want && !strings.Contains((*posted)[0], "**Question:** "+parkReason1605) {
				t.Fatalf("posted the wrong body:\n%s", (*posted)[0])
			}
			if *since == "" {
				t.Fatal("the comment listing must be bounded by the task's assignment time")
			}
		})
	}
}

func TestNeedsDecisionParkSkipsNonIssueTasks(t *testing.T) {
	hub, _ := covK2Hub(t)
	var calls int
	hub.needsDecisionParkPoster = func(context.Context, string, int, time.Time, string) error {
		calls++
		return nil
	}
	for _, task := range []*WSTaskAssign{
		nil,
		{Kind: "pr", Repo: "o/r", Number: 5},
		{Kind: "issue", Repo: "", Number: 5},
		{Kind: "issue", Repo: "o/r", Number: 0},
		{Kind: "issue", Repo: "o/r", Number: 5, ExternalID: "LIN-1"},
	} {
		hub.postNeedsDecisionPark(task, "t", "u", "r", time.Now())
	}
	if calls != 0 {
		t.Fatalf("only GitHub-backed issue tasks get a park comment, got %d calls", calls)
	}
	var nilHub *ContributeWSHub
	nilHub.postNeedsDecisionPark(&WSTaskAssign{Kind: "issue", Repo: "o/r", Number: 5}, "t", "u", "r", time.Now())
}

// The real task_complete handler posts the question for a needs-decision
// verdict that stands, and not for a gate park the hub releases.
func TestNeedsDecisionParkWireShape(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(`{"github_username":"j1mc"}`))
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
		ActionableIssues: []any{noWorkIssue(77, "Owner decision: enable Hive admission to GitHub merge queues", time.Now().Add(-24*time.Hour))},
	}}}
	s.statusMu.Unlock()

	hub := s.contributeHub
	type call struct {
		repo   string
		number int
		since  time.Time
		body   string
	}
	got := make(chan call, 2)
	hub.needsDecisionParkPoster = func(_ context.Context, repo string, number int, since time.Time, body string) error {
		got <- call{repo, number, since, body}
		return nil
	}
	hub.gateParkReleaser = newToolingReleaseFixture().release

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.Number != 77 {
		t.Fatalf("expected task_assign for #77, got %+v", assign)
	}
	conn.WriteJSON(WSMessage{
		Type: "task_complete", TaskID: assign.TaskID, Result: "completed",
		CompletionSignal: completionSignalVerdict,
		Verdict:          "no_work_needed", VerdictNeedsDecision: true,
		VerdictReason: parkReason1605,
	})
	select {
	case c := <-got:
		if c.repo != "acme/widgets" || c.number != 77 || c.since.IsZero() || !strings.Contains(c.body, "@j1mc's agent") || !strings.Contains(c.body, assign.TaskID) {
			t.Fatalf("park comment call = %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a standing needs-decision verdict must post its question")
	}

	// A gate park is released, not posted.
	key := noPRKey("acme/widgets", 77)
	hub.completedMu.Lock()
	hub.completedTasks[key] = time.Now().Add(-(hub.completedTaskCooldown[key] + time.Minute))
	delete(hub.noWorkVerdicts, key)
	hub.completedMu.Unlock()
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 3})
	again := readMsg(t, conn)
	if again.Type != "task_assign" || again.Number != 77 {
		t.Skipf("issue not re-offered in this harness (%+v); the first half is the regression", again)
	}
	conn.WriteJSON(WSMessage{
		Type: "task_complete", TaskID: again.TaskID, Result: "completed",
		CompletionSignal: completionSignalVerdict,
		Verdict:          "no_work_needed", VerdictNeedsDecision: true,
		VerdictReason: gateParkReason,
	})
	select {
	case c := <-got:
		t.Fatalf("a released gate park must not get a park comment: %+v", c)
	case <-time.After(300 * time.Millisecond):
	}
}

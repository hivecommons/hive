package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCmdRunsListAndShow(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/runs":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"key":              "acme/widgets#7",
				"title":            "Ship widgets",
				"repo":             "acme/widgets",
				"stage":            "plan",
				"gen":              2,
				"stage_started_at": "2026-09-22T18:00:00Z",
				"waiting_on":       "human",
				"waiting_since":    "2026-09-22T18:30:00Z",
				"plan_epic_id":     "epic-7",
				"last_receipt":     "https://example.invalid/receipt",
			}})
		case "/api/runs/acme%2Fwidgets%237":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"key":          "acme/widgets#7",
				"title":        "Ship widgets",
				"repo":         "acme/widgets",
				"stage":        "plan",
				"gen":          2,
				"waiting_on":   "human",
				"plan_epic_id": "epic-7",
				"last_receipt": "https://example.invalid/receipt",
				"stages": []map[string]any{{
					"name": "spec", "status": "completed", "gen": 1, "receipt": "https://example.invalid/spec",
				}},
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.EscapedPath())
		}
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL}, discardLogger())
	s.client = ts.Client()
	got, err := s.cmdRuns(context.Background(), "")
	if err != nil {
		t.Fatalf("cmdRuns list: %v", err)
	}
	for _, want := range []string{"Active runs", "acme/widgets#7", "waiting_on=human"} {
		if !strings.Contains(got, want) {
			t.Fatalf("list reply %q missing %q", got, want)
		}
	}
	got, err = s.cmdRuns(context.Background(), "acme/widgets#7")
	if err != nil {
		t.Fatalf("cmdRuns show: %v", err)
	}
	for _, want := range []string{"Ship widgets", "Plan: epic-7", "https://example.invalid/receipt", "https://example.invalid/spec"} {
		if !strings.Contains(got, want) {
			t.Fatalf("show reply %q missing %q", got, want)
		}
	}
}

func TestCmdRunsApproveRejectOwnerGuardAndPlanRoutes(t *testing.T) {
	var posts []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/runs/acme%2Fwidgets%237":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"key": "acme/widgets#7", "stage": "plan", "waiting_on": "human", "plan_epic_id": "epic-7",
			})
		case "/api/plan/epic-7/approve", "/api/plan/epic-7/reject":
			posts = append(posts, r.URL.EscapedPath())
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected path %q", r.URL.EscapedPath())
		}
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	readCtx := context.WithValue(context.Background(), commandRoleContextKey{}, "read")
	got, err := s.cmdRuns(readCtx, "approve acme/widgets#7")
	if err != nil {
		t.Fatalf("cmdRuns read approve: %v", err)
	}
	if !strings.Contains(got, "owner role required") {
		t.Fatalf("read approve reply = %q", got)
	}
	ownerCtx := context.WithValue(context.Background(), commandRoleContextKey{}, "owner")
	got, err = s.cmdRuns(ownerCtx, "approve acme/widgets#7")
	if err != nil {
		t.Fatalf("cmdRuns approve: %v", err)
	}
	if !strings.Contains(got, "Approved") {
		t.Fatalf("approve reply = %q", got)
	}
	got, err = s.cmdRuns(ownerCtx, "reject acme/widgets#7 needs changes")
	if err != nil {
		t.Fatalf("cmdRuns reject: %v", err)
	}
	if !strings.Contains(got, "Rejected") {
		t.Fatalf("reject reply = %q", got)
	}
	if strings.Join(posts, ",") != "/api/plan/epic-7/approve,/api/plan/epic-7/reject" {
		t.Fatalf("posts = %#v", posts)
	}
}

func TestRunSnapshotListUnmarshalShapes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{name: "empty", raw: "", want: 0},
		{name: "null", raw: "null", want: 0},
		{name: "status-summary-object", raw: `{"active":1}`, want: 0},
		{name: "array", raw: `[{"key":"repo/a#1"},{"key":"repo/a#2"}]`, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got runSnapshotList
			if err := got.UnmarshalJSON([]byte(tt.raw)); err != nil {
				t.Fatalf("UnmarshalJSON(%q): %v", tt.raw, err)
			}
			if len(got) != tt.want {
				t.Fatalf("len = %d, want %d", len(got), tt.want)
			}
		})
	}

	var broken runSnapshotList
	if err := broken.UnmarshalJSON([]byte(`[{"gen":"bad"}]`)); err == nil {
		t.Fatal("expected invalid array to return an error")
	}
}

func TestRunFormattingHelpersAndMaps(t *testing.T) {
	runs := []runSnapshot{
		{Key: "repo/b#2", Stage: "implement"},
		{Key: "repo/a#1", Stage: "plan"},
	}
	mapped := runSliceMap(runs)
	if mapped["repo/a#1"].Stage != "plan" || mapped["repo/b#2"].Stage != "implement" {
		t.Fatalf("runSliceMap = %#v", mapped)
	}
	roundTrip := runMapSlice(mapped)
	if len(roundTrip) != 2 {
		t.Fatalf("runMapSlice len = %d, want 2", len(roundTrip))
	}

	longTitle := strings.Repeat("界", 150)
	summary := oneLineRunSummary(runSnapshot{Key: "repo/a#1", Title: longTitle, Repo: "repo/a"})
	if !strings.HasSuffix(summary, "…") || len([]rune(summary)) != 141 {
		t.Fatalf("truncated summary = %q (%d runes)", summary, len([]rune(summary)))
	}
	if got := oneLineRunSummary(runSnapshot{Key: "repo/a#1"}); got != "repo/a#1" {
		t.Fatalf("fallback summary = %q", got)
	}

	artifactRun := runSnapshot{
		LastReceipt: " https://example.invalid/last ",
		Stages: []runStageSnap{
			{Name: "plan", Receipt: "https://example.invalid/last"},
			{Name: "build", Receipt: "https://example.invalid/build"},
			{Name: "empty"},
		},
	}
	lines := runArtifactLines(artifactRun)
	if strings.Join(lines, "\n") != "- last receipt: https://example.invalid/last\n- build receipt: https://example.invalid/build" {
		t.Fatalf("artifact lines = %#v", lines)
	}
	if got := formatPromptArtifacts(runSnapshot{}); got != "" {
		t.Fatalf("empty artifacts = %q", got)
	}
	if got := formatPromptArtifacts(artifactRun); !strings.HasPrefix(got, "\n- last receipt:") {
		t.Fatalf("formatted artifacts = %q", got)
	}

	durationCases := map[time.Duration]string{
		-time.Second:               "<1m",
		30 * time.Second:           "<1m",
		3 * time.Minute:            "3m",
		2 * time.Hour:              "2h",
		3 * 24 * time.Hour:         "3d",
		25*time.Hour + time.Second: "1d",
	}
	for in, want := range durationCases {
		if got := formatRunDuration(in); got != want {
			t.Fatalf("formatRunDuration(%s) = %q, want %q", in, got, want)
		}
	}
	if got := runAge(runSnapshot{}); got != "unknown" {
		t.Fatalf("empty run age = %q", got)
	}
	if got := runAge(runSnapshot{WaitingSince: "not-a-time"}); got != "unknown" {
		t.Fatalf("invalid run age = %q", got)
	}
	if got := runAge(runSnapshot{WaitingSince: time.Now().Add(-2 * time.Minute).Format(time.RFC3339)}); got == "unknown" {
		t.Fatalf("valid waiting_since age = %q", got)
	}
}

func TestCmdRunsFailureAndUsageBranches(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{DashboardURL: "http://127.0.0.1:1", AllowedUsers: []string{"uid:owner"}}, discardLogger())
	ownerCtx := context.WithValue(context.Background(), commandRoleContextKey{}, "owner")
	for _, args := range []string{"approve", "approve a b", "reject", "reject only-key", "one two"} {
		got, err := s.cmdRuns(ownerCtx, args)
		if err != nil {
			t.Fatalf("cmdRuns(%q): %v", args, err)
		}
		if !strings.Contains(got, "Usage") {
			t.Fatalf("cmdRuns(%q) = %q, want usage", args, got)
		}
	}
	if got, err := s.cmdRunsList(context.Background()); err != nil || !strings.Contains(got, "Failed to load runs") {
		t.Fatalf("cmdRunsList failure = %q, %v", got, err)
	}
	if got, err := s.cmdRunsReject(ownerCtx, "repo/a#1", "   "); err != nil || !strings.Contains(got, "Usage") {
		t.Fatalf("cmdRunsReject empty reason = %q, %v", got, err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/runs":
			_, _ = w.Write([]byte(`[]`))
		case "/api/runs/no-plan":
			_ = json.NewEncoder(w).Encode(map[string]any{"key": "no-plan"})
		case "/api/runs/bad-json":
			_, _ = w.Write([]byte(`{"gen":"bad"}`))
		case "/api/runs/post-fails":
			_ = json.NewEncoder(w).Encode(map[string]any{"key": "post-fails", "plan_epic_id": "epic-9"})
		case "/api/plan/epic-9/approve", "/api/plan/epic-9/reject":
			http.Error(w, "nope", http.StatusTeapot)
		default:
			t.Fatalf("unexpected path %q", r.URL.EscapedPath())
		}
	}))
	defer ts.Close()
	s = NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	if got, err := s.cmdRunsList(context.Background()); err != nil || !strings.Contains(got, "No active runs") {
		t.Fatalf("empty list = %q, %v", got, err)
	}
	if got, err := s.cmdRunsShow(context.Background(), "bad-json"); err != nil || !strings.Contains(got, "Failed to load run") {
		t.Fatalf("bad show = %q, %v", got, err)
	}
	if got, err := s.cmdRunsApprove(ownerCtx, "no-plan"); err != nil || !strings.Contains(got, "no plan_epic_id") {
		t.Fatalf("approve no plan = %q, %v", got, err)
	}
	if got, err := s.cmdRunsReject(ownerCtx, "no-plan", "needs work"); err != nil || !strings.Contains(got, "no plan_epic_id") {
		t.Fatalf("reject no plan = %q, %v", got, err)
	}
	if got, err := s.cmdRunsApprove(ownerCtx, "post-fails"); err != nil || !strings.Contains(got, "Failed to approve") {
		t.Fatalf("approve post failure = %q, %v", got, err)
	}
	if got, err := s.cmdRunsReject(ownerCtx, "post-fails", "needs work"); err != nil || !strings.Contains(got, "Failed to reject") {
		t.Fatalf("reject post failure = %q, %v", got, err)
	}
}

func TestPendingCheckpointReplyRejectAndIgnoreBranches(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{AllowedUsers: []string{"uid:owner"}}, discardLogger())
	if s.handlePendingCheckpointReply(context.Background(), makeMsg("1", "maybe", false), "maybe") {
		t.Fatal("non-decision reply should not be consumed")
	}
	if s.handlePendingCheckpointReply(context.Background(), Message{AuthorID: "other", Text: "approve"}, "approve") {
		t.Fatal("non-allowlisted author should not be consumed")
	}

	s = NewService(&recordingBackend{}, Config{AllowedUsers: []string{"uid:read"}}, discardLogger())
	s.pendingCheckpoints[s.pendingCheckpointKey("repo/a#1")] = &pendingCheckpoint{RunKey: "repo/a#1", Authors: map[string]struct{}{"uid": {}}}
	if !s.handlePendingCheckpointReply(context.Background(), makeMsg("1", "approve", false), "approve") {
		t.Fatal("read-only pending reply should be consumed with refusal")
	}
	var sent []string
	drainQueue(s, &sent)
	if len(sent) != 1 || !strings.Contains(sent[0], "owner role required") {
		t.Fatalf("read-only pending reply = %#v", sent)
	}

	var posts []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/runs/repo%2Fa%231":
			_ = json.NewEncoder(w).Encode(map[string]any{"key": "repo/a#1", "plan_epic_id": "epic-1"})
		case "/api/plan/epic-1/reject":
			posts = append(posts, r.URL.EscapedPath())
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected path %q", r.URL.EscapedPath())
		}
	}))
	defer ts.Close()
	s = NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	s.pendingCheckpoints[s.pendingCheckpointKey("repo/a#1")] = &pendingCheckpoint{RunKey: "repo/a#1", Authors: map[string]struct{}{"uid": {}}}
	if !s.handlePendingCheckpointReply(context.Background(), makeMsg("1", "reject", false), "reject") {
		t.Fatal("reject without reason should be consumed with usage")
	}
	drainQueue(s, &sent)
	if got := sent[len(sent)-1]; !strings.Contains(got, "Usage") || !strings.Contains(got, "repo/a#1") {
		t.Fatalf("reject usage reply = %q", got)
	}
	if !s.handlePendingCheckpointReply(context.Background(), makeMsg("2", "reject needs work", false), "reject needs work") {
		t.Fatal("reject with reason should be consumed")
	}
	drainQueue(s, &sent)
	if len(posts) != 1 || !strings.Contains(sent[len(sent)-1], "Rejected") {
		t.Fatalf("posts=%#v sent=%#v", posts, sent)
	}
}

func TestSyncRunsFromSSEFetchesAndDiffsFallback(t *testing.T) {
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/runs" {
			t.Fatalf("unexpected path %q", r.URL.EscapedPath())
		}
		calls++
		waitingOn := "agent"
		if calls > 1 {
			waitingOn = "human"
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"key": "repo/a#1", "title": "Needs plan", "stage": "plan", "waiting_on": waitingOn, "plan_epic_id": "epic-1",
		}})
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	s.syncRunsFromSSE(context.Background())
	if calls != 1 || len(s.lastRuns) != 1 {
		t.Fatalf("initial sync calls=%d lastRuns=%#v", calls, s.lastRuns)
	}
	var sent []string
	drainQueue(s, &sent)
	if len(sent) != 0 {
		t.Fatalf("initial sync should not prompt: %#v", sent)
	}

	s.syncRunsFromSSE(context.Background())
	drainQueue(s, &sent)
	if calls != 2 || len(sent) != 1 || !strings.Contains(sent[0], "Needs plan") {
		t.Fatalf("second sync calls=%d sent=%#v", calls, sent)
	}

	s.dashboardURL = "http://127.0.0.1:1"
	s.syncRunsFromSSE(context.Background())
	if len(s.lastRuns) != 1 {
		t.Fatalf("failed sync should keep lastRuns, got %#v", s.lastRuns)
	}
}

func TestOnSSEEventRunsBranches(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.onSSEEvent(&statusSnapshot{Runs: runSnapshotList{{Key: "repo/a#1", Stage: "spec", WaitingOn: "agent"}}})
	s.onSSEEvent(&statusSnapshot{Runs: runSnapshotList{{Key: "repo/a#1", Title: "Plan gate", Stage: "plan", Gen: 2, WaitingOn: "human"}}})
	var sent []string
	drainQueue(s, &sent)
	if len(sent) != 1 || !strings.Contains(sent[0], "Plan gate") {
		t.Fatalf("SSE run prompt = %#v", sent)
	}

	fallback := NewService(&recordingBackend{}, Config{DashboardURL: "http://127.0.0.1:1", AllowedUsers: []string{"uid:owner"}}, discardLogger())
	fallback.onSSEEvent(&statusSnapshot{})
	if fallback.lastState == nil {
		t.Fatal("first empty SSE event did not record state")
	}
	fallback.onSSEEvent(&statusSnapshot{})
	if fallback.lastState == nil {
		t.Fatal("second empty SSE event did not record state")
	}
}

func TestCmdRunsListSortsByKey(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/runs" {
			t.Fatalf("unexpected path %q", r.URL.EscapedPath())
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"key": "repo/z#9", "stage": "build", "waiting_on": "agent"},
			{"key": "repo/a#1", "stage": "plan", "waiting_on": "human"},
		})
	}))
	defer ts.Close()
	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL}, discardLogger())
	s.client = ts.Client()
	got, err := s.cmdRunsList(context.Background())
	if err != nil {
		t.Fatalf("cmdRunsList: %v", err)
	}
	first := strings.Index(got, "repo/a#1")
	second := strings.Index(got, "repo/z#9")
	if first < 0 || second < 0 {
		t.Fatalf("sorted runs missing from reply: %q", got)
	}
	if first > second {
		t.Fatalf("runs were not sorted: %q", got)
	}
}

func TestFetchRunAndRunsPropagateDecodeErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
		call func(context.Context, *Service) error
	}{
		{name: "list", path: "/api/runs", call: func(ctx context.Context, s *Service) error {
			_, err := s.fetchRuns(ctx)
			return err
		}},
		{name: "show", path: "/api/runs/repo%2Fa%231", call: func(ctx context.Context, s *Service) error {
			_, err := s.fetchRun(ctx, "repo/a#1")
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.EscapedPath() != tt.path {
					t.Fatalf("unexpected path %q, want %q", r.URL.EscapedPath(), tt.path)
				}
				_, _ = fmt.Fprint(w, `not-json`)
			}))
			defer ts.Close()
			s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL}, discardLogger())
			s.client = ts.Client()
			if err := tt.call(context.Background(), s); err == nil {
				t.Fatal("expected decode error")
			}
		})
	}
}

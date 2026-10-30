package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCmdInceptionStateReportsPhaseAndQuestions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Hive-Internal") != "dash-token" {
			t.Fatalf("missing dashboard internal auth: %q", r.Header.Get("X-Hive-Internal"))
		}
		if r.URL.Path != "/api/inception/state" {
			t.Fatalf("path = %q, want /api/inception/state", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"active": true,
			"state": map[string]any{
				"phase":     "clarify",
				"mode":      "greenfield",
				"idea_text": "Build a hive",
				"questions": []map[string]string{{"id": "q1", "text": "Who uses it?"}},
				"answers":   map[string]string{},
			},
		})
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, DashboardToken: "dash-token", AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	got, err := s.cmdInception(context.Background(), "state")
	if err != nil {
		t.Fatalf("cmdInception returned error: %v", err)
	}
	for _, want := range []string{"clarify", "Build a hive", "Who uses it?"} {
		if !strings.Contains(got, want) {
			t.Fatalf("state reply %q missing %q", got, want)
		}
	}
}

func TestCmdInceptionStateReportsKickError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"active": true,
			"state": map[string]any{
				"phase":      "capture",
				"mode":       "greenfield",
				"idea_text":  "Build a hive",
				"kick_error": "brainstorm could not be started: agent brainstorm not found",
			},
		})
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL}, discardLogger())
	s.client = ts.Client()
	got, err := s.cmdInception(context.Background(), "state")
	if err != nil {
		t.Fatalf("cmdInception returned error: %v", err)
	}
	if !strings.Contains(got, "❌ brainstorm could not be started") {
		t.Fatalf("state reply %q missing kick error", got)
	}
}

func TestCmdInceptionStartSurfacesBrainstormRefusal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"inception needs the brainstorm agent, which this hive does not run"}`, http.StatusConflict)
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL}, discardLogger())
	s.client = ts.Client()
	ctx := context.WithValue(context.Background(), commandRoleContextKey{}, "owner")
	got, err := s.cmdInception(ctx, "start build a hive")
	if err != nil {
		t.Fatalf("cmdInception returned error: %v", err)
	}
	if !strings.HasPrefix(got, "❌") || !strings.Contains(got, "brainstorm agent") {
		t.Fatalf("reply = %q, want brainstorm refusal", got)
	}
}

// newInceptionBatchServer serves a clarify-phase state with three questions and
// records every /api/inception/answer request body.
func newInceptionBatchServer(t *testing.T, posts *[]map[string]map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/inception/state":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"active": true,
				"state": map[string]any{
					"phase":     "clarify",
					"mode":      "greenfield",
					"idea_text": "build a thing",
					"questions": []map[string]string{{"id": "q1", "text": "First?"}, {"id": "q2", "text": "Second?"}, {"id": "q3", "text": "Third?"}},
					"answers":   map[string]string{},
				},
			})
		case "/api/inception/answer":
			var body map[string]map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode answer: %v", err)
			}
			*posts = append(*posts, body)
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
}

func newInceptionBatchService(ts *httptest.Server) *Service {
	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, DashboardToken: "dash-token", AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	return s
}

func TestCmdInceptionAnswerBatchesIntoOneSubmission(t *testing.T) {
	var posts []map[string]map[string]string
	ts := newInceptionBatchServer(t, &posts)
	defer ts.Close()
	s := newInceptionBatchService(ts)
	ctx := context.WithValue(context.Background(), commandRoleContextKey{}, "owner")

	for i, text := range []string{"Go", "Everyone"} {
		got, err := s.cmdInception(ctx, fmt.Sprintf("answer %d %s", i+1, text))
		if err != nil {
			t.Fatalf("cmdInception returned error: %v", err)
		}
		if !strings.Contains(got, fmt.Sprintf("Recorded answer %d (%d of 3)", i+1, i+1)) {
			t.Fatalf("reply = %q", got)
		}
		if len(posts) != 0 {
			t.Fatalf("answer %d was submitted before the batch was complete: %#v", i+1, posts)
		}
	}
	state, err := s.cmdInception(ctx, "state")
	if err != nil {
		t.Fatalf("cmdInception state returned error: %v", err)
	}
	if !strings.Contains(state, "1. First? ✅") || !strings.Contains(state, "2. Second? ✅") || strings.Contains(state, "3. Third? ✅") {
		t.Fatalf("state = %q, want buffered answers shown as answered", state)
	}

	got, err := s.cmdInception(ctx, "answer 3 CLI only")
	if err != nil {
		t.Fatalf("cmdInception returned error: %v", err)
	}
	if !strings.Contains(got, "Submitted 3 answers") {
		t.Fatalf("reply = %q", got)
	}
	if len(posts) != 1 {
		t.Fatalf("got %d submissions, want exactly 1: %#v", len(posts), posts)
	}
	want := map[string]string{"q1": "Go", "q2": "Everyone", "q3": "CLI only"}
	for id, v := range want {
		if posts[0]["answers"][id] != v {
			t.Fatalf("batch = %#v, want %s=%q", posts[0], id, v)
		}
	}
}

func TestCmdInceptionSubmitSendsPartialBuffer(t *testing.T) {
	var posts []map[string]map[string]string
	ts := newInceptionBatchServer(t, &posts)
	defer ts.Close()
	s := newInceptionBatchService(ts)
	ctx := context.WithValue(context.Background(), commandRoleContextKey{}, "owner")

	if got, _ := s.cmdInception(ctx, "submit"); !strings.Contains(got, "No buffered answers") || len(posts) != 0 {
		t.Fatalf("empty submit reply = %q, posts = %#v", got, posts)
	}
	if _, err := s.cmdInception(ctx, "answer 2 Everyone"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	got, err := s.cmdInception(ctx, "submit")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !strings.Contains(got, "Submitted 1 answers") || !strings.Contains(got, "remaining 2 questions use their defaults") {
		t.Fatalf("reply = %q", got)
	}
	if len(posts) != 1 || len(posts[0]["answers"]) != 1 || posts[0]["answers"]["q2"] != "Everyone" {
		t.Fatalf("posts = %#v, want one call with only q2", posts)
	}
}

func TestCmdInceptionSkipLetsBatchAutoSubmit(t *testing.T) {
	var posts []map[string]map[string]string
	ts := newInceptionBatchServer(t, &posts)
	defer ts.Close()
	s := newInceptionBatchService(ts)
	ctx := context.WithValue(context.Background(), commandRoleContextKey{}, "owner")

	for _, cmd := range []string{"answer 1 Go", "skip 2"} {
		if _, err := s.cmdInception(ctx, cmd); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
	}
	state, _ := s.cmdInception(ctx, "state")
	if !strings.Contains(state, "2. Second? ⏭️ skipped") {
		t.Fatalf("state = %q, want skipped question", state)
	}
	if len(posts) != 0 {
		t.Fatalf("submitted early: %#v", posts)
	}
	got, err := s.cmdInception(ctx, "answer 3 CLI only")
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if !strings.Contains(got, "Submitted 2 answers") || !strings.Contains(got, "remaining 1 questions use their defaults") {
		t.Fatalf("reply = %q", got)
	}
	if len(posts) != 1 || posts[0]["answers"]["q1"] != "Go" || posts[0]["answers"]["q3"] != "CLI only" || len(posts[0]["answers"]) != 2 {
		t.Fatalf("posts = %#v", posts)
	}
}

func TestCmdInceptionOwnerGuard(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{}, discardLogger())
	ctx := context.WithValue(context.Background(), commandRoleContextKey{}, "read-write")
	got, err := s.cmdInception(ctx, "start build a hive")
	if err != nil {
		t.Fatalf("cmdInception returned error: %v", err)
	}
	if !strings.Contains(got, "owner role required") {
		t.Fatalf("reply = %q, want owner refusal", got)
	}
}

func TestCmdInceptionMutatingSubcommands(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantPath string
		wantBody string
	}{
		{name: "start", args: "start build a hive", wantPath: "/api/inception/start", wantBody: "build a hive"},
		{name: "scan", args: "scan https://example.com/repo.git", wantPath: "/api/inception/scan", wantBody: "https://example.com/repo.git"},
		{name: "facts", args: `facts [{"title":"Vision","body":"Do it","type":"vision","tags":["chat"]}]`, wantPath: "/api/inception/facts", wantBody: "Vision"},
		{name: "approve", args: "approve", wantPath: "/api/inception/approve"},
		{name: "reset", args: "reset", wantPath: "/api/inception/reset"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath, gotBody string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				data, _ := io.ReadAll(r.Body)
				gotBody = string(data)
				w.WriteHeader(http.StatusOK)
			}))
			defer ts.Close()

			s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:owner"}}, discardLogger())
			s.client = ts.Client()
			ctx := context.WithValue(context.Background(), commandRoleContextKey{}, "owner")
			got, err := s.cmdInception(ctx, tt.args)
			if err != nil {
				t.Fatalf("cmdInception returned error: %v", err)
			}
			if !strings.Contains(got, "✅") {
				t.Fatalf("reply = %q, want success", got)
			}
			if gotPath != tt.wantPath {
				t.Fatalf("path = %q, want %q", gotPath, tt.wantPath)
			}
			if tt.wantBody != "" && !strings.Contains(gotBody, tt.wantBody) {
				t.Fatalf("body = %q, want substring %q", gotBody, tt.wantBody)
			}
		})
	}
}

func TestCmdInceptionUsageAndErrors(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{}, discardLogger())
	ctx := context.WithValue(context.Background(), commandRoleContextKey{}, "owner")
	for _, args := range []string{"", "bogus", "start", "scan", "answer nope text", "answer 0 text", "facts", "facts not-json"} {
		got, err := s.cmdInception(ctx, args)
		if err != nil {
			t.Fatalf("cmdInception(%q) returned error: %v", args, err)
		}
		if !strings.Contains(got, "❌") {
			t.Fatalf("cmdInception(%q) = %q, want refusal", args, got)
		}
	}
}

func TestCmdInceptionStateNoActiveRun(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "active": false})
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL}, discardLogger())
	s.client = ts.Client()
	got, err := s.cmdInception(context.Background(), "state")
	if err != nil {
		t.Fatalf("cmdInception returned error: %v", err)
	}
	if !strings.Contains(got, "No active") {
		t.Fatalf("reply = %q", got)
	}
}

func TestDiffInceptionQueuesQuestionsAndMaintainsPending(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{AllowedUsers: []string{"uid:owner", "reader:read"}}, discardLogger())
	prev := &statusSnapshot{Inception: inceptionSnapshot{Active: true, Phase: "capture"}}
	cur := &statusSnapshot{Inception: inceptionSnapshot{
		Active:    true,
		Phase:     "clarify",
		Questions: []inceptionQuestion{{ID: "q1", Text: "First?"}},
		Answers:   map[string]string{},
	}}
	s.diffInception(prev, cur)
	if s.pendingInterviews[s.pendingKey("uid")] == nil {
		t.Fatal("owner pending interview was not seeded")
	}
	if s.pendingInterviews[s.pendingKey("reader")] != nil {
		t.Fatal("read-only user should not get pending interview")
	}
	var sent []string
	drainQueue(s, &sent)
	if len(sent) != 1 || !strings.Contains(sent[0], "First?") {
		t.Fatalf("queued questions = %#v", sent)
	}

	s.diffInception(cur, &statusSnapshot{Inception: inceptionSnapshot{Active: true, Phase: "structure"}})
	if s.pendingInterviews[s.pendingKey("uid")] == nil {
		t.Fatal("structure keeps unanswered pending replies available")
	}
	s.pendingInterviews[s.pendingKey("uid")].Answers["q1"] = "done"
	s.diffInception(cur, &statusSnapshot{Inception: inceptionSnapshot{Active: true, Phase: "structure"}})
	if s.pendingInterviews[s.pendingKey("uid")] != nil {
		t.Fatal("completed pending interview should be cleared in structure")
	}
}

func TestInceptionPendingHelpers(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{}, discardLogger())
	s.registerBuiltinCommands()
	s.mu.RLock()
	_, registered := s.commands["inception"]
	s.mu.RUnlock()
	if !registered {
		t.Fatal("inception command was not registered")
	}

	s.pendingInterviews[s.pendingKey("uid")] = &pendingInterview{
		Questions: []inceptionQuestion{{ID: "q1", Text: "First?"}},
	}
	s.markPendingAnsweredForRole("q1")
	if got := s.pendingInterviews[s.pendingKey("uid")].Answers["q1"]; got != "answered" {
		t.Fatalf("marked answer = %q", got)
	}
	s.clearPendingInterviews()
	if len(s.pendingInterviews) != 0 {
		t.Fatalf("pending interviews not cleared: %#v", s.pendingInterviews)
	}
	if got := formatInceptionQuestions(nil, nil); !strings.Contains(got, "No pending") {
		t.Fatalf("empty question format = %q", got)
	}
}

func TestDiffInceptionClearsInactiveOrComplete(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.pendingInterviews[s.pendingKey("uid")] = &pendingInterview{Questions: []inceptionQuestion{{ID: "q1"}}}
	s.diffInception(&statusSnapshot{}, &statusSnapshot{Inception: inceptionSnapshot{Active: false}})
	if len(s.pendingInterviews) != 0 {
		t.Fatal("inactive inception should clear pending interviews")
	}
	s.pendingInterviews[s.pendingKey("uid")] = &pendingInterview{Questions: []inceptionQuestion{{ID: "q1"}}}
	s.diffInception(&statusSnapshot{}, &statusSnapshot{Inception: inceptionSnapshot{Active: true, Phase: "complete"}})
	if len(s.pendingInterviews) != 0 {
		t.Fatal("complete inception should clear pending interviews")
	}
}

func TestCmdInceptionAnswerRepliesPlainTextOnRejection(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/inception/state":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"active": true,
				"state": map[string]any{
					"phase":     "scaffold",
					"questions": []map[string]string{{"id": "q1", "text": "First?"}, {"id": "q2", "text": "Second?"}},
					"answers":   map[string]string{"q1": "Go"},
				},
			})
		case "/api/inception/answer":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"cannot submit answers in phase scaffold","ok":false}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, DashboardToken: "dash-token", AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	ctx := context.WithValue(context.Background(), commandRoleContextKey{}, "owner")
	got, err := s.cmdInception(ctx, "answer 2 Second")
	if err != nil {
		t.Fatalf("cmdInception returned error: %v", err)
	}
	if strings.Contains(got, "HTTP 400") || strings.Contains(got, `{"error"`) {
		t.Fatalf("reply leaks raw HTTP body: %q", got)
	}
	if !strings.Contains(got, "cannot submit answers in phase scaffold") || !strings.Contains(got, "remaining questions use their defaults") {
		t.Fatalf("reply = %q", got)
	}

	state, err := s.cmdInception(ctx, "state")
	if err != nil {
		t.Fatalf("cmdInception state returned error: %v", err)
	}
	if !strings.Contains(state, "Moving on with 1 of 2 answers") || strings.Contains(state, "Second?") {
		t.Fatalf("state = %q", state)
	}
}

func TestCmdInceptionSkipAllExplainsOneAnswerRequired(t *testing.T) {
	var posts []map[string]map[string]string
	ts := newInceptionBatchServer(t, &posts)
	defer ts.Close()
	s := newInceptionBatchService(ts)
	ctx := context.WithValue(context.Background(), commandRoleContextKey{}, "owner")

	for _, cmd := range []string{"skip 1", "skip 2"} {
		got, err := s.cmdInception(ctx, cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if !strings.Contains(got, "it will use its default") {
			t.Fatalf("%s reply = %q", cmd, got)
		}
	}
	got, err := s.cmdInception(ctx, "skip 3")
	if err != nil {
		t.Fatalf("skip 3: %v", err)
	}
	if !strings.Contains(got, "Skipped question 3") || !strings.Contains(got, "at least one answer") || strings.Contains(got, "No buffered answers") {
		t.Fatalf("final skip reply = %q, want the one-answer constraint", got)
	}
	if got, _ := s.cmdInception(ctx, "submit"); !strings.Contains(got, "at least one answer") || strings.Contains(got, "No buffered answers") {
		t.Fatalf("submit after skipping all = %q", got)
	}
	if len(posts) != 0 {
		t.Fatalf("empty batch was submitted: %#v", posts)
	}
	got, err = s.cmdInception(ctx, "answer 2 Everyone")
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if !strings.Contains(got, "Submitted 1 answers") || !strings.Contains(got, "remaining 2 questions use their defaults") {
		t.Fatalf("reply = %q", got)
	}
	if len(posts) != 1 || len(posts[0]["answers"]) != 1 || posts[0]["answers"]["q2"] != "Everyone" {
		t.Fatalf("posts = %#v", posts)
	}
}

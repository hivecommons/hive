package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCmdInceptionStateReportsPhaseAndQuestions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dash-token" {
			t.Fatalf("missing dashboard bearer auth: %q", r.Header.Get("Authorization"))
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

func TestCmdInceptionAnswerPostsQuestionID(t *testing.T) {
	var answerBody map[string]map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dash-token" {
			t.Fatalf("missing dashboard bearer auth: %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/inception/state":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"active": true,
				"state": map[string]any{
					"phase":     "clarify",
					"questions": []map[string]string{{"id": "q1", "text": "First?"}, {"id": "q2", "text": "Second?"}},
					"answers":   map[string]string{},
				},
			})
		case "/api/inception/answer":
			if err := json.NewDecoder(r.Body).Decode(&answerBody); err != nil {
				t.Fatalf("decode answer: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, DashboardToken: "dash-token", AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	ctx := context.WithValue(context.Background(), commandRoleContextKey{}, "owner")
	got, err := s.cmdInception(ctx, "answer 2 The second answer")
	if err != nil {
		t.Fatalf("cmdInception returned error: %v", err)
	}
	if !strings.Contains(got, "Answered question 2") {
		t.Fatalf("reply = %q", got)
	}
	if answerBody["answers"]["q2"] != "The second answer" {
		t.Fatalf("answer body = %#v, want q2", answerBody)
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

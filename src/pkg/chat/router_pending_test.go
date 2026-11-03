package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouteMessage_NonPrefixedDroppedWithoutPendingInterview(t *testing.T) {
	posts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	s.routeMessage(context.Background(), makeMsg("1", "plain answer", false))

	if posts != 0 {
		t.Fatalf("plain message without pending interview posted %d dashboard requests, want 0", posts)
	}
}

func TestRouteMessage_NonPrefixedConsumedOnlyForPendingAuthor(t *testing.T) {
	var posts int
	var got map[string]map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/inception/state":
			writeClarifyState(t, w, []map[string]string{{"id": "q1", "text": "First?"}})
		case "/api/inception/answer":
			posts++
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatalf("decode answer body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:owner", "other:owner"}}, discardLogger())
	s.client = ts.Client()
	s.pendingInterviews[s.pendingKey("uid")] = &pendingInterview{
		Questions: []inceptionQuestion{{ID: "q1", Text: "First?"}},
		Answers:   map[string]string{},
	}

	s.routeMessage(context.Background(), Message{ID: "2", Text: "not mine", AuthorID: "other"})
	if posts != 0 {
		t.Fatalf("non-pending author posted %d dashboard requests, want 0", posts)
	}

	s.routeMessage(context.Background(), makeMsg("3", "my answer", false))
	if posts != 1 {
		t.Fatalf("pending author posts = %d, want 1", posts)
	}
	if got["answers"]["q1"] != "my answer" {
		t.Fatalf("answer body = %#v, want q1=my answer", got)
	}
}

// Plain replies obey the same one-call rule as `!inception answer`: the
// engine leaves clarify on the first submission, so every reply is buffered
// and the batch goes out once when the last question is answered (#13216).
func TestRouteMessage_PlainRepliesBatchIntoOneSubmission(t *testing.T) {
	var posts []map[string]map[string]string
	ts := newInceptionBatchServer(t, &posts)
	defer ts.Close()
	s := newInceptionBatchService(ts)
	s.pendingInterviews[s.pendingKey("uid")] = &pendingInterview{
		Questions: []inceptionQuestion{{ID: "q1", Text: "First?"}, {ID: "q2", Text: "Second?"}, {ID: "q3", Text: "Third?"}},
		Answers:   map[string]string{},
	}
	ctx := context.WithValue(context.Background(), commandRoleContextKey{}, "owner")

	s.routeMessage(ctx, makeMsg("1", "Go", false))
	var sent []string
	drainQueue(s, &sent)
	if len(sent) != 1 || !strings.Contains(sent[0], "Recorded answer 1 (1 of 3)") {
		t.Fatalf("replies after first answer = %v", sent)
	}
	if len(posts) != 0 {
		t.Fatalf("first plain reply was submitted on its own: %#v", posts)
	}

	// A skipped question is not re-asked; the next plain reply lands on q3.
	if got, err := s.cmdInception(ctx, "skip 2"); err != nil || !strings.Contains(got, "Skipped question 2") {
		t.Fatalf("skip reply = %q, err = %v", got, err)
	}
	s.routeMessage(ctx, makeMsg("2", "CLI only", false))
	sent = nil
	drainQueue(s, &sent)
	if len(sent) != 1 || !strings.Contains(sent[0], "Submitted 2 answers") || !strings.Contains(sent[0], "remaining 1 questions use their defaults") {
		t.Fatalf("replies after last answer = %v", sent)
	}
	if len(posts) != 1 {
		t.Fatalf("got %d submissions, want exactly 1: %#v", len(posts), posts)
	}
	if posts[0]["answers"]["q1"] != "Go" || posts[0]["answers"]["q3"] != "CLI only" || posts[0]["answers"]["q2"] != "" {
		t.Fatalf("batch = %#v, want q1=Go q3=CLI only", posts[0])
	}
}

func TestRouteMessage_PlainReplyKeepsBufferWhenSubmitFails(t *testing.T) {
	answerStatus := http.StatusBadGateway
	posts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/inception/state":
			writeClarifyState(t, w, []map[string]string{{"id": "q1", "text": "First?"}, {"id": "q2", "text": "Second?"}})
		case "/api/inception/answer":
			posts++
			if answerStatus != http.StatusOK {
				http.Error(w, `{"error":"engine busy"}`, answerStatus)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	s.pendingInterviews[s.pendingKey("uid")] = &pendingInterview{
		Questions: []inceptionQuestion{{ID: "q1", Text: "First?"}, {ID: "q2", Text: "Second?"}},
		Answers:   map[string]string{},
	}

	s.routeMessage(context.Background(), makeMsg("1", "first", false))
	s.routeMessage(context.Background(), makeMsg("2", "second", false))
	var sent []string
	drainQueue(s, &sent)
	if posts != 1 {
		t.Fatalf("posts = %d, want 1 batched submission", posts)
	}
	if len(sent) != 2 || !strings.Contains(sent[1], "Failed to submit answers: engine busy") {
		t.Fatalf("replies = %v, want plain error text from the failed batch", sent)
	}

	answerStatus = http.StatusOK
	ctx := context.WithValue(context.Background(), commandRoleContextKey{}, "owner")
	got, err := s.cmdInception(ctx, "submit")
	if err != nil || !strings.Contains(got, "Submitted 2 answers") {
		t.Fatalf("submit after failure = %q, err = %v; buffered answers must survive a failed post", got, err)
	}
}

func writeClarifyState(t *testing.T, w http.ResponseWriter, questions []map[string]string) {
	t.Helper()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":     true,
		"active": true,
		"state": map[string]any{
			"phase":     "clarify",
			"mode":      "greenfield",
			"idea_text": "build a thing",
			"questions": questions,
			"answers":   map[string]string{},
		},
	})
}

// #13215: a dashboard-forwarded plain reply (TrustedRole set) seeds the
// interview from the live inception state when no status snapshot has seeded
// it yet, and an owner trusted role stands in while the allowlist is empty.
func TestRouteMessage_TrustedPlainReplySeedsInterviewFromState(t *testing.T) {
	var got map[string]map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/inception/state":
			writeClarifyState(t, w, []map[string]string{{"id": "language", "text": "Which language?"}, {"id": "users", "text": "Who?"}})
		case "/api/inception/answer":
			if got != nil {
				t.Fatalf("answers were submitted more than once")
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatalf("decode answer body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL}, discardLogger())
	s.client = ts.Client()
	s.routeMessage(context.Background(), Message{ID: "1", Text: "Go, single static binary", AuthorID: "local", TrustedRole: "owner"})
	if got != nil {
		t.Fatalf("first reply was submitted before the interview was complete: %#v", got)
	}
	if pending := s.pendingInterviews[s.pendingKey("local")]; pending == nil || pending.Answers["language"] != "Go, single static binary" {
		t.Fatalf("pending interview not seeded from state: %#v", pending)
	}

	s.routeMessage(context.Background(), Message{ID: "2", Text: "Developers", AuthorID: "local", TrustedRole: "owner"})
	if got["answers"]["language"] != "Go, single static binary" || got["answers"]["users"] != "Developers" {
		t.Fatalf("batch = %#v, want both answers in one submission", got)
	}
}

func TestRouteMessage_TrustedPlainReplyBelowOwnerIgnored(t *testing.T) {
	posts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL}, discardLogger())
	s.client = ts.Client()
	s.routeMessage(context.Background(), Message{ID: "1", Text: "Go", AuthorID: "local", TrustedRole: "read-write"})
	if posts != 0 {
		t.Fatalf("read-write trusted reply made %d dashboard requests, want 0", posts)
	}
}

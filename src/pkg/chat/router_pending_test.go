package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		if r.URL.Path != "/api/inception/answer" {
			t.Fatalf("path = %q, want /api/inception/answer", r.URL.Path)
		}
		posts++
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode answer body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
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

func TestRouteMessage_PendingReplyRetriesSameQuestionAfterPostFailure(t *testing.T) {
	posts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	s.pendingInterviews[s.pendingKey("uid")] = &pendingInterview{
		Questions: []inceptionQuestion{{ID: "q1", Text: "First?"}, {ID: "q2", Text: "Second?"}},
		Answers:   map[string]string{},
	}

	s.routeMessage(context.Background(), makeMsg("1", "first try", false))
	s.routeMessage(context.Background(), makeMsg("2", "second try", false))

	if posts != 2 {
		t.Fatalf("posts = %d, want 2 retries", posts)
	}
	pending := s.pendingInterviews[s.pendingKey("uid")]
	if pending.Answers["q1"] != "" || pending.Answers["q2"] != "" {
		t.Fatalf("failed posts should not mark answers: %#v", pending.Answers)
	}
}

// #13215: a dashboard-forwarded plain reply (TrustedRole set) seeds the
// interview from the live inception state when no status snapshot has seeded
// it yet, and an owner trusted role stands in while the allowlist is empty.
func TestRouteMessage_TrustedPlainReplySeedsInterviewFromState(t *testing.T) {
	var got map[string]map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/inception/state":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"active": true,
				"state": map[string]any{
					"phase":     "clarify",
					"questions": []map[string]string{{"id": "language", "text": "Which language?"}, {"id": "users", "text": "Who?"}},
					"answers":   map[string]string{},
				},
			})
		case "/api/inception/answer":
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
	if got["answers"]["language"] != "Go, single static binary" {
		t.Fatalf("answer body = %#v, want language answered", got)
	}

	got = nil
	s.routeMessage(context.Background(), Message{ID: "2", Text: "Developers", AuthorID: "local", TrustedRole: "owner"})
	if got["answers"]["users"] != "Developers" {
		t.Fatalf("second answer body = %#v, want users answered", got)
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

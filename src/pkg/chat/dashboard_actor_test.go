package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDashboardChatActor(t *testing.T) {
	for _, command := range []string{"!kick worker fix it", "!runs approve repo/a#1", "!runs reject repo/a#1 needs work", "!inception start an idea", "approve", "reject needs work"} {
		t.Run(command, func(t *testing.T) {
			for _, author := range []string{"alice", "bob"} {
				var methods []string
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					methods = append(methods, r.Method)
					if got := r.Header.Get("X-Hive-Chat-Actor"); got != "test:"+author {
						t.Errorf("actor = %q, want test:%s", got, author)
					}
					if got := r.Header.Get("X-Hive-Internal"); got != "secret" {
						t.Errorf("X-Hive-Internal = %q", got)
					}
					if r.Method == http.MethodGet {
						writeRunCheckpointPayload(w, "repo/a#1", "Task", 2)
					}
				}))
				s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, DashboardToken: "secret", AllowedUsers: []string{"alice:owner", "bob:owner"}}, discardLogger())
				s.registerBuiltinCommands()
				s.SetAgentNames([]string{"worker"})
				s.pendingCheckpoints[s.pendingCheckpointKey("repo/a#1")] = &pendingCheckpoint{RunKey: "repo/a#1", Authors: map[string]struct{}{author: {}}}
				s.Deliver(context.Background(), Message{AuthorID: author, Text: command})
				ts.Close()
				posted := false
				for _, method := range methods {
					posted = posted || method == http.MethodPost
				}
				if !posted {
					t.Fatal("command did not POST")
				}
			}
		})
	}
}

func TestDashboardBackgroundRequestHasNoChatActor(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Hive-Chat-Actor"); got != "" {
			t.Errorf("background actor = %q", got)
		}
	}))
	defer ts.Close()
	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL, DashboardToken: "secret"}, discardLogger())
	if _, err := s.dashboardGet(context.Background(), "/api/status"); err != nil {
		t.Fatal(err)
	}
	if err := s.dashboardPost(context.Background(), "/api/test", nil); err != nil {
		t.Fatal(err)
	}
}

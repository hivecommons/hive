package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// liftedHoldServer serves canned comments and hold-label issue events for
// LiftedHoldWasSelfAuthorization tests on acme/widget#7.
type liftedHoldServer struct {
	comments    []map[string]any
	events      []map[string]any
	failEvents  bool
	failComment bool
}

func (s *liftedHoldServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			if s.failComment {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(s.comments)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/7/events":
			if s.failEvents {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(s.events)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newLiftedHoldClient(t *testing.T, s *liftedHoldServer) *Client {
	t.Helper()
	c := NewClientForTest(s.start(t).URL, "acme/widget", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)
	return c
}

func selfAuthNoticeComment(author, createdAt string) map[string]any {
	return map[string]any{
		"body":       SelfAuthorizationNoticeMarker + "\nHeld for human sign-off on the direction",
		"user":       map[string]string{"login": author},
		"created_at": createdAt,
	}
}

func holdEvent(kind, actor, createdAt string) map[string]any {
	return map[string]any{
		"event":      kind,
		"actor":      map[string]string{"login": actor},
		"label":      map[string]string{"name": "hold"},
		"created_at": createdAt,
	}
}

func TestLiftedHoldWasSelfAuthorization_BotHoldLifted(t *testing.T) {
	s := &liftedHoldServer{
		comments: []map[string]any{selfAuthNoticeComment(testHiveAppBotLogin, "2026-09-15T12:00:00Z")},
		events: []map[string]any{
			holdEvent("labeled", testHiveAppBotLogin, "2026-09-15T11:00:00Z"),
			holdEvent("unlabeled", "alice", "2026-09-15T13:00:00Z"),
		},
	}
	c := newLiftedHoldClient(t, s)
	ok, err := c.LiftedHoldWasSelfAuthorization(context.Background(), "acme/widget", 7)
	if err != nil || !ok {
		t.Fatalf("LiftedHoldWasSelfAuthorization = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestLiftedHoldWasSelfAuthorization_HumanReholdProtected(t *testing.T) {
	// A human re-applied hold after the #5117 notice; lifting that hold must
	// not be attributed to self-authorization.
	s := &liftedHoldServer{
		comments: []map[string]any{selfAuthNoticeComment(testHiveAppBotLogin, "2026-09-15T12:00:00Z")},
		events: []map[string]any{
			holdEvent("labeled", testHiveAppBotLogin, "2026-09-15T11:00:00Z"),
			holdEvent("labeled", "alice", "2026-09-15T12:30:00Z"),
			holdEvent("unlabeled", "alice", "2026-09-15T13:00:00Z"),
		},
	}
	c := newLiftedHoldClient(t, s)
	ok, err := c.LiftedHoldWasSelfAuthorization(context.Background(), "acme/widget", 7)
	if err != nil || ok {
		t.Fatalf("LiftedHoldWasSelfAuthorization = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestLiftedHoldWasSelfAuthorization_NegativeCases(t *testing.T) {
	notice := selfAuthNoticeComment(testHiveAppBotLogin, "2026-09-15T12:00:00Z")
	for _, tc := range []struct {
		name     string
		comments []map[string]any
		events   []map[string]any
	}{
		{
			name: "no notice",
			events: []map[string]any{
				holdEvent("labeled", testHiveAppBotLogin, "2026-09-15T11:00:00Z"),
				holdEvent("unlabeled", "alice", "2026-09-15T13:00:00Z"),
			},
		},
		{
			name:     "notice authored by non-bot is ignored",
			comments: []map[string]any{selfAuthNoticeComment("mallory", "2026-09-15T12:00:00Z")},
			events: []map[string]any{
				holdEvent("labeled", testHiveAppBotLogin, "2026-09-15T11:00:00Z"),
				holdEvent("unlabeled", "alice", "2026-09-15T13:00:00Z"),
			},
		},
		{
			name:     "still held: latest event is labeled",
			comments: []map[string]any{notice},
			events: []map[string]any{
				holdEvent("labeled", testHiveAppBotLogin, "2026-09-15T13:00:00Z"),
			},
		},
		{
			name:     "lift happened before the notice",
			comments: []map[string]any{selfAuthNoticeComment(testHiveAppBotLogin, "2026-09-15T14:00:00Z")},
			events: []map[string]any{
				holdEvent("labeled", testHiveAppBotLogin, "2026-09-15T11:00:00Z"),
				holdEvent("unlabeled", "alice", "2026-09-15T13:00:00Z"),
			},
		},
		{
			name:     "hold applied by human, not the bot",
			comments: []map[string]any{notice},
			events: []map[string]any{
				holdEvent("labeled", "alice", "2026-09-15T11:00:00Z"),
				holdEvent("unlabeled", "alice", "2026-09-15T13:00:00Z"),
			},
		},
		{
			name:     "no hold label events at all",
			comments: []map[string]any{notice},
			events:   []map[string]any{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &liftedHoldServer{comments: tc.comments, events: tc.events}
			c := newLiftedHoldClient(t, s)
			ok, err := c.LiftedHoldWasSelfAuthorization(context.Background(), "acme/widget", 7)
			if err != nil || ok {
				t.Fatalf("LiftedHoldWasSelfAuthorization = (%v, %v), want (false, nil)", ok, err)
			}
		})
	}
}

func TestLiftedHoldWasSelfAuthorization_ErrorPaths(t *testing.T) {
	t.Run("comment list error propagates", func(t *testing.T) {
		s := &liftedHoldServer{failComment: true}
		c := newLiftedHoldClient(t, s)
		if _, err := c.LiftedHoldWasSelfAuthorization(context.Background(), "acme/widget", 7); err == nil {
			t.Fatal("expected error from comment listing, got nil")
		}
	})
	t.Run("event list error propagates", func(t *testing.T) {
		s := &liftedHoldServer{
			comments:   []map[string]any{selfAuthNoticeComment(testHiveAppBotLogin, "2026-09-15T12:00:00Z")},
			failEvents: true,
		}
		c := newLiftedHoldClient(t, s)
		if _, err := c.LiftedHoldWasSelfAuthorization(context.Background(), "acme/widget", 7); err == nil {
			t.Fatal("expected error from event listing, got nil")
		}
	})
	t.Run("empty app bot login yields false without requests", func(t *testing.T) {
		s := &liftedHoldServer{}
		c := newLiftedHoldClient(t, s)
		c.SetAppBotLogin("")
		ok, err := c.LiftedHoldWasSelfAuthorization(context.Background(), "acme/widget", 7)
		if err != nil || ok {
			t.Fatalf("LiftedHoldWasSelfAuthorization = (%v, %v), want (false, nil)", ok, err)
		}
	})
	t.Run("unsplittable repo ref yields false", func(t *testing.T) {
		s := &liftedHoldServer{
			comments: []map[string]any{selfAuthNoticeComment(testHiveAppBotLogin, "2026-09-15T12:00:00Z")},
		}
		c := NewClientForTest(s.start(t).URL, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
		c.SetAppBotLogin(testHiveAppBotLogin)
		ok, err := c.LiftedHoldWasSelfAuthorization(context.Background(), "widget-no-owner", 7)
		if err != nil || ok {
			t.Fatalf("LiftedHoldWasSelfAuthorization = (%v, %v), want (false, nil)", ok, err)
		}
	})
}

func TestReleaseLevelHoldIfEligible_ExportedWrapper(t *testing.T) {
	s := &levelHoldServer{comments: []string{levelHoldNotice("quality")}}
	c := newLevelHoldClient(t, s)
	c.prHoldLabel = func(agent string) bool { return false }

	released, reason, err := c.ReleaseLevelHoldIfEligible(context.Background(), "acme", "widget", heldPRFixture())
	if err != nil || !released || reason != "level-hold-released" {
		t.Fatalf("ReleaseLevelHoldIfEligible = (%v, %q, %v), want release", released, reason, err)
	}
	if s.removes != 1 {
		t.Fatalf("removes=%d, want 1", s.removes)
	}
}

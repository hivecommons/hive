package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Correcting a review in place is the difference between fixing the record and
// shouting over it. These tests pin the three properties that make it safe to
// point at someone else's repo: it only ever edits the hive's own advisory
// review, it only does so where it has been granted permission, and it writes
// nothing at all when the text would not change.

type reviseServer struct {
	mu       sync.Mutex
	reviews  []map[string]any
	patched  map[int64]string
	creates  int
	patches  int
	listHits int
}

func newReviseServer(t *testing.T, reviews []map[string]any) (*httptest.Server, *reviseServer) {
	t.Helper()
	state := &reviseServer{reviews: reviews, patched: map[int64]string{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/o/r/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			state.listHits++
			_ = json.NewEncoder(w).Encode(state.reviews)
		case http.MethodPost:
			state.creates++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 999, "state": "COMMENTED"})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	// PATCH /repos/{o}/{r}/pulls/{n}/reviews/{id}
	mux.HandleFunc("/repos/o/r/pulls/7/reviews/", func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		if r.Method != http.MethodPut && r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		var id int64
		parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")
		_, _ = fmt.Sscanf(parts[len(parts)-1], "%d", &id)
		state.patches++
		state.patched[id] = payload.Body
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "body": payload.Body, "state": "COMMENTED"})
	})

	return httptest.NewServer(mux), state
}

func reviseTestClient(t *testing.T, server *httptest.Server, allow []string) *Client {
	t.Helper()
	c := newTestClient(t, server, "o", []string{"o/r"})
	c.SetAppBotLogin("kubestellar-hive[bot]")
	c.SetReviseRepos(allow)
	return c
}

func TestReviseReviewEditsItsOwnReviewInPlace(t *testing.T) {
	server, state := newReviseServer(t, []map[string]any{
		{"id": 11, "state": "COMMENTED", "submitted_at": "2026-09-19T10:00:00Z",
			"user": map[string]any{"login": "kubestellar-hive[bot]"},
			"body": "**Reviewed** — no findings from this perspective."},
	})
	defer server.Close()

	c := reviseTestClient(t, server, []string{"o/r"})
	req := ReviewRequest{Repo: "o/r", Number: 7, Event: "comment", Revise: true}

	got, revised, err := c.reviseReview(context.Background(), req, "Found a real problem at foo.go:42.")
	if err != nil {
		t.Fatalf("reviseReview: %v", err)
	}
	if !revised {
		t.Fatal("expected the review to be revised")
	}
	if got == nil {
		t.Fatal("expected the revised review back")
	}
	if state.patches != 1 {
		t.Errorf("patches = %d, want 1", state.patches)
	}
	if state.creates != 0 {
		t.Errorf("a revision must never post a second review, got %d creates", state.creates)
	}
	if body := state.patched[11]; !strings.Contains(body, "foo.go:42") {
		t.Errorf("patched body = %q, want the corrected review", body)
	}
}

// A no-op PATCH still spends API budget and still races other writers. The
// advisory digest already suppresses identical rewrites; so must this.
func TestReviseReviewSuppressesIdenticalBody(t *testing.T) {
	const body = "**Reviewed** — no findings from this perspective."
	server, state := newReviseServer(t, []map[string]any{
		{"id": 11, "state": "COMMENTED", "submitted_at": "2026-09-19T10:00:00Z",
			"user": map[string]any{"login": "kubestellar-hive[bot]"}, "body": body},
	})
	defer server.Close()

	c := reviseTestClient(t, server, []string{"o/r"})
	req := ReviewRequest{Repo: "o/r", Number: 7, Event: "comment", Revise: true}

	_, revised, err := c.reviseReview(context.Background(), req, body)
	if err != nil {
		t.Fatalf("reviseReview: %v", err)
	}
	if revised {
		t.Error("an unchanged body must not be rewritten")
	}
	if state.patches != 0 {
		t.Errorf("patches = %d, want 0 for an identical body", state.patches)
	}
	// Whitespace-only differences are still identical.
	if _, revised, _ := c.reviseReview(context.Background(), req, "\n  "+body+"  \n"); revised {
		t.Error("whitespace-only difference must not count as a change")
	}
}

// Editing a person's review would be a serious breach. The reviewer may only
// ever touch its own advisory comment.
func TestReviseReviewNeverTouchesSomeoneElsesReview(t *testing.T) {
	server, state := newReviseServer(t, []map[string]any{
		{"id": 21, "state": "COMMENTED", "submitted_at": "2026-09-19T11:00:00Z",
			"user": map[string]any{"login": "castrojo"}, "body": "looks good to me"},
		{"id": 22, "state": "APPROVED", "submitted_at": "2026-09-19T11:30:00Z",
			"user": map[string]any{"login": "kubestellar-hive[bot]"}, "body": "formal approval"},
	})
	defer server.Close()

	c := reviseTestClient(t, server, []string{"o/r"})
	req := ReviewRequest{Repo: "o/r", Number: 7, Event: "comment", Revise: true}

	got, revised, err := c.reviseReview(context.Background(), req, "new text")
	if err != nil {
		t.Fatalf("reviseReview: %v", err)
	}
	if revised || got != nil {
		t.Fatal("must not revise: the only COMMENTED review belongs to a human, and the bot's is a formal APPROVED state")
	}
	if state.patches != 0 {
		t.Errorf("patches = %d, want 0", state.patches)
	}
}

func TestReviseReviewRequiresAllowlistAndIdentity(t *testing.T) {
	server, state := newReviseServer(t, []map[string]any{
		{"id": 11, "state": "COMMENTED", "submitted_at": "2026-09-19T10:00:00Z",
			"user": map[string]any{"login": "kubestellar-hive[bot]"}, "body": "old"},
	})
	defer server.Close()

	req := ReviewRequest{Repo: "o/r", Number: 7, Event: "comment", Revise: true}

	t.Run("repo not allowlisted", func(t *testing.T) {
		c := reviseTestClient(t, server, []string{"other/repo"})
		if _, _, err := c.reviseReview(context.Background(), req, "new"); err == nil {
			t.Fatal("expected an error for a repo outside the allowlist")
		}
		if state.patches != 0 {
			t.Errorf("patches = %d, want 0", state.patches)
		}
	})

	t.Run("no bot identity fails closed", func(t *testing.T) {
		c := reviseTestClient(t, server, []string{"o/r"})
		c.SetAppBotLogin("")
		_, _, err := c.reviseReview(context.Background(), req, "new")
		if err == nil || !strings.Contains(err.Error(), "App bot login") {
			t.Fatalf("err = %v, want a fail-closed identity error", err)
		}
		if state.patches != 0 {
			t.Errorf("patches = %d, want 0", state.patches)
		}
	})
}

// A first review is not a revision. When there is nothing of ours to correct,
// reviseReview reports "no prior review" so the caller posts normally.
func TestReviseReviewFallsBackWhenNoPriorReview(t *testing.T) {
	server, _ := newReviseServer(t, []map[string]any{})
	defer server.Close()

	c := reviseTestClient(t, server, []string{"o/r"})
	got, revised, err := c.reviseReview(context.Background(),
		ReviewRequest{Repo: "o/r", Number: 7, Event: "comment", Revise: true}, "first review")
	if err != nil {
		t.Fatalf("reviseReview: %v", err)
	}
	if got != nil || revised {
		t.Fatal("expected no revision when the hive has never reviewed this PR")
	}
}

// Of several of its own comments, the newest is the one a maintainer is
// reading.
func TestReviseReviewPicksTheMostRecentOwnComment(t *testing.T) {
	server, state := newReviseServer(t, []map[string]any{
		{"id": 11, "state": "COMMENTED", "submitted_at": "2026-09-18T10:00:00Z",
			"user": map[string]any{"login": "kubestellar-hive[bot]"}, "body": "older"},
		{"id": 33, "state": "COMMENTED", "submitted_at": "2026-09-19T10:00:00Z",
			"user": map[string]any{"login": "kubestellar-hive[bot]"}, "body": "newest"},
		{"id": 22, "state": "COMMENTED", "submitted_at": "2026-09-18T20:00:00Z",
			"user": map[string]any{"login": "kubestellar-hive[bot]"}, "body": "middle"},
	})
	defer server.Close()

	c := reviseTestClient(t, server, []string{"o/r"})
	if _, revised, err := c.reviseReview(context.Background(),
		ReviewRequest{Repo: "o/r", Number: 7, Event: "comment", Revise: true}, "corrected"); err != nil || !revised {
		t.Fatalf("reviseReview revised=%v err=%v", revised, err)
	}
	if _, ok := state.patched[33]; !ok {
		t.Errorf("patched %v, want the newest review 33", state.patched)
	}
}

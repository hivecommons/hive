package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// reviewDrafts14551Server holds one review-derived retro draft, one retro
// draft from another writer and one approved fact carrying the same tags.
func reviewDrafts14551Server(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "", logger)
	s.RegisterAPI(testDeps(t))

	vault := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	files := [][2]string{
		{"review-draft.md", "---\ntitle: Review Draft Lesson\ntype: pattern\ntags: [retro, lesson, review-feedback]\nstate: draft\nlayer: project\n---\nReviewers expect tests.\n"},
		{"other-draft.md", "---\ntitle: Other Draft Lesson\ntype: pattern\ntags: [retro, lesson]\nstate: draft\nlayer: project\n---\nOther writer.\n"},
		{"approved-review.md", "---\ntitle: Approved Review Lesson\ntype: pattern\ntags: [retro, lesson, review-feedback]\nlayer: project\n---\nAlready approved.\n"},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(vault, f[0]), []byte(f[1]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	api := knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, logger)
	if err := api.ConnectVault(vault, "vault"); err != nil {
		t.Fatalf("ConnectVault: %v", err)
	}
	s.deps.Knowledge = api
	return s
}

type reviewDraftsList struct {
	Total int `json:"total"`
	Facts []struct {
		Title string                   `json:"title"`
		State knowledge.LifecycleState `json:"state"`
	} `json:"facts"`
}

func decodeReviewDraftsList(t *testing.T, body string) reviewDraftsList {
	t.Helper()
	var out reviewDraftsList
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	return out
}

const reviewDraftsQuery = "/api/knowledge?state=draft&tag=retro," + knowledge.ReviewFeedbackTag

// TestKnowledgeListOwnerFiltersReviewDrafts: the owner lists exactly the
// review-derived drafts with state + tag, and no approved fact (B-AC16).
// After approval the draft is no longer listed (B-AC15).
func TestKnowledgeListOwnerFiltersReviewDrafts(t *testing.T) {
	s := reviewDrafts14551Server(t)

	for _, target := range []string{reviewDraftsQuery, "/api/knowledge?state=draft&tag=review-feedback"} {
		code, body := lifecycle14546Get(t, s, target, "owner", true)
		if code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", target, code, body)
		}
		got := decodeReviewDraftsList(t, body)
		if got.Total != 1 || len(got.Facts) != 1 || got.Facts[0].Title != "Review Draft Lesson" || got.Facts[0].State != knowledge.StateDraft {
			t.Fatalf("%s = %+v, want only the review draft", target, got)
		}
	}

	code, body := lifecycle14546Get(t, s, "/api/knowledge?state=draft", "owner", true)
	if code != http.StatusOK {
		t.Fatalf("state=draft status = %d", code)
	}
	if got := decodeReviewDraftsList(t, body); got.Total != 2 || strings.Contains(body, "Approved Review Lesson") {
		t.Fatalf("state=draft = %s, want both drafts and no approved fact", body)
	}

	if rec := doKnowledgeStatePut(s, "review-draft", "owner", true, `{"state":"approved"}`); rec.Code != http.StatusOK {
		t.Fatalf("approve status = %d body=%s", rec.Code, rec.Body.String())
	}
	_, body = lifecycle14546Get(t, s, reviewDraftsQuery, "owner", true)
	if got := decodeReviewDraftsList(t, body); got.Total != 0 {
		t.Fatalf("after approval = %s, want no drafts awaiting approval", body)
	}
}

// TestKnowledgeListRefusesNonApprovedStateToNonOwner: asking for unapproved
// states without a verified owner is refused and leaks nothing (B-AC16,
// B-AC7 non-owner half).
func TestKnowledgeListRefusesNonApprovedStateToNonOwner(t *testing.T) {
	s := reviewDrafts14551Server(t)
	for _, tc := range []struct {
		role     string
		verified bool
	}{{"read", false}, {"merger", false}, {"owner", false}} {
		for _, target := range []string{reviewDraftsQuery, "/api/knowledge?state=all", "/api/knowledge?state=approved,draft"} {
			code, body := lifecycle14546Get(t, s, target, tc.role, tc.verified)
			if code != http.StatusForbidden {
				t.Errorf("role=%q verified=%v %s status = %d, want 403", tc.role, tc.verified, target, code)
			}
			if strings.Contains(body, "Draft Lesson") {
				t.Errorf("role=%q %s leaked a draft: %s", tc.role, target, body)
			}
		}
	}
}

// TestKnowledgeListApprovedStateAndTagForNonOwner: approved-only listing and
// tag filtering stay open to every reader, and never include drafts.
func TestKnowledgeListApprovedStateAndTagForNonOwner(t *testing.T) {
	s := reviewDrafts14551Server(t)
	for _, target := range []string{"/api/knowledge?state=approved&tag=review-feedback", "/api/knowledge?tag=review-feedback"} {
		code, body := lifecycle14546Get(t, s, target, "read", false)
		if code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", target, code, body)
		}
		got := decodeReviewDraftsList(t, body)
		if got.Total != 1 || got.Facts[0].Title != "Approved Review Lesson" {
			t.Fatalf("%s = %s, want only the approved review lesson", target, body)
		}
	}
}

func TestKnowledgeListRejectsInvalidState(t *testing.T) {
	s := reviewDrafts14551Server(t)
	if code, body := lifecycle14546Get(t, s, "/api/knowledge?state=pending", "owner", true); code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", code, body)
	}
}

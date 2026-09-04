package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/advisory"
)

// --------------------------------------------------------------------------
// truncateDigest — pure function
// --------------------------------------------------------------------------

func TestTruncateDigest_Short(t *testing.T) {
	input := "short digest"
	got := truncateDigest(input)
	if got != input {
		t.Errorf("short digest should be unchanged, got %q", got)
	}
}

func TestTruncateDigest_ExactLimit(t *testing.T) {
	input := strings.Repeat("a", githubCommentCharLimit)
	got := truncateDigest(input)
	if got != input {
		t.Error("exactly-at-limit digest should be unchanged")
	}
}

func TestTruncateDigest_OverLimit(t *testing.T) {
	// Build a long string with newlines so the truncation finds a newline boundary
	var sb strings.Builder
	for sb.Len() < githubCommentCharLimit+1000 {
		sb.WriteString("This is a line of text for the digest.\n")
	}
	input := sb.String()
	got := truncateDigest(input)
	if len(got) >= len(input) {
		t.Errorf("expected truncation, got len=%d (original=%d)", len(got), len(input))
	}
	if !strings.Contains(got, "Digest truncated") {
		t.Error("truncated digest should contain truncation footer")
	}
}

func TestTruncateDigest_OverLimitNoNewlines(t *testing.T) {
	// No newlines — falls back to raw cutoff
	input := strings.Repeat("x", githubCommentCharLimit+500)
	got := truncateDigest(input)
	if !strings.Contains(got, "Digest truncated") {
		t.Error("should contain truncation footer")
	}
}

func TestTruncateDigest_MultiByte(t *testing.T) {
	// Build string with multi-byte UTF-8 characters that exceeds the limit
	// Each emoji is 4 bytes
	var sb strings.Builder
	for sb.Len() < githubCommentCharLimit+500 {
		sb.WriteString("🐝")
	}
	input := sb.String()
	got := truncateDigest(input)
	if !strings.Contains(got, "Digest truncated") {
		t.Error("should contain truncation footer")
	}
}

// --------------------------------------------------------------------------
// findAdvisoryIssue — via httptest
// --------------------------------------------------------------------------

func TestFindAdvisoryIssue_FoundViaSearch(t *testing.T) {
	org, repo := "testorg", "testrepo"
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"items": []map[string]any{
				{"number": 42, "title": advisoryTitle},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	num, err := c.findAdvisoryIssue(context.Background(), org, repo)
	if err != nil {
		t.Fatalf("findAdvisoryIssue: %v", err)
	}
	if num != 42 {
		t.Errorf("issue number = %d, want 42", num)
	}
}

func TestFindAdvisoryIssue_SearchNoMatch_FallbackLabel(t *testing.T) {
	org, repo := "testorg", "testrepo"
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		// Search returns results but none match the title
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"items": []map[string]any{
				{"number": 1, "title": "Some other issue"},
			},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		labels := r.URL.Query().Get("labels")
		if labels == advisoryLabelName {
			json.NewEncoder(w).Encode([]map[string]any{
				{"number": 55, "title": advisoryTitle},
			})
		} else {
			json.NewEncoder(w).Encode([]map[string]any{})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	num, err := c.findAdvisoryIssue(context.Background(), org, repo)
	if err != nil {
		t.Fatalf("findAdvisoryIssue: %v", err)
	}
	if num != 55 {
		t.Errorf("issue number = %d, want 55", num)
	}
}

func TestFindAdvisoryIssue_SearchFails_FallbackLabel(t *testing.T) {
	org, repo := "testorg", "testrepo"
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		labels := r.URL.Query().Get("labels")
		if labels == advisoryLabelName {
			json.NewEncoder(w).Encode([]map[string]any{
				{"number": 77, "title": advisoryTitle},
			})
		} else {
			// Fallback 2: title scan
			json.NewEncoder(w).Encode([]map[string]any{})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	num, err := c.findAdvisoryIssue(context.Background(), org, repo)
	if err != nil {
		t.Fatalf("findAdvisoryIssue: %v", err)
	}
	if num != 77 {
		t.Errorf("issue number = %d, want 77", num)
	}
}

func TestFindAdvisoryIssue_TitleScanFallback(t *testing.T) {
	org, repo := "testorg", "testrepo"
	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		// No matches
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 0,
			"items":       []any{},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		callCount++
		labels := r.URL.Query().Get("labels")
		if labels == advisoryLabelName {
			// Label fallback: no match
			json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		// Title scan fallback: return the advisory issue
		json.NewEncoder(w).Encode([]map[string]any{
			{"number": 99, "title": advisoryTitle},
		})
	})
	// Handle ensureAdvisoryLabel calls
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"name": advisoryLabelName})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/99/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]map[string]any{{"name": advisoryLabelName}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	num, err := c.findAdvisoryIssue(context.Background(), org, repo)
	if err != nil {
		t.Fatalf("findAdvisoryIssue: %v", err)
	}
	if num != 99 {
		t.Errorf("issue number = %d, want 99", num)
	}
}

func TestFindAdvisoryIssue_NotFound(t *testing.T) {
	org, repo := "testorg", "testrepo"
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 0,
			"items":       []any{},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	num, err := c.findAdvisoryIssue(context.Background(), org, repo)
	if err != nil {
		t.Fatalf("findAdvisoryIssue: %v", err)
	}
	if num != 0 {
		t.Errorf("issue number = %d, want 0 (not found)", num)
	}
}

// --------------------------------------------------------------------------
// findDigestComment
// --------------------------------------------------------------------------

func TestFindDigestComment_Found(t *testing.T) {
	org, repo := "testorg", "testrepo"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": 111, "body": "random comment"},
			{"id": 222, "body": advisoryDigestPrefix + " — some digest content"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	id, err := c.findDigestComment(context.Background(), org, repo, 10)
	if err != nil {
		t.Fatalf("findDigestComment: %v", err)
	}
	if id != 222 {
		t.Errorf("comment id = %d, want 222", id)
	}
}

func TestFindDigestComment_NotFound(t *testing.T) {
	org, repo := "testorg", "testrepo"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": 111, "body": "no digest here"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	id, err := c.findDigestComment(context.Background(), org, repo, 10)
	if err != nil {
		t.Fatalf("findDigestComment: %v", err)
	}
	if id != 0 {
		t.Errorf("comment id = %d, want 0 (not found)", id)
	}
}

func TestFindDigestComment_Error(t *testing.T) {
	org, repo := "testorg", "testrepo"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	_, err := c.findDigestComment(context.Background(), org, repo, 10)
	if err == nil {
		t.Error("expected error for API failure")
	}
}

// --------------------------------------------------------------------------
// EnsureAdvisoryIssue
// --------------------------------------------------------------------------

func TestEnsureAdvisoryIssue_ExistingIssue(t *testing.T) {
	org, repo := "testorg", "testrepo"
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"items": []map[string]any{
				{"number": 42, "title": advisoryTitle},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	num, err := c.EnsureAdvisoryIssue(context.Background(), repo)
	if err != nil {
		t.Fatalf("EnsureAdvisoryIssue: %v", err)
	}
	if num != 42 {
		t.Errorf("issue number = %d, want 42", num)
	}
}

func TestEnsureAdvisoryIssue_CreatesNew(t *testing.T) {
	org, repo := "testorg", "testrepo"
	var labelCreated, issueCreated bool
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 0,
			"items":       []any{},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			issueCreated = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"number": 100,
				"title":  advisoryTitle,
			})
			return
		}
		// GET requests for findAdvisoryIssue fallbacks
		json.NewEncoder(w).Encode([]map[string]any{})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			labelCreated = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"name": advisoryLabelName})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	num, err := c.EnsureAdvisoryIssue(context.Background(), repo)
	if err != nil {
		t.Fatalf("EnsureAdvisoryIssue: %v", err)
	}
	if num != 100 {
		t.Errorf("issue number = %d, want 100", num)
	}
	if !labelCreated {
		t.Error("label should have been created")
	}
	if !issueCreated {
		t.Error("issue should have been created")
	}
}

func TestEnsureAdvisoryIssue_LabelAlreadyExists(t *testing.T) {
	org, repo := "testorg", "testrepo"
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 0,
			"items":       []any{},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"number": 101,
				"title":  advisoryTitle,
			})
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		// Simulate "already_exists" error
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]any{
			"message": "Validation Failed",
			"errors":  []map[string]string{{"code": "already_exists"}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	num, err := c.EnsureAdvisoryIssue(context.Background(), repo)
	if err != nil {
		t.Fatalf("EnsureAdvisoryIssue: %v", err)
	}
	if num != 101 {
		t.Errorf("issue number = %d, want 101", num)
	}
}

func TestEnsureAdvisoryIssue_WithOrgPrefix(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"items": []map[string]any{
				{"number": 50, "title": advisoryTitle},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "defaultorg", []string{"repo"})
	// Pass org/repo format
	num, err := c.EnsureAdvisoryIssue(context.Background(), "otherog/otherrepo")
	if err != nil {
		t.Fatalf("EnsureAdvisoryIssue: %v", err)
	}
	if num != 50 {
		t.Errorf("issue number = %d, want 50", num)
	}
}

func TestEnsureAdvisoryIssue_LabelCreateFails(t *testing.T) {
	org, repo := "testorg", "testrepo"
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 0,
			"items":       []any{},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"number": 102,
				"title":  advisoryTitle,
			})
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		// Label creation fails with non-already_exists error
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"message": "forbidden"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	num, err := c.EnsureAdvisoryIssue(context.Background(), repo)
	if err != nil {
		t.Fatalf("EnsureAdvisoryIssue: %v", err)
	}
	// Should still create issue, just without label
	if num != 102 {
		t.Errorf("issue number = %d, want 102", num)
	}
}

// --------------------------------------------------------------------------
// PostAdvisoryDigest
// --------------------------------------------------------------------------

func TestPostAdvisoryDigest_CreateNew(t *testing.T) {
	org, repo := "testorg", "testrepo"
	var commentCreated bool
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			// No existing digest comment
			json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		if r.Method == "POST" {
			commentCreated = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	err := c.PostAdvisoryDigest(context.Background(), repo, 10, "## 🐝 Advisory Digest\nSome content")
	if err != nil {
		t.Fatalf("PostAdvisoryDigest: %v", err)
	}
	if !commentCreated {
		t.Error("expected comment to be created")
	}
}

func TestPostAdvisoryDigest_UpdateExisting(t *testing.T) {
	org, repo := "testorg", "testrepo"
	var commentUpdated bool
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": 555, "body": advisoryDigestPrefix + " — old content"},
			})
			return
		}
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/comments/555", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			commentUpdated = true
			json.NewEncoder(w).Encode(map[string]any{"id": 555})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+" — new content")
	if err != nil {
		t.Fatalf("PostAdvisoryDigest: %v", err)
	}
	if !commentUpdated {
		t.Error("expected comment to be updated")
	}
}

func TestPostAdvisoryDigest_UpdateError(t *testing.T) {
	org, repo := "testorg", "testrepo"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": 555, "body": advisoryDigestPrefix + " — old content"},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/comments/555", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+" — new")
	if err == nil {
		t.Error("expected error when update fails")
	}
}

func TestPostAdvisoryDigest_CreateError(t *testing.T) {
	org, repo := "testorg", "testrepo"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		if r.Method == "POST" {
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	err := c.PostAdvisoryDigest(context.Background(), repo, 10, "digest content")
	if err == nil {
		t.Error("expected error when create fails")
	}
}

func TestPostAdvisoryDigest_WithOrgPrefix(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/otherog/otherrepo/issues/5/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		if r.Method == "POST" {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "defaultorg", []string{"repo"})
	err := c.PostAdvisoryDigest(context.Background(), "otherog/otherrepo", 5, "content")
	if err != nil {
		t.Fatalf("PostAdvisoryDigest with org prefix: %v", err)
	}
}

func TestPostAdvisoryDigest_FindCommentError_StillCreates(t *testing.T) {
	org, repo := "testorg", "testrepo"
	var created bool
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			// Error when looking for existing digest
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.Method == "POST" {
			created = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	err := c.PostAdvisoryDigest(context.Background(), repo, 10, "digest")
	if err != nil {
		t.Fatalf("PostAdvisoryDigest: %v", err)
	}
	if !created {
		t.Error("expected comment to be created after find error")
	}
}

// TestPostAdvisoryDigestAcceptsPreRenderedMentionSafeBody asserts the posting
// path preserves a caller-rendered, mention-safe digest body.
func TestPostAdvisoryDigestAcceptsPreRenderedMentionSafeBody(t *testing.T) {
	org, repo := "testorg", "testrepo"
	var postedBody string
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		if r.Method == "POST" {
			var payload struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decoding comment payload: %v", err)
			}
			postedBody = payload.Body
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	raw := "## 🐝 Advisory Digest\n- PR #665 (open, by @omerap12) adds a helper\n- ping user@example.com about @some-user"
	rendered := advisory.NeutralizeMentions(raw)
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, rendered); err != nil {
		t.Fatalf("PostAdvisoryDigest: %v", err)
	}
	// A mention-position "@username" (@ at start of text or after a
	// non-alphanumeric character other than ` or /) must never survive.
	rawMention := regexp.MustCompile("(^|[^A-Za-z0-9`/])@[A-Za-z0-9]")
	if rawMention.MatchString(postedBody) {
		t.Errorf("posted digest body contains a raw @mention:\n%s", postedBody)
	}
	if !strings.Contains(postedBody, "`omerap12`") {
		t.Errorf("expected neutralized `omerap12` in posted body:\n%s", postedBody)
	}
	if !strings.Contains(postedBody, "`some-user`") {
		t.Errorf("expected neutralized `some-user` in posted body:\n%s", postedBody)
	}
	if !strings.Contains(postedBody, "user@example.com") {
		t.Errorf("email address must survive sanitization:\n%s", postedBody)
	}
}

// --------------------------------------------------------------------------
// ensureAdvisoryLabel
// --------------------------------------------------------------------------

func TestEnsureAdvisoryLabel_Success(t *testing.T) {
	org, repo := "testorg", "testrepo"
	var labelAdded bool
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"name": advisoryLabelName})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		labelAdded = true
		json.NewEncoder(w).Encode([]map[string]any{{"name": advisoryLabelName}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.ensureAdvisoryLabel(context.Background(), org, repo, 10)
	if !labelAdded {
		t.Error("expected label to be added to issue")
	}
}

func TestEnsureAdvisoryLabel_AlreadyExists(t *testing.T) {
	org, repo := "testorg", "testrepo"
	var labelAdded bool
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]any{
			"message": "Validation Failed",
			"errors":  []map[string]string{{"code": "already_exists"}},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		labelAdded = true
		json.NewEncoder(w).Encode([]map[string]any{{"name": advisoryLabelName}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.ensureAdvisoryLabel(context.Background(), org, repo, 10)
	if !labelAdded {
		t.Error("expected label to be added when it already exists")
	}
}

func TestEnsureAdvisoryLabel_CreateFails(t *testing.T) {
	org, repo := "testorg", "testrepo"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"message": "forbidden"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	// Should not panic — just returns early
	c.ensureAdvisoryLabel(context.Background(), org, repo, 10)
}

// --------------------------------------------------------------------------
// findAdvisoryIssue — title scan with PR filtering
// --------------------------------------------------------------------------

func TestFindAdvisoryIssue_TitleScanSkipsPRs(t *testing.T) {
	org, repo := "testorg", "testrepo"
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 0,
			"items":       []any{},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		labels := r.URL.Query().Get("labels")
		if labels == advisoryLabelName {
			json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		// Title scan: PR with advisory title + real issue
		json.NewEncoder(w).Encode([]map[string]any{
			{"number": 10, "title": advisoryTitle, "pull_request": map[string]any{"url": "..."}},
			{"number": 20, "title": advisoryTitle},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"name": advisoryLabelName})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/20/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{{"name": advisoryLabelName}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	num, err := c.findAdvisoryIssue(context.Background(), org, repo)
	if err != nil {
		t.Fatalf("findAdvisoryIssue: %v", err)
	}
	if num != 20 {
		t.Errorf("issue number = %d, want 20 (should skip PR #10)", num)
	}
}

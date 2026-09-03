package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
)

// evidenceServer is a canned GitHub API for the intent-evidence fetchers.
// It serves a single PR (acme/widgets#7) with paginated changed files and
// reviews, plus arbitrary issues keyed by "owner/repo#number".
type evidenceServer struct {
	prBody       string
	changedFiles int // value reported in the PR's changed_files field
	filePages    [][]map[string]any
	reviews      []map[string]any
	issues       map[string]map[string]any // "owner/repo#n" -> issue JSON
	failPR       bool
	failFiles    bool
	failReviews  bool
}

func (s *evidenceServer) client(t *testing.T) *github.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/acme/widgets/pulls/7/files":
			if s.failFiles {
				http.Error(w, `{"message":"files boom"}`, http.StatusInternalServerError)
				return
			}
			page := 1
			if p := r.URL.Query().Get("page"); p != "" {
				fmt.Sscanf(p, "%d", &page)
			}
			if page < 1 || page > len(s.filePages) {
				fmt.Fprint(w, "[]")
				return
			}
			if page < len(s.filePages) {
				w.Header().Set("Link", fmt.Sprintf(`<%s?page=%d>; rel="next", <%s?page=%d>; rel="last"`,
					r.URL.Path, page+1, r.URL.Path, len(s.filePages)))
			}
			if err := json.NewEncoder(w).Encode(s.filePages[page-1]); err != nil {
				t.Errorf("encoding files page: %v", err)
			}
		case r.URL.Path == "/repos/acme/widgets/pulls/7/reviews":
			if s.failReviews {
				http.Error(w, `{"message":"reviews boom"}`, http.StatusInternalServerError)
				return
			}
			if err := json.NewEncoder(w).Encode(s.reviews); err != nil {
				t.Errorf("encoding reviews: %v", err)
			}
		case r.URL.Path == "/repos/acme/widgets/pulls/7":
			if s.failPR {
				http.Error(w, `{"message":"pr boom"}`, http.StatusInternalServerError)
				return
			}
			if err := json.NewEncoder(w).Encode(map[string]any{
				"number":        7,
				"body":          s.prBody,
				"changed_files": s.changedFiles,
			}); err != nil {
				t.Errorf("encoding PR: %v", err)
			}
		default:
			// Issue lookups: /repos/{owner}/{repo}/issues/{n}
			owner, repo, n := parseIssuePath(r.URL.Path)
			if owner == "" {
				http.NotFound(w, r)
				return
			}
			key := fmt.Sprintf("%s/%s#%d", owner, repo, n)
			issue, ok := s.issues[key]
			if !ok {
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
				return
			}
			if err := json.NewEncoder(w).Encode(issue); err != nil {
				t.Errorf("encoding issue %s: %v", key, err)
			}
		}
	}))
	t.Cleanup(server.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return github.NewClientForTest(server.URL, "acme", []string{"widgets"}, logger)
}

// parseIssuePath extracts owner, repo, and number from
// /repos/{owner}/{repo}/issues/{n}; owner is "" on mismatch.
func parseIssuePath(path string) (string, string, int) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 5 || parts[0] != "repos" || parts[3] != "issues" {
		return "", "", 0
	}
	n, err := strconv.Atoi(parts[4])
	if err != nil {
		return "", "", 0
	}
	return parts[1], parts[2], n
}

func changedFileJSON(name, status string, adds, dels int) map[string]any {
	return map[string]any{
		"filename":  name,
		"status":    status,
		"additions": adds,
		"deletions": dels,
	}
}

func TestFetchIntentPREvidence_NilClient(t *testing.T) {
	var nilClient *github.Client
	if _, _, _, err := fetchIntentPREvidence(context.Background(), nil, "acme/widgets", 7); !errors.Is(err, github.ErrNoGitHubClient) {
		t.Errorf("nil client: err = %v, want ErrNoGitHubClient", err)
	}
	if _, _, _, err := fetchIntentPREvidence(context.Background(), nilClient, "acme/widgets", 7); !errors.Is(err, github.ErrNoGitHubClient) {
		t.Errorf("typed-nil client: err = %v, want ErrNoGitHubClient", err)
	}
}

func TestFetchIntentPREvidence_InvalidRepo(t *testing.T) {
	srv := &evidenceServer{}
	client := srv.client(t)
	for _, repo := range []string{"widgets", "/widgets", "acme/", ""} {
		if _, _, _, err := fetchIntentPREvidence(context.Background(), client, repo, 7); err == nil {
			t.Errorf("repo %q: expected error, got nil", repo)
		}
	}
}

func TestFetchIntentPREvidence_HappyPathPaginatedFilesAndApproval(t *testing.T) {
	srv := &evidenceServer{
		prBody:       "Fixes #12",
		changedFiles: 3,
		filePages: [][]map[string]any{
			{
				changedFileJSON("a.go", "modified", 10, 2),
				changedFileJSON("a_test.go", "added", 30, 0),
			},
			{
				changedFileJSON("docs/readme.md", "removed", 0, 5),
			},
		},
		reviews: []map[string]any{
			prReview("alice", "MEMBER", "APPROVED"),
		},
	}
	client := srv.client(t)

	body, files, approved, err := fetchIntentPREvidence(context.Background(), client, "acme/widgets", 7)
	if err != nil {
		t.Fatalf("fetchIntentPREvidence: %v", err)
	}
	if body != "Fixes #12" {
		t.Errorf("body = %q, want %q", body, "Fixes #12")
	}
	if !approved {
		t.Error("approved = false, want true (maintainer approval present)")
	}
	want := []intent.ChangedFile{
		{Filename: "a.go", Status: "modified", Additions: 10, Deletions: 2},
		{Filename: "a_test.go", Status: "added", Additions: 30, Deletions: 0},
		{Filename: "docs/readme.md", Status: "removed", Additions: 0, Deletions: 5},
	}
	if len(files) != len(want) {
		t.Fatalf("files = %d entries, want %d: %+v", len(files), len(want), files)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Errorf("files[%d] = %+v, want %+v", i, files[i], want[i])
		}
	}
}

func TestFetchIntentPREvidence_NoApproval(t *testing.T) {
	srv := &evidenceServer{
		prBody:       "body",
		changedFiles: 1,
		filePages:    [][]map[string]any{{changedFileJSON("a.go", "modified", 1, 1)}},
		reviews: []map[string]any{
			prReview("drive-by", "CONTRIBUTOR", "APPROVED"),
		},
	}
	client := srv.client(t)
	_, _, approved, err := fetchIntentPREvidence(context.Background(), client, "acme/widgets", 7)
	if err != nil {
		t.Fatalf("fetchIntentPREvidence: %v", err)
	}
	if approved {
		t.Error("approved = true, want false (only non-maintainer approval)")
	}
}

func TestFetchIntentPREvidence_IncompleteFileList(t *testing.T) {
	srv := &evidenceServer{
		prBody:       "body",
		changedFiles: 5, // reported > actually returned (1)
		filePages:    [][]map[string]any{{changedFileJSON("a.go", "modified", 1, 1)}},
	}
	client := srv.client(t)
	_, _, _, err := fetchIntentPREvidence(context.Background(), client, "acme/widgets", 7)
	if err == nil {
		t.Fatal("expected incomplete-file-list error, got nil")
	}
}

func TestFetchIntentPREvidence_APIErrors(t *testing.T) {
	tests := []struct {
		name string
		srv  *evidenceServer
	}{
		{"PR get fails", &evidenceServer{failPR: true}},
		{"file listing fails", &evidenceServer{failFiles: true}},
		{"reviews listing fails", &evidenceServer{
			changedFiles: 1,
			filePages:    [][]map[string]any{{changedFileJSON("a.go", "modified", 1, 1)}},
			failReviews:  true,
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := tc.srv.client(t)
			if _, _, _, err := fetchIntentPREvidence(context.Background(), client, "acme/widgets", 7); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestFetchIntentIssueTexts_NilClient(t *testing.T) {
	var nilClient *github.Client
	if _, err := fetchIntentIssueTexts(context.Background(), nil, "acme/widgets", "Fixes #1"); !errors.Is(err, github.ErrNoGitHubClient) {
		t.Errorf("nil client: err = %v, want ErrNoGitHubClient", err)
	}
	if _, err := fetchIntentIssueTexts(context.Background(), nilClient, "acme/widgets", "Fixes #1"); !errors.Is(err, github.ErrNoGitHubClient) {
		t.Errorf("typed-nil client: err = %v, want ErrNoGitHubClient", err)
	}
}

func TestFetchIntentIssueTexts_NoRefs(t *testing.T) {
	srv := &evidenceServer{}
	client := srv.client(t)
	out, err := fetchIntentIssueTexts(context.Background(), client, "acme/widgets", "no linked issues here")
	if err != nil {
		t.Fatalf("fetchIntentIssueTexts: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("out = %+v, want empty", out)
	}
}

func TestFetchIntentIssueTexts_DefaultAndExplicitRepos(t *testing.T) {
	srv := &evidenceServer{
		issues: map[string]map[string]any{
			"acme/widgets#12": {"number": 12, "title": "default-repo issue", "body": "widgets body"},
			"acme/gadgets#3":  {"number": 3, "title": "cross-repo issue", "body": "gadgets body"},
		},
	}
	client := srv.client(t)

	body := "Fixes #12 and refs acme/gadgets#3"
	out, err := fetchIntentIssueTexts(context.Background(), client, "acme/widgets", body)
	if err != nil {
		t.Fatalf("fetchIntentIssueTexts: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("out = %d entries, want 2: %+v", len(out), out)
	}
	if out[0].Source != "issue acme/widgets#12" || out[0].Title != "default-repo issue" || out[0].Body != "widgets body" {
		t.Errorf("out[0] = %+v, want default-repo issue evidence", out[0])
	}
	if out[1].Source != "issue acme/gadgets#3" || out[1].Title != "cross-repo issue" || out[1].Body != "gadgets body" {
		t.Errorf("out[1] = %+v, want cross-repo issue evidence", out[1])
	}
}

func TestFetchIntentIssueTexts_InvalidDefaultRepoSkipsRef(t *testing.T) {
	srv := &evidenceServer{}
	client := srv.client(t)
	// The bare "#5" ref resolves to the default repo, which has no owner/name
	// split — the ref must be skipped, not fail the whole fetch.
	out, err := fetchIntentIssueTexts(context.Background(), client, "not-a-repo", "Fixes #5")
	if err != nil {
		t.Fatalf("fetchIntentIssueTexts: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("out = %+v, want empty (invalid default repo skipped)", out)
	}
}

func TestFetchIntentIssueTexts_LookupErrorReturnsPartial(t *testing.T) {
	srv := &evidenceServer{
		issues: map[string]map[string]any{
			"acme/widgets#12": {"number": 12, "title": "first", "body": "ok"},
			// acme/widgets#99 intentionally missing -> 404
		},
	}
	client := srv.client(t)

	out, err := fetchIntentIssueTexts(context.Background(), client, "acme/widgets", "Fixes #12, refs #99")
	if err == nil {
		t.Fatal("expected error for missing linked issue, got nil")
	}
	if len(out) != 1 || out[0].Title != "first" {
		t.Errorf("out = %+v, want the one successfully fetched issue", out)
	}
}

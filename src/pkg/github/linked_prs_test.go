package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPRCloseEvidenceConfiguredLineKeyword(t *testing.T) {
	tests := []struct {
		name          string
		defaultBranch string
		base          string
		body          string
		nodes         []map[string]any
		want          bool
		wantRelation  bool
	}{
		{
			name:          "github relation wins first",
			defaultBranch: "v5",
			base:          "feature",
			body:          "Refs #9140",
			nodes:         []map[string]any{{"number": 9140, "repository": map[string]any{"nameWithOwner": "hivecommons/hive"}}},
			want:          true,
			wantRelation:  true,
		},
		{
			name:          "default branch closing keyword",
			defaultBranch: "v5",
			base:          "v5",
			body:          "Fixes #9140",
			want:          true,
		},
		{
			name:          "version line closing keyword",
			defaultBranch: "v5",
			base:          "v6",
			body:          "Fixes: hivecommons/hive#9140",
			want:          true,
		},
		{
			name:          "unrelated branch is not promoted",
			defaultBranch: "v5",
			base:          "feature/fix",
			body:          "Fixes #9140",
			want:          false,
		},
		{
			name:          "configured line without closing keyword stays open",
			defaultBranch: "v5",
			base:          "v6",
			body:          "Refs #9140",
			want:          false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/graphql") {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
					"repository": map[string]any{
						"defaultBranchRef": map[string]any{"name": tt.defaultBranch},
						"pullRequest": map[string]any{
							"body":        tt.body,
							"baseRefName": tt.base,
							"merged":      true,
							"closingIssuesReferences": map[string]any{
								"nodes": tt.nodes,
							},
						},
					},
				}})
			}))
			t.Cleanup(srv.Close)
			c := NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, testLogger())
			got, err := c.PRCloseEvidence(context.Background(), "hivecommons/hive", 9277, 9140)
			if err != nil {
				t.Fatalf("PRCloseEvidence: %v", err)
			}
			if got.Closes != tt.want {
				t.Fatalf("Closes = %v, want %v (evidence %+v)", got.Closes, tt.want, got)
			}
			if got.GitHubRelation != tt.wantRelation {
				t.Fatalf("GitHubRelation = %v, want %v", got.GitHubRelation, tt.wantRelation)
			}
		})
	}
}

func TestCloseIssueForConfiguredLinePRPostsAuditCommentAndCloses(t *testing.T) {
	var postedComment string
	var closed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/hivecommons/hive/issues/9140/comments":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues/9140/comments":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedComment = body["body"]
			_, _ = w.Write([]byte(`{"html_url":"https://github.com/hivecommons/hive/issues/9140#issuecomment-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/hivecommons/hive/issues/9140":
			_, _ = w.Write([]byte(`{"number":9140,"labels":[]}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/hivecommons/hive/issues/9140":
			closed = true
			_, _ = w.Write([]byte(`{"number":9140,"state":"closed"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/hivecommons/hive/issues/9140/labels/needs-reporter-confirmation":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, testLogger())
	err := c.CloseIssueForConfiguredLinePR(context.Background(), "hivecommons/hive", 9140, 9277, PRCloseEvidence{
		Branch:                 "v6",
		MatchedClosingFragment: "Fixes #9140",
	})
	if err != nil {
		t.Fatalf("CloseIssueForConfiguredLinePR: %v", err)
	}
	if !closed {
		t.Fatal("issue was not closed")
	}
	if !strings.Contains(postedComment, "Closed by #9277 (merged to `v6`).") ||
		!strings.Contains(postedComment, "GitHub only auto-closes for default-branch PRs") ||
		!strings.Contains(postedComment, "`Fixes #9140`") {
		t.Fatalf("unexpected comment: %q", postedComment)
	}
}

func TestIsConfiguredIssueClosingLine(t *testing.T) {
	for _, tt := range []struct {
		branch, def string
		want        bool
	}{
		{"v5", "v5", true},
		{"v6", "v5", true},
		{"main", "main", true},
		{"release/v6", "v5", false},
		{"feature", "v5", false},
	} {
		if got := IsConfiguredIssueClosingLine(tt.branch, tt.def); got != tt.want {
			t.Errorf("IsConfiguredIssueClosingLine(%q, %q) = %v, want %v", tt.branch, tt.def, got, tt.want)
		}
	}
}

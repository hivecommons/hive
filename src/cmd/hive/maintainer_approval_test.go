package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/intent"
)

// newReviewsTestClient serves canned pages of PR reviews for
// /repos/{owner}/{repo}/pulls/{number}/reviews and returns a go-github
// client pointed at the test server. Pages are 1-indexed via the ?page=
// query parameter; a Link header advertises the next page while one exists.
func newReviewsTestClient(t *testing.T, pages [][]map[string]any) *gh.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls/7/reviews" {
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			fmt.Sscanf(p, "%d", &page)
		}
		if page < 1 || page > len(pages) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "[]")
			return
		}
		if page < len(pages) {
			w.Header().Set("Link", fmt.Sprintf(`<%s?page=%d>; rel="next", <%s?page=%d>; rel="last"`,
				r.URL.Path, page+1, r.URL.Path, len(pages)))
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(pages[page-1]); err != nil {
			t.Errorf("encoding reviews page: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	client := gh.NewClient(nil)
	base, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	client.BaseURL = base
	return client
}

func prReview(login, association, state string) map[string]any {
	return map[string]any{
		"user":               map[string]any{"login": login},
		"author_association": association,
		"state":              state,
	}
}

func TestHasMaintainerApproval(t *testing.T) {
	tests := []struct {
		name  string
		pages [][]map[string]any
		want  bool
	}{
		{
			name:  "no reviews",
			pages: [][]map[string]any{{}},
			want:  false,
		},
		{
			name: "maintainer approval",
			pages: [][]map[string]any{{
				prReview("alice", "MEMBER", "APPROVED"),
			}},
			want: true,
		},
		{
			name: "non-maintainer approval does not count",
			pages: [][]map[string]any{{
				prReview("drive-by", "CONTRIBUTOR", "APPROVED"),
				prReview("stranger", "NONE", "APPROVED"),
			}},
			want: false,
		},
		{
			name: "latest review wins: approval superseded by changes requested",
			pages: [][]map[string]any{{
				prReview("alice", "OWNER", "APPROVED"),
				prReview("alice", "OWNER", "CHANGES_REQUESTED"),
			}},
			want: false,
		},
		{
			name: "latest review wins: changes requested superseded by approval",
			pages: [][]map[string]any{{
				prReview("alice", "OWNER", "CHANGES_REQUESTED"),
				prReview("alice", "OWNER", "APPROVED"),
			}},
			want: true,
		},
		{
			name: "dismissed approval does not count",
			pages: [][]map[string]any{{
				prReview("alice", "MEMBER", "APPROVED"),
				prReview("alice", "MEMBER", "DISMISSED"),
			}},
			want: false,
		},
		{
			name: "any maintainer changes-requested vetoes another maintainer approval",
			pages: [][]map[string]any{{
				prReview("alice", "MEMBER", "APPROVED"),
				prReview("bob", "COLLABORATOR", "CHANGES_REQUESTED"),
			}},
			want: false,
		},
		{
			name: "comment-only maintainer review is not approval",
			pages: [][]map[string]any{{
				prReview("alice", "MEMBER", "COMMENTED"),
			}},
			want: false,
		},
		{
			name: "review with empty login is ignored",
			pages: [][]map[string]any{{
				prReview("", "MEMBER", "APPROVED"),
			}},
			want: false,
		},
		{
			name: "pagination: veto on a later page is honored",
			pages: [][]map[string]any{
				{prReview("alice", "MEMBER", "APPROVED")},
				{prReview("alice", "MEMBER", "CHANGES_REQUESTED")},
			},
			want: false,
		},
		{
			name: "pagination: approval on a later page is honored",
			pages: [][]map[string]any{
				{prReview("drive-by", "CONTRIBUTOR", "COMMENTED")},
				{prReview("alice", "OWNER", "APPROVED")},
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newReviewsTestClient(t, tc.pages)
			got, err := hasMaintainerApproval(context.Background(), client, "acme", "widgets", 7)
			if err != nil {
				t.Fatalf("hasMaintainerApproval: %v", err)
			}
			if got != tc.want {
				t.Errorf("hasMaintainerApproval = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasMaintainerApprovalAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	client := gh.NewClient(nil)
	base, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	client.BaseURL = base

	approved, err := hasMaintainerApproval(context.Background(), client, "acme", "widgets", 7)
	if err == nil {
		t.Fatal("expected error from failing reviews API, got nil")
	}
	if approved {
		t.Error("approved must be false on API error")
	}
}

func TestMaintainerAssociation(t *testing.T) {
	tests := []struct {
		association string
		want        bool
	}{
		{"OWNER", true},
		{"MEMBER", true},
		{"COLLABORATOR", true},
		{"owner", true},
		{" member ", true},
		{"CONTRIBUTOR", false},
		{"FIRST_TIME_CONTRIBUTOR", false},
		{"NONE", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := maintainerAssociation(tc.association); got != tc.want {
			t.Errorf("maintainerAssociation(%q) = %v, want %v", tc.association, got, tc.want)
		}
	}
}

func TestAlignmentSummary(t *testing.T) {
	tests := []struct {
		name      string
		alignment intent.AlignmentVerdict
		want      string
	}{
		{
			name:      "empty verdict falls back to generic message",
			alignment: intent.AlignmentVerdict{},
			want:      "intent alignment check reported misalignment",
		},
		{
			name:      "rationale only",
			alignment: intent.AlignmentVerdict{Rationale: "diff touches unrelated files"},
			want:      "diff touches unrelated files",
		},
		{
			name: "misaligned deterministic finding is included with files",
			alignment: intent.AlignmentVerdict{
				DeterministicFindings: []intent.AlignmentFinding{
					{Code: "F1", Status: intent.AlignmentStatusMisaligned, Reason: "unexpected file", Files: []string{"a.go", "b.go"}},
					{Code: "F2", Status: intent.AlignmentStatusAligned, Reason: "fine"},
				},
			},
			want: "F1: unexpected file (a.go, b.go)",
		},
		{
			name: "misaligned model rationale is appended",
			alignment: intent.AlignmentVerdict{
				Rationale: "top-level",
				Model:     &intent.ModelAlignmentVerdict{Status: intent.AlignmentStatusMisaligned, Rationale: "scope creep"},
			},
			want: "top-level\nmodel: scope creep",
		},
		{
			name: "aligned model verdict is omitted",
			alignment: intent.AlignmentVerdict{
				Rationale: "top-level",
				Model:     &intent.ModelAlignmentVerdict{Status: intent.AlignmentStatusAligned, Rationale: "all good"},
			},
			want: "top-level",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := alignmentSummary(tc.alignment); got != tc.want {
				t.Errorf("alignmentSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

package reviewgate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func githubFake(t *testing.T, reviews []map[string]any, perms map[string]map[string]string) *gh.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7/reviews":
			_ = json.NewEncoder(w).Encode(reviews)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/collaborators/"):
			login := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/o/r/collaborators/"), "/permission")
			p, ok := perms[login]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(p)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := gh.NewClient(nil)
	base, _ := url.Parse(srv.URL + "/")
	c.BaseURL = base
	return c
}

func review(id int64, login, state, submitted string) map[string]any {
	return map[string]any{"id": id, "user": map[string]string{"login": login}, "state": state, "commit_id": "head", "submitted_at": submitted}
}

func TestCheckGitHub(t *testing.T) {
	perms := map[string]map[string]string{
		"maint":   {"permission": "write"},
		"lead":    {"permission": "write", "role_name": "maintain"},
		"reader":  {"permission": "read"},
		"triager": {"permission": "read", "role_name": "triage"},
	}
	for _, tc := range []struct {
		name    string
		reviews []map[string]any
		want    int
	}{
		{"writer change request blocks", []map[string]any{review(1, "maint", "CHANGES_REQUESTED", "2026-11-26T10:00:00Z")}, 1},
		{"maintain role blocks", []map[string]any{review(1, "lead", "CHANGES_REQUESTED", "2026-11-26T10:00:00Z")}, 1},
		{"same reviewer approval releases", []map[string]any{
			review(1, "maint", "CHANGES_REQUESTED", "2026-11-26T10:00:00Z"),
			review(2, "maint", "APPROVED", "2026-11-26T11:00:00Z"),
		}, 0},
		{"dismissed releases", []map[string]any{review(1, "maint", "DISMISSED", "2026-11-26T10:00:00Z")}, 0},
		{"read access does not block", []map[string]any{review(1, "reader", "CHANGES_REQUESTED", "2026-11-26T10:00:00Z")}, 0},
		{"triage does not block", []map[string]any{review(1, "triager", "CHANGES_REQUESTED", "2026-11-26T10:00:00Z")}, 0},
		{"not a collaborator does not block", []map[string]any{review(1, "stranger", "CHANGES_REQUESTED", "2026-11-26T10:00:00Z")}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks, err := CheckGitHub(context.Background(), githubFake(t, tc.reviews, perms), "o", "r", 7)
			if err != nil {
				t.Fatal(err)
			}
			if len(blocks) != tc.want {
				t.Fatalf("blocks = %+v, want %d", blocks, tc.want)
			}
		})
	}
}

func TestGitHubReviewsNotFoundIsNoReviews(t *testing.T) {
	reviews, err := GitHubReviews(context.Background(), githubFake(t, nil, nil), "o", "r", 99)
	if err != nil || len(reviews) != 0 {
		t.Fatalf("reviews = %v, err = %v; want none for a 404", reviews, err)
	}
}

func TestGitHubReviewsServerErrorIsReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := gh.NewClient(nil)
	base, _ := url.Parse(srv.URL + "/")
	c.BaseURL = base
	if _, err := CheckGitHub(context.Background(), c, "o", "r", 7); err == nil {
		t.Fatal("CheckGitHub = nil error on a 503, want the read failure so callers fail closed")
	}
}

func TestGitHubNilClient(t *testing.T) {
	if _, err := GitHubReviews(context.Background(), nil, "o", "r", 7); err == nil {
		t.Fatal("GitHubReviews(nil client) = nil error")
	}
	if _, err := GitHubWriter(nil, "o", "r")(context.Background(), "x"); err == nil {
		t.Fatal("GitHubWriter(nil client) = nil error")
	}
}

func TestGitHubWriterPropagatesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	c := gh.NewClient(nil)
	base, _ := url.Parse(srv.URL + "/")
	c.BaseURL = base
	if _, err := GitHubWriter(c, "o", "r")(context.Background(), "maint"); err == nil {
		t.Fatal("GitHubWriter on a 502 = nil error, want it returned so Evaluate fails closed")
	}
}

func TestGitHubReviewsPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<`+"http://"+r.Host+r.URL.Path+`?page=2>; rel="next"`)
			_ = json.NewEncoder(w).Encode([]map[string]any{review(1, "maint", "CHANGES_REQUESTED", "2026-11-26T10:00:00Z"), nil})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{review(2, "maint", "APPROVED", "2026-11-26T11:00:00Z")})
	}))
	defer srv.Close()
	c := gh.NewClient(nil)
	base, _ := url.Parse(srv.URL + "/")
	c.BaseURL = base
	reviews, err := GitHubReviews(context.Background(), c, "o", "r", 7)
	if err != nil || len(reviews) != 2 {
		t.Fatalf("reviews = %+v, err = %v; want both pages", reviews, err)
	}
}

package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/workflowmodel/ingest"
)

// branchRulesServer serves the two endpoints FetchBranchRules reads with
// the given status and body for each.
func branchRulesServer(t *testing.T, protStatus int, protBody string, rulesStatus int, rulesBody string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/branches/main/protection", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(protStatus)
		_, _ = w.Write([]byte(protBody))
	})
	mux.HandleFunc("/repos/acme/widgets/rules/branches/main", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(rulesStatus)
		_, _ = w.Write([]byte(rulesBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchBranchRulesProtection(t *testing.T) {
	srv := branchRulesServer(t, http.StatusOK, `{
		"required_status_checks": {"contexts": ["dco"], "checks": [{"context": "pull-widgets-unit"}, {"context": ""}]},
		"required_pull_request_reviews": {"required_approving_review_count": 2, "require_code_owner_reviews": true, "dismiss_stale_reviews": true},
		"enforce_admins": {"enabled": true},
		"required_linear_history": {"enabled": true},
		"required_signatures": {"enabled": true}
	}`, http.StatusOK, `{}`)
	c := newTestClient(t, srv, "acme", nil)
	got, err := c.FetchBranchRules(context.Background(), "acme/widgets", "main")
	if err != nil {
		t.Fatal(err)
	}
	want := &ingest.BranchRules{
		Branch: "main", Source: ingest.BranchRulesProtection, Endpoint: "repos/acme/widgets/branches/main/protection",
		RequiredChecks: []string{"dco", "pull-widgets-unit"}, RequiredApprovals: 2,
		RequireCodeOwnerReview: true, DismissStaleReviews: true, EnforceAdmins: true, LinearHistory: true, SignedCommits: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rules = %+v\nwant %+v", got, want)
	}
}

func TestFetchBranchRulesFallsBackToRulesets(t *testing.T) {
	srv := branchRulesServer(t, http.StatusNotFound, `{"message":"Branch not protected"}`, http.StatusOK, `[
		{"type": "pull_request", "parameters": {"required_approving_review_count": 1, "require_code_owner_review": true, "dismiss_stale_reviews_on_push": false}},
		{"type": "pull_request", "parameters": {"required_approving_review_count": 3}},
		{"type": "required_status_checks", "parameters": {"required_status_checks": [{"context": "ci/lint"}, {"context": "ci/test"}]}},
		{"type": "required_linear_history"},
		{"type": "required_signatures"}
	]`)
	c := newTestClient(t, srv, "acme", nil)
	got, err := c.FetchBranchRules(context.Background(), "acme/widgets", "main")
	if err != nil {
		t.Fatal(err)
	}
	want := &ingest.BranchRules{
		Branch: "main", Source: ingest.BranchRulesRulesets, Endpoint: "repos/acme/widgets/rules/branches/main",
		RequiredChecks: []string{"ci/lint", "ci/test"}, RequiredApprovals: 3, RequireCodeOwnerReview: true, LinearHistory: true, SignedCommits: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rules = %+v\nwant %+v", got, want)
	}
}

func TestFetchBranchRulesNothingReported(t *testing.T) {
	cases := map[string]struct {
		protStatus, rulesStatus int
		protBody, rulesBody     string
	}{
		"unprotected and no rulesets": {http.StatusNotFound, http.StatusNotFound, `{}`, `{}`},
		"forbidden then forbidden":    {http.StatusForbidden, http.StatusForbidden, `{}`, `{}`},
		"empty protection object":     {http.StatusOK, http.StatusOK, `{}`, `[]`},
		"empty ruleset list":          {http.StatusNotFound, http.StatusOK, `{}`, `[]`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := branchRulesServer(t, tc.protStatus, tc.protBody, tc.rulesStatus, tc.rulesBody)
			c := newTestClient(t, srv, "acme", nil)
			got, err := c.FetchBranchRules(context.Background(), "acme/widgets", "main")
			if err != nil || got != nil {
				t.Fatalf("got %+v, %v; want nil, nil", got, err)
			}
		})
	}
}

func TestFetchBranchRulesErrors(t *testing.T) {
	t.Run("nil client", func(t *testing.T) {
		var c *Client
		if _, err := c.FetchBranchRules(context.Background(), "acme/widgets", "main"); !errors.Is(err, ErrNoGitHubClient) {
			t.Fatalf("err = %v, want ErrNoGitHubClient", err)
		}
	})
	t.Run("bad repo", func(t *testing.T) {
		srv := branchRulesServer(t, http.StatusOK, `{}`, http.StatusOK, `[]`)
		c := newTestClient(t, srv, "acme", nil)
		if _, err := c.FetchBranchRules(context.Background(), "widgets", "main"); err == nil {
			t.Fatal("want error for repo without owner")
		}
	})
	t.Run("protection server error", func(t *testing.T) {
		srv := branchRulesServer(t, http.StatusInternalServerError, `{}`, http.StatusOK, `[]`)
		c := newTestClient(t, srv, "acme", nil)
		if _, err := c.FetchBranchRules(context.Background(), "acme/widgets", "main"); err == nil {
			t.Fatal("want error on 500 from protection")
		}
	})
	t.Run("rules server error", func(t *testing.T) {
		srv := branchRulesServer(t, http.StatusNotFound, `{}`, http.StatusInternalServerError, `{}`)
		c := newTestClient(t, srv, "acme", nil)
		if _, err := c.FetchBranchRules(context.Background(), "acme/widgets", "main"); err == nil {
			t.Fatal("want error on 500 from rules")
		}
	})
}

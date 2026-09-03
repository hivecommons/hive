package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/worksource"
)

// TestResolveAdvisoryDigestRoute pins the dispatch contract for
// governor.advisory.target: unset and "github" keep the pinned GitHub issue,
// "linear" needs linear_issue and otherwise fails closed, and an unknown
// target is an error naming the key — never a silent GitHub fallback.
func TestResolveAdvisoryDigestRoute(t *testing.T) {
	cases := []struct {
		name        string
		target      string
		linearIssue string
		wantTarget  string
		wantIssue   string
		wantErr     error // nil means success; sentinel or non-nil means "error expected"
		wantErrText string
	}{
		{name: "unset defaults to github", wantTarget: config.AdvisoryTargetGitHub},
		{name: "explicit github", target: "github", wantTarget: config.AdvisoryTargetGitHub},
		{name: "github ignores a stray linear_issue", target: "github", linearIssue: "ONB-1", wantTarget: config.AdvisoryTargetGitHub},
		{name: "linear with issue", target: "linear", linearIssue: "ONB-123", wantTarget: config.AdvisoryTargetLinear, wantIssue: "ONB-123"},
		{name: "linear case-insensitive", target: "Linear", linearIssue: "ONB-123", wantTarget: config.AdvisoryTargetLinear, wantIssue: "ONB-123"},
		{name: "linear without issue fails closed", target: "linear", wantTarget: config.AdvisoryTargetLinear, wantErr: worksource.ErrLinearAdvisoryIssueUnset, wantErrText: "governor.advisory.linear_issue"},
		{name: "unknown target fails closed", target: "jira", wantTarget: "jira", wantErr: errors.New("x"), wantErrText: "governor.advisory.target"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Governor.Advisory.Target = tc.target
			cfg.Governor.Advisory.LinearIssue = tc.linearIssue
			target, issue, err := resolveAdvisoryDigestRoute(cfg)
			if target != tc.wantTarget || issue != tc.wantIssue {
				t.Errorf("route = (%q, %q), want (%q, %q)", target, issue, tc.wantTarget, tc.wantIssue)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if errors.Is(tc.wantErr, worksource.ErrLinearAdvisoryIssueUnset) && !errors.Is(err, worksource.ErrLinearAdvisoryIssueUnset) {
				t.Errorf("err = %v, want ErrLinearAdvisoryIssueUnset", err)
			}
			if !strings.Contains(err.Error(), tc.wantErrText) {
				t.Errorf("err = %q, want it to name %q", err, tc.wantErrText)
			}
		})
	}
	if target, _, err := resolveAdvisoryDigestRoute(nil); target != config.AdvisoryTargetGitHub || err != nil {
		t.Errorf("nil cfg = (%q, %v), want github with no error", target, err)
	}
}

// TestPostAdvisoryDigestToLinear_UsesWorkSourceKey confirms the Linear route
// authenticates with governor.work_source.linear.api_key and lands the digest
// on the configured issue through the shared poster.
func TestPostAdvisoryDigestToLinear_UsesWorkSourceKey(t *testing.T) {
	var auth string
	var created string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		var req struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var resp map[string]interface{}
		if strings.Contains(req.Query, "commentCreate") {
			created, _ = req.Variables["body"].(string)
			resp = map[string]interface{}{"data": map[string]interface{}{"commentCreate": map[string]interface{}{"success": true}}}
		} else {
			resp = map[string]interface{}{"data": map[string]interface{}{"issue": map[string]interface{}{
				"id": "uuid", "identifier": "ONB-7", "comments": map[string]interface{}{"nodes": []interface{}{}},
			}}}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	orig := linearAdvisoryPosterFor
	linearAdvisoryPosterFor = func(cfg *config.Config) *worksource.LinearAdvisoryPoster {
		return worksource.NewLinearAdvisoryPoster(cfg.Governor.WorkSource.Linear.APIKey, srv.URL, srv.Client())
	}
	defer func() { linearAdvisoryPosterFor = orig }()

	cfg := &config.Config{}
	cfg.Governor.WorkSource.Type = "linear"
	cfg.Governor.WorkSource.Linear.APIKey = "lin_api_test"
	cfg.Governor.Advisory.Target = "linear"
	cfg.Governor.Advisory.LinearIssue = "ONB-7"

	_, issue, err := resolveAdvisoryDigestRoute(cfg)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if err := postAdvisoryDigestToLinear(context.Background(), cfg, issue, "## 🐝 Advisory Digest\n- x"); err != nil {
		t.Fatalf("post: %v", err)
	}
	if auth != "lin_api_test" {
		t.Errorf("Authorization = %q, want the work source api key", auth)
	}
	if !strings.HasPrefix(created, "## 🐝 Advisory Digest") {
		t.Errorf("created comment = %q, want the digest body", created)
	}
}

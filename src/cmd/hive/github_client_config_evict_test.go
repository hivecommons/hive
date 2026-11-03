package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
)

// The attribution audit sink is the one point every hive merge path passes
// through; a pr_merged or pr_closed record there must take the PR off the
// dashboard's listed items, and anything else must leave them alone.
func TestEvictHiveClosedPRFromAuditRecord(t *testing.T) {
	now := time.Date(2026, 10, 26, 18, 25, 46, 0, time.UTC)
	newSrv := func() *dashboard.Server {
		srv := dashboard.NewServer(0, restoreTestLogger())
		srv.UpdateStatus(&dashboard.StatusPayload{Repos: []dashboard.FrontendRepo{{
			Name: "docs", Full: "hivecommons/docs", PRs: 2,
			OpenPrs: []any{
				github.PullRequest{Repo: "hivecommons/docs", Number: 469, State: "open", UpdatedAt: now.Add(-time.Hour)},
				github.PullRequest{Repo: "hivecommons/docs", Number: 470, State: "open", UpdatedAt: now.Add(-time.Hour)},
			},
		}}})
		return srv
	}
	listed := func(srv *dashboard.Server) []int {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		var out struct {
			Repos []struct {
				OpenPrs []struct{ Number int } `json:"openPrs"`
			} `json:"repos"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("status body is not JSON: %v\n%s", err, rec.Body.String())
		}
		var nums []int
		for _, r := range out.Repos {
			for _, pr := range r.OpenPrs {
				nums = append(nums, pr.Number)
			}
		}
		return nums
	}

	cases := []struct {
		name string
		rec  github.AuditRecord
		want []int
	}{
		{"pr_merged evicts", github.AuditRecord{Action: github.AuditActionPRMerged, Repo: "hivecommons/docs", Target: 469}, []int{470}},
		{"pr_closed evicts", github.AuditRecord{Action: github.AuditActionPRClosed, Repo: "hivecommons/docs", Target: 470}, []int{469}},
		{"other action ignored", github.AuditRecord{Action: github.AuditActionAgentPRCreated, Repo: "hivecommons/docs", Target: 469}, []int{469, 470}},
		{"no target ignored", github.AuditRecord{Action: github.AuditActionPRMerged, Repo: "hivecommons/docs"}, []int{469, 470}},
		{"no repo ignored", github.AuditRecord{Action: github.AuditActionPRMerged, Target: 469}, []int{469, 470}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newSrv()
			evictHiveClosedPR(srv, tc.rec, now)
			got := listed(srv)
			if len(got) != len(tc.want) {
				t.Fatalf("listed PRs = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("listed PRs = %v, want %v", got, tc.want)
				}
			}
		})
	}
	evictHiveClosedPR(nil, github.AuditRecord{Action: github.AuditActionPRMerged, Repo: "hivecommons/docs", Target: 469}, now)
}

package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func TestIsDCOContext(t *testing.T) {
	tests := map[string]bool{
		"DCO":       true,
		"dco":       true,
		" Dco ":     true,
		"":          false,
		"dco-check": false,
		"DCO bot":   false,
		"tide":      false,
	}
	for name, want := range tests {
		if got := isDCOContext(name); got != want {
			t.Errorf("isDCOContext(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestDCOContextSucceeded(t *testing.T) {
	status := func(ctx, state string) *gh.RepoStatus {
		return &gh.RepoStatus{Context: gh.Ptr(ctx), State: gh.Ptr(state)}
	}
	run := func(id int64, name, status, conclusion string, started time.Time) *gh.CheckRun {
		cr := &gh.CheckRun{ID: gh.Ptr(id), Name: gh.Ptr(name), Status: gh.Ptr(status), StartedAt: &gh.Timestamp{Time: started}}
		if conclusion != "" {
			cr.Conclusion = gh.Ptr(conclusion)
		}
		return cr
	}
	t0 := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		statuses []*gh.RepoStatus
		runs     []*gh.CheckRun
		want     bool
	}{
		{name: "nothing", want: false},
		{name: "status success", statuses: []*gh.RepoStatus{status("DCO", "success")}, want: true},
		{name: "status failure only", statuses: []*gh.RepoStatus{status("DCO", "failure")}, want: false},
		{name: "non-dco status success", statuses: []*gh.RepoStatus{status("build", "success")}, want: false},
		{
			name:     "status failure but check-run success",
			statuses: []*gh.RepoStatus{status("dco", "failure")},
			runs:     []*gh.CheckRun{run(1, "DCO", "completed", "success", t0)},
			want:     true,
		},
		{name: "check-run action_required", runs: []*gh.CheckRun{run(1, "DCO", "completed", "action_required", t0)}, want: false},
		{name: "check-run still running", runs: []*gh.CheckRun{run(1, "DCO", "in_progress", "", t0)}, want: false},
		{
			name: "newer rerun failure supersedes older success",
			runs: []*gh.CheckRun{
				run(1, "DCO", "completed", "success", t0),
				run(2, "DCO", "completed", "failure", t0.Add(time.Minute)),
			},
			want: false,
		},
		{
			name: "newer rerun success supersedes older failure",
			runs: []*gh.CheckRun{
				run(1, "DCO", "completed", "failure", t0),
				run(2, "DCO", "completed", "success", t0.Add(time.Minute)),
			},
			want: true,
		},
		{
			name: "same-name check from a different app is independent",
			runs: []*gh.CheckRun{
				{ID: gh.Ptr(int64(1)), Name: gh.Ptr("dco"), Status: gh.Ptr("completed"), Conclusion: gh.Ptr("failure"), App: &gh.App{ID: gh.Ptr(int64(10))}},
				{ID: gh.Ptr(int64(2)), Name: gh.Ptr("dco"), Status: gh.Ptr("completed"), Conclusion: gh.Ptr("success"), App: &gh.App{ID: gh.Ptr(int64(20))}},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dcoContextSucceeded(tt.statuses, tt.runs); got != tt.want {
				t.Fatalf("dcoContextSucceeded = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldIgnoreMetaContextForServerEnforcement(t *testing.T) {
	tests := []struct {
		name         string
		ctx          string
		ignoreMeta   bool
		dcoSatisfied bool
		want         bool
	}{
		{name: "disabled ignores nothing even for meta", ctx: "tide", want: false},
		{name: "disabled ignores nothing for satisfied dco", ctx: "DCO", dcoSatisfied: true, want: false},
		{name: "meta check", ctx: "tide", ignoreMeta: true, want: true},
		{name: "meta prefix", ctx: "copilot-review", ignoreMeta: true, want: true},
		{name: "ignorable ci check", ctx: "Playwright", ignoreMeta: true, want: true},
		{name: "chromium shard", ctx: "test (chromium, shard 1/4)", ignoreMeta: true, want: true},
		{name: "real check never ignored", ctx: "build", ignoreMeta: true, dcoSatisfied: true, want: false},
		{name: "dco with sibling success", ctx: "DCO", ignoreMeta: true, dcoSatisfied: true, want: true},
		{name: "dco without sibling success", ctx: "DCO", ignoreMeta: true, want: false},
		{name: "lowercase dco with sibling success", ctx: "dco", ignoreMeta: true, dcoSatisfied: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldIgnoreMetaContextForServerEnforcement(tt.ctx, tt.ignoreMeta, tt.dcoSatisfied)
			if got != tt.want {
				t.Fatalf("shouldIgnoreMetaContextForServerEnforcement(%q,%v,%v) = %v, want %v", tt.ctx, tt.ignoreMeta, tt.dcoSatisfied, got, tt.want)
			}
		})
	}
}

// TestEvaluateCommitCIDCOAndMetaContexts covers the path added for
// unprotected/server-enforced bases: a failing or pending DCO context is
// ignored when a sibling DCO context already succeeded, metadata contexts are
// recorded in IgnoredMetaContexts, and a DCO success never counts as
// positive CI evidence.
func TestEvaluateCommitCIDCOAndMetaContexts(t *testing.T) {
	type status struct {
		Context string `json:"context"`
		State   string `json:"state"`
	}
	tests := []struct {
		name         string
		statuses     []status
		checks       []map[string]any
		opts         CommitCIOptions
		wantGreen    bool
		wantReason   string
		wantIgnored  []string
		wantEvidence int
	}{
		{
			name:     "server-enforced: action_required DCO check ignored when DCO status succeeded",
			statuses: []status{{Context: "DCO", State: "success"}},
			checks: []map[string]any{
				{"id": 1, "name": "DCO", "status": "completed", "conclusion": "action_required"},
				{"id": 2, "name": "build", "status": "completed", "conclusion": "success"},
			},
			opts:         CommitCIOptions{UnknownRequiredChecksServerEnforced: true},
			wantGreen:    true,
			wantIgnored:  []string{"DCO"},
			wantEvidence: 1,
		},
		{
			name: "server-enforced: pending DCO check still blocks without a sibling success",
			checks: []map[string]any{
				{"id": 1, "name": "DCO", "status": "in_progress"},
				{"id": 2, "name": "build", "status": "completed", "conclusion": "success"},
			},
			opts:         CommitCIOptions{UnknownRequiredChecksServerEnforced: true},
			wantReason:   "check-pending",
			wantEvidence: 1,
		},
		{
			name:     "server-enforced: failing DCO status with no success is not ignored",
			statuses: []status{{Context: "dco", State: "failure"}},
			checks: []map[string]any{
				{"id": 1, "name": "build", "status": "completed", "conclusion": "success"},
			},
			opts:         CommitCIOptions{UnknownRequiredChecksServerEnforced: true},
			wantGreen:    true, // completed failures are left to the merge endpoint
			wantEvidence: 1,
		},
		{
			name: "required-known-empty: pending meta check-run ignored and recorded",
			checks: []map[string]any{
				{"id": 1, "name": "tide", "status": "in_progress"},
				{"id": 2, "name": "build", "status": "completed", "conclusion": "success"},
			},
			opts:         CommitCIOptions{RequiredKnownEmpty: true},
			wantGreen:    true,
			wantIgnored:  []string{"tide"},
			wantEvidence: 1,
		},
		{
			name: "required-known-empty: skipped meta check-run is not recorded",
			checks: []map[string]any{
				{"id": 1, "name": "tide", "status": "completed", "conclusion": "skipped"},
				{"id": 2, "name": "build", "status": "completed", "conclusion": "success"},
			},
			opts:         CommitCIOptions{RequiredKnownEmpty: true},
			wantGreen:    true,
			wantEvidence: 1,
		},
		{
			name: "required-known-empty: pending meta status ignored, successful one not recorded, sorted",
			statuses: []status{
				{Context: "netlify/site/deploy-preview", State: "pending"},
				{Context: "copilot", State: "pending"},
				{Context: "tide", State: "success"},
			},
			checks: []map[string]any{
				{"id": 1, "name": "build", "status": "completed", "conclusion": "success"},
			},
			opts:         CommitCIOptions{RequiredKnownEmpty: true},
			wantGreen:    true,
			wantIgnored:  []string{"copilot", "netlify/site/deploy-preview"},
			wantEvidence: 1,
		},
		{
			name:     "DCO success is not positive CI evidence",
			statuses: []status{{Context: "DCO", State: "success"}},
			checks: []map[string]any{
				{"id": 1, "name": "DCO", "status": "completed", "conclusion": "success"},
			},
			opts:       CommitCIOptions{UnknownRequiredChecksServerEnforced: true, RequireEvidence: true},
			wantReason: "ci-unverified",
		},
		{
			name:     "required-known: DCO handling does not bypass a required pending check",
			statuses: []status{{Context: "DCO", State: "success"}},
			checks: []map[string]any{
				{"id": 1, "name": "DCO", "status": "in_progress"},
				{"id": 2, "name": "build", "status": "completed", "conclusion": "success"},
			},
			opts: CommitCIOptions{
				Required:      map[string]bool{"DCO": true, "build": true},
				RequiredKnown: true,
			},
			wantReason:   "required-check-pending:DCO",
			wantEvidence: 1,
		},
		{
			name: "legacy fail-closed: pending meta check is skipped without being recorded",
			checks: []map[string]any{
				{"id": 1, "name": "tide", "status": "in_progress"},
				{"id": 2, "name": "build", "status": "completed", "conclusion": "success"},
			},
			opts:         CommitCIOptions{},
			wantGreen:    true,
			wantEvidence: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/status":
					json.NewEncoder(w).Encode(map[string]any{"state": "pending", "total_count": len(tt.statuses), "statuses": tt.statuses})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/check-runs":
					json.NewEncoder(w).Encode(map[string]any{"total_count": len(tt.checks), "check_runs": tt.checks})
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()
			client := gh.NewClient(nil)
			base, err := url.Parse(api.URL + "/")
			if err != nil {
				t.Fatalf("ParseURL: %v", err)
			}
			client.BaseURL = base

			st, err := EvaluateCommitCI(context.Background(), client, "acme", "widget", "sha", tt.opts)
			if err != nil {
				t.Fatalf("EvaluateCommitCI returned error: %v", err)
			}
			if st.Green != tt.wantGreen || st.Reason != tt.wantReason {
				t.Fatalf("EvaluateCommitCI = (%v,%q), want (%v,%q)", st.Green, st.Reason, tt.wantGreen, tt.wantReason)
			}
			if len(st.IgnoredMetaContexts) != 0 || len(tt.wantIgnored) != 0 {
				if !reflect.DeepEqual(st.IgnoredMetaContexts, tt.wantIgnored) {
					t.Fatalf("IgnoredMetaContexts = %v, want %v", st.IgnoredMetaContexts, tt.wantIgnored)
				}
			}
			if st.SuccessfulCIEvidence != tt.wantEvidence {
				t.Fatalf("SuccessfulCIEvidence = %d, want %d", st.SuccessfulCIEvidence, tt.wantEvidence)
			}
			if want := len(tt.statuses) + len(tt.checks); st.Evidence != want {
				t.Fatalf("Evidence = %d, want %d", st.Evidence, want)
			}
		})
	}
}

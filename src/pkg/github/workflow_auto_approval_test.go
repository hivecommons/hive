package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/writeback"
)

func TestWorkflowApprovalTrusted(t *testing.T) {
	defaults := (config.GovernorCIConfig{}).WorkflowApprovalActors()
	for _, tc := range []struct {
		name, author, actor, app string
		admitted                 bool
		actors                   []string
		want                     bool
	}{
		{name: "dependabot author", author: "dependabot[bot]", actors: defaults, want: true},
		{name: "renovate trigger", author: "outsider", actor: "renovate[bot]", actors: defaults, want: true},
		{name: "app author", author: "hive[bot]", app: "hive[bot]", want: true},
		{name: "app trigger", author: "outsider", actor: "hive[bot]", app: "hive[bot]", want: true},
		{name: "owner admission", author: "outsider", admitted: true, want: true},
		{name: "case and whitespace", actor: "CUSTOM[bot]", actors: []string{" custom[bot] "}, want: true},
		{name: "unknown human", author: "outsider", actors: defaults},
		{name: "other app", author: "other[bot]", app: "hive[bot]", actors: defaults},
		{name: "empty identity", actors: []string{""}},
		{name: "empty allowlist", author: "dependabot[bot]", actors: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := workflowApprovalTrusted(tc.author, tc.actor, tc.app, tc.admitted, tc.actors); got != tc.want {
				t.Fatalf("trusted = %v, want %v", got, tc.want)
			}
		})
	}
}

type workflowApprovalFixture struct {
	mu             sync.Mutex
	fresh          *gh.PullRequest
	runs           []*gh.WorkflowRun
	secondPage     []*gh.WorkflowRun
	posts          []string
	probes         int
	freshReads     int
	approvalStatus int
	listStatus     int
}

func gatedRun(id int64, actor string) *gh.WorkflowRun {
	return &gh.WorkflowRun{
		ID: gh.Ptr(id), HeadSHA: gh.Ptr("head1"), Event: gh.Ptr("pull_request"),
		Status: gh.Ptr("completed"), Conclusion: gh.Ptr(WorkflowApprovalStatus),
		TriggeringActor: &gh.User{Login: gh.Ptr(actor)},
		HeadRepository:  &gh.Repository{FullName: gh.Ptr("fork/repo")},
	}
}

func newWorkflowApprovalFixture(t *testing.T) (*Client, *workflowApprovalFixture, []PullRequest) {
	t.Helper()
	f := &workflowApprovalFixture{
		fresh: &gh.PullRequest{State: gh.Ptr("open"), Number: gh.Ptr(7), User: &gh.User{Login: gh.Ptr("dependabot[bot]")},
			Head: &gh.PullRequestBranch{SHA: gh.Ptr("head1"), Repo: &gh.Repository{FullName: gh.Ptr("fork/repo")}}},
		runs: []*gh.WorkflowRun{gatedRun(11, "outsider"), gatedRun(12, "outsider")},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo/actions/runs":
			f.probes++
			if r.URL.Query().Get("head_sha") == "" || r.URL.Query().Get("event") != "pull_request" {
				t.Errorf("unscoped probe: %s", r.URL)
			}
			if f.listStatus != 0 {
				http.Error(w, `{"message":"unavailable"}`, f.listStatus)
				return
			}
			if r.URL.Query().Get("page") == "2" {
				_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": f.secondPage})
				return
			}
			if len(f.secondPage) > 0 {
				w.Header().Set("Link", `<http://`+r.Host+`/repos/org/repo/actions/runs?page=2>; rel="next"`)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": f.runs})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo/pulls/7":
			f.freshReads++
			_ = json.NewEncoder(w).Encode(f.fresh)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/approve"):
			f.posts = append(f.posts, r.URL.Path)
			if f.approvalStatus != 0 {
				http.Error(w, `{"message":"Resource not accessible by integration"}`, f.approvalStatus)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "org", []string{"repo"})
	return c, f, []PullRequest{{Repo: "org/repo", Number: 7, Author: "dependabot[bot]", HeadSHA: "head1", CIStatus: "pending"}}
}

func TestSweepWorkflowApprovalsOncePerHeadAndInvalidatesProbe(t *testing.T) {
	c, f, prs := newWorkflowApprovalFixture(t)
	ctx := context.Background()
	c.SweepWorkflowApprovals(ctx, prs, config.GovernorCIConfig{})
	if len(f.posts) != 2 || f.probes != 1 {
		t.Fatalf("posts=%v probes=%d", f.posts, f.probes)
	}
	stats := c.WorkflowApprovalsStatus()
	if stats.Approved != 2 || stats.Denied != 0 || stats.Last == nil || stats.Last.RunID != 12 || stats.Last.Head != "head1" {
		t.Fatalf("stats=%+v", stats)
	}
	// A successful POST discards the negative probe immediately, and a
	// still-held result must not cause another POST for the same head.
	c.SweepWorkflowApprovals(ctx, prs, config.GovernorCIConfig{})
	if len(f.posts) != 2 || f.probes != 2 || f.freshReads != 1 {
		t.Fatalf("second pass: posts=%v probes=%d fresh=%d", f.posts, f.probes, f.freshReads)
	}
	prs[0].HeadSHA, prs[0].CIActivity = "head2", nil
	f.fresh.Head.SHA = gh.Ptr("head2")
	f.runs = []*gh.WorkflowRun{gatedRun(21, "outsider")}
	f.runs[0].HeadSHA = gh.Ptr("head2")
	c.SweepWorkflowApprovals(ctx, prs, config.GovernorCIConfig{})
	if len(f.posts) != 3 || c.WorkflowApprovalsStatus().Approved != 3 {
		t.Fatalf("new head did not re-arm: posts=%v", f.posts)
	}
}

func TestSweepWorkflowApprovalsPagination(t *testing.T) {
	c, f, prs := newWorkflowApprovalFixture(t)
	f.secondPage = []*gh.WorkflowRun{gatedRun(13, "outsider")}
	c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
	if len(f.posts) != 3 || f.probes != 2 {
		t.Fatalf("later gated runs omitted: posts=%v probes=%d", f.posts, f.probes)
	}
}

func TestSweepWorkflowApprovalsConcurrentPasses(t *testing.T) {
	c, f, _ := newWorkflowApprovalFixture(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.SweepWorkflowApprovals(context.Background(), []PullRequest{{Repo: "org/repo", Number: 7, HeadSHA: "head1", CIStatus: "pending"}}, config.GovernorCIConfig{})
		}()
	}
	wg.Wait()
	if len(f.posts) != 2 || c.WorkflowApprovalsStatus().Approved != 2 {
		t.Fatalf("concurrent passes repeated approvals: posts=%v stats=%+v", f.posts, c.WorkflowApprovalsStatus())
	}
}

func TestWorkflowApprovalSurvivesCheckEvidenceOverlay(t *testing.T) {
	t.Cleanup(ResetCIEvidenceForTest(nil))
	c, f, prs := newWorkflowApprovalFixture(t)
	f.approvalStatus = http.StatusForbidden
	now := time.Now().UTC()
	prs[0].CIActivity = &PRCIActivity{Head: "head1", Source: "github-check-runs", ObservedAt: &now}
	sharedCIEvidence().recordListed("org/repo", "head1", prs[0].CIActivity)
	c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
	got := HeadCIActivity("org/repo", "head1", prs[0].CIActivity)
	if got.WorkflowApproval == nil || !got.WorkflowApproval.PermissionDenied {
		t.Fatalf("check evidence discarded the later workflow refusal: %+v", got)
	}
	cleared := *prs[0].CIActivity
	cleared.WorkflowApproval = nil
	if got := HeadCIActivity("org/repo", "head1", &cleared); got.WorkflowApproval != nil {
		t.Fatal("check overlay revived an old approval gate")
	}
}

func TestSweepWorkflowApprovalsObservesApprovalWithoutMutatingOldSnapshot(t *testing.T) {
	c, f, prs := newWorkflowApprovalFixture(t)
	c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
	old := prs[0].CIActivity
	f.runs = nil // the approval started the runs; they are no longer gated
	c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
	if prs[0].CIActivity.WorkflowApproval != nil || old.WorkflowApproval == nil {
		t.Fatal("stale gate persisted or an older dashboard snapshot was mutated")
	}
}

func TestSweepWorkflowApprovalsDoesNotRetryOtherAPIErrors(t *testing.T) {
	c, f, prs := newWorkflowApprovalFixture(t)
	f.approvalStatus = http.StatusInternalServerError
	for range 2 {
		c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
	}
	if len(f.posts) != 2 || c.WorkflowApprovalsStatus().PermissionErrors != 0 {
		t.Fatalf("unexpected retry or permission verdict: posts=%v stats=%+v", f.posts, c.WorkflowApprovalsStatus())
	}
}

func TestSweepWorkflowApprovalsPermissionRefusalOnce(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			c, f, prs := newWorkflowApprovalFixture(t)
			f.approvalStatus = code
			c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
			if !prs[0].CIActivity.WorkflowApproval.PermissionDenied {
				t.Fatal("missing manual-approval evidence")
			}
			// CI enrichment creates a new snapshot each cycle. The refusal
			// must survive without another POST or even another probe.
			prs[0].CIActivity = nil
			c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
			stats := c.WorkflowApprovalsStatus()
			if len(f.posts) != 1 || f.probes != 1 || stats.PermissionErrors != 1 || stats.Approved != 0 || !prs[0].CIActivity.WorkflowApproval.PermissionDenied {
				t.Fatalf("posts=%v probes=%d stats=%+v activity=%+v", f.posts, f.probes, stats, prs[0].CIActivity)
			}
		})
	}
}

func TestSweepWorkflowApprovalsTrustIsPerRun(t *testing.T) {
	c, f, prs := newWorkflowApprovalFixture(t)
	f.fresh.User.Login = gh.Ptr("outsider")
	f.runs[0].TriggeringActor.Login = gh.Ptr("renovate[bot]")
	c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
	want := []string{"/repos/org/repo/actions/runs/11/approve"}
	if !reflect.DeepEqual(f.posts, want) || c.WorkflowApprovalsStatus().Denied != 1 {
		t.Fatalf("one actor authorized another's run: posts=%v stats=%+v", f.posts, c.WorkflowApprovalsStatus())
	}
}

func TestSweepWorkflowApprovalsDeniedCanBeAdmittedLater(t *testing.T) {
	c, f, prs := newWorkflowApprovalFixture(t)
	f.fresh.User.Login = gh.Ptr("outsider")
	for range 2 {
		c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
	}
	if len(f.posts) != 0 || c.WorkflowApprovalsStatus().Denied != 2 || f.probes != 1 {
		t.Fatalf("untrusted: posts=%v stats=%+v probes=%d", f.posts, c.WorkflowApprovalsStatus(), f.probes)
	}
	f.fresh.Labels = []*gh.Label{{Name: gh.Ptr("lgtm")}}
	c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
	if len(f.posts) != 2 {
		t.Fatalf("owner admission did not release head: %v", f.posts)
	}
}

func TestSweepWorkflowApprovalsGuards(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*Client, *workflowApprovalFixture, []PullRequest, *config.GovernorCIConfig)
	}{
		{"disabled", func(_ *Client, _ *workflowApprovalFixture, _ []PullRequest, p *config.GovernorCIConfig) {
			p.AutoApproveWorkflows = gh.Ptr(false)
		}},
		{"empty actors", func(_ *Client, _ *workflowApprovalFixture, _ []PullRequest, p *config.GovernorCIConfig) {
			p.AutoApproveWorkflowActors = gh.Ptr([]string{})
		}},
		{"draft", func(_ *Client, _ *workflowApprovalFixture, p []PullRequest, _ *config.GovernorCIConfig) {
			p[0].Draft = true
		}},
		{"held", func(_ *Client, _ *workflowApprovalFixture, p []PullRequest, _ *config.GovernorCIConfig) {
			p[0].Labels = []string{"hold/review"}
		}},
		{"do not merge", func(_ *Client, _ *workflowApprovalFixture, p []PullRequest, _ *config.GovernorCIConfig) {
			p[0].Labels = []string{"do-not-merge"}
		}},
		{"sentinel", func(c *Client, _ *workflowApprovalFixture, p []PullRequest, _ *config.GovernorCIConfig) {
			c.SetSentinelAlertLabel("alert")
			p[0].Labels = []string{"alert"}
		}},
		{"paused", func(c *Client, _ *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			c.SetRepoPausedFunc(func(string) bool { return true })
		}},
		{"fresh hold", func(_ *Client, f *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			f.fresh.Labels = []*gh.Label{{Name: gh.Ptr("hold")}}
		}},
		{"fresh draft", func(_ *Client, f *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			f.fresh.Draft = gh.Ptr(true)
		}},
		{"fresh closed", func(_ *Client, f *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			f.fresh.State = gh.Ptr("closed")
		}},
		{"head changed", func(_ *Client, f *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			f.fresh.Head.SHA = gh.Ptr("newhead")
		}},
		{"wrong run head", func(_ *Client, f *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			for _, r := range f.runs {
				r.HeadSHA = gh.Ptr("oldhead")
			}
		}},
		{"wrong head repository", func(_ *Client, f *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			for _, r := range f.runs {
				r.HeadRepository.FullName = gh.Ptr("other/repo")
			}
		}},
		{"wrong PR", func(_ *Client, f *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			for _, r := range f.runs {
				r.PullRequests = []*gh.PullRequest{{Number: gh.Ptr(8)}}
			}
		}},
		{"writeback disabled", func(c *Client, _ *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			c.SetWritebackGate(writeback.New(writeback.Config{PerRepo: map[string]writeback.RepoOverride{"org/repo": {Mode: writeback.RepoModeOff}}}, writeback.Deps{}, ""))
		}},
		{"different event", func(_ *Client, f *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			for _, r := range f.runs {
				r.Event = gh.Ptr("push")
			}
		}},
		{"no gated runs", func(_ *Client, f *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			for _, r := range f.runs {
				r.Conclusion = gh.Ptr("success")
			}
		}},
		{"probe error", func(_ *Client, f *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			f.listStatus = http.StatusServiceUnavailable
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f, prs := newWorkflowApprovalFixture(t)
			policy := config.GovernorCIConfig{}
			tc.setup(c, f, prs, &policy)
			c.SweepWorkflowApprovals(context.Background(), prs, policy)
			if len(f.posts) != 0 {
				t.Fatalf("guard allowed approvals: %v", f.posts)
			}
		})
	}
}

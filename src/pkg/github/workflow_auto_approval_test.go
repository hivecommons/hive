package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestWorkflowApprovalTrusted(t *testing.T) {
	defaults := (config.GovernorCIConfig{}).WorkflowApprovalActors()
	for _, tc := range []struct {
		name, author, actor, app string
		admitted                 bool
		actors                   []string
		want                     bool
	}{
		{name: "dependabot author", author: "dependabot[bot]", actor: "someone", actors: defaults, want: true},
		{name: "renovate triggering actor", author: "outsider", actor: "renovate[bot]", actors: defaults, want: true},
		{name: "app author", author: "hive[bot]", actor: "outsider", app: "hive[bot]", want: true},
		{name: "app triggering actor", author: "outsider", actor: "hive[bot]", app: "hive[bot]", want: true},
		{name: "owner admission", author: "outsider", actor: "outsider", admitted: true, want: true},
		{name: "case and whitespace", author: "outsider", actor: "CUSTOM[bot]", actors: []string{" custom[bot] "}, want: true},
		{name: "untrusted human", author: "outsider", actor: "outsider", actors: defaults},
		{name: "other app", author: "other[bot]", actor: "other[bot]", app: "hive[bot]", actors: defaults},
		{name: "empty identities", actors: []string{""}},
		{name: "explicit empty allowlist", author: "dependabot[bot]", actor: "dependabot[bot]", actors: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := workflowApprovalTrusted(tc.author, tc.actor, tc.app, tc.admitted, tc.actors); got != tc.want {
				t.Fatalf("trusted = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWorkflowRunApprovable(t *testing.T) {
	base := func() *gh.WorkflowRun { return gatedRun(11, "head1", "dependabot[bot]") }
	for _, tc := range []struct {
		name     string
		mutate   func(*gh.WorkflowRun)
		headRepo string
		want     bool
	}{
		{name: "pull_request run on head", want: true},
		{name: "head repository matches", headRepo: "fork/repo", want: true},
		{name: "pull_request_target is never approved", mutate: func(r *gh.WorkflowRun) { r.Event = gh.Ptr("pull_request_target") }},
		{name: "push event", mutate: func(r *gh.WorkflowRun) { r.Event = gh.Ptr("push") }},
		{name: "other head", mutate: func(r *gh.WorkflowRun) { r.HeadSHA = gh.Ptr("old") }},
		{name: "other head repository", headRepo: "other/repo"},
		{name: "linked to other PR", mutate: func(r *gh.WorkflowRun) { r.PullRequests = []*gh.PullRequest{{Number: gh.Ptr(8)}} }},
		{name: "linked to this PR", mutate: func(r *gh.WorkflowRun) { r.PullRequests = []*gh.PullRequest{{Number: gh.Ptr(7)}} }, want: true},
		{name: "no id", mutate: func(r *gh.WorkflowRun) { r.ID = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := base()
			if tc.mutate != nil {
				tc.mutate(run)
			}
			if got := workflowRunApprovable(run, 7, "head1", tc.headRepo); got != tc.want {
				t.Fatalf("approvable = %v, want %v", got, tc.want)
			}
		})
	}
}

// The probe behind the #16599 caption must keep counting every gated run,
// whatever its event; the event filter belongs to the approval decision
// only (hivecommons/hive#16682).
func TestWorkflowApprovalProbeCountsEveryEvent(t *testing.T) {
	target := gatedRun(12, "head1", "outsider")
	target.Event = gh.Ptr("pull_request_target")
	got := workflowApprovalFromRuns([]*gh.WorkflowRun{gatedRun(11, "head1", "outsider"), target}, time.Now())
	if got == nil || got.Pending != 2 {
		t.Fatalf("probe narrowed the caption: %+v", got)
	}
	var event string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		event = r.URL.Query().Get("event")
		_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []*gh.WorkflowRun{target}})
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"probe-events"})
	t.Cleanup(func() { forgetWorkflowApproval("org/probe-events", "head1") })
	if wa := c.workflowRunsAwaitingApproval(context.Background(), "org/probe-events", "head1"); wa == nil || wa.Pending != 1 {
		t.Fatalf("pull_request_target gated run dropped from the probe: %+v", wa)
	}
	if event != "" {
		t.Fatalf("probe filtered by event=%q", event)
	}
}

func gatedRun(id int64, head, actor string) *gh.WorkflowRun {
	return &gh.WorkflowRun{
		ID: gh.Ptr(id), HeadSHA: gh.Ptr(head), Event: gh.Ptr("pull_request"),
		Status: gh.Ptr("completed"), Conclusion: gh.Ptr(WorkflowApprovalStatus),
		TriggeringActor: &gh.User{Login: gh.Ptr(actor)},
		HeadRepository:  &gh.Repository{FullName: gh.Ptr("fork/repo")},
	}
}

type workflowApprovalFixture struct {
	mu             sync.Mutex
	fresh          *gh.PullRequest
	posts          []string
	freshReads     int
	approvalStatus int
}

func (f *workflowApprovalFixture) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts), f.freshReads
}

// newWorkflowApprovalFixture serves the PR re-read and the approve POST
// for org/repo#7 at head1, a dependabot PR with two gated runs triggered
// by an untrusted actor.
func newWorkflowApprovalFixture(t *testing.T) (*Client, *workflowApprovalFixture, []PullRequest) {
	t.Helper()
	f := &workflowApprovalFixture{
		fresh: &gh.PullRequest{State: gh.Ptr("open"), Number: gh.Ptr(7), User: &gh.User{Login: gh.Ptr("dependabot[bot]")},
			Head: &gh.PullRequestBranch{SHA: gh.Ptr("head1"), Repo: &gh.Repository{FullName: gh.Ptr("fork/repo")}}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo/pulls/7":
			f.freshReads++
			_ = json.NewEncoder(w).Encode(f.fresh)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/approve"):
			f.posts = append(f.posts, r.URL.Path)
			if f.approvalStatus != 0 {
				http.Error(w, `{"message":"Resource not accessible by integration"}`, f.approvalStatus)
				return
			}
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "org", []string{"repo"})
	c.workflowApprovals.stageStore = stagelabels.NewStore("", nil)
	return c, f, []PullRequest{gatedPR("dependabot[bot]", gatedRun(11, "head1", "outsider"), gatedRun(12, "head1", "outsider"))}
}

// admitHead records a person's dashboard Approve of head on org/repo#7's
// stage record, the way api_repo_item_decision.go does.
func admitHead(t *testing.T, c *Client, head string) {
	t.Helper()
	key := stagelabels.Key("org/repo", 7)
	store := c.workflowApprovals.stageStore
	store.Observe(key, stagelabels.ReviewPending, "test")
	if !store.NoteAdmission(key, head, stagelabels.OperatorApprovalAdmission("owner"), time.Now()) {
		t.Fatal("NoteAdmission refused the record")
	}
}

func gatedPR(author string, runs ...*gh.WorkflowRun) PullRequest {
	now := time.Now().UTC()
	return PullRequest{Repo: "org/repo", Number: 7, Author: author, HeadSHA: "head1", CIStatus: "pending", State: "open",
		CIActivity: &PRCIActivity{Head: "head1", ObservedAt: &now, WorkflowApproval: workflowApprovalFromRuns(runs, now)}}
}

func TestSweepWorkflowApprovalsOncePerHeadAndForgetsProbe(t *testing.T) {
	c, f, prs := newWorkflowApprovalFixture(t)
	workflowApprovalCache.Store("org/repo@head1", workflowApprovalEntry{at: time.Now(), val: prs[0].CIActivity.WorkflowApproval})
	t.Cleanup(func() { forgetWorkflowApproval("org/repo", "head1") })
	c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
	if posts, reads := f.counts(); posts != 2 || reads != 1 {
		t.Fatalf("first pass: posts=%d fresh reads=%d", posts, reads)
	}
	if _, ok := workflowApprovalCache.Load("org/repo@head1"); ok {
		t.Fatal("probe cache kept after approving; the next cycle would not see the runs start")
	}
	stats := c.WorkflowApprovalsStatus()
	if stats.Approved != 2 || stats.Denied != 0 || stats.Last == nil || stats.Last.RunID != 12 || stats.Last.Outcome != "approved" {
		t.Fatalf("stats=%+v", stats)
	}
	// Same head still gated next cycle: no second pass, not even a re-read.
	c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
	if posts, reads := f.counts(); posts != 2 || reads != 1 {
		t.Fatalf("second pass repeated: posts=%d fresh reads=%d", posts, reads)
	}
	// A new head re-arms the pass.
	next := gatedPR("dependabot[bot]", gatedRun(21, "head2", "outsider"))
	next.HeadSHA, next.CIActivity.Head = "head2", "head2"
	f.mu.Lock()
	f.fresh.Head.SHA = gh.Ptr("head2")
	f.mu.Unlock()
	c.SweepWorkflowApprovals(context.Background(), []PullRequest{next}, config.GovernorCIConfig{})
	if posts, _ := f.counts(); posts != 3 {
		t.Fatalf("new head not approved: posts=%d", posts)
	}
}

func TestSweepWorkflowApprovalsConcurrentPassesApproveOnce(t *testing.T) {
	c, f, _ := newWorkflowApprovalFixture(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prs := []PullRequest{gatedPR("dependabot[bot]", gatedRun(11, "head1", "outsider"))}
			c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
		}()
	}
	wg.Wait()
	if posts, _ := f.counts(); posts != 1 {
		t.Fatalf("concurrent passes approved %d times", posts)
	}
}

func TestSweepWorkflowApprovalsPermissionRefusalOnce(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			c, f, prs := newWorkflowApprovalFixture(t)
			f.approvalStatus = code
			original := prs[0].CIActivity.WorkflowApproval
			c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
			if wa := prs[0].CIActivity.WorkflowApproval; wa == nil || !wa.PermissionDenied {
				t.Fatalf("refusal not surfaced: %+v", wa)
			}
			if original.PermissionDenied {
				t.Fatal("shared probe result was mutated")
			}
			// Next cycle: a fresh probe snapshot, no new POST, and the
			// refusal still reaches the caption through EnrichCIStatus's
			// ledger overlay, even on cycles that skip the sweep.
			prs = []PullRequest{gatedPR("dependabot[bot]", gatedRun(11, "head1", "outsider"), gatedRun(12, "head1", "outsider"))}
			c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
			if posts, _ := f.counts(); posts != 1 {
				t.Fatalf("refusal retried: posts=%d", posts)
			}
			if wa := c.workflowApprovalWithLedger("org/repo", 7, "head1", prs[0].CIActivity.WorkflowApproval); wa == nil || !wa.PermissionDenied {
				t.Fatalf("ledger lost the refusal: %+v", wa)
			}
			if wa := c.workflowApprovalWithLedger("org/repo", 8, "head1", prs[0].CIActivity.WorkflowApproval); wa.PermissionDenied {
				t.Fatal("refusal leaked to another PR")
			}
			stats := c.WorkflowApprovalsStatus()
			if stats.PermissionErrors != 1 || stats.Approved != 0 || stats.Last == nil || stats.Last.Outcome != "permission_error" {
				t.Fatalf("stats=%+v", stats)
			}
		})
	}
}

func TestSweepWorkflowApprovalsUntrustedThenAdmitted(t *testing.T) {
	c, f, _ := newWorkflowApprovalFixture(t)
	f.fresh.User.Login = gh.Ptr("outsider")
	for range 2 {
		prs := []PullRequest{gatedPR("outsider", gatedRun(11, "head1", "outsider"), gatedRun(12, "head1", "outsider"))}
		c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
	}
	if posts, reads := f.counts(); posts != 0 || reads != 0 {
		t.Fatalf("untrusted head: posts=%d fresh reads=%d", posts, reads)
	}
	if stats := c.WorkflowApprovalsStatus(); stats.Denied != 2 {
		t.Fatalf("denials not counted once per head: %+v", stats)
	}
	// The automerge label alone is not an admission: it is not bound to a
	// head and says nothing about who applied it.
	f.mu.Lock()
	f.fresh.Labels = []*gh.Label{{Name: gh.Ptr(c.AutoMergeLabel())}}
	f.mu.Unlock()
	prs := []PullRequest{gatedPR("outsider", gatedRun(11, "head1", "outsider"))}
	prs[0].Labels = []string{c.AutoMergeLabel()}
	c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
	if posts, _ := f.counts(); posts != 0 {
		t.Fatalf("bare automerge label released the head: posts=%d", posts)
	}
	// A person's admission of this head does.
	admitHead(t, c, "head1")
	c.SweepWorkflowApprovals(context.Background(), prs, config.GovernorCIConfig{})
	if posts, _ := f.counts(); posts != 1 {
		t.Fatalf("owner admission did not release the head: posts=%d", posts)
	}
}

// A contributor admitted at one head pushes another: the admission was for
// what the person read, so the new head's gated runs wait for a person.
func TestSweepWorkflowApprovalsAdmissionIsHeadBound(t *testing.T) {
	c, f, _ := newWorkflowApprovalFixture(t)
	admitHead(t, c, "head1")
	f.mu.Lock()
	f.fresh.User.Login = gh.Ptr("outsider")
	f.fresh.Head.SHA = gh.Ptr("head2")
	f.fresh.Labels = []*gh.Label{{Name: gh.Ptr(c.AutoMergeLabel())}}
	f.mu.Unlock()
	next := gatedPR("outsider", gatedRun(21, "head2", "outsider"))
	next.HeadSHA, next.CIActivity.Head = "head2", "head2"
	next.Labels = []string{c.AutoMergeLabel()}
	c.SweepWorkflowApprovals(context.Background(), []PullRequest{next}, config.GovernorCIConfig{})
	if posts, reads := f.counts(); posts != 0 || reads != 0 {
		t.Fatalf("admission at head1 approved head2: posts=%d fresh reads=%d", posts, reads)
	}
	if stats := c.WorkflowApprovalsStatus(); stats.Denied != 1 || stats.Approved != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	if !c.workflowApprovalAdmitted("org/repo", 7, "head1") || c.workflowApprovalAdmitted("org/repo", 7, "head2") || c.workflowApprovalAdmitted("org/repo", 8, "head1") {
		t.Fatal("admission lookup is not bound to repo, number and head")
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
		{"explicit empty actors", func(_ *Client, _ *workflowApprovalFixture, _ []PullRequest, p *config.GovernorCIConfig) {
			p.AutoApproveWorkflowActors = &[]string{}
		}},
		{"pull_request_target runs", func(_ *Client, _ *workflowApprovalFixture, p []PullRequest, _ *config.GovernorCIConfig) {
			for _, r := range p[0].CIActivity.WorkflowApproval.runs {
				r.Event = gh.Ptr("pull_request_target")
			}
		}},
		{"draft", func(_ *Client, _ *workflowApprovalFixture, p []PullRequest, _ *config.GovernorCIConfig) {
			p[0].Draft = true
		}},
		{"held", func(_ *Client, _ *workflowApprovalFixture, p []PullRequest, _ *config.GovernorCIConfig) {
			p[0].Labels = []string{"hold"}
		}},
		{"paused", func(c *Client, _ *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			c.SetRepoPausedFunc(func(string) bool { return true })
		}},
		{"stale activity head", func(_ *Client, _ *workflowApprovalFixture, p []PullRequest, _ *config.GovernorCIConfig) {
			p[0].CIActivity.Head = "old"
		}},
		{"fresh head moved", func(_ *Client, f *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			f.fresh.Head.SHA = gh.Ptr("head2")
		}},
		{"fresh closed", func(_ *Client, f *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			f.fresh.State = gh.Ptr("closed")
		}},
		{"fresh hold label", func(_ *Client, f *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			f.fresh.Labels = []*gh.Label{{Name: gh.Ptr("hold")}}
		}},
		{"fresh author untrusted", func(_ *Client, f *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			f.fresh.User.Login = gh.Ptr("outsider")
		}},
		{"run from other head repository", func(_ *Client, f *workflowApprovalFixture, _ []PullRequest, _ *config.GovernorCIConfig) {
			f.fresh.Head.Repo.FullName = gh.Ptr("other/repo")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f, prs := newWorkflowApprovalFixture(t)
			policy := config.GovernorCIConfig{}
			tc.setup(c, f, prs, &policy)
			c.SweepWorkflowApprovals(context.Background(), prs, policy)
			if posts, _ := f.counts(); posts != 0 {
				t.Fatalf("guard allowed %d approvals", posts)
			}
		})
	}
}

func TestWorkflowApprovalLedgerEviction(t *testing.T) {
	var l workflowApprovalLedger
	now := time.Now()
	old := workflowApprovalKey{repo: "org/repo", number: 7, head: "head1"}
	other := workflowApprovalKey{repo: "org/repo", number: 9, head: "head9"}
	l.touchLocked(old, now)
	l.touchLocked(other, now)
	l.touchLocked(workflowApprovalKey{repo: "org/repo", number: 7, head: "head2"}, now)
	if _, ok := l.attempts[old]; ok || len(l.attempts) != 2 {
		t.Fatalf("superseded head kept: %+v", l.attempts)
	}
	l.touchLocked(other, now.Add(workflowApprovalAttemptTTL))
	if _, ok := l.attempts[other]; !ok || len(l.attempts) != 1 {
		t.Fatalf("TTL eviction wrong: %+v", l.attempts)
	}
}

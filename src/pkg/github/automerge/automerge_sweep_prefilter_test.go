package automerge

// The list-response prefilters are the cheap gates that keep the sweeps from
// spending a PullRequests.Get on PRs the list payload already disqualifies.
// They fail toward "skip", so every reason string is pinned here: a renamed or
// dropped reason would silently change what the per-tick skip aggregation logs.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	gh "github.com/google/go-github/v72/github"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

func newPrefilterEngine(t *testing.T) *Engine {
	t.Helper()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "http://127.0.0.1:0")
	client.SetAppBotLogin(testHiveAppBotLogin)
	client.SetExemptLabels([]string{"lfx"})
	return New(client, Options{})
}

func selfAuthoredListPR(mutate func(*gh.PullRequest)) *gh.PullRequest {
	pr := &gh.PullRequest{
		Number: gh.Ptr(7),
		State:  gh.Ptr("open"),
		User:   &gh.User{Login: gh.Ptr(testHiveAppBotLogin)},
		Head:   &gh.PullRequestBranch{SHA: gh.Ptr("abc123")},
	}
	if mutate != nil {
		mutate(pr)
	}
	return pr
}

func TestPrefilterSelfAuthoredPRReasons(t *testing.T) {
	c := newPrefilterEngine(t)
	cases := []struct {
		name   string
		mutate func(*gh.PullRequest)
		want   string
	}{
		{"clean candidate passes", nil, ""},
		{"blank state still passes", func(pr *gh.PullRequest) { pr.State = nil }, ""},
		{"closed", func(pr *gh.PullRequest) { pr.State = gh.Ptr("closed") }, "closed"},
		{"draft", func(pr *gh.PullRequest) { pr.Draft = gh.Ptr(true) }, "draft"},
		{"not app authored", func(pr *gh.PullRequest) { pr.User = &gh.User{Login: gh.Ptr("mallory")} }, "not-app-authored"},
		{"held", func(pr *gh.PullRequest) {
			pr.Labels = []*gh.Label{{Name: gh.Ptr("do-not-merge/hold")}}
		}, "held"},
		{"exempt label", func(pr *gh.PullRequest) {
			pr.Labels = []*gh.Label{{Name: gh.Ptr("LFX")}}
		}, "exempt-label"},
		{"nil head", func(pr *gh.PullRequest) { pr.Head = nil }, "missing-head-sha"},
		{"blank head sha", func(pr *gh.PullRequest) { pr.Head = &gh.PullRequestBranch{} }, "missing-head-sha"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.prefilterSelfAuthoredPR(selfAuthoredListPR(tc.mutate)); got != tc.want {
				t.Fatalf("prefilterSelfAuthoredPR = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPrefilterSelfAuthoredPRNilIsSkipped(t *testing.T) {
	c := newPrefilterEngine(t)
	if got := c.prefilterSelfAuthoredPR(nil); got != "missing-head-sha" {
		t.Fatalf("prefilterSelfAuthoredPR(nil) = %q, want %q", got, "missing-head-sha")
	}
}

func queuedListIssue(labels ...string) *gh.Issue {
	issue := &gh.Issue{
		Number:           gh.Ptr(7),
		PullRequestLinks: &gh.PullRequestLinks{URL: gh.Ptr("https://api.github.invalid/repos/acme/widget/pulls/7")},
	}
	for _, l := range labels {
		issue.Labels = append(issue.Labels, &gh.Label{Name: gh.Ptr(l)})
	}
	return issue
}

func TestPrefilterQueuedIssueReasons(t *testing.T) {
	c := newPrefilterEngine(t)
	label := hgithub.AutoMergeQueuedLabel
	cases := []struct {
		name  string
		issue *gh.Issue
		want  string
	}{
		{"labeled PR passes", queuedListIssue(label), ""},
		{"nil issue", nil, "not-pull-request"},
		{"plain issue", &gh.Issue{Number: gh.Ptr(8), Labels: []*gh.Label{{Name: gh.Ptr(label)}}}, "not-pull-request"},
		{"label removed since listing", queuedListIssue("kind/bug"), "label-removed"},
		{"held", queuedListIssue(label, "hold"), "held"},
		{"exempt label", queuedListIssue(label, "LFX"), "exempt-label"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.prefilterQueuedIssue(tc.issue, label); got != tc.want {
				t.Fatalf("prefilterQueuedIssue = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSelfAuthoredSweepIntervalClampsSmallBacklogsToTheDefaultAllowance(t *testing.T) {
	// A post-filter backlog smaller than the default allowance must not
	// shorten the tick below what the static allowance would have produced:
	// the allowance is a floor, not a measurement.
	base := selfAuthoredSweepIntervalForCandidates(6, selfAuthoredSweepCandidateAllowance)
	if got := selfAuthoredSweepIntervalForCandidates(6, 1); got != base {
		t.Fatalf("interval for tiny backlog = %v, want the default-allowance interval %v", got, base)
	}
	if got := selfAuthoredSweepIntervalForCandidates(6, 0); got != base {
		t.Fatalf("interval for empty backlog = %v, want the default-allowance interval %v", got, base)
	}
}

// The package-level one-shot wrappers must behave exactly like building the
// engine by hand; a drift here would let callers of the convenience API skip
// the fail-closed construction in New.

func TestPackageLevelSweepQueuedAutoMergesWrapsTheEngine(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues" {
			w.Write([]byte("[]"))
			return
		}
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
	}))
	defer api.Close()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	result, err := SweepQueuedAutoMerges(context.Background(), client, Options{}, AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if result.Seen != 0 || len(result.Merged) != 0 {
		t.Fatalf("result = %+v, want an empty sweep", result)
	}
}

func TestPackageLevelStartSelfAuthoredSweepHonoursTheACMMGate(t *testing.T) {
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "http://127.0.0.1:0")
	client.SetAppBotLogin(testHiveAppBotLogin)
	level := 1
	// acmmAllowed=false must return without starting the ticker goroutine;
	// nothing to observe beyond "does not panic and does not hang".
	StartSelfAuthoredAutoMergeSweep(context.Background(), client, 1, false, &level, Options{})
}

func TestSweepSelfAuthoredAutoMergesSurfacesListErrors(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	if _, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{}); err == nil {
		t.Fatal("SweepSelfAuthoredAutoMerges returned nil error for a failing list call")
	}
}

func TestSweepSelfAuthoredAutoMergesCountsCandidateFetchErrorsAsSkips(t *testing.T) {
	// A PR that survives the cheap list gates but whose evaluation fetch
	// fails must be logged and skipped, not abort the whole sweep.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls":
			w.Write([]byte(`[{"number":7,"state":"open","draft":false,` +
				`"user":{"login":"` + testHiveAppBotLogin + `"},"head":{"sha":"sha7"}}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	if result.Seen != 1 || result.Candidates != 1 || result.Skipped != 1 || len(result.Merged) != 0 {
		t.Fatalf("result = %+v, want the failing candidate counted and skipped", result)
	}
}

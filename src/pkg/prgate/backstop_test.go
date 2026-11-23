package prgate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// sweepAPI is a fake GitHub with several open PRs: the gate's API (per-PR
// re-read, recorded posts) and the backstop's list and status reads.
type sweepAPI struct {
	prs      map[string][]OpenPR
	subjects map[int]Subject
	statuses map[string]map[string]Status
	listErr  error
	postErr  error

	lists, statusReads, reads int
	posts                     []string // "number@head|context"
	postNumbers               []int
}

func newSweepAPI() *sweepAPI {
	return &sweepAPI{prs: map[string][]OpenPR{}, subjects: map[int]Subject{}, statuses: map[string]map[string]Status{}}
}

func (a *sweepAPI) addPR(repo string, pr OpenPR) {
	a.prs[repo] = append(a.prs[repo], pr)
	a.subjects[pr.Number] = Subject{HeadSHA: pr.HeadSHA, Title: pr.Title, Draft: pr.Draft, State: "open"}
}

func (a *sweepAPI) ListOpenPRs(_ context.Context, repo string, limit int) ([]OpenPR, error) {
	a.lists++
	if limit != MaxBackstopPRsPerRepo {
		return nil, fmt.Errorf("limit %d", limit)
	}
	return a.prs[repo], a.listErr
}

func (a *sweepAPI) HeadStatuses(_ context.Context, _ string, sha string) (map[string]Status, error) {
	a.statusReads++
	return a.statuses[sha], nil
}

func (a *sweepAPI) ReadSubject(_ context.Context, _ string, number int) (Subject, error) {
	a.reads++
	return a.subjects[number], nil
}

func (a *sweepAPI) PostStatus(_ context.Context, _ string, number int, sha, statusContext, _, _ string) error {
	a.posts = append(a.posts, fmt.Sprintf("%d@%s|%s", number, sha[:1], statusContext))
	a.postNumbers = append(a.postNumbers, number)
	return a.postErr
}

func sweepHead(n int) string { return fmt.Sprintf("%040x", n) }

func newSweepGate(api *sweepAPI, mode Mode) *Gate {
	return New(api, func(repo string) Mode {
		if repo == "o/r" || repo == "o/second" {
			return mode
		}
		return ModeOff
	}, quiet())
}

// A head with no gate status (a webhook the hive missed) gets it; a draft is
// skipped without a read; a head that carries the planned status needs no
// post; a repository out of scope costs no request.
func TestSweepPostsOnMissedHeadsOnly(t *testing.T) {
	api := newSweepAPI()
	api.addPR("o/r", OpenPR{Number: 1, HeadSHA: sweepHead(1), Title: "✨ missed"})
	api.addPR("o/r", OpenPR{Number: 2, HeadSHA: sweepHead(2), Title: "✨ draft", Draft: true})
	api.addPR("o/r", OpenPR{Number: 3, HeadSHA: sweepHead(3), Title: "✨ present"})
	api.statuses[sweepHead(3)] = map[string]Status{ContextVerifyShadow: Plan(ModeShadow, "✨ present")[0]}
	g := newSweepGate(api, ModeShadow)

	res := g.Sweep(context.Background(), api, []string{"o/r", "o/out-of-scope"})
	if res.Repos != 1 || res.Listed != 3 || res.Drafts != 1 || res.Present != 1 || res.Missed != 1 || res.Posted != 1 {
		t.Fatalf("result = %+v", res)
	}
	if len(api.posts) != 1 || api.postNumbers[0] != 1 || !strings.HasSuffix(api.posts[0], ContextVerifyShadow) {
		t.Fatalf("posts = %v, want only the shadow status on #1", api.posts)
	}
	if api.lists != 1 {
		t.Errorf("lists = %d, want 1 (the out-of-scope repository is never listed)", api.lists)
	}
	if api.reads != 1 {
		t.Errorf("PR re-reads = %d, want 1 (only the missed head is re-read before posting)", api.reads)
	}

	// The next sweep knows both heads: no status read, no post.
	reads := api.statusReads
	res = g.Sweep(context.Background(), api, []string{"o/r"})
	if res.Known != 2 || res.Missed != 0 || api.statusReads != reads || len(api.posts) != 1 {
		t.Fatalf("second sweep = %+v statusReads=%d posts=%v", res, api.statusReads-reads, api.posts)
	}
}

// After a restart the hive's memory is empty: the sweep reads what GitHub
// already holds instead of posting every head again, and re-posts where the
// status is missing or stale (a title edited while the hive was down).
func TestSweepAfterARestartRepostsOnlyMissingOrStale(t *testing.T) {
	api := newSweepAPI()
	api.addPR("o/r", OpenPR{Number: 1, HeadSHA: sweepHead(1), Title: "✨ kept"})
	api.addPR("o/r", OpenPR{Number: 2, HeadSHA: sweepHead(2), Title: "✨ edited #4"})
	api.statuses[sweepHead(1)] = map[string]Status{ContextVerifyShadow: Plan(ModeShadow, "✨ kept")[0]}
	api.statuses[sweepHead(2)] = map[string]Status{ContextVerifyShadow: Plan(ModeShadow, "✨ edited")[0]}

	res := newSweepGate(api, ModeShadow).Sweep(context.Background(), api, []string{"o/r"})
	if res.Present != 1 || res.Missed != 1 || len(api.posts) != 1 || api.postNumbers[0] != 2 {
		t.Fatalf("result = %+v posts = %v, want only the stale #2 reposted", res, api.posts)
	}
}

// In enforce the sweep posts the real names on a missed head, e.g. the head
// a github.token autofix pushed, which fires no pull_request_target run.
func TestSweepEnforcePostsTheRealNamesOnAMissedHead(t *testing.T) {
	api := newSweepAPI()
	api.addPR("o/r", OpenPR{Number: 5, HeadSHA: sweepHead(5), Title: "🌱 build(deps): bump x from 1 to 2"})
	g := newSweepGate(api, ModeEnforce)
	g.Grant = func(context.Context) (GrantInfo, error) { return grantWriteFixture, nil }
	g.RefreshGrant(context.Background())
	// The sweep posts whatever the event path would for this mode: the real
	// names once enforce is available on a confirmed grant.
	want := map[string]bool{}
	for _, st := range Plan(g.mode("o/r"), "🌱 build(deps): bump x from 1 to 2") {
		want[st.Context] = true
	}
	if EnforceAvailable && (len(want) != 3 || !want[ContextVerify]) {
		t.Fatalf("enforce on a confirmed grant plans %v, want the real names", want)
	}

	g.Sweep(context.Background(), api, []string{"o/r"})
	var got []string
	for _, p := range api.posts {
		got = append(got, p[strings.Index(p, "|")+1:])
	}
	if len(got) != len(want) {
		t.Fatalf("posts = %v, want %v", api.posts, want)
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("posted %q", c)
		}
		if err := PostableContext(c); err != nil {
			t.Errorf("posted a context off the allowlist: %v", err)
		}
	}
}

// The listed head is pinned: a PR whose head moved between the list and the
// re-read gets nothing from the sweep (the push that moved it fires its own
// event), and a PR closed in between gets nothing either.
func TestSweepPinsTheListedHead(t *testing.T) {
	api := newSweepAPI()
	api.addPR("o/r", OpenPR{Number: 1, HeadSHA: sweepHead(1), Title: "✨ moved"})
	api.addPR("o/r", OpenPR{Number: 2, HeadSHA: sweepHead(2), Title: "✨ closed"})
	api.subjects[1] = Subject{HeadSHA: sweepHead(9), Title: "✨ moved", State: "open"}
	api.subjects[2] = Subject{HeadSHA: sweepHead(2), Title: "✨ closed", State: "closed"}

	res := newSweepGate(api, ModeShadow).Sweep(context.Background(), api, []string{"o/r"})
	if len(api.posts) != 0 || res.Missed != 2 || res.Posted != 0 {
		t.Fatalf("result = %+v posts = %v", res, api.posts)
	}
}

// Mode off, a missing grant and the 403 cooldown all cost no request; a 403
// during a sweep stops that repository and is not retried in a loop.
func TestSweepCostsNothingWhenItCannotPost(t *testing.T) {
	t.Run("mode off", func(t *testing.T) {
		api := newSweepAPI()
		api.addPR("o/r", OpenPR{Number: 1, HeadSHA: sweepHead(1), Title: "✨ x"})
		newSweepGate(api, ModeOff).Sweep(context.Background(), api, []string{"o/r"})
		if api.lists+api.statusReads+api.reads+len(api.posts) != 0 {
			t.Errorf("mode off made requests: lists=%d statusReads=%d reads=%d posts=%d", api.lists, api.statusReads, api.reads, len(api.posts))
		}
	})
	t.Run("grant missing", func(t *testing.T) {
		api := newSweepAPI()
		api.addPR("o/r", OpenPR{Number: 1, HeadSHA: sweepHead(1), Title: "✨ x"})
		g := newSweepGate(api, ModeShadow)
		g.Grant = func(context.Context) (GrantInfo, error) { return grantNoneFixture, nil }
		g.Sweep(context.Background(), api, []string{"o/r"})
		if api.lists != 0 || len(api.posts) != 0 {
			t.Errorf("a missing grant listed %d times and posted %v", api.lists, api.posts)
		}
	})
	t.Run("403 during the sweep", func(t *testing.T) {
		api := newSweepAPI()
		for n := 1; n <= 3; n++ {
			api.addPR("o/r", OpenPR{Number: n, HeadSHA: sweepHead(n), Title: "✨ x"})
		}
		api.postErr = errors.Join(ErrForbidden, errors.New("403"))
		g := newSweepGate(api, ModeShadow)
		g.Sweep(context.Background(), api, []string{"o/r"})
		if len(api.posts) != 1 {
			t.Fatalf("posts after a 403 = %v, want the one refused write", api.posts)
		}
		lists := api.lists
		g.Sweep(context.Background(), api, []string{"o/r"})
		if api.lists != lists || len(api.posts) != 1 {
			t.Errorf("the cooldown must skip the repository: lists=%d posts=%v", api.lists-lists, api.posts)
		}
	})
}

// One sweep hands at most MaxBackstopPostsPerSweep heads to the poster; the
// rest are deferred and picked up by the next sweep.
func TestSweepIsBoundedPerSweep(t *testing.T) {
	api := newSweepAPI()
	const extra = 5
	for n := 1; n <= MaxBackstopPostsPerSweep+extra; n++ {
		api.addPR("o/r", OpenPR{Number: n, HeadSHA: sweepHead(n), Title: "✨ x"})
	}
	g := newSweepGate(api, ModeShadow)
	res := g.Sweep(context.Background(), api, []string{"o/r"})
	if res.Missed != MaxBackstopPostsPerSweep || res.Deferred != extra || len(api.posts) != MaxBackstopPostsPerSweep {
		t.Fatalf("first sweep = %+v posts=%d", res, len(api.posts))
	}
	res = g.Sweep(context.Background(), api, []string{"o/r"})
	if res.Missed != extra || res.Known != MaxBackstopPostsPerSweep || len(api.posts) != MaxBackstopPostsPerSweep+extra {
		t.Fatalf("second sweep = %+v posts=%d", res, len(api.posts))
	}
}

// A list failure on one repository is counted and the next repository is
// still swept.
func TestSweepListFailureDoesNotStopOtherRepos(t *testing.T) {
	api := &failingListAPI{sweepAPI: newSweepAPI(), failRepo: "o/r"}
	api.addPR("o/second", OpenPR{Number: 1, HeadSHA: sweepHead(1), Title: "✨ x"})
	res := newSweepGate(api.sweepAPI, ModeShadow).Sweep(context.Background(), api, []string{"o/r", "o/second"})
	if res.Errors != 1 || res.Posted != 1 {
		t.Fatalf("result = %+v", res)
	}
}

type failingListAPI struct {
	*sweepAPI
	failRepo string
}

func (f *failingListAPI) ListOpenPRs(ctx context.Context, repo string, limit int) ([]OpenPR, error) {
	if repo == f.failRepo {
		return nil, errors.New("502")
	}
	return f.sweepAPI.ListOpenPRs(ctx, repo, limit)
}

func TestSweepOnANilGateOrWithoutAnAPIIsANoop(t *testing.T) {
	var g *Gate
	if res := g.Sweep(context.Background(), newSweepAPI(), []string{"o/r"}); res != (SweepResult{}) {
		t.Errorf("nil gate swept: %+v", res)
	}
	if res := newSweepGate(newSweepAPI(), ModeShadow).Sweep(context.Background(), nil, []string{"o/r"}); res != (SweepResult{}) {
		t.Errorf("nil API swept: %+v", res)
	}
}

// contextSweepAPI adds the check-run and status read the escalation needs.
type contextSweepAPI struct {
	*sweepAPI
	has          map[string]bool // head -> carries verify PR contents
	contextReads int
}

func (c *contextSweepAPI) HeadHasContext(_ context.Context, _, sha, name string) (bool, error) {
	c.contextReads++
	if name != ContextVerify {
		return false, fmt.Errorf("asked for %q", name)
	}
	return c.has[sha], nil
}

// After the cutoff, a missing grant with the required context absent on the
// recent heads is logged with the github_app_statuses_permission_missing
// keyword so the fleet report files it; before the cutoff, or while the
// workflow still supplies the context, nothing is raised. Nothing is posted.
func TestSweepEscalatesAMissingGrantAfterTheCutoff(t *testing.T) {
	cases := []struct {
		name        string
		now         time.Time
		carried     bool
		wantEscal   bool
		wantListing bool
	}{
		{name: "before the cutoff", now: Cutoff.Add(-time.Hour)},
		{name: "after the cutoff, context absent", now: Cutoff.Add(time.Hour), wantEscal: true, wantListing: true},
		{name: "after the cutoff, workflow still supplies it", now: Cutoff.Add(time.Hour), carried: true, wantListing: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &contextSweepAPI{sweepAPI: newSweepAPI(), has: map[string]bool{}}
			api.addPR("o/r", OpenPR{Number: 1, HeadSHA: sweepHead(1), Title: "✨ draft", Draft: true})
			for n := 2; n <= 2+MaxEscalationHeads; n++ {
				api.addPR("o/r", OpenPR{Number: n, HeadSHA: sweepHead(n), Title: "✨ x"})
				api.has[sweepHead(n)] = tc.carried
			}
			var logs bytes.Buffer
			g := New(api, func(string) Mode { return ModeShadow }, slog.New(slog.NewTextHandler(&logs, nil)))
			g.Now = func() time.Time { return tc.now }
			g.Grant = func(context.Context) (GrantInfo, error) { return grantNoneFixture, nil }

			res := g.Sweep(context.Background(), api, []string{"o/r"})
			if got := res.Escalated == 1; got != tc.wantEscal {
				t.Fatalf("escalated = %d, want %t (result %+v)", res.Escalated, tc.wantEscal, res)
			}
			if got := strings.Contains(logs.String(), GrantMissingEscalationMsg); got != tc.wantEscal {
				t.Errorf("escalation log present = %t, want %t:\n%s", got, tc.wantEscal, logs.String())
			}
			if (api.lists > 0) != tc.wantListing {
				t.Errorf("lists = %d, want listing %t", api.lists, tc.wantListing)
			}
			if api.contextReads > MaxEscalationHeads {
				t.Errorf("context reads = %d, want at most %d", api.contextReads, MaxEscalationHeads)
			}
			if len(api.posts) != 0 || api.reads != 0 {
				t.Errorf("a missing grant posted %v (re-reads %d)", api.posts, api.reads)
			}
		})
	}
}

// The escalation message is the fleet report's keyword for the class, so a
// renamed message cannot silently stop the filing.
func TestGrantMissingEscalationMsgNamesTheStatusesPermission(t *testing.T) {
	if !strings.Contains(strings.ToLower(GrantMissingEscalationMsg), "the github app lacks the statuses: write permission") {
		t.Fatalf("message %q lost the permission wording", GrantMissingEscalationMsg)
	}
}

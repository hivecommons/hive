package forgesweep

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/effects"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/writeback"
)

var errBoom = errors.New("boom")

// fakeAPI is a scripted forge.SweepAPI. Embedding the interface keeps the
// Forge methods the sweeps never call unimplemented.
type fakeAPI struct {
	forge.SweepAPI

	openErr   error
	open      []forge.ChangeRequest
	pulls     map[int]forge.PullFacts
	pullErr   map[int]error
	labeler   map[int]string
	pushed    map[int]bool // a push followed the label
	labelErr  error
	perms     map[string]string
	permErr   error
	status    map[string][2]any // sha -> {state, total}
	statusErr error
	mergeErr  error
	merges    []forge.MergeOptions

	self       string
	selfErr    error
	merged     []forge.MergedPull
	mergedErr  error
	issues     map[int]string // number -> state; "pull" marks a pull request
	issueErr   error
	comments   map[int][]string
	commentErr error
	reopened   bool
	reopenErr  error
	postErr    error
	posted     map[int][]string
	closeErr   error
	closed     []int
}

func (f *fakeAPI) ListOpenChangeRequests(context.Context, string) ([]forge.ChangeRequest, error) {
	return f.open, f.openErr
}
func (f *fakeAPI) PullFacts(_ context.Context, _ string, n int) (forge.PullFacts, error) {
	if err := f.pullErr[n]; err != nil {
		return forge.PullFacts{}, err
	}
	return f.pulls[n], nil
}
func (f *fakeAPI) LabelLastAdded(_ context.Context, _ string, n int, _ string) (forge.LabelAdd, bool, error) {
	l, ok := f.labeler[n]
	return forge.LabelAdd{Login: l, PushedAfter: f.pushed[n]}, ok, f.labelErr
}
func (f *fakeAPI) Permission(_ context.Context, _, login string) (string, error) {
	return f.perms[login], f.permErr
}
func (f *fakeAPI) CombinedStatus(_ context.Context, _, sha string) (string, int, error) {
	s := f.status[sha]
	state, _ := s[0].(string)
	total, _ := s[1].(int)
	return state, total, f.statusErr
}
func (f *fakeAPI) Merge(_ context.Context, _ string, _ int, opts forge.MergeOptions) error {
	f.merges = append(f.merges, opts)
	return f.mergeErr
}
func (f *fakeAPI) Self(context.Context) (string, error) { return f.self, f.selfErr }
func (f *fakeAPI) RecentlyMergedPulls(context.Context, string, int) ([]forge.MergedPull, error) {
	return f.merged, f.mergedErr
}
func (f *fakeAPI) IssueFacts(_ context.Context, _ string, n int) (forge.IssueFacts, error) {
	st := f.issues[n]
	return forge.IssueFacts{State: st, IsPull: st == "pull", Author: "reporter", Labels: []string{"bug"}}, f.issueErr
}
func (f *fakeAPI) IssueCommentBodies(_ context.Context, _ string, n int) ([]string, error) {
	return f.comments[n], f.commentErr
}
func (f *fakeAPI) IssueReopened(context.Context, string, int) (bool, error) {
	return f.reopened, f.reopenErr
}
func (f *fakeAPI) CreateIssueComment(_ context.Context, _ string, n int, body string) error {
	if f.postErr != nil {
		return f.postErr
	}
	if f.posted == nil {
		f.posted = map[int][]string{}
	}
	f.posted[n] = append(f.posted[n], body)
	return nil
}
func (f *fakeAPI) CloseIssue(_ context.Context, _ string, n int) error {
	if f.closeErr != nil {
		return f.closeErr
	}
	f.closed = append(f.closed, n)
	return nil
}

// readyAPI is one PR (#7) that passes every automerge gate.
func readyAPI() *fakeAPI {
	return &fakeAPI{
		open:    []forge.ChangeRequest{{Number: 7, Labels: []string{"lgtm"}}},
		pulls:   map[int]forge.PullFacts{7: {Number: 7, State: "open", Mergeable: true, Author: "hive-bot", Labels: []string{"lgtm"}, HeadSHA: "abc"}},
		labeler: map[int]string{7: "maintainer"},
		perms:   map[string]string{"maintainer": "write"},
		status:  map[string][2]any{"abc": {"success", 1}},
	}
}

func sweeperFor(api *fakeAPI) *Sweeper {
	return &Sweeper{
		Repos:            []string{"widgets"},
		API:              func(string) (forge.SweepAPI, error) { return api, nil },
		AutoMergeEnabled: func(string) bool { return true },
		AutoMergeLabel:   "lgtm",
		BlockingLabels:   []string{"hold", "", "sentinel-alert"},
	}
}

type recordingBoundary struct {
	claims []effects.Claim
	deny   bool
}

func (b *recordingBoundary) Execute(ctx context.Context, c effects.Claim, effect func(context.Context) (effects.Result, error)) (effects.Result, error) {
	b.claims = append(b.claims, c)
	if b.deny {
		return effects.Result{}, effects.ErrDenied
	}
	return effect(ctx)
}

func TestAutoMergeMergesAtPinnedHeadThroughBoundary(t *testing.T) {
	api := readyAPI()
	s := sweeperFor(api)
	b := &recordingBoundary{}
	s.Mutation = b
	got := s.AutoMerge(context.Background())
	if len(got) != 1 || !got[0].Merged {
		t.Fatalf("AutoMerge = %+v, want one merge", got)
	}
	if want := []forge.MergeOptions{{Strategy: "squash", HeadSHA: "abc"}}; !reflect.DeepEqual(api.merges, want) {
		t.Errorf("merges = %+v, want %+v", api.merges, want)
	}
	if len(b.claims) != 1 || b.claims[0].Kind != effects.KindPullRequestMerge || b.claims[0].Target != "7" || b.claims[0].Inputs["expect_sha"] != "abc" {
		t.Errorf("claims = %+v", b.claims)
	}
}

// TestAutoMergeAcceptsAssignableLabeller is #19914: with a write-access (not
// admin) hive user, Forgejo refuses the permission API and the labeller's
// access reads as forge.PermissionAssignable. That still releases the merge.
func TestAutoMergeAcceptsAssignableLabeller(t *testing.T) {
	api := readyAPI()
	api.perms["maintainer"] = forge.PermissionAssignable
	got := sweeperFor(api).AutoMerge(context.Background())
	if len(got) != 1 || !got[0].Merged {
		t.Fatalf("AutoMerge = %+v, want the merge an assignable labeller released", got)
	}
}

func TestAutoMergeGates(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*fakeAPI, *Sweeper)
		reason string
	}{
		{"read failed", func(a *fakeAPI, _ *Sweeper) { a.pullErr = map[int]error{7: errBoom} }, "read-failed"},
		{"already merged", func(a *fakeAPI, _ *Sweeper) { p := a.pulls[7]; p.Merged = true; a.pulls[7] = p }, "already-merged"},
		{"closed", func(a *fakeAPI, _ *Sweeper) { p := a.pulls[7]; p.State = "closed"; a.pulls[7] = p }, "not-open"},
		{"draft", func(a *fakeAPI, _ *Sweeper) { p := a.pulls[7]; p.Draft = true; a.pulls[7] = p }, "draft"},
		{"label gone", func(a *fakeAPI, _ *Sweeper) { p := a.pulls[7]; p.Labels = nil; a.pulls[7] = p }, "not-labelled"},
		{"no head", func(a *fakeAPI, _ *Sweeper) { p := a.pulls[7]; p.HeadSHA = ""; a.pulls[7] = p }, "no-head"},
		{"hold", func(a *fakeAPI, _ *Sweeper) { p := a.pulls[7]; p.Labels = []string{"lgtm", "Hold"}; a.pulls[7] = p }, "blocked-by-label:hold"},
		{"sentinel", func(a *fakeAPI, _ *Sweeper) {
			p := a.pulls[7]
			p.Labels = []string{"lgtm", "sentinel-alert"}
			a.pulls[7] = p
		}, "blocked-by-label:sentinel-alert"},
		{"not mergeable", func(a *fakeAPI, _ *Sweeper) { p := a.pulls[7]; p.Mergeable = false; a.pulls[7] = p }, "not-mergeable"},
		{"timeline error", func(a *fakeAPI, _ *Sweeper) { a.labelErr = errBoom }, "timeline-read-failed"},
		{"labeler unknown", func(a *fakeAPI, _ *Sweeper) { a.labeler = nil }, "label-adder-unknown"},
		{"self approved", func(a *fakeAPI, _ *Sweeper) { a.labeler[7] = "HIVE-BOT" }, "self-approved"},
		{"pushed after label", func(a *fakeAPI, _ *Sweeper) { a.pushed = map[int]bool{7: true} }, "pushed-after-label"},
		{"permission error", func(a *fakeAPI, _ *Sweeper) { a.permErr = errBoom }, "permission-read-failed"},
		{"labeler read only", func(a *fakeAPI, _ *Sweeper) { a.perms["maintainer"] = "read" }, "labeler-lacks-write"},
		{"labeler none", func(a *fakeAPI, _ *Sweeper) { a.perms["maintainer"] = "none" }, "labeler-lacks-write"},
		{"status error", func(a *fakeAPI, _ *Sweeper) { a.statusErr = errBoom }, "status-read-failed"},
		{"no ci", func(a *fakeAPI, _ *Sweeper) { a.status["abc"] = [2]any{"", 0} }, "no-ci"},
		{"ci pending", func(a *fakeAPI, _ *Sweeper) { a.status["abc"] = [2]any{"pending", 2} }, "ci-pending"},
		{"ci unknown", func(a *fakeAPI, _ *Sweeper) { a.status["abc"] = [2]any{" ", 2} }, "ci-unknown"},
		{"head moved", func(a *fakeAPI, _ *Sweeper) { a.mergeErr = forge.ErrConflict }, "head-moved-or-conflict"},
		{"forge refused", func(a *fakeAPI, _ *Sweeper) { a.mergeErr = forge.ErrNotMergeable }, "forge-refused"},
		{"held at merge", func(a *fakeAPI, _ *Sweeper) { a.mergeErr = forge.ErrHoldActive }, "held"},
		{"merge failed", func(a *fakeAPI, _ *Sweeper) { a.mergeErr = errBoom }, "merge-failed"},
		{"boundary denied", func(_ *fakeAPI, s *Sweeper) { s.Mutation = &recordingBoundary{deny: true} }, "mutation-denied"},
	} {
		t.Run(c.name, func(t *testing.T) {
			api := readyAPI()
			s := sweeperFor(api)
			c.mutate(api, s)
			got := s.AutoMerge(context.Background())
			if len(got) != 1 || got[0].Merged || got[0].Reason != c.reason {
				t.Errorf("AutoMerge = %+v, want reason %q", got, c.reason)
			}
		})
	}
}

func TestAutoMergeAlreadyMergedAtMergeIsDone(t *testing.T) {
	api := readyAPI()
	api.mergeErr = fmt.Errorf("wrapped: %w", forge.ErrAlreadyMerged)
	s := sweeperFor(api)
	s.MergeMethod = "merge"
	got := s.AutoMerge(context.Background())
	if len(got) != 1 || !got[0].Merged || api.merges[0].Strategy != "merge" {
		t.Errorf("AutoMerge = %+v merges %+v", got, api.merges)
	}
}

func TestAutoMergeSkipsAndLimits(t *testing.T) {
	ctx := context.Background()
	// Off: no API, no switch, no label.
	for _, s := range []*Sweeper{{}, {API: sweeperFor(readyAPI()).API}, {API: sweeperFor(readyAPI()).API, AutoMergeEnabled: func(string) bool { return true }}} {
		if got := s.AutoMerge(ctx); got != nil {
			t.Errorf("AutoMerge = %+v, want nil", got)
		}
	}
	// Repo switched off, client error, list error: nothing judged.
	off := sweeperFor(readyAPI())
	off.AutoMergeEnabled = func(string) bool { return false }
	if got := off.AutoMerge(ctx); len(got) != 0 {
		t.Errorf("disabled repo: %+v", got)
	}
	noClient := sweeperFor(readyAPI())
	noClient.API = func(string) (forge.SweepAPI, error) { return nil, errBoom }
	if got := noClient.AutoMerge(ctx); len(got) != 0 {
		t.Errorf("no client: %+v", got)
	}
	listErr := readyAPI()
	listErr.openErr = errBoom
	if got := sweeperFor(listErr).AutoMerge(ctx); len(got) != 0 {
		t.Errorf("list error: %+v", got)
	}
	// Drafts and unlabelled PRs are not even read.
	api := readyAPI()
	api.open = []forge.ChangeRequest{{Number: 1, Draft: true, Labels: []string{"lgtm"}}, {Number: 2}, {Number: 7, Labels: []string{"LGTM"}}, {Number: 7, Labels: []string{"lgtm"}}}
	s := sweeperFor(api)
	s.MaxMerges = 1
	s.Repos = []string{"widgets", "gadgets"}
	got := s.AutoMerge(ctx)
	if len(got) != 1 || got[0].Number != 7 || len(api.merges) != 1 {
		t.Errorf("AutoMerge = %+v merges %d, want one merge capped by MaxMerges", got, len(api.merges))
	}
	// Default cap and a cancelled context.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if got := sweeperFor(readyAPI()).AutoMerge(cctx); len(got) != 0 {
		t.Errorf("cancelled: %+v", got)
	}
}

func TestClosingRefs(t *testing.T) {
	text := "Fixes #12, closes: #3 and resolved #12. Refs #9. Fixes owner/repo#4. Fix #0. prefix#5 closes #x"
	if got, want := ClosingRefs(text), []int{12, 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("ClosingRefs = %v, want %v", got, want)
	}
	for _, kw := range []string{"close", "closes", "closed", "fix", "fixes", "fixed", "resolve", "resolves", "resolved", "FIXES"} {
		if got := ClosingRefs(kw + " #1"); !reflect.DeepEqual(got, []int{1}) {
			t.Errorf("ClosingRefs(%q) = %v", kw, got)
		}
	}
	if got := ClosingRefs("Fixes #99999999999999999999"); len(got) != 0 {
		t.Errorf("overflow ref = %v", got)
	}
}

var sweepNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func closeAPI() *fakeAPI {
	return &fakeAPI{
		self:   "hive-bot",
		perms:  map[string]string{"hive-bot": "write"},
		merged: []forge.MergedPull{{Number: 30, Author: "hive-bot", Body: "Fixes #5", Base: "release", MergedAt: sweepNow.Add(-time.Hour)}},
		issues: map[int]string{5: "open"},
	}
}

func closeSweeper(api *fakeAPI) *Sweeper {
	s := sweeperFor(api)
	s.Now = func() time.Time { return sweepNow }
	return s
}

func TestCloseOnMergeClosesTheGap(t *testing.T) {
	api := closeAPI()
	b := &recordingBoundary{}
	s := closeSweeper(api)
	s.Mutation = b
	got := s.CloseOnMerge(context.Background())
	if len(got) != 1 || !got[0].Closed || got[0].Issue != 5 || got[0].PR != 30 {
		t.Fatalf("CloseOnMerge = %+v", got)
	}
	if !reflect.DeepEqual(api.closed, []int{5}) || len(api.posted[5]) != 1 || !strings.Contains(api.posted[5][0], "<!-- hive-close-on-merge pr=30 -->") {
		t.Errorf("closed %v posted %v", api.closed, api.posted)
	}
	if strings.ContainsAny(api.posted[5][0], "—–") {
		t.Errorf("comment carries a dash: %q", api.posted[5][0])
	}
	if len(b.claims) != 1 || b.claims[0].Kind != effects.KindIssueComment || b.claims[0].Target != "5" {
		t.Errorf("claims = %+v", b.claims)
	}
}

func TestCloseOnMergeSkips(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*fakeAPI)
		reason string // "" means no outcome at all
	}{
		{"too fresh", func(a *fakeAPI) { a.merged[0].MergedAt = sweepNow.Add(-time.Minute) }, ""},
		{"too old", func(a *fakeAPI) { a.merged[0].MergedAt = sweepNow.Add(-8 * 24 * time.Hour) }, ""},
		{"needs confirmation", func(a *fakeAPI) { a.merged[0].Body += "\nhive: needs-confirmation" }, ""},
		{"no closing keyword", func(a *fakeAPI) { a.merged[0].Body = "Refs #5" }, ""},
		{"no author", func(a *fakeAPI) { a.merged[0].Author = "" }, ""},
		{"untrusted author", func(a *fakeAPI) { a.merged[0].Author = "drive-by"; a.perms["drive-by"] = "read" }, ""},
		{"author permission error", func(a *fakeAPI) { a.merged[0].Author = "drive-by"; a.permErr = errBoom }, ""},
		// Only on the assignee list (#19914): not enough to honour an
		// author's closing keywords.
		{"author only assignable", func(a *fakeAPI) {
			a.merged[0].Author = "drive-by"
			a.perms["drive-by"] = forge.PermissionAssignable
		}, ""},
		// The hive's own user (GET /user, or forgejo.login for a
		// repo-limited token, #19994) is trusted only with write on the
		// repo, so a login naming someone else closes nothing.
		{"own user without write", func(a *fakeAPI) { a.perms["hive-bot"] = "read" }, ""},
		{"own user only assignable", func(a *fakeAPI) { a.perms["hive-bot"] = forge.PermissionAssignable }, ""},
		{"own user unknown to the permission API", func(a *fakeAPI) { delete(a.perms, "hive-bot") }, ""},
		{"issue read error", func(a *fakeAPI) { a.issueErr = errBoom }, "issue-read-failed"},
		{"is a pull", func(a *fakeAPI) { a.issues[5] = "pull" }, "is-pull-request"},
		{"already closed", func(a *fakeAPI) { a.issues[5] = "closed" }, "already-closed"},
		{"comments error", func(a *fakeAPI) { a.commentErr = errBoom }, "comments-read-failed"},
		{"reopened", func(a *fakeAPI) {
			a.comments = map[int][]string{5: {"x", "<!-- hive-close-on-merge pr=30 -->\nClosing"}}
			a.reopened = true
		}, "reopened-after-close"},
		{"timeline error", func(a *fakeAPI) {
			a.comments = map[int][]string{5: {"<!-- hive-close-on-merge pr=30 -->"}}
			a.reopenErr = errBoom
		}, "timeline-read-failed"},
		{"comment error", func(a *fakeAPI) { a.postErr = errBoom }, "close-comment-failed"},
		{"close error", func(a *fakeAPI) { a.closeErr = errBoom }, "close-failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			api := closeAPI()
			c.mutate(api)
			got := closeSweeper(api).CloseOnMerge(context.Background())
			if c.reason == "" {
				if len(got) != 0 {
					t.Errorf("CloseOnMerge = %+v, want no outcome", got)
				}
				return
			}
			if len(got) != 1 || got[0].Closed || got[0].Reason != c.reason {
				t.Errorf("CloseOnMerge = %+v, want reason %q", got, c.reason)
			}
		})
	}
}

func TestCloseOnMergeAsksTheWritebackGate(t *testing.T) {
	var asked []writeback.Item
	gate := func(allow bool, reason string) func(string, writeback.Item, writeback.Kind, string) writeback.Decision {
		return func(repo string, item writeback.Item, kind writeback.Kind, preview string) writeback.Decision {
			if repo != "widgets" || kind != writeback.KindIssueClose || !strings.Contains(preview, "#30") {
				t.Errorf("gate asked repo=%q kind=%q preview=%q", repo, kind, preview)
			}
			asked = append(asked, item)
			return writeback.Decision{Allowed: allow, Reason: reason}
		}
	}
	refused := closeAPI()
	s := closeSweeper(refused)
	s.Gate = gate(false, "level")
	got := s.CloseOnMerge(context.Background())
	if len(got) != 1 || got[0].Closed || got[0].Reason != "writeback:level" || len(refused.posted) != 0 || len(refused.closed) != 0 {
		t.Errorf("refused: %+v posted %v closed %v", got, refused.posted, refused.closed)
	}
	noReason := closeAPI()
	s = closeSweeper(noReason)
	s.Gate = gate(false, "")
	if got := s.CloseOnMerge(context.Background()); len(got) != 1 || got[0].Reason != "writeback:refused" {
		t.Errorf("refused without reason: %+v", got)
	}
	allowed := closeAPI()
	s = closeSweeper(allowed)
	s.Gate = gate(true, "own-item")
	if got := s.CloseOnMerge(context.Background()); len(got) != 1 || !got[0].Closed {
		t.Errorf("allowed: %+v", got)
	}
	if len(asked) != 3 || asked[0].Number != 5 || asked[0].Author != "reporter" || asked[0].HiveAuthored || asked[0].Labels[0] != "bug" {
		t.Errorf("items = %+v", asked)
	}
}

func TestCloseOnMergeRetriesAFailedCloseWithoutASecondComment(t *testing.T) {
	api := closeAPI()
	api.comments = map[int][]string{5: {"<!-- hive-close-on-merge pr=30 -->"}}
	got := closeSweeper(api).CloseOnMerge(context.Background())
	if len(got) != 1 || !got[0].Closed || len(api.posted) != 0 {
		t.Errorf("CloseOnMerge = %+v posted %v", got, api.posted)
	}
}

func TestCloseOnMergeTrustsWriters(t *testing.T) {
	api := closeAPI()
	api.merged[0].Author = "maintainer"
	api.perms["maintainer"] = "admin"
	if got := closeSweeper(api).CloseOnMerge(context.Background()); len(got) != 1 || !got[0].Closed {
		t.Errorf("CloseOnMerge = %+v", got)
	}
}

func TestCloseOnMergeRepoErrors(t *testing.T) {
	ctx := context.Background()
	if got := (&Sweeper{}).CloseOnMerge(ctx); got != nil {
		t.Errorf("no API: %+v", got)
	}
	noClient := closeSweeper(closeAPI())
	noClient.API = func(string) (forge.SweepAPI, error) { return nil, errBoom }
	selfErr := closeAPI()
	selfErr.selfErr = errBoom
	listErr := closeAPI()
	listErr.mergedErr = errBoom
	for name, s := range map[string]*Sweeper{"client": noClient, "self": closeSweeper(selfErr), "list": closeSweeper(listErr)} {
		if got := s.CloseOnMerge(ctx); len(got) != 0 {
			t.Errorf("%s error: %+v", name, got)
		}
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if got := closeSweeper(closeAPI()).CloseOnMerge(cctx); len(got) != 0 {
		t.Errorf("cancelled: %+v", got)
	}
	// The real clock is used when Now is unset; a merge an hour before the
	// fixed sweepNow is long past the max age by then only if the test runs
	// far in the future, so only check it does not panic.
	s := sweeperFor(closeAPI())
	_ = s.CloseOnMerge(ctx)
}

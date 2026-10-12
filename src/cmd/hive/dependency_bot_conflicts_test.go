package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
)

const renovateNeverBody = "This PR contains the following updates:\n\n" +
	"### Configuration\n\n📅 **Schedule**: Branch creation - At any time.\n\n" +
	"🚦 **Automerge**: Disabled by config.\n\n" +
	renovateRebaseNeverMarker + "\n\n" +
	"🔕 **Ignore**: Close this PR and you won't be reminded about this update again.\n\n" +
	" - [ ] <!-- rebase-check -->If you want to rebase/retry this PR, check this box\n"

const renovateDefaultBody = "This PR contains the following updates:\n\n" +
	"♻ **Rebasing**: Whenever PR becomes conflicted, or you tick the rebase/retry checkbox.\n\n" +
	" - [ ] <!-- rebase-check -->If you want to rebase/retry this PR, check this box\n"

func renovateConflictPR(sha string) github.PullRequest {
	return github.PullRequest{
		Repo:           "acme/widgets",
		Number:         42,
		Author:         "renovate[bot]",
		State:          "open",
		MergeableState: "dirty",
		HeadSHA:        sha,
		BaseRef:        "main",
	}
}

func TestDependencyBotConflictSelected(t *testing.T) {
	base := renovateConflictPR("sha-1")
	with := func(mut func(*github.PullRequest)) github.PullRequest {
		pr := base
		pr.Labels = append([]string(nil), base.Labels...)
		mut(&pr)
		return pr
	}
	cases := []struct {
		name string
		pr   github.PullRequest
		body string
		want bool
	}{
		{"renovate dirty with rebase-never marker", base, renovateNeverBody, true},
		{"conflicting state spelled conflicting", with(func(p *github.PullRequest) { p.MergeableState = "CONFLICTING" }), renovateNeverBody, true},
		{"no marker: default rebasing policy", base, renovateDefaultBody, false},
		{"empty body", base, "", false},
		{"clean", with(func(p *github.PullRequest) { p.MergeableState = "clean" }), renovateNeverBody, false},
		{"draft", with(func(p *github.PullRequest) { p.Draft = true }), renovateNeverBody, false},
		{"closed", with(func(p *github.PullRequest) { p.State = "closed" }), renovateNeverBody, false},
		{"already needs-human", with(func(p *github.PullRequest) { p.Labels = []string{escalation.NeedsHumanLabel} }), renovateNeverBody, false},
		{"hold", with(func(p *github.PullRequest) { p.Labels = []string{"hold"} }), renovateNeverBody, false},
		{"do-not-merge", with(func(p *github.PullRequest) { p.Labels = []string{"do-not-merge"} }), renovateNeverBody, false},
		{"dependabot same state", with(func(p *github.PullRequest) { p.Author = "dependabot[bot]" }), renovateNeverBody, false},
		{"mergeraptor same state", with(func(p *github.PullRequest) { p.Author = "mergeraptor[bot]" }), renovateNeverBody, false},
		{"human author", with(func(p *github.PullRequest) { p.Author = "alice" }), renovateNeverBody, false},
		{"hive agent bot", with(func(p *github.PullRequest) { p.Author = "hive-agent[bot]" }), renovateNeverBody, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dependencyBotConflictSelected(tc.pr, tc.body); got != tc.want {
				t.Fatalf("dependencyBotConflictSelected = %v, want %v", got, tc.want)
			}
		})
	}
}

type depBotFakeWriter struct {
	comments     []string
	adds         []string
	removes      []string
	failAddLabel bool
}

func (f *depBotFakeWriter) CreateIssueComment(_ context.Context, repo string, number int, body string) error {
	f.comments = append(f.comments, escalation.Key(repo, number)+": "+body)
	return nil
}

func (f *depBotFakeWriter) AddLabels(_ context.Context, repo string, number int, labels []string) error {
	if f.failAddLabel {
		return errors.New("403 forbidden")
	}
	f.adds = append(f.adds, escalation.Key(repo, number)+": "+strings.Join(labels, ","))
	return nil
}

func (f *depBotFakeWriter) RemoveLabel(_ context.Context, repo string, number int, label string) error {
	f.removes = append(f.removes, escalation.Key(repo, number)+": "+label)
	return nil
}

var (
	_ forge.IssueWriter = (*depBotFakeWriter)(nil)
	_ labelRemover      = (*depBotFakeWriter)(nil)
)

type depBotFakeReader struct {
	body        string
	state       string
	bodyFetches int
}

func (r *depBotFakeReader) GetPRBody(context.Context, string, int) (string, error) {
	r.bodyFetches++
	return r.body, nil
}

func (r *depBotFakeReader) GetPRState(context.Context, string, int) (github.PRState, error) {
	if r.state == "" {
		return github.PRState{State: "open"}, nil
	}
	return github.PRState{State: r.state}, nil
}

type depBotHarness struct {
	t      *testing.T
	path   string
	store  *depBotConflictStore
	writer *depBotFakeWriter
	reader *depBotFakeReader
}

func newDepBotHarness(t *testing.T) *depBotHarness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dependency-bot-conflicts.json")
	return &depBotHarness{
		t:      t,
		path:   path,
		store:  loadDepBotConflictStore(path),
		writer: &depBotFakeWriter{},
		reader: &depBotFakeReader{body: renovateNeverBody},
	}
}

func (h *depBotHarness) pass(prs ...github.PullRequest) {
	h.t.Helper()
	a := &github.ActionableResult{}
	a.PRs.Items = prs
	now := func() time.Time { return time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC) }
	sweepDependencyBotConflicts(context.Background(), h.store, "acme", h.writer, h.reader, a, now, discardLogger())
}

const depBotKey = "acme/widgets#42"

func TestDependencyBotConflictSweepFirstPassCommentsThenLabels(t *testing.T) {
	h := newDepBotHarness(t)
	h.pass(renovateConflictPR("sha-1"))

	if len(h.writer.comments) != 1 {
		t.Fatalf("comments = %v, want exactly one", h.writer.comments)
	}
	if !strings.Contains(h.writer.comments[0], "conflicts with `main`") ||
		!strings.Contains(h.writer.comments[0], "If you want to rebase/retry this PR, check this box") {
		t.Fatalf("comment = %q, want base branch and checkbox named", h.writer.comments[0])
	}
	if want := depBotKey + ": " + escalation.NeedsHumanLabel; len(h.writer.adds) != 1 || h.writer.adds[0] != want {
		t.Fatalf("adds = %v, want [%q]", h.writer.adds, want)
	}
	if len(h.writer.removes) != 0 {
		t.Fatalf("removes = %v, want none", h.writer.removes)
	}
}

func TestDependencyBotConflictSweepSecondPassSameSHANoNewComment(t *testing.T) {
	h := newDepBotHarness(t)
	h.pass(renovateConflictPR("sha-1"))
	// The label is now on the PR; even if a pass observed it absent (a human
	// took it off deliberately) the same head must not get a second comment
	// or a re-added label.
	labeled := renovateConflictPR("sha-1")
	labeled.Labels = []string{escalation.NeedsHumanLabel}
	h.pass(labeled)
	h.pass(renovateConflictPR("sha-1"))

	if len(h.writer.comments) != 1 {
		t.Fatalf("comments = %v, want still exactly one", h.writer.comments)
	}
	if len(h.writer.adds) != 1 {
		t.Fatalf("adds = %v, want still exactly one", h.writer.adds)
	}
	if h.reader.bodyFetches != 1 {
		t.Fatalf("body fetches = %d, want 1 (same head is not re-read)", h.reader.bodyFetches)
	}
}

func TestDependencyBotConflictSweepLabelFailureRetriesOnlyLabel(t *testing.T) {
	h := newDepBotHarness(t)
	h.writer.failAddLabel = true
	h.pass(renovateConflictPR("sha-1"))
	if len(h.writer.comments) != 1 || len(h.writer.adds) != 0 {
		t.Fatalf("pass 1: comments=%v adds=%v, want one comment and a failed label", h.writer.comments, h.writer.adds)
	}

	h.writer.failAddLabel = false
	h.pass(renovateConflictPR("sha-1"))
	if len(h.writer.comments) != 1 {
		t.Fatalf("pass 2 comments = %v, want no second comment", h.writer.comments)
	}
	if want := depBotKey + ": " + escalation.NeedsHumanLabel; len(h.writer.adds) != 1 || h.writer.adds[0] != want {
		t.Fatalf("pass 2 adds = %v, want the label retried once", h.writer.adds)
	}
}

func TestDependencyBotConflictSweepUndoRemovesOwnLabelOnce(t *testing.T) {
	h := newDepBotHarness(t)
	h.pass(renovateConflictPR("sha-1"))

	clean := renovateConflictPR("sha-2")
	clean.MergeableState = "clean"
	clean.Labels = []string{escalation.NeedsHumanLabel}
	h.pass(clean)
	h.pass(clean)

	if want := depBotKey + ": " + escalation.NeedsHumanLabel; len(h.writer.removes) != 1 || h.writer.removes[0] != want {
		t.Fatalf("removes = %v, want exactly one removal of %q", h.writer.removes, want)
	}
	if len(h.writer.comments) != 1 {
		t.Fatalf("comments = %v, want only the original comment", h.writer.comments)
	}
}

func TestDependencyBotConflictSweepUndoOnClose(t *testing.T) {
	h := newDepBotHarness(t)
	h.pass(renovateConflictPR("sha-1"))

	h.reader.state = "closed"
	h.pass() // the PR left the open snapshot
	if len(h.writer.removes) != 1 {
		t.Fatalf("removes = %v, want one removal after close", h.writer.removes)
	}
	if _, ok := h.store.get(depBotKey); ok {
		t.Fatal("record kept after close; want it forgotten")
	}
}

func TestDependencyBotConflictSweepUnknownStateDoesNotUndo(t *testing.T) {
	h := newDepBotHarness(t)
	h.pass(renovateConflictPR("sha-1"))

	unknown := renovateConflictPR("sha-1")
	unknown.MergeableState = ""
	unknown.Labels = []string{escalation.NeedsHumanLabel}
	h.pass(unknown)
	h.pass() // absent from the snapshot but still open on the forge
	if len(h.writer.removes) != 0 {
		t.Fatalf("removes = %v, want none without evidence of a rebase or close", h.writer.removes)
	}
}

func TestDependencyBotConflictSweepNeverRemovesForeignLabel(t *testing.T) {
	h := newDepBotHarness(t)
	pre := renovateConflictPR("sha-1")
	pre.Labels = []string{escalation.NeedsHumanLabel}
	h.pass(pre)

	clean := pre
	clean.MergeableState = "clean"
	h.pass(clean)

	if len(h.writer.comments) != 0 || len(h.writer.adds) != 0 || len(h.writer.removes) != 0 {
		t.Fatalf("comments=%v adds=%v removes=%v, want no writes for a pre-existing label",
			h.writer.comments, h.writer.adds, h.writer.removes)
	}
}

func TestDependencyBotConflictSweepRestartReloadsRecord(t *testing.T) {
	h := newDepBotHarness(t)
	h.pass(renovateConflictPR("sha-1"))

	// Simulate a restart: a fresh store loaded from the same file. The PR is
	// observed without the label (e.g. a stale snapshot) on the same head.
	h.store = loadDepBotConflictStore(h.path)
	h.pass(renovateConflictPR("sha-1"))

	if len(h.writer.comments) != 1 {
		t.Fatalf("comments = %v, want no duplicate after reload", h.writer.comments)
	}
	if len(h.writer.adds) != 1 {
		t.Fatalf("adds = %v, want no re-added label after reload", h.writer.adds)
	}
	rec, ok := h.store.get(depBotKey)
	if !ok || rec.HeadSHA != "sha-1" || !rec.LabelApplied {
		t.Fatalf("reloaded record = %+v (ok=%v), want head sha-1 with label applied", rec, ok)
	}
}

func TestDependencyBotConflictSweepSkipsBodyWithoutMarker(t *testing.T) {
	h := newDepBotHarness(t)
	h.reader.body = renovateDefaultBody
	h.pass(renovateConflictPR("sha-1"))
	h.pass(renovateConflictPR("sha-1"))

	if len(h.writer.comments) != 0 || len(h.writer.adds) != 0 {
		t.Fatalf("comments=%v adds=%v, want no writes without the rebase-never marker", h.writer.comments, h.writer.adds)
	}
	if h.reader.bodyFetches != 1 {
		t.Fatalf("body fetches = %d, want 1 per head", h.reader.bodyFetches)
	}
}

func TestDependencyBotConflictSweepNewHeadCommentsAgain(t *testing.T) {
	h := newDepBotHarness(t)
	h.pass(renovateConflictPR("sha-1"))
	// A person removed the label; Renovate later regenerated the branch and
	// it conflicts again on a new head.
	h.pass(renovateConflictPR("sha-2"))
	if len(h.writer.comments) != 2 {
		t.Fatalf("comments = %v, want one per head SHA", h.writer.comments)
	}
}

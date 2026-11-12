package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
)

// depBotRebaseWindow is the default governor.dependency_bot_rebase_after_s;
// the harness passes 0 so the sweep resolves the default itself.
const depBotRebaseWindow = time.Duration(config.DefaultDependencyBotRebaseAfterS) * time.Second

// depBotRebaseFakeReader is a reader that can also list commits and make
// the Renovate rebase request. Its request runs the real checkbox edit
// against body, so tests see the exact description that would be written.
type depBotRebaseFakeReader struct {
	depBotFakeReader
	commits     []github.PRCommit
	commitReads int
	denied      bool
	edits       []string
}

func (r *depBotRebaseFakeReader) ListPRCommits(context.Context, string, int) ([]github.PRCommit, error) {
	r.commitReads++
	return r.commits, nil
}

func (r *depBotRebaseFakeReader) RequestRenovateRebase(_ context.Context, _ string, _ int, _ string) (github.RenovateRebaseCheckbox, error) {
	ticked, state := github.TickRenovateRebaseCheckbox(r.body)
	if state == github.RenovateRebaseCheckboxUnticked {
		r.edits = append(r.edits, ticked)
		r.body = ticked
	}
	return state, nil
}

func (r *depBotRebaseFakeReader) BotRebaseRequestAllowed(string, int) bool { return !r.denied }

var (
	_ depBotCommitLister    = (*depBotRebaseFakeReader)(nil)
	_ depBotRebaseRequester = (*depBotRebaseFakeReader)(nil)
	_ depBotRebasePolicy    = (*depBotRebaseFakeReader)(nil)
	_ depBotCommitLister    = (*github.Client)(nil)
	_ depBotRebaseRequester = (*github.Client)(nil)
)

type depBotRebaseHarness struct {
	t      *testing.T
	path   string
	store  *depBotConflictStore
	writer *depBotFakeWriter
	reader *depBotRebaseFakeReader
}

func newDepBotRebaseHarness(t *testing.T, body string, commits ...github.PRCommit) *depBotRebaseHarness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dependency-bot-conflicts.json")
	newTestEscalationStore(t) // the sweep reads the transient re-run ledger
	return &depBotRebaseHarness{
		t:      t,
		path:   path,
		store:  loadDepBotConflictStore(path),
		writer: &depBotFakeWriter{},
		reader: &depBotRebaseFakeReader{depBotFakeReader: depBotFakeReader{body: body}, commits: commits},
	}
}

func (h *depBotRebaseHarness) passAt(at time.Time, prs ...github.PullRequest) {
	h.t.Helper()
	a := &github.ActionableResult{}
	a.PRs.Items = prs
	sweepDependencyBotConflicts(context.Background(), h.store, "acme", 0, h.writer, h.reader, a, func() time.Time { return at }, discardLogger())
}

func (h *depBotRebaseHarness) assertWrites(stage string, edits, comments, labels int) {
	h.t.Helper()
	if len(h.reader.edits) != edits || len(h.writer.comments) != comments || len(h.writer.adds) != labels {
		h.t.Fatalf("%s: edits=%d comments=%v labels=%v; want %d edits, %d comments, %d labels",
			stage, len(h.reader.edits), h.writer.comments, h.writer.adds, edits, comments, labels)
	}
}

func depBotRRCommit(author, sha string) github.PRCommit {
	return github.PRCommit{SHA: sha, Author: author, ParentCount: 1}
}

// depBotRRBaseMerge is the "Merge branch 'main' into …" commit GitHub's Update
// branch button writes as the person who clicked it.
func depBotRRBaseMerge(author, sha string) github.PRCommit {
	return github.PRCommit{SHA: sha, Author: author, ParentCount: 2}
}

func depBotRRBehindPR(author, sha string) github.PullRequest {
	return depBotPR(author, sha, func(p *github.PullRequest) {
		p.MergeableState = "behind"
		if author != "dependabot[bot]" {
			p.HeadRef = "renovate/widget-2.x"
		}
	})
}

// depBotRRStranded is a Renovate PR someone updated with the Update branch
// button: the head is a person's merge of the base.
func depBotRRStranded(t *testing.T, body string) *depBotRebaseHarness {
	return newDepBotRebaseHarness(t, body, depBotRRCommit("renovate[bot]", "c1"), depBotRRBaseMerge("alice", "sha-2"))
}

func depBotRRWantCheckboxEdit(t *testing.T, before, after string) {
	t.Helper()
	want := strings.Replace(before, "- [ ] <!-- rebase-check -->", "- [x] <!-- rebase-check -->", 1)
	if want == before {
		t.Fatal("fixture has no unticked rebase-check box")
	}
	if after != want {
		t.Fatalf("edited description differs beyond the checkbox:\n got %q\nwant %q", after, want)
	}
}

func TestDependencyBotStrandedRenovateTicksOnlyTheCheckbox(t *testing.T) {
	for name, body := range map[string]string{
		"lf":   renovateDefaultBody,
		"crlf": strings.ReplaceAll(renovateDefaultBody, "\n", "\r\n"),
	} {
		t.Run(name, func(t *testing.T) {
			h := depBotRRStranded(t, body)
			pr := depBotRRBehindPR("renovate[bot]", "sha-2")
			h.passAt(depBotT0, pr)
			h.passAt(depBotT0.Add(depBotRebaseWindow-time.Minute), pr)
			h.assertWrites("before the window", 0, 0, 0)

			h.passAt(depBotT0.Add(depBotRebaseWindow), pr)
			h.assertWrites("at the window", 1, 0, 0)
			depBotRRWantCheckboxEdit(t, body, h.reader.edits[0])

			h.passAt(depBotT0.Add(depBotRebaseWindow+time.Minute), pr)
			h.assertWrites("next pass", 1, 0, 0)
			if h.reader.commitReads != 1 {
				t.Fatalf("commit reads = %d, want 1 per head", h.reader.commitReads)
			}
		})
	}
}

func TestDependencyBotStrandedAlreadyTickedIsNotEdited(t *testing.T) {
	ticked := strings.Replace(renovateDefaultBody, "- [ ] <!-- rebase-check -->", "- [x] <!-- rebase-check -->", 1)
	h := depBotRRStranded(t, ticked)
	pr := depBotRRBehindPR("renovate[bot]", "sha-2")
	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(depBotRebaseWindow), pr)
	h.assertWrites("ticked box at the window", 0, 0, 0)

	// The box stayed ticked a whole window and the head did not move.
	h.passAt(depBotT0.Add(2*depBotRebaseWindow), pr)
	h.assertWrites("a window after", 0, 1, 1)
	if !strings.Contains(h.writer.comments[0], "has been ticked") {
		t.Fatalf("comment = %q, want it to say the box was already ticked", h.writer.comments[0])
	}
}

func TestDependencyBotStrandedNoCheckboxHandsOff(t *testing.T) {
	h := newDepBotRebaseHarness(t, "Bumps widget from 1 to 2.\n", depBotRRCommit("mergeraptor[bot]", "c1"), depBotRRBaseMerge("alice", "sha-2"))
	pr := depBotRRBehindPR("mergeraptor[bot]", "sha-2")
	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(depBotRebaseWindow), pr)
	h.assertWrites("no checkbox", 0, 1, 1)
}

func TestDependencyBotBotHeadOnlyBehindIsLeftAlone(t *testing.T) {
	// A Renovate deployment running as another App commits as renovate[bot]:
	// still the bot's own head.
	h := newDepBotRebaseHarness(t, renovateDefaultBody, depBotRRCommit("renovate[bot]", "sha-1"))
	pr := depBotRRBehindPR("mergeraptor[bot]", "sha-1")
	for _, at := range []time.Duration{0, depBotRebaseWindow - time.Minute, depBotRebaseWindow, 3 * depBotRebaseWindow, 30 * time.Hour} {
		h.passAt(depBotT0.Add(at), pr)
	}
	h.assertWrites("bot head behind", 0, 0, 0)
	if h.reader.commitReads != 1 || h.reader.bodyFetches != 1 {
		t.Fatalf("commit reads = %d, body fetches = %d; want one of each per head", h.reader.commitReads, h.reader.bodyFetches)
	}
}

func TestDependencyBotForeignHeadBeforeWindowIsLeftAlone(t *testing.T) {
	h := depBotRRStranded(t, renovateDefaultBody)
	pr := depBotRRBehindPR("renovate[bot]", "sha-2")
	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(depBotRebaseWindow/2), pr)
	h.passAt(depBotT0.Add(depBotRebaseWindow-time.Second), pr)
	h.assertWrites("before the window", 0, 0, 0)
	if h.reader.commitReads != 0 {
		t.Fatalf("commit reads = %d, want none before the window", h.reader.commitReads)
	}
}

// Dependabot refuses every comment command a GitHub App posts ("Sorry, only
// users with push access can use that command.",
// dependabot/dependabot-core#9147), so a stranded Dependabot PR is handed to
// a person at the window, with the reason in plain words, and no
// `@dependabot` command is ever posted.
func TestDependencyBotStrandedDependabotHandsOffWithoutCommand(t *testing.T) {
	h := newDepBotRebaseHarness(t, "Bumps widget from 1 to 2.\n", depBotRRCommit("dependabot[bot]", "c1"), depBotRRBaseMerge("alice", "sha-2"))
	pr := depBotRRBehindPR("dependabot[bot]", "sha-2")
	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(depBotRebaseWindow-time.Minute), pr)
	h.assertWrites("before the window", 0, 0, 0)

	h.passAt(depBotT0.Add(depBotRebaseWindow), pr)
	h.assertWrites("at the window", 0, 1, 1)
	c := h.writer.comments[0]
	for _, want := range []string{
		depBotStrandedReasonText,
		"does not accept commands from apps",
		"someone with push access comments `" + github.DependabotRecreateCommand + "`",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("comment = %q, want %q", c, want)
		}
	}
	if rec, ok := h.store.get(depBotKey); !ok || rec.Reason != depBotHandoffStranded || rec.HeadSHA != "sha-2" {
		t.Fatalf("record = %+v (ok=%v), want the stranded hand-off on sha-2", rec, ok)
	}
	if st, _ := h.store.rebaseState(depBotKey); !st.RequestedAt.IsZero() || st.Via != "" {
		t.Fatalf("rebase state = %+v, want no request recorded", st)
	}

	h.passAt(depBotT0.Add(2*depBotRebaseWindow), pr)
	h.assertWrites("later pass", 0, 1, 1)
	for _, posted := range h.writer.comments {
		if strings.TrimSpace(strings.TrimPrefix(posted, depBotKey+": ")) == github.DependabotRecreateCommand {
			t.Fatalf("posted the bare %q command as the App", github.DependabotRecreateCommand)
		}
	}
	if h.reader.bodyFetches != 0 {
		t.Fatalf("body fetches = %d, want none for Dependabot", h.reader.bodyFetches)
	}
}

// A record an older build wrote after commenting `@dependabot recreate`
// (refused by Dependabot) still loads, and the PR goes to a person on the
// next pass instead of waiting a window for a rebase that is not coming.
func TestDependencyBotStrandedLegacyDependabotRecordHandsOff(t *testing.T) {
	h := newDepBotRebaseHarness(t, "Bumps widget from 1 to 2.\n", depBotRRCommit("dependabot[bot]", "c1"), depBotRRBaseMerge("alice", "sha-2"))
	pr := depBotRRBehindPR("dependabot[bot]", "sha-2")
	h.store.markRebaseRequested(depBotKey, "sha-2", depBotViaDependabotLegacy, depBotT0)
	h.store = loadDepBotConflictStore(h.path)
	if st, ok := h.store.rebaseState(depBotKey); !ok || st.Via != "dependabot-recreate" {
		t.Fatalf("reloaded state = %+v (ok=%v), want the legacy record", st, ok)
	}

	h.passAt(depBotT0.Add(time.Minute), pr)
	h.assertWrites("legacy record", 0, 1, 1)
	if c := h.writer.comments[0]; !strings.Contains(c, "does not accept commands from apps") {
		t.Fatalf("comment = %q, want the plain-words reason", c)
	}
}

func TestDependencyBotStrandedHeadUnchangedHandsOffOnce(t *testing.T) {
	h := depBotRRStranded(t, renovateDefaultBody)
	pr := depBotRRBehindPR("renovate[bot]", "sha-2")
	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(depBotRebaseWindow), pr)
	h.passAt(depBotT0.Add(2*depBotRebaseWindow-time.Minute), pr)
	h.assertWrites("before the second window", 1, 0, 0)

	h.passAt(depBotT0.Add(2*depBotRebaseWindow), pr)
	h.assertWrites("a window after the request", 1, 1, 1)
	c := h.writer.comments[0]
	if !strings.Contains(c, depBotStrandedReasonText) || !strings.Contains(c, "Hive ticked the rebase/retry box") || !strings.Contains(c, "behind `main`") {
		t.Fatalf("comment = %q, want the reason, the request and the base named", c)
	}
	if want := depBotKey + ": " + escalation.NeedsHumanLabel; h.writer.adds[0] != want {
		t.Fatalf("adds = %v, want [%q]", h.writer.adds, want)
	}

	labeled := pr
	labeled.Labels = []string{escalation.NeedsHumanLabel}
	h.passAt(depBotT0.Add(3*depBotRebaseWindow), labeled)
	h.passAt(depBotT0.Add(4*depBotRebaseWindow), pr) // label seen absent on a stale snapshot
	h.assertWrites("later passes", 1, 1, 1)
	if rec, ok := h.store.get(depBotKey); !ok || rec.Reason != depBotHandoffStranded || rec.HeadSHA != "sha-2" {
		t.Fatalf("record = %+v (ok=%v), want the stranded hand-off on sha-2", rec, ok)
	}
}

func TestDependencyBotStrandedNewHeadResets(t *testing.T) {
	h := depBotRRStranded(t, renovateDefaultBody)
	pr := depBotRRBehindPR("renovate[bot]", "sha-2")
	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(depBotRebaseWindow), pr)
	h.assertWrites("request", 1, 0, 0)

	// Renovate regenerated the branch; base moved again since.
	h.reader.commits = []github.PRCommit{depBotRRCommit("renovate[bot]", "sha-3")}
	next := depBotRRBehindPR("renovate[bot]", "sha-3")
	at := depBotT0.Add(depBotRebaseWindow + 10*time.Minute)
	h.passAt(at, next)
	st, ok := h.store.rebaseState(depBotKey)
	if !ok || st.HeadSHA != "sha-3" || !st.RequestedAt.IsZero() || !st.StuckSince.Equal(at) {
		t.Fatalf("state = %+v (ok=%v), want a fresh record for sha-3", st, ok)
	}
	h.passAt(depBotT0.Add(5*depBotRebaseWindow), next)
	h.assertWrites("new bot head", 1, 0, 0)
}

func TestDependencyBotStrandedKindOffReportsOnly(t *testing.T) {
	h := depBotRRStranded(t, renovateDefaultBody)
	h.reader.denied = true
	pr := depBotRRBehindPR("renovate[bot]", "sha-2")
	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(depBotRebaseWindow-time.Minute), pr)
	h.assertWrites("before the window", 0, 0, 0)
	h.passAt(depBotT0.Add(depBotRebaseWindow), pr)
	h.assertWrites("at the window", 0, 1, 1)
	if c := h.writer.comments[0]; !strings.Contains(c, depBotStrandedReasonText) || !strings.Contains(c, "its last commit is not the bot's") {
		t.Fatalf("comment = %q, want the stranded reason", c)
	}
}

func TestDependencyBotStrandedKindOffConflictKeepsExistingPath(t *testing.T) {
	h := newDepBotRebaseHarness(t, "", depBotRRCommit("dependabot[bot]", "c1"), depBotRRBaseMerge("alice", "sha-2"))
	h.reader.denied = true
	pr := depBotPR("dependabot[bot]", "sha-2", conflicting)
	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(depBotRebaseWindow), pr)
	h.assertWrites("before the conflict grace", 0, 0, 0)
	h.passAt(depBotT0.Add(depBotConflictGrace), pr)
	h.assertWrites("conflict grace", 0, 1, 1)
	if !strings.Contains(h.writer.comments[0], "Dependabot PR has conflicted with `main` for over 24 hours") {
		t.Fatalf("comment = %q, want the existing conflict hand-off", h.writer.comments[0])
	}
}

func TestDependencyBotStrandedConflictAsksBeforeHandingOff(t *testing.T) {
	// A conflicting rebaseWhen-never Renovate PR on the bot's own head: with
	// the kind allowed it is asked about first, not handed off at once.
	h := newDepBotRebaseHarness(t, renovateNeverBody, depBotRRCommit("renovate[bot]", "sha-1"))
	pr := renovateConflictPR("sha-1")
	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(time.Hour), pr)
	h.assertWrites("before the window", 0, 0, 0)

	h.passAt(depBotT0.Add(depBotRebaseWindow), pr)
	h.assertWrites("at the window", 1, 0, 0)
	depBotRRWantCheckboxEdit(t, renovateNeverBody, h.reader.edits[0])

	h.passAt(depBotT0.Add(2*depBotRebaseWindow), pr)
	h.assertWrites("a window after the request", 1, 1, 1)
	if !strings.Contains(h.writer.comments[0], "has conflicted with `main`") {
		t.Fatalf("comment = %q, want the conflict named", h.writer.comments[0])
	}
	h.passAt(depBotT0.Add(depBotConflictGrace+time.Hour), pr)
	h.assertWrites("past the conflict grace", 1, 1, 1)
}

func TestDependencyBotStrandedForeignWorkIsNotRegenerated(t *testing.T) {
	h := newDepBotRebaseHarness(t, renovateDefaultBody, depBotRRCommit("renovate[bot]", "c1"), depBotRRCommit("alice", "sha-2"))
	pr := depBotRRBehindPR("renovate[bot]", "sha-2")
	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(depBotRebaseWindow), pr)
	h.assertWrites("foreign work", 0, 1, 1)
	if !strings.Contains(h.writer.comments[0], "drop commits someone else pushed") {
		t.Fatalf("comment = %q, want it to say why the bot was not asked", h.writer.comments[0])
	}
}

func TestDependencyBotStrandedStateSurvivesRestart(t *testing.T) {
	h := depBotRRStranded(t, renovateDefaultBody)
	pr := depBotRRBehindPR("renovate[bot]", "sha-2")
	h.passAt(depBotT0, pr)

	h.store = loadDepBotConflictStore(h.path)
	h.passAt(depBotT0.Add(depBotRebaseWindow), pr)
	h.assertWrites("first-seen kept across a restart", 1, 0, 0)

	h.store = loadDepBotConflictStore(h.path)
	st, ok := h.store.rebaseState(depBotKey)
	if !ok || st.HeadSHA != "sha-2" || !st.RequestedAt.Equal(depBotT0.Add(depBotRebaseWindow)) || st.Via != depBotViaRenovateCheckbox {
		t.Fatalf("reloaded state = %+v (ok=%v), want the request on sha-2", st, ok)
	}
	h.passAt(depBotT0.Add(2*depBotRebaseWindow-time.Minute), pr)
	h.assertWrites("after the second restart", 1, 0, 0)
	h.passAt(depBotT0.Add(2*depBotRebaseWindow), pr)
	h.assertWrites("hand-off on the persisted request clock", 1, 1, 1)
}

func TestDependencyBotStrandedHandoffUndoneWhenCaughtUp(t *testing.T) {
	h := depBotRRStranded(t, renovateDefaultBody)
	h.reader.denied = true
	pr := depBotRRBehindPR("renovate[bot]", "sha-2")
	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(depBotRebaseWindow), pr)
	h.assertWrites("hand-off", 0, 1, 1)

	clean := depBotPR("renovate[bot]", "sha-4", func(p *github.PullRequest) {
		p.HeadRef = "renovate/widget-2.x"
		p.Labels = []string{escalation.NeedsHumanLabel}
	})
	h.passAt(depBotT0.Add(depBotRebaseWindow+time.Hour), clean)
	if want := depBotKey + ": " + escalation.NeedsHumanLabel; len(h.writer.removes) != 1 || h.writer.removes[0] != want {
		t.Fatalf("removes = %v, want the stranded label taken back once", h.writer.removes)
	}
}

func TestDepBotFactsFromCommits(t *testing.T) {
	pr := depBotRRBehindPR("mergeraptor[bot]", "h")
	cases := []struct {
		name          string
		commits       []github.PRCommit
		head, work, o bool
	}{
		{"bot only (renovate commits under another App)", []github.PRCommit{depBotRRCommit("renovate[bot]", "a"), depBotRRCommit("mergeraptor[bot]", "h")}, false, false, true},
		{"person merged the base", []github.PRCommit{depBotRRCommit("renovate[bot]", "a"), depBotRRBaseMerge("alice", "h")}, true, false, true},
		{"person committed a change", []github.PRCommit{depBotRRCommit("renovate[bot]", "a"), depBotRRCommit("alice", "h")}, true, true, true},
		{"workflow fix then bot", []github.PRCommit{depBotRRCommit("github-actions[bot]", "a"), depBotRRCommit("renovate[bot]", "h")}, false, true, true},
		{"list behind the snapshot", []github.PRCommit{depBotRRCommit("renovate[bot]", "a")}, false, false, false},
		{"empty", nil, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := depBotFactsFromCommits(pr, tc.commits)
			if ok != tc.o || f.ForeignHead != tc.head || f.ForeignWork != tc.work {
				t.Fatalf("facts = %+v ok=%v; want head=%v work=%v ok=%v", f, ok, tc.head, tc.work, tc.o)
			}
		})
	}
}

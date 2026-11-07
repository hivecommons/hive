package automerge

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

type fakeNudgeSink struct {
	nudges   []ContributorNudge
	resolved []int
	outcome  ContributorNudgeOutcome
	// presence overrides the default (a registered relay whose online state
	// mirrors outcome.Online) returned by Presence.
	presence *ContributorPresence
	abandons []ContributorAbandon
}

func (f *fakeNudgeSink) Abandoned(_ context.Context, a ContributorAbandon) {
	f.abandons = append(f.abandons, a)
}

func (f *fakeNudgeSink) Presence(_ context.Context, _ string) ContributorPresence {
	if f.presence != nil {
		return *f.presence
	}
	return ContributorPresence{Registered: true, Online: f.outcome.Online}
}

// nudgeOnlySink is a sink without live presence (the pre-presence hub, or
// hivecommons/hive#12561's lazy wrapper before it forwards Presence).
type nudgeOnlySink struct{ inner *fakeNudgeSink }

func (s nudgeOnlySink) Nudge(ctx context.Context, n ContributorNudge) ContributorNudgeOutcome {
	return s.inner.Nudge(ctx, n)
}

func (s nudgeOnlySink) Resolve(ctx context.Context, owner, repo string, number int) {
	s.inner.Resolve(ctx, owner, repo, number)
}

func (f *fakeNudgeSink) Nudge(_ context.Context, n ContributorNudge) ContributorNudgeOutcome {
	f.nudges = append(f.nudges, n)
	return f.outcome
}

func (f *fakeNudgeSink) Resolve(_ context.Context, _, _ string, number int) {
	f.resolved = append(f.resolved, number)
}

func nudgePR(number int, head string, labels ...string) *gh.PullRequest {
	pr := stagePR(number, labels...)
	pr.Head.SHA = gh.Ptr(head)
	pr.User = &gh.User{Login: gh.Ptr("mallory")}
	// mallory's PR was opened by her own hive: the relay trailer makes it
	// author/clanker, the only kind the nudge and adoption apply to.
	pr.Body = gh.Ptr("Fixes the widget.\n\n— hive: backend=pi model=gpt-6\n")
	pr.Title = gh.Ptr("fix: widget")
	pr.HTMLURL = gh.Ptr("https://github.com/acme/widget/pull/7")
	pr.CreatedAt = &gh.Timestamp{Time: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)}
	// A contributor's hive posted it (author/clanker): the 072 interrupt
	// machinery applies even when the sink has no presence to consult.
	pr.Body = gh.Ptr("Fixes the widget.\n\n— hive: mallory-hive\n")
	return pr
}

func newNudgeEngine(t *testing.T, apiURL string, clock *contributorLaneClock, sink ContributorNudgeSink, policy StageLabelPolicy) *Engine {
	t.Helper()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	store := stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), clock.now)
	return New(client, Options{
		Now:              clock.now,
		StageLabelPolicy: func() StageLabelPolicy { return policy },
		StageTracker:     NewStageTracker(),
		StageStore:       store,
		ContributorNudge: sink,
	})
}

func seededStageAPI(t *testing.T) (*stageAPI, func()) {
	t.Helper()
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	srv := newStageAPI(t, api)
	return api, srv.Close
}

func nudgeComments(api *stageAPI) []string {
	var out []string
	for _, c := range api.comments {
		if strings.HasPrefix(c, ContributorNudgeMarkerPrefix) {
			out = append(out, c)
		}
	}
	return out
}

// One nudge comment per PR (hivecommons/hive#17067): posted on entering 072,
// rewritten in place for a new head red for the same reason class and for a
// changed class, rewritten once more as the repeat reminder after
// RenudgeInterval, never a third time for the same class; leaving 072
// resolves the sink entry.
func TestContributorNudgeOncePerPRAndReason(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	api.statefulComments = true
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	sink := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "spawn", Online: true}}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, sink, StageLabelPolicy{Enabled: true, ContributorNudge: true, RenudgeInterval: 72 * time.Hour})
	ctx := context.Background()
	observe := func(pr *gh.PullRequest, reason string) StageLabelResult {
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, reason, false, nil)
		e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
		res, err := e.ApplyStageLabels(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	lastEdit := func() string {
		if len(api.commentEdits) == 0 {
			return ""
		}
		return api.commentEdits[len(api.commentEdits)-1]
	}
	in072 := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	stalled := append([]string{"stage/stalled"}, in072...)

	res := observe(nudgePR(7, "aaa"), "check-failing:unit")
	if got := nudgeComments(api); len(got) != 1 {
		t.Fatalf("nudges=%d comments=%v", len(got), api.comments)
	} else {
		for _, want := range []string{"reason=check-failing head=aaa n=1 mode=spawn", "@mallory", "`unit`", "push a new head", "parallel fix session"} {
			if !strings.Contains(got[0], want) {
				t.Fatalf("comment missing %q:\n%s", want, got[0])
			}
		}
	}
	if res.Comments != 1 || len(sink.nudges) != 1 || sink.nudges[0].Nudge != 1 || sink.nudges[0].Author != "mallory" ||
		!reflect.DeepEqual(sink.nudges[0].FailingChecks, []string{"unit"}) || sink.nudges[0].HumanChangesRequested {
		t.Fatalf("res=%+v sink=%+v", res, sink.nudges)
	}

	// Same head, same class, next cycles: idempotent, even past the stall
	// threshold (24h) — the repeat window is 72h.
	api.reset()
	clock.advance(time.Hour)
	observe(nudgePR(7, "aaa", in072...), "check-failing:unit")
	if api.writes() != 0 || len(api.commentEdits) != 0 || len(sink.nudges) != 1 {
		t.Fatalf("re-nudged early: comments=%v edits=%v sink=%d", api.comments, api.commentEdits, len(sink.nudges))
	}
	clock.advance(23 * time.Hour)
	observe(nudgePR(7, "aaa", in072...), "check-failing:unit")
	if len(nudgeComments(api)) != 0 || len(api.commentEdits) != 0 || len(sink.nudges) != 1 {
		t.Fatalf("stall threshold nudged: comments=%v edits=%v", api.comments, api.commentEdits)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"stage/stalled"}}) {
		t.Fatalf("stalled label: %v", api.added)
	}

	// New head, still red for the same class (another check): the one
	// comment is rewritten, nothing new is posted, the relay is told, and
	// the class count does not move.
	api.reset()
	clock.advance(time.Hour)
	res = observe(nudgePR(7, "bbb", stalled...), "check-failing:lint")
	if res.Comments != 0 || len(nudgeComments(api)) != 0 || len(api.commentEdits) != 1 {
		t.Fatalf("new head same class: res=%+v comments=%v edits=%v", res, api.comments, api.commentEdits)
	}
	for _, want := range []string{"reason=check-failing head=bbb n=1 mode=spawn", "`lint`"} {
		if !strings.Contains(lastEdit(), want) {
			t.Fatalf("rewrite missing %q:\n%s", want, lastEdit())
		}
	}
	if len(sink.nudges) != 2 || sink.nudges[1].Nudge != 1 || sink.nudges[1].Stalled {
		t.Fatalf("sink=%+v", sink.nudges)
	}
	rec, _ := e.stageStore.Get(stageKey("acme", "widget", 7))
	if rec.NudgeReason != "check-failing" || rec.NudgeReasonCount != 1 || rec.NudgeHead != "bbb" {
		t.Fatalf("ledger %+v", rec)
	}

	// 72h after the class was first counted: the one repeat reminder, as a
	// rewrite; then never again for this class.
	api.reset()
	clock.advance(48 * time.Hour)
	observe(nudgePR(7, "bbb", stalled...), "check-failing:lint")
	if len(nudgeComments(api)) != 0 || len(api.commentEdits) != 2 || !strings.Contains(lastEdit(), "reason=check-failing head=bbb n=2") || !strings.Contains(lastEdit(), "last automatic reminder") {
		t.Fatalf("repeat nudge: comments=%v edits=%v", api.comments, api.commentEdits)
	}
	if len(sink.nudges) != 3 || !sink.nudges[2].Stalled || sink.nudges[2].Nudge != 2 {
		t.Fatalf("sink=%+v", sink.nudges)
	}
	api.reset()
	clock.advance(96 * time.Hour)
	observe(nudgePR(7, "bbb", stalled...), "check-failing:lint")
	if len(nudgeComments(api)) != 0 || len(api.commentEdits) != 2 || len(sink.nudges) != 3 {
		t.Fatalf("third nudge: comments=%v edits=%v sink=%d", api.comments, api.commentEdits, len(sink.nudges))
	}

	// A different class on a new head restarts the count — still as a
	// rewrite of the same comment.
	api.reset()
	res = observe(nudgePR(7, "ccc", stalled...), "changes-requested")
	if res.Comments != 0 || len(nudgeComments(api)) != 0 || len(api.commentEdits) != 3 {
		t.Fatalf("changed reason: res=%+v comments=%v edits=%v", res, api.comments, api.commentEdits)
	}
	for _, want := range []string{"reason=changes-requested head=ccc n=1", "reviewer requested changes"} {
		if !strings.Contains(lastEdit(), want) {
			t.Fatalf("rewrite missing %q:\n%s", want, lastEdit())
		}
	}
	if len(sink.nudges) != 4 || !sink.nudges[3].HumanChangesRequested || sink.nudges[3].Nudge != 1 {
		t.Fatalf("sink=%+v", sink.nudges)
	}
	if len(sink.resolved) != 0 {
		t.Fatalf("resolved while still in 072: %v", sink.resolved)
	}
	if len(api.comments) != 0 {
		t.Fatalf("a second nudge comment was posted: %v", api.comments)
	}

	// New head goes green and awaits review: leaves 072 → Resolve once.
	api.reset()
	observe(nudgePR(7, "ddd", in072...), "required-check-missing:unit")
	if !reflect.DeepEqual(sink.resolved, []int{7}) {
		t.Fatalf("resolved=%v", sink.resolved)
	}
	observe(nudgePR(7, "ddd", "stage/050-review-pending", "role/contributor", "author/clanker"), "required-check-missing:unit")
	if !reflect.DeepEqual(sink.resolved, []int{7}) {
		t.Fatalf("resolve repeated: %v", sink.resolved)
	}
}

// Merged/closed while in 072 resolves the sink; a disabled nudge policy
// posts nothing and never calls the sink; a nil sink still comments.
func TestContributorNudgeTerminalDisabledAndNilSink(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	ctx := context.Background()

	sink := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "queued", QueueDepth: 3}}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, sink, StageLabelPolicy{Enabled: true, ContributorNudge: true})
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", nudgePR(7, "aaa"), "conflicting", false, nil)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if got := nudgeComments(api); len(got) != 1 || !strings.Contains(got[0], "mode=queued") || !strings.Contains(got[0], "(3 waiting)") || !strings.Contains(got[0], "needs a rebase") {
		t.Fatalf("queued nudge: %v", api.comments)
	}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", nudgePR(7, "aaa"), "closed", false, nil)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sink.resolved, []int{7}) {
		t.Fatalf("terminal resolve: %v", sink.resolved)
	}

	api.reset()
	sink = &fakeNudgeSink{}
	e = newNudgeEngine(t, apiURLOf(t, api), clock, sink, StageLabelPolicy{Enabled: true})
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", nudgePR(8, "aaa"), "check-failing:unit", false, nil)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if len(nudgeComments(api)) != 0 || len(sink.nudges) != 0 {
		t.Fatalf("disabled nudge wrote: %v %v", api.comments, sink.nudges)
	}

	api.reset()
	api.statefulComments = true
	e = newNudgeEngine(t, apiURLOf(t, api), clock, nil, StageLabelPolicy{Enabled: true, ContributorNudge: true, RenudgeInterval: time.Hour})
	pr := nudgePR(9, "aaa")
	e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, "status-failure", false, nil)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if got := nudgeComments(api); len(got) != 1 || !strings.Contains(got[0], "mode=comment") || !strings.Contains(got[0], "`status-failure`") {
		t.Fatalf("nil-sink nudge: %v", api.comments)
	}
	// RenudgeInterval times the repeat: rewritten (not re-posted) after 1h.
	api.reset()
	clock.advance(time.Hour)
	e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", nudgePR(9, "aaa", "stage/072-returned-to-contributor", "role/trusted", "author/clanker"), "status-failure", false, nil)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if len(nudgeComments(api)) != 0 || len(api.commentEdits) != 1 || !strings.Contains(api.commentEdits[0], "reason=status-failure head=aaa n=2") {
		t.Fatalf("renudge interval: comments=%v edits=%v", api.comments, api.commentEdits)
	}
	// A hive-authored PR (self lane → 070-fixing) is never nudged.
	api.reset()
	e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", nudgePR(10, "aaa"), "check-failing:unit", false, nil)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if len(nudgeComments(api)) != 0 {
		t.Fatalf("self-lane nudged: %v", api.comments)
	}
}

func TestNudgeDiagnosisAndDue(t *testing.T) {
	checks, asks, human := nudgeDiagnosis("required-check-failing:lint", stageDetail{})
	if !reflect.DeepEqual(checks, []string{"lint"}) || asks != nil || human {
		t.Fatalf("%v %v %v", checks, asks, human)
	}
	_, asks, _ = nudgeDiagnosis(contributorLaneReasonChangesRequested, stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Verdict: "changes_requested"}})
	if len(asks) != 1 {
		t.Fatalf("swarm ask duplicated: %v", asks)
	}
	_, asks, _ = nudgeDiagnosis("label:needs-rebase", stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Verdict: "reject"}})
	if len(asks) != 2 {
		t.Fatalf("asks=%v", asks)
	}
	_, asks, _ = nudgeDiagnosis("something-else", stageDetail{})
	if !reflect.DeepEqual(asks, []string{"something-else"}) {
		t.Fatalf("asks=%v", asks)
	}
	checks, asks, _ = nudgeDiagnosis("optional-check-failing:links", stageDetail{RedChecks: []string{"Playwright", "links"}})
	if !reflect.DeepEqual(checks, []string{"Playwright", "links"}) || asks != nil {
		t.Fatalf("red optional: %v %v", checks, asks)
	}
	checks, _, _ = nudgeDiagnosis("optional-check-failing:links", stageDetail{})
	if !reflect.DeepEqual(checks, []string{"links"}) {
		t.Fatalf("red optional without detail: %v", checks)
	}
}

func TestNudgeDue(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	counted := stagelabels.Record{NudgeHead: "a", NudgeReason: "check-failing", NudgeReasonCount: 1, NudgeReasonAt: now.Add(-73 * time.Hour)}
	cases := []struct {
		name    string
		rec     stagelabels.Record
		head    string
		reason  string
		repeat  time.Duration
		ordinal int
		ok      bool
	}{
		{name: "empty head", rec: stagelabels.Record{}, head: "", reason: "check-failing:unit", repeat: time.Hour},
		{name: "first nudge", rec: stagelabels.Record{}, head: "a", reason: "check-failing:unit", repeat: time.Hour, ordinal: 1, ok: true},
		{name: "same head same class inside window", rec: counted, head: "a", reason: "check-failing:lint", repeat: 100 * time.Hour},
		{name: "same head same class after window", rec: counted, head: "a", reason: "check-failing:lint", repeat: 72 * time.Hour, ordinal: 2, ok: true},
		{name: "zero repeat never re-nudges", rec: counted, head: "a", reason: "check-failing:unit", repeat: 0},
		{name: "zero NudgeReasonAt never re-nudges", rec: stagelabels.Record{NudgeHead: "a", NudgeReason: "check-failing", NudgeReasonCount: 1}, head: "a", reason: "check-failing:unit", repeat: time.Hour},
		{name: "cap reached", rec: stagelabels.Record{NudgeHead: "a", NudgeReason: "check-failing", NudgeReasonCount: 2, NudgeReasonAt: now.Add(-200 * time.Hour)}, head: "a", reason: "check-failing:unit", repeat: time.Hour},
		{name: "new head same class rewrites uncounted", rec: counted, head: "b", reason: "required-check-failing:lint", repeat: time.Hour, ordinal: 0, ok: true},
		{name: "new head same class at cap still rewrites", rec: stagelabels.Record{NudgeHead: "a", NudgeReason: "check-failing", NudgeReasonCount: 2, NudgeReasonAt: now}, head: "b", reason: "check-failing:unit", repeat: time.Hour, ordinal: 0, ok: true},
		{name: "changed class restarts", rec: counted, head: "a", reason: "conflicting", repeat: time.Hour, ordinal: 1, ok: true},
		{name: "legacy record without class", rec: stagelabels.Record{NudgeHead: "a", NudgeCount: 2, LastNudgeAt: now}, head: "a", reason: "check-failing:unit", repeat: time.Hour, ordinal: 1, ok: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, ok := nudgeDue(tc.rec, tc.head, tc.reason, tc.repeat, now)
			if n != tc.ordinal || ok != tc.ok {
				t.Fatalf("nudgeDue = (%d, %v), want (%d, %v)", n, ok, tc.ordinal, tc.ok)
			}
		})
	}
}

// apiURLOf returns the httptest URL backing api (recorded by newStageAPI).
func apiURLOf(t *testing.T, api *stageAPI) string {
	t.Helper()
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.url == "" {
		t.Fatal("stageAPI has no server URL")
	}
	return api.url
}

func trustedHuman071PR(number int, head string, labels ...string) *gh.PullRequest {
	pr := stagePR(number, append([]string{"role/trusted"}, labels...)...)
	pr.Head.SHA = gh.Ptr(head)
	pr.User = &gh.User{Login: gh.Ptr("clubanderson")}
	return pr
}

func humanNoteComments(api *stageAPI) []string {
	var out []string
	for _, c := range api.comments {
		if strings.HasPrefix(c, TrustedHuman071MarkerPrefix) {
			out = append(out, c)
		}
	}
	return out
}

// A trusted human PR entering 071 gets one note per head with the reason, with
// ContributorNudge off; a new head re-arms it.
func TestTrustedHuman071NoteOncePerHeadAndReArms(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, srv.URL, clock)
	sweep := func(head string) {
		t.Helper()
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", trustedHuman071PR(41, head), "conflicting", false, nil)
		e.noteStageRole("acme", "widget", 41, stagelabels.RoleTrusted)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	sweep("aaa")
	notes := humanNoteComments(api)
	if len(notes) != 1 {
		t.Fatalf("want one note, got %q", api.comments)
	}
	for _, want := range []string{TrustedHuman071MarkerPrefix + " head=aaa -->", "@clubanderson", "`stage/071-needs-author`", "conflicts with the base"} {
		if !strings.Contains(notes[0], want) {
			t.Fatalf("note lacks %q:\n%s", want, notes[0])
		}
	}
	clock.at = clock.at.Add(10 * time.Minute)
	sweep("aaa")
	if len(humanNoteComments(api)) != 1 {
		t.Fatalf("same head re-commented: %q", api.comments)
	}
	sweep("bbb")
	notes = humanNoteComments(api)
	if len(notes) != 2 || !strings.Contains(notes[1], "head=bbb -->") {
		t.Fatalf("new head did not re-arm: %q", api.comments)
	}
}

func TestTrustedHuman071NoteNotForClankerOrUntrusted(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, srv.URL, clock)

	clanker := nudgePR(42, "ccc", "role/trusted")
	e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", clanker, "conflicting", false, nil)
	e.noteStageRole("acme", "widget", 42, stagelabels.RoleTrusted)

	outsider := stagePR(43)
	outsider.User = &gh.User{Login: gh.Ptr("mallory")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", outsider, "conflicting", false, nil)
	e.noteStageRole("acme", "widget", 43, stagelabels.RoleUntrusted)

	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := humanNoteComments(api); len(got) != 0 {
		t.Fatalf("note posted for clanker/untrusted: %q", got)
	}
}

func TestTrustedHuman071CommentReasons(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		detail stageDetail
		want   string
	}{
		{"conflict", "conflicting", stageDetail{}, "conflicts with the base"},
		{"failing check", "check-failing:unit", stageDetail{}, "- `unit`"},
		{"sign-off", stagelabels.LaneReasonDCOMissing, stageDetail{DCOTotal: 2, DCOUnsigned: 1}, "`Signed-off-by`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checks, asks, _ := nudgeDiagnosisRouted(tc.reason, tc.detail, nil)
			body := TrustedHuman071Comment(ContributorNudge{Author: "clubanderson", HeadSHA: "abc", FailingChecks: checks, ReviewAsks: asks}, stagelabels.NeedsAuthor)
			if !strings.HasPrefix(body, TrustedHuman071MarkerPrefix+" head=abc -->") || !strings.Contains(body, tc.want) {
				t.Fatalf("body lacks %q:\n%s", tc.want, body)
			}
		})
	}
}

// hivecommons/hive#14668: a fix-pr task the relay failed right after
// dispatch is not reported as received; the comment says the relay could not
// take it, names the relay's reason and promises the retry.
func TestContributorNudgeCommentRelayFailed(t *testing.T) {
	n := ContributorNudge{Author: "alice", HeadSHA: "abc", Nudge: 1}
	body := ContributorNudgeComment(n, ContributorNudgeOutcome{Mode: "queued", Online: true, RelayFailed: true, RelayError: "codex: session busy"})
	if !strings.Contains(body, "could not take this as a `fix-pr` task (codex: session busy)") || !strings.Contains(body, "retry") {
		t.Fatalf("relay-failed comment = %q", body)
	}
	if strings.Contains(body, "received this as a `fix-pr` task") || strings.Contains(body, "Queued in your relay fix queue") {
		t.Fatalf("relay-failed comment must not claim delivery or plain queueing: %q", body)
	}
	if body := ContributorNudgeComment(n, ContributorNudgeOutcome{Mode: "queued", QueueDepth: 2}); !strings.Contains(body, "Queued in your relay fix queue (2 waiting)") {
		t.Fatalf("plain queued comment = %q", body)
	}
}

// The closing-link reasons (hivecommons/hive#15025) name the number and the
// fix, and only the contradiction — a body-only problem — says a body edit
// releases the PR.
func TestContributorNudgeClosingLinkReasons(t *testing.T) {
	for _, tc := range []struct {
		reason   string
		ask      []string
		sentence string
	}{
		{
			reason:   "closing-contradiction:#1023",
			ask:      []string{"The body says #1023 stays open", "also `Closes #1023`", "No new commit is needed."},
			sentence: "The hive will not merge it until the PR body is fixed.",
		},
		{
			reason:   "closing-keyword-targets-pr:#1036",
			ask:      []string{"`fix #1036`", "closes pull request #1036 on merge", "`same change as #1036`"},
			sentence: "The hive will not merge it until the head changes.",
		},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			checks, asks, human := nudgeDiagnosis(tc.reason, stageDetail{})
			if len(checks) != 0 || human || len(asks) != 1 {
				t.Fatalf("nudgeDiagnosis(%q) = checks %v asks %v human %v, want one ask", tc.reason, checks, asks, human)
			}
			for _, w := range tc.ask {
				if !strings.Contains(asks[0], w) {
					t.Fatalf("ask %q does not contain %q", asks[0], w)
				}
			}
			body := ContributorNudgeComment(ContributorNudge{Author: "alice", HeadSHA: "abc1234", Nudge: 1, Reason: tc.reason, ReviewAsks: asks}, ContributorNudgeOutcome{})
			if !strings.Contains(body, tc.sentence) || !strings.Contains(body, asks[0]) {
				t.Fatalf("nudge comment = %q, want %q and the ask", body, tc.sentence)
			}
		})
	}
}

func TestTrustedHuman071NoteNeutralisesCopiedMentions(t *testing.T) {
	n := ContributorNudge{Author: "alice", HeadSHA: "abc",
		FailingChecks: []string{"lint @bob"},
		ReviewAsks:    []string{"@carol asked for a test"}}
	got := TrustedHuman071Note(n, stagelabels.NeedsAuthor, true)
	if !strings.Contains(got, "@alice this PR is held") {
		t.Fatalf("deliberate author mention lost: %s", got)
	}
	for _, live := range []string{"@bob", "@carol"} {
		if strings.Contains(got, live) {
			t.Fatalf("copied mention %s survived: %s", live, got)
		}
	}
	if !strings.Contains(got, "`carol`") || !strings.Contains(got, "lint bob") {
		t.Fatalf("copied logins not kept readably: %s", got)
	}
}

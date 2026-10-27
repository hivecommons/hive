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

// One nudge per head on entering 072, a second at the stall threshold, never
// a third; a new head re-arms; leaving 072 resolves the sink entry.
func TestContributorNudgeOncePerHeadThenAtStall(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	sink := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "spawn", Online: true}}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, sink, StageLabelPolicy{Enabled: true, ContributorNudge: true})
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

	res := observe(nudgePR(7, "aaa"), "check-failing:unit")
	if got := nudgeComments(api); len(got) != 1 {
		t.Fatalf("nudges=%d comments=%v", len(got), api.comments)
	} else {
		c := got[0]
		for _, want := range []string{"head=aaa n=1 mode=spawn", "@mallory", "`unit`", "push a new head", "parallel fix session"} {
			if !strings.Contains(c, want) {
				t.Fatalf("comment missing %q:\n%s", want, c)
			}
		}
	}
	if res.Comments != 1 || len(sink.nudges) != 1 || sink.nudges[0].Nudge != 1 || sink.nudges[0].Author != "mallory" ||
		!reflect.DeepEqual(sink.nudges[0].FailingChecks, []string{"unit"}) || sink.nudges[0].HumanChangesRequested {
		t.Fatalf("res=%+v sink=%+v", res, sink.nudges)
	}

	// Same head, same stage, next cycles: idempotent.
	api.reset()
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	clock.advance(time.Hour)
	observe(nudgePR(7, "aaa", labels...), "check-failing:unit")
	if api.writes() != 0 || len(sink.nudges) != 1 {
		t.Fatalf("re-nudged early: comments=%v sink=%d", api.comments, len(sink.nudges))
	}

	// Stall threshold (dwell 24h): stalled label + second nudge, no third.
	clock.advance(23 * time.Hour)
	observe(nudgePR(7, "aaa", labels...), "check-failing:unit")
	if got := nudgeComments(api); len(got) != 1 || !strings.Contains(got[0], "n=2") || !strings.Contains(got[0], "last automatic reminder") {
		t.Fatalf("stall nudge: %v", api.comments)
	}
	if !reflect.DeepEqual(api.added, [][]string{{"stage/stalled"}}) {
		t.Fatalf("stalled label: %v", api.added)
	}
	if len(sink.nudges) != 2 || !sink.nudges[1].Stalled {
		t.Fatalf("sink=%+v", sink.nudges)
	}
	api.reset()
	clock.advance(48 * time.Hour)
	observe(nudgePR(7, "aaa", "stage/stalled", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "check-failing:unit")
	if len(nudgeComments(api)) != 0 || len(sink.nudges) != 2 {
		t.Fatalf("third nudge posted: %v", api.comments)
	}

	// Author pushes a new head that is still red: count restarts.
	api.reset()
	observe(nudgePR(7, "bbb", "stage/stalled", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "changes-requested")
	if got := nudgeComments(api); len(got) != 1 || !strings.Contains(got[0], "head=bbb n=1") || !strings.Contains(got[0], "reviewer requested changes") {
		t.Fatalf("new-head nudge: %v", api.comments)
	}
	if len(sink.nudges) != 3 || !sink.nudges[2].HumanChangesRequested {
		t.Fatalf("sink=%+v", sink.nudges)
	}
	if len(sink.resolved) != 0 {
		t.Fatalf("resolved while still in 072: %v", sink.resolved)
	}

	// New head goes green and awaits review: leaves 072 → Resolve once.
	api.reset()
	observe(nudgePR(7, "ccc", "stage/072-returned-to-contributor", "role/contributor", "author/clanker"), "required-check-missing:unit")
	if !reflect.DeepEqual(sink.resolved, []int{7}) {
		t.Fatalf("resolved=%v", sink.resolved)
	}
	observe(nudgePR(7, "ccc", "stage/050-review-pending", "role/contributor", "author/clanker"), "required-check-missing:unit")
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
	e = newNudgeEngine(t, apiURLOf(t, api), clock, nil, StageLabelPolicy{Enabled: true, ContributorNudge: true, RenudgeInterval: time.Hour})
	pr := nudgePR(9, "aaa")
	e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, "status-failure", false, nil)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if got := nudgeComments(api); len(got) != 1 || !strings.Contains(got[0], "mode=comment") || !strings.Contains(got[0], "`status-failure`") {
		t.Fatalf("nil-sink nudge: %v", api.comments)
	}
	// RenudgeInterval overrides the dwell: second nudge after 1h, not 24h.
	api.reset()
	clock.advance(time.Hour)
	e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", nudgePR(9, "aaa", "stage/072-returned-to-contributor", "role/trusted", "author/clanker"), "status-failure", false, nil)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if got := nudgeComments(api); len(got) != 1 || !strings.Contains(got[0], "n=2") {
		t.Fatalf("renudge interval: %v", api.comments)
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
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if _, ok := nudgeDue(stagelabels.Record{}, "", time.Hour, now); ok {
		t.Fatal("empty head due")
	}
	if n, ok := nudgeDue(stagelabels.Record{NudgeHead: "a", NudgeCount: 1, LastNudgeAt: now.Add(-time.Hour)}, "a", 0, now); ok || n != 0 {
		t.Fatal("zero renudge interval should never re-nudge")
	}
	if n, ok := nudgeDue(stagelabels.Record{NudgeHead: "a", NudgeCount: 1}, "a", time.Hour, now); ok || n != 0 {
		t.Fatal("zero LastNudgeAt re-nudged")
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

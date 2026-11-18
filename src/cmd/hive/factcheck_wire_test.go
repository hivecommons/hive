package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/factcheck"
	"github.com/hivecommons/hive/pkg/github"
)

const (
	fcHead1 = "1111111111111111111111111111111111111111"
	fcHead2 = "2222222222222222222222222222222222222222"
	fcKey   = "cncf/techdocs#424"
)

var fcNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

type fcSource struct {
	prs      map[string]github.FactCheckPR
	fetchErr error
	fetches  int
	readied  []string
	readyErr error
	comments []string
}

func (f *fcSource) FetchPRFactCheck(_ context.Context, repo string, number int) (github.FactCheckPR, error) {
	f.fetches++
	if f.fetchErr != nil {
		return github.FactCheckPR{}, f.fetchErr
	}
	return f.prs[repo+"#"+strconv.Itoa(number)], nil
}

func (f *fcSource) MarkPRReadyForReview(_ context.Context, nodeID string) error {
	if f.readyErr != nil {
		return f.readyErr
	}
	f.readied = append(f.readied, nodeID)
	return nil
}

func (f *fcSource) CreateIssueComment(_ context.Context, _ string, _ int, body string) error {
	f.comments = append(f.comments, body)
	return nil
}

type fcKick struct{ agent, msg string }

type fcKicker struct {
	kicks []fcKick
	err   error
}

func (k *fcKicker) Kick(agent, message string) error {
	if k.err != nil {
		return k.err
	}
	k.kicks = append(k.kicks, fcKick{agent, message})
	return nil
}

func fcMarkerBody(round int, v factcheck.Verdict, head string) string {
	return "**Fact-check**\n\n" + factcheck.FormatMarker(factcheck.Marker{Round: round, Verdict: v, Head: head})
}

func fcPR(head string) github.FactCheckPR {
	return github.FactCheckPR{NodeID: "PR_1", State: "open", Draft: true, HeadSHA: head, HeadRef: "docs-analysis/x"}
}

type fcRig struct {
	src    *fcSource
	kicker *fcKicker
	store  *factcheck.Store
	sweep  factCheckSweep
}

func newFCRig(t *testing.T, pr github.FactCheckPR) *fcRig {
	t.Helper()
	store, err := factcheck.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &fcRig{src: &fcSource{prs: map[string]github.FactCheckPR{fcKey: pr}}, kicker: &fcKicker{}, store: store}
	r.sweep = factCheckSweep{
		Source: r.src,
		Store:  store,
		Policy: factcheck.Policy{
			AuthorAgents: []string{"quality"}, Independence: factcheck.IndependenceModel,
			MaxRounds: 3, ReadyPolicy: factcheck.ReadyHuman, RevisionWait: 6 * time.Hour,
		},
		Owners:      map[string]string{fcKey: "quality"},
		Kicker:      r.kicker,
		Author:      func(string) factcheck.Agent { return factcheck.Agent{Name: "quality", Model: "claude-opus-4-6"} },
		AuthorRoute: func(owner string) (string, string) { return owner, "" },
		Reviewers: func() factCheckReviewers {
			return factCheckReviewers{Configured: 1, Available: []factcheck.Agent{{Name: "reviewer", Model: "claude-fable-5"}}}
		},
		AuthoritativeFiles: []string{"MAINTAINERS"},
		MaxPRs:             10,
		Now:                fcNow,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return r
}

func (r *fcRig) run(now time.Time) factCheckResult {
	s := r.sweep
	s.Now = now
	return s.run(context.Background(), parseFactCheckKey)
}

func TestFactCheckSweepKicksAnIndependentReviewerOnce(t *testing.T) {
	r := newFCRig(t, fcPR(fcHead1))
	res := r.run(fcNow)
	if res.Reviews != 1 || len(r.kicker.kicks) != 1 || r.kicker.kicks[0].agent != "reviewer" {
		t.Fatalf("want one kick to reviewer, got %+v kicks=%+v", res, r.kicker.kicks)
	}
	brief := r.kicker.kicks[0].msg
	for _, want := range []string{"cncf/techdocs#424 (round 1 of 3)", "`quality` agent", "`MAINTAINERS`", fcHead1[:12]} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief missing %q", want)
		}
	}
	if got := r.store.Get(fcKey); got.ReviewKicks != 1 || got.LastReviewHead != fcHead1 {
		t.Fatalf("round not recorded: %+v", got)
	}
	// The next pass finds the review in flight and does nothing.
	if res := r.run(fcNow.Add(5 * time.Minute)); res.Reviews != 0 || len(r.kicker.kicks) != 1 {
		t.Fatalf("a review in flight must not be re-kicked: %+v", res)
	}
}

func TestFactCheckSweepQualification(t *testing.T) {
	tests := []struct {
		name      string
		owner     string
		labels    []string
		prLabels  []string
		wantKick  bool
		wantFetch bool
	}{
		{"qualifying agent", "quality", nil, nil, true, true},
		{"replica of the qualifying agent", "quality-2", nil, nil, true, true},
		{"other agent, no labels configured: not even read", "scanner", nil, nil, false, false},
		{"other agent, label configured but PR lacks it", "scanner", []string{"analysis"}, nil, false, true},
		{"other agent carrying the label", "scanner", []string{"analysis"}, []string{"analysis"}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := fcPR(fcHead1)
			pr.Labels = tt.prLabels
			r := newFCRig(t, pr)
			r.sweep.Owners = map[string]string{fcKey: tt.owner}
			r.sweep.Policy.Labels = tt.labels
			r.sweep.Author = func(string) factcheck.Agent { return factcheck.Agent{} }
			r.run(fcNow)
			if got := len(r.kicker.kicks) == 1; got != tt.wantKick {
				t.Fatalf("kicked = %v, want %v", got, tt.wantKick)
			}
			if got := r.src.fetches > 0; got != tt.wantFetch {
				t.Fatalf("fetched = %v, want %v", got, tt.wantFetch)
			}
		})
	}
}

func TestFactCheckSweepClosedAndPausedAreSkipped(t *testing.T) {
	closed := fcPR(fcHead1)
	closed.State = "merged"
	r := newFCRig(t, closed)
	if r.run(fcNow); len(r.kicker.kicks) != 0 {
		t.Fatal("a merged PR is never reviewed")
	}
	r = newFCRig(t, fcPR(fcHead1))
	r.sweep.AuthorPaused = func(string) bool { return true }
	if r.run(fcNow); len(r.kicker.kicks) != 0 || r.src.fetches != 0 {
		t.Fatal("a paused author stays paused: no read, no kick")
	}
}

func TestFactCheckSweepEnforcesIndependence(t *testing.T) {
	r := newFCRig(t, fcPR(fcHead1))
	r.sweep.Reviewers = func() factCheckReviewers {
		return factCheckReviewers{Configured: 1, Available: []factcheck.Agent{{Name: "reviewer", Model: "claude-opus-4-6"}}}
	}
	res := r.run(fcNow)
	if len(r.kicker.kicks) != 0 || res.NeedsHuman != 1 || len(r.src.comments) != 1 {
		t.Fatalf("a reviewer on the author's model must be refused and handed to a person: %+v kicks=%d", res, len(r.kicker.kicks))
	}
	if !strings.Contains(r.src.comments[0], "no independent reviewer is available") || !factcheck.HasNeedsHumanMarker(r.src.comments[0]) {
		t.Fatalf("hand-off note: %s", r.src.comments[0])
	}
	// The note is posted once even though the PR keeps qualifying.
	r.run(fcNow.Add(time.Hour))
	if len(r.src.comments) != 1 {
		t.Fatalf("hand-off note repeated: %d", len(r.src.comments))
	}
}

func TestFactCheckSweepAuthorCanNeverReviewItself(t *testing.T) {
	r := newFCRig(t, fcPR(fcHead1))
	r.sweep.Policy.Independence = factcheck.IndependenceAgent
	r.sweep.Reviewers = func() factCheckReviewers {
		return factCheckReviewers{Configured: 1, Available: []factcheck.Agent{{Name: "quality", Model: "gpt-6.1-sol"}}}
	}
	r.run(fcNow)
	if len(r.kicker.kicks) != 0 {
		t.Fatal("the authoring agent was kicked to review its own PR")
	}
}

func TestFactCheckSweepReviewerLaneStates(t *testing.T) {
	r := newFCRig(t, fcPR(fcHead1))
	r.sweep.Reviewers = func() factCheckReviewers { return factCheckReviewers{} }
	if res := r.run(fcNow); res.NeedsHuman != 1 || !strings.Contains(r.src.comments[0], "no reviewer agent is configured") {
		t.Fatalf("an unconfigured lane is a hand-off: %+v", res)
	}

	r = newFCRig(t, fcPR(fcHead1))
	r.sweep.Reviewers = func() factCheckReviewers { return factCheckReviewers{Configured: 2} }
	if res := r.run(fcNow); res.NeedsHuman != 0 || len(r.src.comments) != 0 || len(r.kicker.kicks) != 0 {
		t.Fatalf("an unreachable lane is retried quietly: %+v", res)
	}
}

func TestFactCheckSweepKickFailureIsRetriedAndNotCounted(t *testing.T) {
	r := newFCRig(t, fcPR(fcHead1))
	r.kicker.err = errors.New("shared session pool is busy")
	if res := r.run(fcNow); res.Reviews != 0 {
		t.Fatalf("a refused kick is not a round: %+v", res)
	}
	if got := r.store.Get(fcKey); got.ReviewKicks != 0 {
		t.Fatalf("a refused kick must not spend a round: %+v", got)
	}
	r.kicker.err = nil
	if res := r.run(fcNow.Add(time.Minute)); res.Reviews != 1 {
		t.Fatalf("the retry should land: %+v", res)
	}
}

func TestFactCheckSweepFetchErrorIsCounted(t *testing.T) {
	r := newFCRig(t, fcPR(fcHead1))
	r.src.fetchErr = errors.New("boom")
	if res := r.run(fcNow); res.Errors != 1 || len(r.kicker.kicks) != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestFactCheckSweepFindingsGoToTheAuthorOnceThenAPerson(t *testing.T) {
	pr := fcPR(fcHead1)
	pr.Reviews = []github.FactCheckReview{{
		URL: "https://example.invalid/r/1", Hive: true, Body: fcMarkerBody(1, factcheck.VerdictFindings, fcHead1),
		Inline: []github.FactCheckInline{{Path: "analyses/a.md", Line: 8, Body: "**attribution** wrong role\nVerify: cat MAINTAINERS\nConfidence: high"}},
	}}
	r := newFCRig(t, pr)
	res := r.run(fcNow)
	if res.Revisions != 1 || len(r.kicker.kicks) != 1 || r.kicker.kicks[0].agent != "quality" {
		t.Fatalf("findings must go to the author: %+v kicks=%+v", res, r.kicker.kicks)
	}
	for _, want := range []string{"FACT-CHECK FINDINGS on your PR cncf/techdocs#424", "analyses/a.md:8", "Verify: cat MAINTAINERS", "`docs-analysis/x`"} {
		if !strings.Contains(r.kicker.kicks[0].msg, want) {
			t.Errorf("revision brief missing %q", want)
		}
	}
	if r.run(fcNow.Add(time.Hour)); len(r.kicker.kicks) != 1 {
		t.Fatal("the author is kicked once per round, not once per pass")
	}
	if res := r.run(fcNow.Add(7 * time.Hour)); res.NeedsHuman != 1 || !strings.Contains(r.src.comments[0], "did not push a revision") {
		t.Fatalf("an author that never pushes is handed to a person: %+v comments=%v", res, r.src.comments)
	}
}

func TestFactCheckSweepAuthorUnavailableWaitsThenHandsOff(t *testing.T) {
	pr := fcPR(fcHead1)
	pr.Reviews = []github.FactCheckReview{{Hive: true, Body: fcMarkerBody(1, factcheck.VerdictFindings, fcHead1)}}
	r := newFCRig(t, pr)
	r.sweep.AuthorRoute = func(string) (string, string) { return "", "no agent" }
	if res := r.run(fcNow); res.Revisions != 0 || len(r.kicker.kicks) != 0 {
		t.Fatalf("%+v", res)
	}
	if res := r.run(fcNow.Add(7 * time.Hour)); res.NeedsHuman != 1 {
		t.Fatalf("with nobody to revise, the wait still ends in a hand-off: %+v", res)
	}
}

func TestFactCheckSweepRevisionKickFailureRetries(t *testing.T) {
	pr := fcPR(fcHead1)
	pr.Reviews = []github.FactCheckReview{{Hive: true, Body: fcMarkerBody(1, factcheck.VerdictFindings, fcHead1)}}
	r := newFCRig(t, pr)
	r.kicker.err = errors.New("shared session pool is busy")
	r.run(fcNow)
	r.kicker.err = nil
	if res := r.run(fcNow.Add(time.Minute)); res.Revisions != 1 {
		t.Fatalf("the revision kick should retry: %+v", res)
	}
}

func TestFactCheckSweepAuthorRevisionStartsTheNextRound(t *testing.T) {
	pr := fcPR(fcHead2) // the author pushed a new head after round 1
	pr.Reviews = []github.FactCheckReview{{Hive: true, Body: fcMarkerBody(1, factcheck.VerdictFindings, fcHead1)}}
	r := newFCRig(t, pr)
	_ = r.store.Update(fcKey, fcNow, func(st *factcheck.State) { st.ReviewKicks = 1; st.RevisionHead = fcHead1; st.RevisionAt = fcNow })
	res := r.run(fcNow.Add(time.Hour))
	if res.Reviews != 1 || !strings.Contains(r.kicker.kicks[0].msg, "(round 2 of 3)") {
		t.Fatalf("the new head must get round 2: %+v kicks=%+v", res, r.kicker.kicks)
	}
}

func TestFactCheckSweepIgnoresForgedMarkers(t *testing.T) {
	pr := fcPR(fcHead1)
	// A person (or another bot) types a passing marker; it must not unlock anything.
	pr.Reviews = []github.FactCheckReview{{Hive: false, Human: false, Author: "other-bot", Body: fcMarkerBody(1, factcheck.VerdictPass, fcHead1)}}
	r := newFCRig(t, pr)
	r.sweep.Policy.ReadyPolicy = factcheck.ReadyHive
	res := r.run(fcNow)
	if len(r.src.readied) != 0 || res.Readied != 0 {
		t.Fatal("a marker from anyone but the hive must not mark the PR ready")
	}
	if res.Reviews != 1 {
		t.Fatalf("the PR still needs its real review: %+v", res)
	}
}

func TestFactCheckSweepReadyPolicy(t *testing.T) {
	pass := func() github.FactCheckPR {
		pr := fcPR(fcHead1)
		pr.Reviews = []github.FactCheckReview{{Hive: true, Body: fcMarkerBody(1, factcheck.VerdictPass, fcHead1)}}
		return pr
	}
	t.Run("hive policy marks a draft ready once", func(t *testing.T) {
		r := newFCRig(t, pass())
		r.sweep.Policy.ReadyPolicy = factcheck.ReadyHive
		if res := r.run(fcNow); res.Readied != 1 || len(r.src.readied) != 1 || r.src.readied[0] != "PR_1" {
			t.Fatalf("%+v readied=%v", res, r.src.readied)
		}
		r.run(fcNow.Add(time.Hour))
		if len(r.src.readied) != 1 {
			t.Fatal("marked ready twice")
		}
	})
	t.Run("human policy leaves the draft alone", func(t *testing.T) {
		r := newFCRig(t, pass())
		if res := r.run(fcNow); res.Readied != 0 || len(r.src.readied) != 0 || len(r.kicker.kicks) != 0 {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("a failed mutation is retried", func(t *testing.T) {
		r := newFCRig(t, pass())
		r.sweep.Policy.ReadyPolicy = factcheck.ReadyHive
		r.src.readyErr = errors.New("forbidden")
		if res := r.run(fcNow); res.Readied != 0 {
			t.Fatalf("%+v", res)
		}
		r.src.readyErr = nil
		if res := r.run(fcNow.Add(time.Minute)); res.Readied != 1 {
			t.Fatalf("%+v", res)
		}
	})
	t.Run("a pass on an older head never marks a newer one ready", func(t *testing.T) {
		pr := pass()
		pr.HeadSHA = fcHead2
		r := newFCRig(t, pr)
		r.sweep.Policy.ReadyPolicy = factcheck.ReadyHive
		if res := r.run(fcNow); res.Readied != 0 || res.Reviews != 1 {
			t.Fatalf("%+v", res)
		}
	})
}

func TestFactCheckSweepStopsWhenAPersonReviews(t *testing.T) {
	pr := fcPR(fcHead1)
	pr.Reviews = []github.FactCheckReview{{Author: "maintainer", Human: true, Body: ""}}
	r := newFCRig(t, pr)
	if res := r.run(fcNow); res.Reviews+res.Revisions+res.Readied+res.NeedsHuman != 0 || len(r.kicker.kicks) != 0 {
		t.Fatalf("agents must not talk over a person's review: %+v", res)
	}
}

func TestFactCheckSweepRoundsAreBoundedWhateverTheReviewerDoes(t *testing.T) {
	r := newFCRig(t, fcPR(fcHead1))
	now := fcNow
	for i := 0; i < 50; i++ {
		now = now.Add(2 * time.Hour) // longer than factcheck.ReviewWait: a silent reviewer is re-asked
		r.run(now)
	}
	if got := len(r.kicker.kicks); got != r.sweep.Policy.MaxRounds {
		t.Fatalf("a silent reviewer got %d kicks, limit %d", got, r.sweep.Policy.MaxRounds)
	}
	if len(r.src.comments) != 1 || !strings.Contains(r.src.comments[0], "round limit") {
		t.Fatalf("the loop must end in exactly one hand-off note: %v", r.src.comments)
	}
}

func TestFactCheckSweepAlreadyHandedOffIsRespected(t *testing.T) {
	pr := fcPR(fcHead1)
	pr.Comments = []github.FactCheckComment{{Hive: true, Body: factcheck.NeedsHumanNote("x", 1, 3)}}
	r := newFCRig(t, pr)
	if r.run(fcNow); len(r.kicker.kicks) != 0 || len(r.src.comments) != 0 {
		t.Fatal("a PR the hive already handed to a person gets nothing more")
	}
}

func TestFactCheckSweepCursorRotates(t *testing.T) {
	r := newFCRig(t, fcPR(fcHead1))
	r.sweep.Owners = map[string]string{"o/r#1": "x", "o/r#2": "x", "o/r#3": "x"}
	r.sweep.MaxPRs = 2
	if res := r.run(fcNow); res.NextCursor != 2 {
		t.Fatalf("cursor = %d", res.NextCursor)
	}
	r.sweep.Cursor = 2
	if res := r.run(fcNow); res.NextCursor != 1 {
		t.Fatalf("cursor wraps, got %d", res.NextCursor)
	}
	r.sweep.Owners = nil
	if res := r.run(fcNow); res.NextCursor != 0 {
		t.Fatalf("no owners: %+v", res)
	}
}

func TestParseFactCheckKey(t *testing.T) {
	tests := []struct {
		key  string
		repo string
		n    int
		ok   bool
	}{
		{"cncf/techdocs#424", "cncf/techdocs", 424, true},
		{"o/r#1", "o/r", 1, true},
		{"techdocs#424", "", 0, false},
		{"o/r#", "", 0, false},
		{"o/r#0", "", 0, false},
		{"o/r#x", "", 0, false},
		{"#5", "", 0, false},
		{"", "", 0, false},
	}
	for _, tt := range tests {
		repo, n, ok := parseFactCheckKey(tt.key)
		if repo != tt.repo || n != tt.n || ok != tt.ok {
			t.Errorf("parse(%q) = %q,%d,%v want %q,%d,%v", tt.key, repo, n, ok, tt.repo, tt.n, tt.ok)
		}
	}
}

func TestFactCheckMarkerHelpers(t *testing.T) {
	pr := fcPR(fcHead2)
	pr.Reviews = []github.FactCheckReview{
		{Hive: true, Body: fcMarkerBody(1, factcheck.VerdictFindings, fcHead1), URL: "r1"},
		{Hive: true, Body: fcMarkerBody(2, factcheck.VerdictFindings, fcHead2), URL: "r2"},
		{Hive: false, Body: fcMarkerBody(9, factcheck.VerdictPass, fcHead2), URL: "forged"},
		{Hive: true, Body: "no marker here", URL: "r3"},
	}
	if got := factCheckMarkers(pr); len(got) != 2 {
		t.Fatalf("only the hive's marked reviews count: %+v", got)
	}
	rev, m, ok := latestFactCheckReview(pr)
	if !ok || rev.URL != "r2" || m.Round != 2 {
		t.Fatalf("latest review on the current head: %+v %+v %v", rev, m, ok)
	}
	pr.HeadSHA = "3333333333333333333333333333333333333333"
	if _, _, ok := latestFactCheckReview(pr); ok {
		t.Fatal("no review names the new head")
	}
	if factCheckNeedsHumanPosted(pr) {
		t.Fatal("no hand-off yet")
	}
	pr.Reviews = append(pr.Reviews, github.FactCheckReview{Hive: true, Body: factcheck.NeedsHumanMarker})
	if !factCheckNeedsHumanPosted(pr) {
		t.Fatal("hand-off inside a hive review counts")
	}
	pr.Reviews = nil
	pr.Comments = []github.FactCheckComment{{Hive: false, Body: factcheck.NeedsHumanMarker}}
	if factCheckNeedsHumanPosted(pr) {
		t.Fatal("a hand-off marker typed by someone else is not the hive's")
	}
	if got := factCheckParticipants(github.FactCheckPR{Reviews: []github.FactCheckReview{{Author: "p", Human: true}}}); len(got) != 1 || !got[0].Human || !got[0].Substantive {
		t.Fatalf("participants: %+v", got)
	}
	if got := factCheckFindings(github.FactCheckReview{Inline: []github.FactCheckInline{{Path: "a", Line: 2, Body: "b", URL: "u"}}}); len(got) != 1 || got[0].Path != "a" || got[0].Line != 2 {
		t.Fatalf("findings: %+v", got)
	}
}

func TestFactCheckConfigResolution(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agents = map[string]config.AgentConfig{
		"quality":    {Enabled: true, Backend: "copilot", Model: "claude-opus-4-6", Role: "quality"},
		"reviewer":   {Enabled: true, Backend: "copilot", Model: "claude-fable-5", Role: "reviewer"},
		"reviewer-2": {Enabled: true, Role: "reviewer", ReplicaOf: "reviewer"},
		"off":        {Enabled: false, Role: "reviewer"},
		"zeta":       {Enabled: true, Role: "Reviewer"},
	}
	if got := factCheckReviewerNames(cfg); strings.Join(got, ",") != "reviewer,zeta" {
		t.Fatalf("role-derived reviewers (enabled, no replicas, sorted) = %v", got)
	}
	cfg.Review.ReviewerAgents = []string{" guide ", ""}
	if got := factCheckReviewerNames(cfg); strings.Join(got, ",") != "guide" {
		t.Fatalf("review.reviewer_agents = %v", got)
	}
	cfg.Review.AnalysisFactCheck.ReviewerAgents = []string{"zeta"}
	if got := factCheckReviewerNames(cfg); strings.Join(got, ",") != "zeta" {
		t.Fatalf("the block's own list wins = %v", got)
	}
	if a := factCheckAgent(cfg, "quality"); a.Model != "claude-opus-4-6" || a.Backend != "copilot" {
		t.Fatalf("agent = %+v", a)
	}
	if a := factCheckAgent(cfg, "ghost"); a.Name != "ghost" || a.Model != "" {
		t.Fatalf("unknown agent keeps its name and has no model: %+v", a)
	}

	pol := factCheckPolicy(config.AnalysisFactCheckConfig{
		AuthorAgents: []string{" quality "}, Labels: []string{"analysis"}, ReviewerModel: " gpt-6.1-sol ",
		Independence: "family", MaxRounds: 4, ReadyPolicy: "hive", RevisionWait: "2h",
	})
	if pol.AuthorAgents[0] != "quality" || pol.Labels[0] != "analysis" || pol.ReviewerModel != "gpt-6.1-sol" ||
		pol.Independence != factcheck.IndependenceFamily || pol.MaxRounds != 4 ||
		pol.ReadyPolicy != factcheck.ReadyHive || pol.RevisionWait != 2*time.Hour {
		t.Fatalf("policy = %+v", pol)
	}
}

func TestSweepAnalysisFactCheckIsInertUnlessEnabled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sweepAnalysisFactCheck(context.Background(), nil, nil, nil, logger)
	cfg := &config.Config{}
	before := factCheckLastRun
	sweepAnalysisFactCheck(context.Background(), cfg, nil, nil, logger)
	if !factCheckLastRun.Equal(before) {
		t.Fatal("a disabled pass must not even start its timer")
	}
}

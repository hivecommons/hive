package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
)

func dbrTransientPR(repo string, number int, author, head string) github.PullRequest {
	pr := redPR(repo, number, author, head)
	pr.TransientCITriaged = true
	return pr
}

// A trusted bot's transient-triaged red PR gets the one re-run per head a
// hive PR gets; a PR from someone who is neither a hive agent nor a trusted
// bot does not. On the old code only isHiveAgentAuthor passed, and the bot
// head was never recorded as triaged, so nothing was re-run.
func TestRerunTransientTriagedPRs_TrustedBotAuthor(t *testing.T) {
	store, clock := newTestEscalationStore(t)
	cfg := escalationTestConfig() // trusted_bot_authors unset: dependabot[bot]
	bot := dbrTransientPR("widgets", 8, "dependabot[bot]", "d1")
	person := dbrTransientPR("widgets", 9, "someone", "p1")
	untrusted := dbrTransientPR("widgets", 10, "renovate[bot]", "r1")
	// Different failing checks, so the three red PRs do not read as one
	// shared CI failure (escalation.MarkSharedFailures).
	person.FailingChecks = []string{"lint"}
	untrusted.FailingChecks = []string{"docs"}
	actionable := actionableWith(bot, person, untrusted)
	recordRedStaleness(cfg, actionable)
	rerunner := &fakeTransientRerunner{out: github.TransientCIRerunOutcome{Settled: true, RunIDs: []int64{42}}}

	reran := rerunTransientTriagedPRs(context.Background(), cfg, rerunner, actionable, map[string]bool{}, nil, discardLogger())
	if len(rerunner.calls) != 1 || rerunner.calls[0].Number != 8 || rerunner.calls[0].HeadSHA != "d1" {
		t.Fatalf("re-run calls = %+v, want only the trusted bot's head d1", rerunner.calls)
	}
	if !reran["acme/widgets#8"] {
		t.Fatalf("reran = %v, want the bot PR reported", reran)
	}

	// The reaper only walks hive-agent PRs: the bot key in the skip set
	// changes nothing, and the bot PR draws no fix kick either way.
	*clock = clock.Add(escalation.RedPRStaleAfter + time.Minute)
	kicker := &fakeKicker{}
	reapStuckRedPRs(cfg, actionable, reaperSkipAfterRerun(map[string]bool{}, reran), kicker, nil, discardLogger())
	if len(kicker.kicks) != 0 {
		t.Fatalf("fix kicks = %v, want none for bot or human PRs", kicker.kicks)
	}
	if store.Attempts("acme/widgets", 8) != 0 || store.ReEngagements("acme/widgets", 8) != 0 {
		t.Fatal("the bot PR entered the fix-loop budget")
	}

	// Once per head: a second pass on d1 does not re-run; a new head does.
	recordRedStaleness(cfg, actionable)
	rerunTransientTriagedPRs(context.Background(), cfg, rerunner, actionable, map[string]bool{}, nil, discardLogger())
	if len(rerunner.calls) != 1 {
		t.Fatalf("re-run calls = %d after a second pass on the same head, want 1", len(rerunner.calls))
	}
	bot.HeadSHA = "d2"
	actionable = actionableWith(bot, person, untrusted)
	recordRedStaleness(cfg, actionable)
	rerunTransientTriagedPRs(context.Background(), cfg, rerunner, actionable, map[string]bool{}, nil, discardLogger())
	if len(rerunner.calls) != 2 || rerunner.calls[1].HeadSHA != "d2" {
		t.Fatalf("re-run calls = %+v, want the new head d2 re-run", rerunner.calls)
	}
}

// An explicit empty auto_merge.trusted_bot_authors keeps the re-run to hive
// PRs.
func TestRerunTransientTriagedPRs_TrustedBotListEmpty(t *testing.T) {
	newTestEscalationStore(t)
	cfg := escalationTestConfig()
	cfg.AutoMerge.TrustedBotAuthors = []string{}
	actionable := actionableWith(dbrTransientPR("widgets", 8, "dependabot[bot]", "d1"))
	recordRedStaleness(cfg, actionable)
	rerunner := &fakeTransientRerunner{out: github.TransientCIRerunOutcome{Settled: true, RunIDs: []int64{42}}}
	rerunTransientTriagedPRs(context.Background(), cfg, rerunner, actionable, map[string]bool{}, nil, discardLogger())
	if len(rerunner.calls) != 0 {
		t.Fatalf("re-ran a bot PR with trusted_bot_authors empty: %+v", rerunner.calls)
	}
}

// dbrCycle runs one eval cycle's relevant steps in their real order:
// recordRedStaleness, the dependency-bot hand-off sweep, then the transient
// re-run (when rerunner is set).
func dbrCycle(t *testing.T, h *depBotHarness, at time.Time, pr github.PullRequest, rerunner *fakeTransientRerunner) {
	t.Helper()
	cfg := escalationTestConfig()
	a := actionableWith(pr)
	recordRedStaleness(cfg, a)
	h.passAt(at, pr)
	if rerunner != nil {
		rerunTransientTriagedPRs(context.Background(), cfg, rerunner, a, map[string]bool{}, nil, discardLogger())
	}
}

func dbrTransientBotPR(head string) github.PullRequest {
	return depBotPR("dependabot[bot]", head, func(p *github.PullRequest) { redCI(p); p.TransientCITriaged = true })
}

// The red-CI hand-off of a transient-triaged trusted-bot head waits for the
// hive's one re-run, keeps its grace clock while the re-run runs, and when
// the head is red again says the re-run failed again. The old code handed
// it off at the grace with "nothing has re-run the check since".
func TestDependencyBotRedCIHandoffWaitsForTransientRerun(t *testing.T) {
	h := newDepBotHarness(t)
	pr := dbrTransientBotPR("sha-tr1")

	// Re-run owed (the re-run step has not run yet): no hand-off, even
	// past the grace.
	dbrCycle(t, h, depBotT0, pr, nil)
	dbrCycle(t, h, depBotT0.Add(depBotRedCIGrace), pr, nil)
	if len(h.writer.comments) != 0 || len(h.writer.adds) != 0 {
		t.Fatalf("re-run owed: comments=%v adds=%v, want none", h.writer.comments, h.writer.adds)
	}

	rerunner := &fakeTransientRerunner{out: github.TransientCIRerunOutcome{Settled: true, RunIDs: []int64{42}}}
	dbrCycle(t, h, depBotT0.Add(depBotRedCIGrace+time.Minute), pr, rerunner)
	if len(rerunner.calls) != 1 || len(h.writer.comments) != 0 {
		t.Fatalf("re-run calls=%d comments=%v, want one re-run and no hand-off", len(rerunner.calls), h.writer.comments)
	}

	// The re-run is running: CI pending, no failing check.
	running := pr
	running.CIStatus, running.FailingChecks, running.TransientCITriaged = "pending", nil, false
	dbrCycle(t, h, depBotT0.Add(depBotRedCIGrace+10*time.Minute), running, rerunner)
	if len(h.writer.comments) != 0 {
		t.Fatalf("re-run running: comments=%v, want none", h.writer.comments)
	}

	// Red again after the re-run (new run ids, so the markers are stale):
	// handed off at once on the first red sighting's clock.
	again := pr
	again.TransientCITriaged = false
	dbrCycle(t, h, depBotT0.Add(depBotRedCIGrace+40*time.Minute), again, rerunner)
	if len(rerunner.calls) != 1 {
		t.Fatalf("re-run calls = %d, want the head re-run only once", len(rerunner.calls))
	}
	if len(h.writer.comments) != 1 || len(h.writer.adds) != 1 {
		t.Fatalf("still red after the re-run: comments=%v adds=%v, want one hand-off", h.writer.comments, h.writer.adds)
	}
	c := h.writer.comments[0]
	if !strings.Contains(c, "re-ran the failed jobs once, and they failed again") || strings.Contains(c, "nothing has re-run") {
		t.Fatalf("comment = %q, want it to say the re-run failed again", c)
	}
	if rec, ok := h.store.get(depBotKey); !ok || rec.Reason != depBotHandoffRedCI || rec.HeadSHA != "sha-tr1" {
		t.Fatalf("record = %+v (ok=%v), want red-ci on sha-tr1", rec, ok)
	}
}

// A re-run the write-back gate refused still reaches a person, after the
// usual grace, with a comment saying the re-run was not allowed and why.
func TestDependencyBotRedCIHandoffAfterRefusedRerun(t *testing.T) {
	h := newDepBotHarness(t)
	pr := dbrTransientBotPR("sha-tr2")
	refused := &fakeTransientRerunner{out: github.TransientCIRerunOutcome{Settled: true, Skipped: "needs-opt-in-label"}}

	dbrCycle(t, h, depBotT0, pr, refused)
	dbrCycle(t, h, depBotT0.Add(depBotRedCIGrace-time.Minute), pr, refused)
	if len(refused.calls) != 1 || len(h.writer.comments) != 0 {
		t.Fatalf("before grace: re-run calls=%d comments=%v, want one attempt and no hand-off", len(refused.calls), h.writer.comments)
	}
	dbrCycle(t, h, depBotT0.Add(depBotRedCIGrace), pr, refused)
	if len(h.writer.comments) != 1 || len(h.writer.adds) != 1 {
		t.Fatalf("refused re-run: comments=%v adds=%v, want one hand-off", h.writer.comments, h.writer.adds)
	}
	c := h.writer.comments[0]
	for _, want := range []string{"not caused by the bump", "Hive was not allowed to re-run it (`needs-opt-in-label`)", "re-run the failed check"} {
		if !strings.Contains(c, want) {
			t.Errorf("comment = %q, want %q", c, want)
		}
	}
}

// A red bot head nobody triaged as transient is handed off at the grace as
// before, with no re-run.
func TestDependencyBotRedCIHandoffUntriagedUnchanged(t *testing.T) {
	h := newDepBotHarness(t)
	pr := depBotPR("dependabot[bot]", "sha-tr3", redCI)
	rerunner := &fakeTransientRerunner{out: github.TransientCIRerunOutcome{Settled: true, RunIDs: []int64{42}}}
	dbrCycle(t, h, depBotT0, pr, rerunner)
	dbrCycle(t, h, depBotT0.Add(depBotRedCIGrace), pr, rerunner)
	if len(rerunner.calls) != 0 {
		t.Fatalf("re-ran an untriaged head: %+v", rerunner.calls)
	}
	if len(h.writer.comments) != 1 || !strings.Contains(h.writer.comments[0], "Hive does not repair dependency-bot PRs") {
		t.Fatalf("comments = %v, want the ordinary red-CI hand-off", h.writer.comments)
	}
}

// A re-run that stays owed (the run listing keeps failing) does not strand
// the PR: past the backstop a person is told anyway.
func TestDependencyBotRedCIHandoffOwedRerunBackstop(t *testing.T) {
	h := newDepBotHarness(t)
	pr := dbrTransientBotPR("sha-tr4")
	failing := &fakeTransientRerunner{err: errors.New("502")}
	dbrCycle(t, h, depBotT0, pr, failing)
	dbrCycle(t, h, depBotT0.Add(depBotRedCIGrace+depBotTransientRerunBackstop-time.Minute), pr, failing)
	if len(h.writer.comments) != 0 {
		t.Fatalf("inside the backstop: comments=%v, want none", h.writer.comments)
	}
	dbrCycle(t, h, depBotT0.Add(depBotRedCIGrace+depBotTransientRerunBackstop), pr, failing)
	if len(h.writer.comments) != 1 || !strings.Contains(h.writer.comments[0], "nothing has re-run the check since") {
		t.Fatalf("comments = %v, want the hand-off past the backstop", h.writer.comments)
	}
}

func TestDependencyBotRedCICommentAfterRerun(t *testing.T) {
	pr := depBotPR("dependabot[bot]", "sha-1", redCI)
	cases := []struct {
		name    string
		rerun   depBotTransientRerun
		want    []string
		notWant []string
	}{
		{"re-run failed again", depBotTransientRerun{State: depBotRerunDone},
			[]string{"Dependabot PR has failed `build`, `test`", "re-ran the failed jobs once, and they failed again", "pin or ignore the dependency"},
			[]string{"nothing has re-run", "not allowed"}},
		{"gate refused", depBotTransientRerun{State: depBotRerunNotRun, Skipped: "below-min-level"},
			[]string{"Hive was not allowed to re-run it (`below-min-level`)", "re-run the failed check once the cause has cleared"},
			[]string{"pin or ignore the dependency", "nothing has re-run"}},
		{"no failed runs", depBotTransientRerun{State: depBotRerunNotRun, Skipped: "no-failed-runs"},
			[]string{"no failed GitHub Actions run on this head"},
			[]string{"not allowed"}},
	}
	for _, tc := range cases {
		c := dependencyBotRedCIComment(pr, tc.rerun)
		for _, w := range tc.want {
			if !strings.Contains(c, w) {
				t.Errorf("%s: comment = %q, want %q", tc.name, c, w)
			}
		}
		for _, w := range tc.notWant {
			if strings.Contains(c, w) {
				t.Errorf("%s: comment = %q, must not contain %q", tc.name, c, w)
			}
		}
	}
}

// A re-run owed on a head that has been red longer than the backstop still
// holds the hand-off until it has been owed for the backstop. Before, a
// triage that landed after a held hand-off (a pending rebase request) was
// handed off with needs-human in the same pass, and the label then kept the
// re-run from ever running.
func TestDependencyBotTransientRerunHoldsAfterLateTriage(t *testing.T) {
	owed := depBotTransientRerun{State: depBotRerunOwed}
	limit := depBotRedCIGrace + depBotTransientRerunBackstop
	cases := []struct {
		name      string
		rerun     depBotTransientRerun
		red, owed time.Duration
		want      bool
	}{
		{"owed, inside the red backstop", owed, limit - time.Minute, 0, true},
		{"owed, red past the backstop, just triaged", owed, limit + time.Hour, 0, true},
		{"owed, red past the backstop, owed past the backstop", owed, limit + time.Hour, depBotTransientRerunBackstop, false},
		{"done", depBotTransientRerun{State: depBotRerunDone}, time.Minute, 0, false},
		{"not triaged", depBotTransientRerun{}, time.Minute, 0, false},
	}
	for _, tc := range cases {
		if got := tc.rerun.holdsHandoff(tc.red, tc.owed); got != tc.want {
			t.Errorf("%s: holdsHandoff(%s, %s) = %v, want %v", tc.name, tc.red, tc.owed, got, tc.want)
		}
	}
}

// The owed clock starts on the first sighting, survives later passes on the
// same head, and starts again on a new head.
func TestDependencyBotRerunOwedSince(t *testing.T) {
	s := loadDepBotConflictStore(filepath.Join(t.TempDir(), "dependency-bot-conflicts.json"))
	t0 := depBotT0
	s.observe("acme/widgets#7", "sha-a", depBotHandoffRedCI, t0)
	t1 := t0.Add(9 * time.Hour)
	if got := s.rerunOwedSince("acme/widgets#7", "sha-a", t1); !got.Equal(t1) {
		t.Fatalf("first sighting = %v, want %v", got, t1)
	}
	if got := s.rerunOwedSince("acme/widgets#7", "sha-a", t1.Add(time.Hour)); !got.Equal(t1) {
		t.Fatalf("later pass = %v, want the first sighting %v", got, t1)
	}
	t2 := t1.Add(2 * time.Hour)
	s.observe("acme/widgets#7", "sha-b", depBotHandoffRedCI, t2)
	if got := s.rerunOwedSince("acme/widgets#7", "sha-b", t2.Add(time.Minute)); !got.Equal(t2.Add(time.Minute)) {
		t.Fatalf("new head = %v, want a fresh clock", got)
	}
}

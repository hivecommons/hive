package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
)

var depBotT0 = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func (h *depBotHarness) passAt(at time.Time, prs ...github.PullRequest) {
	h.t.Helper()
	a := &github.ActionableResult{}
	a.PRs.Items = prs
	sweepDependencyBotConflicts(context.Background(), h.store, "acme", h.writer, h.reader, a, func() time.Time { return at }, discardLogger())
}

func depBotPR(author, sha string, mut func(*github.PullRequest)) github.PullRequest {
	pr := github.PullRequest{
		Repo:           "acme/widgets",
		Number:         42,
		Author:         author,
		State:          "open",
		MergeableState: "clean",
		CIStatus:       "success",
		HeadSHA:        sha,
		BaseRef:        "main",
	}
	if mut != nil {
		mut(&pr)
	}
	return pr
}

func conflicting(p *github.PullRequest) { p.MergeableState = "dirty" }

func redCI(p *github.PullRequest) {
	p.MergeableState = "blocked"
	p.CIStatus = "failure"
	p.FailingChecks = []string{"build", "test"}
}

func TestDependencyBotHandoffCandidate(t *testing.T) {
	cases := []struct {
		name string
		pr   github.PullRequest
		want bool
	}{
		{"renovate", depBotPR("renovate[bot]", "s", nil), true},
		{"dependabot", depBotPR("dependabot[bot]", "s", nil), true},
		{"mergeraptor", depBotPR("mergeraptor[bot]", "s", nil), true},
		{"human", depBotPR("alice", "s", nil), false},
		{"hive agent bot", depBotPR("hive-agent[bot]", "s", nil), false},
		{"draft", depBotPR("dependabot[bot]", "s", func(p *github.PullRequest) { p.Draft = true }), false},
		{"closed", depBotPR("dependabot[bot]", "s", func(p *github.PullRequest) { p.State = "closed" }), false},
		{"needs-human", depBotPR("dependabot[bot]", "s", func(p *github.PullRequest) { p.Labels = []string{escalation.NeedsHumanLabel} }), false},
		{"hold", depBotPR("dependabot[bot]", "s", func(p *github.PullRequest) { p.Labels = []string{"hold"} }), false},
		{"do-not-merge", depBotPR("dependabot[bot]", "s", func(p *github.PullRequest) { p.Labels = []string{"do-not-merge"} }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dependencyBotHandoffCandidate(tc.pr); got != tc.want {
				t.Fatalf("dependencyBotHandoffCandidate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDependencyBotHandoffReason(t *testing.T) {
	cases := []struct {
		name   string
		pr     github.PullRequest
		held   string
		want   string
		wantOK bool
	}{
		{"green", depBotPR("dependabot[bot]", "s", nil), "", "", false},
		{"pending", depBotPR("dependabot[bot]", "s", func(p *github.PullRequest) { p.CIStatus = "pending" }), "", "", false},
		{"held check", depBotPR("dependabot[bot]", "s", nil), "required-check-held:ci/build", depBotHandoffHeld, true},
		{"never started", depBotPR("dependabot[bot]", "s", nil), "required-check-never-started:ci/build", depBotHandoffHeld, true},
		{"never ran", depBotPR("dependabot[bot]", "s", nil), "required-check-never-ran:ci/build", depBotHandoffHeld, true},
		{"conflict", depBotPR("dependabot[bot]", "s", conflicting), "", depBotHandoffConflict, true},
		{"needs-rebase label", depBotPR("dependabot[bot]", "s", func(p *github.PullRequest) { p.Labels = []string{"needs-rebase"} }), "", depBotHandoffConflict, true},
		{"red ci", depBotPR("dependabot[bot]", "s", redCI), "", depBotHandoffRedCI, true},
		{"red ci still running", depBotPR("dependabot[bot]", "s", func(p *github.PullRequest) { redCI(p); p.CIChecksRunning = true }), "", "", false},
		{"red ci external", depBotPR("dependabot[bot]", "s", func(p *github.PullRequest) { redCI(p); p.ExternalCIDeferred = true }), "", "", false},
		{"red ci transient still hands off", depBotPR("dependabot[bot]", "s", func(p *github.PullRequest) { redCI(p); p.TransientCITriaged = true }), "", depBotHandoffRedCI, true},
		{"red ci shared incident", depBotPR("dependabot[bot]", "s", func(p *github.PullRequest) { redCI(p); p.SharedCIIncident = 7 }), "", "", false},
		{"failure without failing checks", depBotPR("dependabot[bot]", "s", func(p *github.PullRequest) { p.CIStatus = "failure" }), "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := dependencyBotHandoffReason(tc.pr, tc.held)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("dependencyBotHandoffReason = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestDependencyBotHandoffDependabotConflictWaitsForGrace(t *testing.T) {
	h := newDepBotHarness(t)
	pr := depBotPR("dependabot[bot]", "sha-1", conflicting)

	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(depBotConflictGrace-time.Minute), pr)
	if len(h.writer.comments) != 0 || len(h.writer.adds) != 0 {
		t.Fatalf("before grace: comments=%v adds=%v, want none", h.writer.comments, h.writer.adds)
	}

	h.passAt(depBotT0.Add(depBotConflictGrace), pr)
	if len(h.writer.comments) != 1 {
		t.Fatalf("comments = %v, want exactly one after the grace period", h.writer.comments)
	}
	if c := h.writer.comments[0]; !strings.Contains(c, "Dependabot PR has conflicted with `main`") || !strings.Contains(c, "`@dependabot rebase`") {
		t.Fatalf("comment = %q, want Dependabot wording with the rebase command", c)
	}
	if want := depBotKey + ": " + escalation.NeedsHumanLabel; len(h.writer.adds) != 1 || h.writer.adds[0] != want {
		t.Fatalf("adds = %v, want [%q]", h.writer.adds, want)
	}
	if h.reader.bodyFetches != 0 {
		t.Fatalf("body fetches = %d, want none for a non-Renovate PR", h.reader.bodyFetches)
	}

	h.passAt(depBotT0.Add(2*depBotConflictGrace), pr)
	if len(h.writer.comments) != 1 || len(h.writer.adds) != 1 {
		t.Fatalf("repeat tick: comments=%v adds=%v, want no new writes", h.writer.comments, h.writer.adds)
	}
}

func TestDependencyBotHandoffMergeraptorConflictWaitsForGrace(t *testing.T) {
	h := newDepBotHarness(t)
	pr := depBotPR("mergeraptor[bot]", "sha-1", conflicting)
	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(depBotConflictGrace), pr)
	if len(h.writer.comments) != 1 || !strings.Contains(h.writer.comments[0], "mergeraptor PR has conflicted") {
		t.Fatalf("comments = %v, want one mergeraptor hand-off", h.writer.comments)
	}
}

func TestDependencyBotHandoffRenovateWithoutMarkerWaitsForGrace(t *testing.T) {
	h := newDepBotHarness(t)
	h.reader.body = renovateDefaultBody
	pr := renovateConflictPR("sha-1")

	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(time.Hour), pr)
	if len(h.writer.comments) != 0 {
		t.Fatalf("before grace: comments = %v, want none", h.writer.comments)
	}
	h.passAt(depBotT0.Add(depBotConflictGrace), pr)
	if len(h.writer.comments) != 1 {
		t.Fatalf("comments = %v, want one after the grace period", h.writer.comments)
	}
	if c := h.writer.comments[0]; !strings.Contains(c, "Renovate has not rebased it") || !strings.Contains(c, "If you want to rebase/retry this PR, check this box") {
		t.Fatalf("comment = %q, want the waited Renovate wording naming the checkbox", c)
	}
	if h.reader.bodyFetches != 1 {
		t.Fatalf("body fetches = %d, want 1 per head", h.reader.bodyFetches)
	}
}

func TestDependencyBotHandoffRenovateNeverMarkerIsImmediate(t *testing.T) {
	h := newDepBotHarness(t)
	h.passAt(depBotT0, renovateConflictPR("sha-1"))
	if len(h.writer.comments) != 1 || h.writer.comments[0] != depBotKey+": "+dependencyBotConflictComment("main") {
		t.Fatalf("comments = %v, want the existing rebaseWhen-never comment at once", h.writer.comments)
	}
	h.passAt(depBotT0.Add(depBotConflictGrace), renovateConflictPR("sha-1"))
	if len(h.writer.comments) != 1 {
		t.Fatalf("comments = %v, want no second comment once the grace passes", h.writer.comments)
	}
}

func TestDependencyBotHandoffGraceRestartsWhenConflictClears(t *testing.T) {
	h := newDepBotHarness(t)
	pr := depBotPR("dependabot[bot]", "sha-1", conflicting)
	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(12*time.Hour), depBotPR("dependabot[bot]", "sha-1", nil))
	h.passAt(depBotT0.Add(30*time.Hour), pr)
	if len(h.writer.comments) != 0 {
		t.Fatalf("comments = %v, want the grace clock restarted after the conflict cleared", h.writer.comments)
	}
	h.passAt(depBotT0.Add(30*time.Hour+depBotConflictGrace), pr)
	if len(h.writer.comments) != 1 {
		t.Fatalf("comments = %v, want one once the restarted grace passes", h.writer.comments)
	}
}

func TestDependencyBotHandoffGraceSurvivesRestart(t *testing.T) {
	h := newDepBotHarness(t)
	pr := depBotPR("dependabot[bot]", "sha-1", conflicting)
	h.passAt(depBotT0, pr)

	h.store = loadDepBotConflictStore(h.path)
	h.passAt(depBotT0.Add(depBotConflictGrace), pr)
	if len(h.writer.comments) != 1 {
		t.Fatalf("comments = %v, want the persisted grace clock to fire after a restart", h.writer.comments)
	}
}

func TestDependencyBotHandoffRedCIWaitsForGraceThenHandsOff(t *testing.T) {
	h := newDepBotHarness(t)
	pr := depBotPR("dependabot[bot]", "sha-1", redCI)

	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(depBotRedCIGrace-time.Minute), pr)
	if len(h.writer.comments) != 0 {
		t.Fatalf("before grace: comments = %v, want none", h.writer.comments)
	}
	h.passAt(depBotT0.Add(depBotRedCIGrace), pr)
	if len(h.writer.comments) != 1 {
		t.Fatalf("comments = %v, want exactly one", h.writer.comments)
	}
	if c := h.writer.comments[0]; !strings.Contains(c, "Dependabot PR has failed `build`, `test`") || !strings.Contains(c, "pin or ignore the dependency") || !strings.Contains(c, "re-run the failed check") {
		t.Fatalf("comment = %q, want the failing checks and the actions named", c)
	}
	if len(h.writer.adds) != 1 {
		t.Fatalf("adds = %v, want needs-human once", h.writer.adds)
	}
	rec, ok := h.store.get(depBotKey)
	if !ok || rec.Reason != depBotHandoffRedCI || rec.HeadSHA != "sha-1" {
		t.Fatalf("record = %+v (ok=%v), want red-ci on sha-1", rec, ok)
	}

	labeled := pr
	labeled.Labels = []string{escalation.NeedsHumanLabel}
	h.passAt(depBotT0.Add(2*depBotRedCIGrace), labeled)
	h.passAt(depBotT0.Add(3*depBotRedCIGrace), pr)
	if len(h.writer.comments) != 1 || len(h.writer.adds) != 1 {
		t.Fatalf("repeat ticks: comments=%v adds=%v, want no new writes", h.writer.comments, h.writer.adds)
	}
}

// hivecommons/hive#12977: when Hive's own triage marked every failing run at
// this head as transient, the hand-off asks for a re-run and does not tell the
// owner to fix, pin or close a bump that did not break the build.
func TestDependencyBotRedCICommentHonorsTransientTriage(t *testing.T) {
	cases := []struct {
		name      string
		transient bool
		want      []string
		notWant   []string
	}{
		{"untriaged", false,
			[]string{"re-run the failed check", "If the bump breaks the build", "pin or ignore the dependency"},
			[]string{"not caused by the bump"}},
		{"transient triage on this head", true,
			[]string{"not caused by the bump", "re-run the failed check"},
			[]string{"pin or ignore the dependency", "fix the code or lockfile", "close the PR", "breaks the build"}},
	}
	for _, tc := range cases {
		pr := depBotPR("renovate[bot]", "sha-1", func(p *github.PullRequest) { redCI(p); p.TransientCITriaged = tc.transient })
		c := dependencyBotRedCIComment(pr)
		if !strings.Contains(c, "Renovate PR has failed `build`, `test`") {
			t.Errorf("%s: comment = %q, want the failing checks named", tc.name, c)
		}
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

func TestDependencyBotHandoffRedCIStillRunningNeverHandsOff(t *testing.T) {
	h := newDepBotHarness(t)
	pr := depBotPR("renovate[bot]", "sha-1", func(p *github.PullRequest) { redCI(p); p.CIChecksRunning = true })
	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(2*depBotRedCIGrace), pr)
	if len(h.writer.comments) != 0 || len(h.writer.adds) != 0 {
		t.Fatalf("comments=%v adds=%v, want none while CI is still running", h.writer.comments, h.writer.adds)
	}
}

func TestDependencyBotHandoffRedCIUndoWhenGreen(t *testing.T) {
	h := newDepBotHarness(t)
	pr := depBotPR("dependabot[bot]", "sha-1", redCI)
	h.passAt(depBotT0, pr)
	h.passAt(depBotT0.Add(depBotRedCIGrace), pr)

	stillRed := pr
	stillRed.HeadSHA = "sha-2"
	stillRed.Labels = []string{escalation.NeedsHumanLabel}
	h.passAt(depBotT0.Add(5*time.Hour), stillRed)
	if len(h.writer.removes) != 0 {
		t.Fatalf("removes = %v, want the label kept while CI is still red", h.writer.removes)
	}

	green := depBotPR("dependabot[bot]", "sha-2", func(p *github.PullRequest) { p.Labels = []string{escalation.NeedsHumanLabel} })
	h.passAt(depBotT0.Add(6*time.Hour), green)
	h.passAt(depBotT0.Add(7*time.Hour), green)
	if want := depBotKey + ": " + escalation.NeedsHumanLabel; len(h.writer.removes) != 1 || h.writer.removes[0] != want {
		t.Fatalf("removes = %v, want one removal once CI is green", h.writer.removes)
	}
}

func TestDependencyBotHandoffParkedPRSkipped(t *testing.T) {
	for _, label := range []string{escalation.NeedsHumanLabel, "hold", "do-not-merge"} {
		t.Run(label, func(t *testing.T) {
			h := newDepBotHarness(t)
			pr := depBotPR("dependabot[bot]", "sha-1", func(p *github.PullRequest) { redCI(p); p.Labels = []string{label} })
			h.store.noteHold(depBotKey, "sha-1", "required-check-held:ci/build")
			h.passAt(depBotT0, pr)
			h.passAt(depBotT0.Add(2*depBotRedCIGrace), pr)
			if len(h.writer.comments) != 0 || len(h.writer.adds) != 0 {
				t.Fatalf("comments=%v adds=%v, want no writes on a parked PR", h.writer.comments, h.writer.adds)
			}
		})
	}
}

func TestDependencyBotHandoffTerminalHoldIsImmediateOncePerHead(t *testing.T) {
	h := newDepBotHarness(t)
	h.store.noteHold(depBotKey, "sha-1", "required-check-held:ci/build")
	pr := depBotPR("dependabot[bot]", "sha-1", func(p *github.PullRequest) { p.MergeableState = "blocked"; p.CIStatus = "pending" })

	h.passAt(depBotT0, pr)
	if len(h.writer.comments) != 1 {
		t.Fatalf("comments = %v, want one at once", h.writer.comments)
	}
	if c := h.writer.comments[0]; !strings.Contains(c, "`ci/build`") || !strings.Contains(c, "Approve and run") {
		t.Fatalf("comment = %q, want the held check and the approve action named", c)
	}
	if len(h.writer.adds) != 1 {
		t.Fatalf("adds = %v, want needs-human once", h.writer.adds)
	}

	h.passAt(depBotT0.Add(time.Hour), pr)
	if len(h.writer.comments) != 1 || len(h.writer.adds) != 1 {
		t.Fatalf("repeat tick: comments=%v adds=%v, want no new writes", h.writer.comments, h.writer.adds)
	}

	// A new head: the old hold no longer applies and the label comes off.
	moved := depBotPR("dependabot[bot]", "sha-2", func(p *github.PullRequest) {
		p.CIStatus = "pending"
		p.Labels = []string{escalation.NeedsHumanLabel}
	})
	h.passAt(depBotT0.Add(2*time.Hour), moved)
	if want := depBotKey + ": " + escalation.NeedsHumanLabel; len(h.writer.removes) != 1 || h.writer.removes[0] != want {
		t.Fatalf("removes = %v, want one removal after the head changed", h.writer.removes)
	}
	if got := h.store.heldReason(depBotKey, "sha-2"); got != "" {
		t.Fatalf("heldReason for the new head = %q, want none", got)
	}
	h.passAt(depBotT0.Add(3*time.Hour), depBotPR("dependabot[bot]", "sha-2", func(p *github.PullRequest) { p.CIStatus = "pending" }))
	if len(h.writer.comments) != 1 {
		t.Fatalf("comments = %v, want no hand-off for a new head without a hold", h.writer.comments)
	}
}

func TestDependencyBotHandoffTerminalHoldIgnoresNonBotPR(t *testing.T) {
	h := newDepBotHarness(t)
	h.store.noteHold(depBotKey, "sha-1", "required-check-held:ci/build")
	h.passAt(depBotT0, depBotPR("hive-agent[bot]", "sha-1", nil))
	if len(h.writer.comments) != 0 {
		t.Fatalf("comments = %v, want none for a PR outside dependencyBots", h.writer.comments)
	}
}

func TestDependencyBotHeldCheckComment(t *testing.T) {
	cases := []struct {
		reason string
		want   []string
	}{
		{"required-check-held:ci/build", []string{"`ci/build`", "waiting for approval", "gh run approve"}},
		{"required-check-never-started:ci/build", []string{"`ci/build`", "never started", "close and reopen"}},
		{"required-check-never-ran:ci/build", []string{"`ci/build`", "even after a rebase was requested", "merge the PR by hand"}},
		{"workflow-approval-required:missing-workflow:ci/build", []string{"`ci/build`", "/ok-to-test", "hive-ok-to-test", "hivectl ok-to-test-template"}},
	}
	for _, tc := range cases {
		got := dependencyBotHeldCheckComment("renovate[bot]", tc.reason)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Fatalf("comment for %q = %q, want it to contain %q", tc.reason, got, w)
			}
		}
		if !strings.Contains(got, "Renovate PR") {
			t.Fatalf("comment for %q = %q, want the bot named", tc.reason, got)
		}
	}
}

// hivecommons/hive#13013: the sweep no longer approves held runs itself; it
// posts `/ok-to-test` and reports workflow-approval-required:<ctx>. That hold
// is not handed off while the repository workflow may still approve the
// runs; only the missing-workflow variant (after the grace) is.
func TestDependencyBotHoldNeedsPerson(t *testing.T) {
	cases := map[string]bool{
		"workflow-approval-required:ci/build":                  false,
		"workflow-approval-required:missing-workflow:ci/build": true,
		"required-check-held:ci/build":                         true,
		"required-check-never-started:ci/build":                true,
		"required-check-never-ran:ci/build":                    true,
	}
	for reason, want := range cases {
		if got := dependencyBotHoldNeedsPerson(reason); got != want {
			t.Errorf("dependencyBotHoldNeedsPerson(%q) = %v, want %v", reason, got, want)
		}
	}
}

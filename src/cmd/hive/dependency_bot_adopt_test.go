package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// depBotAdoptFake is a policy with every check answered by the test.
type depBotAdoptFake struct {
	verdicts     map[string]string // head SHA -> swarm verdict
	pathHeld     bool
	pathErr      error
	pathChecks   int
	verdictReads int
}

func (f *depBotAdoptFake) policy() depBotAdoptPolicy {
	return depBotAdoptPolicy{
		Enabled:       true,
		StageAdoption: true,
		TrustedBot:    func(author string) bool { return author == "dependabot[bot]" || author == "mergeraptor[bot]" },
		HumanMerge: func(pr github.PullRequest) bool {
			return hasLabel(pr.Labels, "needs-owner-merge")
		},
		HeadVerdict: func(_ string, _ int, head string) string {
			f.verdictReads++
			return f.verdicts[head]
		},
		HumanMergePath: func(context.Context, string, int, string) (bool, error) {
			f.pathChecks++
			return f.pathHeld, f.pathErr
		},
	}
}

func newDepBotAdoptFake() *depBotAdoptFake {
	return &depBotAdoptFake{verdicts: map[string]string{"sha-1": "changes_requested", "sha-2": "changes_requested"}}
}

func (h *depBotHarness) adoptPassAt(at time.Time, p depBotAdoptPolicy, prs ...github.PullRequest) {
	h.t.Helper()
	a := &github.ActionableResult{}
	a.PRs.Items = prs
	sweepDependencyBotConflictsWith(context.Background(), h.store, "acme", 0, p, h.writer, h.reader, a, func() time.Time { return at }, discardLogger())
}

// redPastGrace runs the two passes that start the red-CI clock and reach the
// grace on pr's head.
func (h *depBotHarness) redPastGrace(start time.Time, p depBotAdoptPolicy, pr github.PullRequest) time.Time {
	h.t.Helper()
	h.adoptPassAt(start, p, pr)
	at := start.Add(depBotRedCIGrace)
	h.adoptPassAt(at, p, pr)
	return at
}

func (h *depBotHarness) adoptLabelAdds() int {
	n := 0
	for _, a := range h.writer.adds {
		if strings.HasSuffix(a, ": "+delegatedecision.LabelAdoptSupersede) {
			n++
		}
	}
	return n
}

func (h *depBotHarness) wantHandoffOnly(what string) {
	h.t.Helper()
	if len(h.writer.comments) != 1 {
		h.t.Fatalf("%s: comments = %v, want the one person hand-off", what, h.writer.comments)
	}
	if n := h.adoptLabelAdds(); n != 0 {
		h.t.Fatalf("%s: adds = %v, want no adoption request", what, h.writer.adds)
	}
	if want := depBotKey + ": " + escalation.NeedsHumanLabel; len(h.writer.adds) != 1 || h.writer.adds[0] != want {
		h.t.Fatalf("%s: adds = %v, want [%q]", what, h.writer.adds, want)
	}
}

// Switch off: the red-CI hand-off is exactly today's, and no verdict is read.
func TestDependencyBotAdoptSwitchOffKeepsHandoff(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotAdoptFake()
	p := f.policy()
	p.Enabled = false
	h.redPastGrace(depBotT0, p, depBotPR("dependabot[bot]", "sha-1", redCI))
	h.wantHandoffOnly("switch off")
	if f.verdictReads != 0 || f.pathChecks != 0 {
		t.Fatalf("switch off read verdicts=%d paths=%d, want none", f.verdictReads, f.pathChecks)
	}
	if _, ok := h.store.adoptRequest(depBotKey); ok {
		t.Fatal("switch off recorded an adoption request")
	}
}

// Switch on, red past the grace, swarm said changes_requested on this head:
// one adoption label, no hand-off comment, no needs-human. Later passes on the
// same head (and a restart) never repeat it; a new head is eligible again.
func TestDependencyBotAdoptRequestsOncePerHead(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotAdoptFake()
	pr := depBotPR("dependabot[bot]", "sha-1", redCI)

	h.adoptPassAt(depBotT0, f.policy(), pr)
	h.adoptPassAt(depBotT0.Add(depBotRedCIGrace-time.Minute), f.policy(), pr)
	if len(h.writer.adds) != 0 || len(h.writer.comments) != 0 {
		t.Fatalf("before grace: adds=%v comments=%v, want none", h.writer.adds, h.writer.comments)
	}
	h.adoptPassAt(depBotT0.Add(depBotRedCIGrace), f.policy(), pr)
	if want := depBotKey + ": " + delegatedecision.LabelAdoptSupersede; len(h.writer.adds) != 1 || h.writer.adds[0] != want {
		t.Fatalf("adds = %v, want exactly [%q]", h.writer.adds, want)
	}
	if len(h.writer.comments) != 0 {
		t.Fatalf("comments = %v, want no person hand-off", h.writer.comments)
	}
	rec, ok := h.store.adoptRequest(depBotKey)
	if !ok || rec.HeadSHA != "sha-1" || rec.Failed {
		t.Fatalf("adopt record = %+v (ok=%v), want sha-1, not failed", rec, ok)
	}

	// Second pass on the same head: nothing new, even with the label on.
	labeled := pr
	labeled.Labels = []string{delegatedecision.LabelAdoptSupersede}
	h.adoptPassAt(depBotT0.Add(2*depBotRedCIGrace), f.policy(), labeled)
	h.adoptPassAt(depBotT0.Add(3*depBotRedCIGrace), f.policy(), pr)
	if len(h.writer.adds) != 1 || len(h.writer.comments) != 0 {
		t.Fatalf("same head again: adds=%v comments=%v, want no new writes", h.writer.adds, h.writer.comments)
	}

	// A restart reloads the request from disk.
	h.store = loadDepBotConflictStore(h.path)
	h.adoptPassAt(depBotT0.Add(4*depBotRedCIGrace), f.policy(), pr)
	if len(h.writer.adds) != 1 || len(h.writer.comments) != 0 {
		t.Fatalf("after restart: adds=%v comments=%v, want no new writes", h.writer.adds, h.writer.comments)
	}

	// The bot pushed a new head: eligible again once it is red past the grace.
	next := depBotPR("dependabot[bot]", "sha-2", redCI)
	h.redPastGrace(depBotT0.Add(5*depBotRedCIGrace), f.policy(), next)
	if n := h.adoptLabelAdds(); n != 2 || len(h.writer.comments) != 0 {
		t.Fatalf("new head: adds=%v comments=%v, want a second adoption request and no hand-off", h.writer.adds, h.writer.comments)
	}
	if rec, _ := h.store.adoptRequest(depBotKey); rec.HeadSHA != "sha-2" {
		t.Fatalf("adopt record head = %q, want sha-2", rec.HeadSHA)
	}
}

// Every case the switch must leave to a person gets today's hand-off and no
// adoption request.
func TestDependencyBotAdoptSkipsToHandoff(t *testing.T) {
	cases := []struct {
		name   string
		author string
		mut    func(*github.PullRequest)
		tweak  func(*depBotAdoptFake, *depBotAdoptPolicy)
	}{
		{"verdict missing", "dependabot[bot]", nil, func(f *depBotAdoptFake, _ *depBotAdoptPolicy) { delete(f.verdicts, "sha-1") }},
		{"verdict advisory", "dependabot[bot]", nil, func(f *depBotAdoptFake, _ *depBotAdoptPolicy) { f.verdicts["sha-1"] = "advisory" }},
		{"verdict approve", "dependabot[bot]", nil, func(f *depBotAdoptFake, _ *depBotAdoptPolicy) { f.verdicts["sha-1"] = string(review.VerdictApprove) }},
		{"verdict requires_human", "dependabot[bot]", nil, func(f *depBotAdoptFake, _ *depBotAdoptPolicy) {
			f.verdicts["sha-1"] = string(review.VerdictRequiresHuman)
		}},
		{"verdict on another head only", "dependabot[bot]", nil, func(f *depBotAdoptFake, _ *depBotAdoptPolicy) {
			delete(f.verdicts, "sha-1")
			f.verdicts["sha-0"] = "changes_requested"
		}},
		{"human-merge path", "dependabot[bot]", nil, func(f *depBotAdoptFake, _ *depBotAdoptPolicy) { f.pathHeld = true }},
		{"human-merge path unreadable", "dependabot[bot]", nil, func(f *depBotAdoptFake, _ *depBotAdoptPolicy) { f.pathErr = errors.New("502") }},
		{"no human-merge path checker", "dependabot[bot]", nil, func(_ *depBotAdoptFake, p *depBotAdoptPolicy) { p.HumanMergePath = nil }},
		{"needs-owner-merge label", "dependabot[bot]", func(p *github.PullRequest) { p.Labels = []string{"needs-owner-merge"} }, nil},
		{"adoption disabled", "dependabot[bot]", nil, func(_ *depBotAdoptFake, p *depBotAdoptPolicy) { p.StageAdoption = false }},
		{"not a trusted bot", "renovate[bot]", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDepBotHarness(t)
			f := newDepBotAdoptFake()
			p := f.policy()
			if tc.tweak != nil {
				tc.tweak(f, &p)
			}
			pr := depBotPR(tc.author, "sha-1", func(p *github.PullRequest) {
				redCI(p)
				if tc.mut != nil {
					tc.mut(p)
				}
			})
			h.redPastGrace(depBotT0, p, pr)
			h.wantHandoffOnly(tc.name)
			if _, ok := h.store.adoptRequest(depBotKey); ok {
				t.Fatal("a skipped head recorded an adoption request")
			}
		})
	}
}

// A held PR is already out of the hand-off sweep (dependencyBotHandoffCandidate);
// the switch does not change that: no request, no comment. A `hold` alone also
// keeps skipReason from asking, should the candidate filter ever widen.
func TestDependencyBotAdoptSkipsHeldPR(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotAdoptFake()
	pr := depBotPR("dependabot[bot]", "sha-1", func(p *github.PullRequest) {
		redCI(p)
		p.Labels = []string{"hold"}
	})
	h.redPastGrace(depBotT0, f.policy(), pr)
	if len(h.writer.adds) != 0 || len(h.writer.comments) != 0 {
		t.Fatalf("held PR: adds=%v comments=%v, want none", h.writer.adds, h.writer.comments)
	}
	for _, label := range []string{"hold", "do-not-merge", "do-not-merge/hold"} {
		if why := f.policy().skipReason(context.Background(), "acme/widgets", depBotPR("dependabot[bot]", "sha-1", func(p *github.PullRequest) {
			redCI(p)
			p.Labels = []string{label}
		})); why != depBotAdoptSkipHoldLabel {
			t.Fatalf("%s: skipReason = %q, want %q", label, why, depBotAdoptSkipHoldLabel)
		}
	}
}

// A green PR is never asked about, switch on or off.
func TestDependencyBotAdoptGreenDoesNothing(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotAdoptFake()
	pr := depBotPR("dependabot[bot]", "sha-1", nil)
	h.redPastGrace(depBotT0, f.policy(), pr)
	if len(h.writer.adds) != 0 || len(h.writer.comments) != 0 || f.verdictReads != 0 {
		t.Fatalf("green PR: adds=%v comments=%v verdict reads=%d, want none", h.writer.adds, h.writer.comments, f.verdictReads)
	}
}

// A failed label write is recorded for the head, so it is not retried; the
// next pass gives the person the usual hand-off.
func TestDependencyBotAdoptLabelFailureFallsBackToHandoff(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotAdoptFake()
	pr := depBotPR("dependabot[bot]", "sha-1", redCI)

	h.adoptPassAt(depBotT0, f.policy(), pr)
	h.writer.failAddLabel = true
	h.adoptPassAt(depBotT0.Add(depBotRedCIGrace), f.policy(), pr)
	if len(h.writer.comments) != 0 {
		t.Fatalf("failure pass: comments = %v, want none yet", h.writer.comments)
	}
	if rec, ok := h.store.adoptRequest(depBotKey); !ok || !rec.Failed || rec.HeadSHA != "sha-1" {
		t.Fatalf("adopt record = %+v (ok=%v), want a failed request on sha-1", rec, ok)
	}

	h.writer.failAddLabel = false
	h.adoptPassAt(depBotT0.Add(depBotRedCIGrace+time.Minute), f.policy(), pr)
	h.wantHandoffOnly("after a failed request")
	h.adoptPassAt(depBotT0.Add(2*depBotRedCIGrace), f.policy(), pr)
	if len(h.writer.comments) != 1 || h.adoptLabelAdds() != 0 {
		t.Fatalf("repeat: comments=%v adds=%v, want no retry and no second hand-off", h.writer.comments, h.writer.adds)
	}
}

// The verdict is the swarm's aggregate on exactly the PR's head, under the
// full or the short repo name, whatever the trusted-bot lane later logs.
func TestDependencyBotHeadVerdict(t *testing.T) {
	art := review.Artifact{Items: []review.Aggregate{
		{Repo: "widgets", Number: 42, HeadSHA: "sha-1", Verdict: review.VerdictChangesRequested},
		{Repo: "acme/widgets", Number: 42, HeadSHA: "sha-0", Verdict: review.VerdictApprove},
		{Repo: "acme/gadgets", Number: 7, HeadSHA: "sha-9", Verdict: review.VerdictRequiresHuman},
	}}
	for _, tc := range []struct {
		repo   string
		number int
		head   string
		want   string
	}{
		{"acme/widgets", 42, "sha-1", "changes_requested"},
		{"ACME/Widgets", 42, "sha-1", "changes_requested"},
		{"acme/widgets", 43, "sha-1", ""},
		{"acme/widgets", 42, "sha-0", "approve"},
		{"acme/widgets", 42, "sha-2", ""},
		{"acme/widgets", 42, "", ""},
		{"acme/gadgets", 7, "sha-9", "requires_human"},
	} {
		if got := depBotHeadVerdict(art, tc.repo, tc.number, tc.head); got != tc.want {
			t.Errorf("depBotHeadVerdict(%s#%d@%s) = %q, want %q", tc.repo, tc.number, tc.head, got, tc.want)
		}
	}
}

// The live policy is off unless auto_merge.dependency_bot_adopt.enabled is
// set, and reads stage adoption and the trusted bot set from the config.
func TestDependencyBotAdoptPolicyFromConfig(t *testing.T) {
	cfg := &config.Config{}
	if p := depBotAdoptPolicyFromConfig(cfg, &depBotFakeReader{}, discardLogger()); p.Enabled {
		t.Fatal("policy enabled with the switch unset")
	}
	cfg.AutoMerge.DependencyBotAdopt.Enabled = true
	cfg.AutoMerge.TrustedBotAuthors = []string{"mergeraptor[bot]"}
	p := depBotAdoptPolicyFromConfig(cfg, &depBotFakeReader{}, discardLogger())
	if !p.Enabled || !p.StageAdoption {
		t.Fatalf("policy = %+v, want enabled with stage adoption on by default", p)
	}
	if !p.TrustedBot("mergeraptor[bot]") || p.TrustedBot("dependabot[bot]") {
		t.Fatal("TrustedBot does not follow auto_merge.trusted_bot_authors")
	}
	if p.HumanMergePath != nil {
		t.Fatal("a reader without HumanMergePathHeld must leave the path check unset (fail closed)")
	}
	if !p.HumanMerge(github.PullRequest{Labels: []string{"needs-owner-merge"}}) || p.HumanMerge(github.PullRequest{Title: "chore(deps): bump x"}) {
		t.Fatal("HumanMerge does not follow the human-merge rule defaults")
	}
	off := false
	cfg.Governor.Labels.Stage.Adoption.Enabled = &off
	if p := depBotAdoptPolicyFromConfig(cfg, &depBotFakeReader{}, discardLogger()); p.StageAdoption {
		t.Fatal("StageAdoption on with governor.labels.stage.adoption.enabled: false")
	}
}

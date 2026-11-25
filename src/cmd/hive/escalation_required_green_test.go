package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// requiredGreenPR is advisoryRedPR whose every required check also completed
// with a passing conclusion and which GitHub reports unstable (mergeable,
// only checks it does not require failing): the shape of Danathar/tududi#658
// after the reviewer's repair, where only "Mutation check (advisory)" stayed
// red.
func requiredGreenPR(headSHA string) github.PullRequest {
	pr := advisoryRedPR(headSHA)
	pr.Protection.RequiredChecksPassed = true
	pr.Mergeable = github.MergeableYes
	pr.MergeableState = "unstable"
	return pr
}

// requiredGreenConfig declares the required-check set the merge classifier
// needs before it treats a red PR as optional-only.
func requiredGreenConfig() *config.Config {
	cfg := escalationTestConfig()
	cfg.AutoMerge.RequiredChecks = []string{"test"}
	return cfg
}

// provenanceWriter is depBotFakeWriter that can also answer whether the
// newest needs-human event is the App's own `labeled`.
type provenanceWriter struct {
	depBotFakeWriter
	byApp bool
	err   error
	reads int
}

func (w *provenanceWriter) StaleRedNeedsHumanLabeledByApp(context.Context, string, int) (bool, error) {
	w.reads++
	return w.byApp, w.err
}

var _ staleRedLabelActorReader = (*provenanceWriter)(nil)

func TestEscalationObservationRequiredGreen(t *testing.T) {
	cfg := requiredGreenConfig()
	with := func(edit func(*github.PullRequest)) github.PullRequest {
		pr := requiredGreenPR("abc")
		edit(&pr)
		return pr
	}
	advisoryRequired := escalationTestConfig()
	advisoryRequired.AutoMerge.RequiredChecks = []string{advisoryCheck}
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		pr   github.PullRequest
		want bool
	}{
		{"every required check green, advisory red, unstable", cfg, requiredGreenPR("abc"), true},
		{"required set unknown", cfg, with(func(p *github.PullRequest) { p.Protection = nil }), false},
		{"known-empty required set", cfg, with(func(p *github.PullRequest) {
			p.Protection = &github.ProtectionFacts{RequiredChecksKnown: true}
		}), false},
		{"a required check with no run", cfg, with(func(p *github.PullRequest) {
			p.Protection.RequiredChecksAllReported = false
			p.Protection.RequiredChecksPassed = false
		}), false},
		{"a required check failed", cfg, with(func(p *github.PullRequest) {
			p.FailingChecks = []string{"test", advisoryCheck}
			p.Protection.FailingRequiredChecks = []string{"test"}
			p.Protection.RequiredChecksPassed = false
		}), false},
		// Cancelled, timed out or still queued: reported, not failing, but
		// not passed either (see github.ProtectionFacts.RequiredChecksPassed).
		{"a required check cancelled or timed out", cfg, with(func(p *github.PullRequest) { p.Protection.RequiredChecksPassed = false }), false},
		{"checks still running", cfg, with(func(p *github.PullRequest) { p.CIChecksRunning = true }), false},
		{"nothing failing yet, CI still pending", cfg, with(func(p *github.PullRequest) {
			p.CIStatus = "pending"
			p.FailingChecks = nil
		}), false},
		// With no declared set the merge classifier files a red PR as
		// failing, so a lift would only hand it to FIX-BEFORE-NEW.
		{"no declared required set", escalationTestConfig(), requiredGreenPR("abc"), false},
		{"declared set names the failing check", advisoryRequired, requiredGreenPR("abc"), false},
		{"mergeable_state blocked", cfg, with(func(p *github.PullRequest) { p.MergeableState = "blocked" }), false},
		{"mergeable_state clean", cfg, with(func(p *github.PullRequest) { p.MergeableState = "clean" }), false},
		{"mergeability unknown", cfg, with(func(p *github.PullRequest) { p.Mergeable = github.MergeableUnknown }), false},
		{"reviewer-recommend-close", cfg, with(func(p *github.PullRequest) {
			p.Labels = []string{escalation.NeedsHumanLabel, scheduler.ReviewerRecommendCloseLabel}
		}), false},
		{"reviewer-handoff", cfg, with(func(p *github.PullRequest) {
			p.Labels = []string{escalation.NeedsHumanLabel, scheduler.ReviewerHandoffLabel}
		}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := escalationObservation(tc.cfg, tc.pr, nil)
			if o.RequiredGreen != tc.want {
				t.Errorf("RequiredGreen = %v, want %v (Red = %v, Pending = %v)", o.RequiredGreen, tc.want, o.Red, o.Pending)
			}
			if o.RequiredGreen && (o.Red || !o.Pending) {
				t.Errorf("a required-green head must stay Pending, not Red: Red = %v, Pending = %v", o.Red, o.Pending)
			}
		})
	}
}

// escalateWidgets drives acme/widgets#7 through two required-red heads so the
// sweep posts its evidence comment and adds needs-human.
func escalateWidgets(t *testing.T, cfg *config.Config, w forgeWriterWithLog) {
	t.Helper()
	cfg.Escalation.Threshold = 2
	for _, sha := range []string{"sha-1", "sha-2"} {
		runEscalationSweep(context.Background(), cfg, w, actionableWith(redPR("widgets", 7, "hive-agent", sha)), nil, nil, discardLogger())
	}
	if comments, adds := w.written(); comments != 1 || adds != 1 {
		t.Fatalf("setup: want one escalation comment and one label add, got %d and %d", comments, adds)
	}
}

// forgeWriterWithLog is a fake writer whose writes a test can count.
type forgeWriterWithLog interface {
	labelRemover
	CreateIssueComment(ctx context.Context, repo string, number int, body string) error
	AddLabels(ctx context.Context, repo string, number int, labels []string) error
	written() (comments, adds int)
}

func (f *depBotFakeWriter) written() (int, int) { return len(f.comments), len(f.adds) }

// End to end through the sweep: a PR the hive escalated over required-red
// heads gets its needs-human lifted, with one comment, once a later head is
// green on every required check and red only on the advisory one, after one
// read confirming the label is the App's. Later passes with the label gone
// read nothing, write nothing and count nothing.
func TestRunEscalationSweep_RequiredGreenLiftsHiveEscalation(t *testing.T) {
	store, clock := newTestEscalationStore(t)
	cfg := requiredGreenConfig()
	w := &provenanceWriter{byApp: true}
	ctx := context.Background()
	prKey := escalation.Key("acme/widgets", 7)
	escalateWidgets(t, cfg, w)

	repaired := requiredGreenPR("sha-3")
	repaired.Labels = []string{escalation.NeedsHumanLabel}
	got := runEscalationSweep(ctx, cfg, w, actionableWith(repaired), nil, nil, discardLogger())
	if got[prKey] {
		t.Fatalf("escalated = %v, want the PR out of the escalated set", got)
	}
	if w.reads != 1 {
		t.Fatalf("label provenance read %d times, want 1", w.reads)
	}
	if want := prKey + ": " + escalation.NeedsHumanLabel; len(w.removes) != 1 || w.removes[0] != want {
		t.Fatalf("removes = %v, want [%q]", w.removes, want)
	}
	if len(w.comments) != 2 || !strings.Contains(w.comments[1], escalation.RequiredGreenUnparkReason) {
		t.Fatalf("comments = %v, want the un-park comment with the required-green reason", w.comments)
	}

	// The label is gone; the advisory check stays red across new passes.
	repaired.Labels = nil
	for i := 0; i < 4; i++ {
		*clock = clock.Add(escalation.LabelUnparkGrace + escalation.RedPRStaleAfter)
		recordRedStaleness(cfg, actionableWith(repaired))
		if got := runEscalationSweep(ctx, cfg, w, actionableWith(repaired), nil, nil, discardLogger()); len(got) != 0 {
			t.Fatalf("pass %d: escalated = %v, want none", i, got)
		}
	}
	if len(w.comments) != 2 || len(w.adds) != 1 || len(w.removes) != 1 || w.reads != 1 {
		t.Fatalf("later passes wrote or read: comments=%v adds=%v removes=%v reads=%d", w.comments, w.adds, w.removes, w.reads)
	}
	if n := store.Attempts("acme/widgets", 7); n != 0 {
		t.Errorf("advisory-only passes counted %d attempts", n)
	}
}

// A needs-human a person put back after the hive's (the newest labeled event
// is not the App's) is never lifted, and the ledger stops asking.
func TestRunEscalationSweep_RequiredGreenLeavesRelabeledPR(t *testing.T) {
	newTestEscalationStore(t)
	cfg := requiredGreenConfig()
	w := &provenanceWriter{byApp: false}
	escalateWidgets(t, cfg, w)

	pr := requiredGreenPR("sha-3")
	pr.Labels = []string{escalation.NeedsHumanLabel}
	for i := 0; i < 3; i++ {
		got := runEscalationSweep(context.Background(), cfg, w, actionableWith(pr), nil, nil, discardLogger())
		if !got[escalation.Key("acme/widgets", 7)] {
			t.Fatalf("pass %d: escalated = %v, want the person's needs-human kept", i, got)
		}
	}
	if len(w.removes) != 0 || len(w.comments) != 1 {
		t.Fatalf("a re-labeled PR was un-parked: comments=%v removes=%v", w.comments, w.removes)
	}
	if w.reads != 1 {
		t.Fatalf("label provenance read %d times, want 1 (disowned after the first)", w.reads)
	}
}

// A failed provenance read, or a writer that cannot read label events at
// all, fails closed: no lift on that pass. The failed read is retried.
func TestRunEscalationSweep_RequiredGreenFailsClosedOnProvenance(t *testing.T) {
	newTestEscalationStore(t)
	cfg := requiredGreenConfig()
	w := &provenanceWriter{byApp: true, err: errors.New("events: 502")}
	escalateWidgets(t, cfg, w)

	pr := requiredGreenPR("sha-3")
	pr.Labels = []string{escalation.NeedsHumanLabel}
	for i := 0; i < 2; i++ {
		runEscalationSweep(context.Background(), cfg, w, actionableWith(pr), nil, nil, discardLogger())
	}
	if len(w.removes) != 0 || w.reads != 2 {
		t.Fatalf("read errors: removes=%v reads=%d, want no removal and a read each pass", w.removes, w.reads)
	}
	// The read recovers: the lift fires.
	w.err = nil
	runEscalationSweep(context.Background(), cfg, w, actionableWith(pr), nil, nil, discardLogger())
	if len(w.removes) != 1 {
		t.Fatalf("removes = %v after the read recovered, want one", w.removes)
	}

	// A writer with no label-event reader never lifts.
	newTestEscalationStore(t)
	plain := &depBotFakeWriter{}
	cfg = requiredGreenConfig()
	escalateWidgets(t, cfg, plain)
	runEscalationSweep(context.Background(), cfg, plain, actionableWith(pr), nil, nil, discardLogger())
	if len(plain.removes) != 0 {
		t.Fatalf("a writer without a label-event reader lifted: removes=%v", plain.removes)
	}
}

// A needs-human a person applied is never lifted by a required-green head,
// and its provenance is never read.
func TestRunEscalationSweep_RequiredGreenLeavesPersonsLabel(t *testing.T) {
	newTestEscalationStore(t)
	cfg := requiredGreenConfig()
	w := &provenanceWriter{byApp: true}
	pr := requiredGreenPR("sha-1")
	pr.Labels = []string{escalation.NeedsHumanLabel}

	for i := 0; i < 3; i++ {
		got := runEscalationSweep(context.Background(), cfg, w, actionableWith(pr), nil, nil, discardLogger())
		if !got[escalation.Key("acme/widgets", 7)] {
			t.Fatalf("pass %d: escalated = %v, want the person's needs-human kept", i, got)
		}
	}
	if len(w.removes) != 0 || len(w.comments) != 0 || len(w.adds) != 0 || w.reads != 0 {
		t.Fatalf("a person's needs-human was touched: comments=%v adds=%v removes=%v reads=%d", w.comments, w.adds, w.removes, w.reads)
	}
}

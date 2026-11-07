package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
)

type fakeTransientRerunner struct {
	calls []github.PullRequest
	out   github.TransientCIRerunOutcome
	err   error
}

func (f *fakeTransientRerunner) RerunTransientCIFailures(_ context.Context, pr github.PullRequest) (github.TransientCIRerunOutcome, error) {
	f.calls = append(f.calls, pr)
	return f.out, f.err
}

func transientRedPR() github.PullRequest {
	pr := redPR("widgets", 7, "hive-agent", "abc")
	pr.TransientCITriaged = true
	return pr
}

// projectbluefin/common#1647: an agent said "infrastructure, not this change"
// four times, yet the fix loop delivered six fix re-dispatches on the same
// head and escalated the PR. A head whose every failing run carries a
// hive-ci-transient marker must draw no fix kick and spend no budget, from
// the reaper or the merge watcher's hook.
func TestTransientTriagedHeadDrawsNoFixKick(t *testing.T) {
	store, clock := newTestEscalationStore(t)
	cfg := escalationTestConfig()
	actionable := actionableWith(transientRedPR())
	recordRedStaleness(cfg, actionable)
	*clock = clock.Add(escalation.RedPRStaleAfter + time.Minute)
	kicker := &fakeKicker{}

	for i := 0; i < escalation.MaxReEngagements+2; i++ {
		reapStuckRedPRs(cfg, actionable, map[string]bool{}, kicker, nil, discardLogger())
		*clock = clock.Add(escalation.MaxReEngageCooldown + time.Minute)
	}
	if hook := mergeReEngageHook(cfg, kicker, nil, discardLogger()); hook("widgets", 7) {
		t.Error("merge watcher re-engaged a transient-triaged head")
	}
	if len(kicker.kicks) != 0 {
		t.Fatalf("transient-triaged head got %d fix kicks, want 0", len(kicker.kicks))
	}
	if got := store.ReEngagements("acme/widgets", 7); got != 0 {
		t.Fatalf("transient-triaged head spent %d re-engagements, want 0", got)
	}
}

// The hive re-runs the failed jobs once per head; a head still red after
// that re-run (new run ids, so the markers are stale) is back in the
// ordinary fix loop.
func TestRerunTransientTriagedPRs_OncePerHeadThenFixLoopResumes(t *testing.T) {
	store, clock := newTestEscalationStore(t)
	cfg := escalationTestConfig()
	pr := transientRedPR()
	actionable := actionableWith(pr)
	recordRedStaleness(cfg, actionable)
	rerunner := &fakeTransientRerunner{out: github.TransientCIRerunOutcome{Settled: true, RunIDs: []int64{42}}}
	var audited []string
	audit := func(repo string, number int, detail string) { audited = append(audited, detail) }

	rerunTransientTriagedPRs(context.Background(), cfg, rerunner, actionable, map[string]bool{}, audit, discardLogger())
	rerunTransientTriagedPRs(context.Background(), cfg, rerunner, actionable, map[string]bool{}, audit, discardLogger())
	if len(rerunner.calls) != 1 {
		t.Fatalf("re-run calls = %d, want exactly one per head", len(rerunner.calls))
	}
	if len(audited) != 1 || !strings.Contains(audited[0], "runs=1") {
		t.Fatalf("audit = %v, want one line naming the re-run", audited)
	}
	if store.TransientRerunOwed("acme/widgets", 7, "abc") {
		t.Fatal("re-run still owed after it was spent")
	}

	// Red again after the re-run: the re-run gave the checks new run ids,
	// so the markers no longer cover the head.
	pr.TransientCITriaged = false
	actionable = actionableWith(pr)
	recordRedStaleness(cfg, actionable)
	*clock = clock.Add(escalation.RedPRStaleAfter + time.Minute)
	kicker := &fakeKicker{}
	reapStuckRedPRs(cfg, actionable, map[string]bool{}, kicker, nil, discardLogger())
	if len(kicker.kicks) != 1 {
		t.Fatalf("after the one re-run the fix loop must resume, got %d kicks", len(kicker.kicks))
	}

	// A new head with a fresh transient triage earns its own re-run.
	pr.HeadSHA, pr.TransientCITriaged = "def", true
	actionable = actionableWith(pr)
	recordRedStaleness(cfg, actionable)
	rerunTransientTriagedPRs(context.Background(), cfg, rerunner, actionable, map[string]bool{}, nil, discardLogger())
	if len(rerunner.calls) != 2 || rerunner.calls[1].HeadSHA != "def" {
		t.Fatalf("new head was not re-run: %+v", rerunner.calls)
	}
}

func TestRerunTransientTriagedPRs_Skips(t *testing.T) {
	t.Run("escalated PR is a person's", func(t *testing.T) {
		newTestEscalationStore(t)
		cfg := escalationTestConfig()
		pr := transientRedPR()
		pr.Labels = []string{escalation.NeedsHumanLabel}
		actionable := actionableWith(pr)
		recordRedStaleness(cfg, actionable)
		rerunner := &fakeTransientRerunner{out: github.TransientCIRerunOutcome{Settled: true}}
		rerunTransientTriagedPRs(context.Background(), cfg, rerunner, actionable, map[string]bool{"acme/widgets#7": true}, nil, discardLogger())
		if len(rerunner.calls) != 0 {
			t.Fatal("re-ran an escalated PR")
		}
	})
	t.Run("human-authored PR", func(t *testing.T) {
		newTestEscalationStore(t)
		cfg := escalationTestConfig()
		pr := transientRedPR()
		pr.Author = "someone"
		actionable := actionableWith(pr)
		recordRedStaleness(cfg, actionable)
		rerunner := &fakeTransientRerunner{out: github.TransientCIRerunOutcome{Settled: true}}
		rerunTransientTriagedPRs(context.Background(), cfg, rerunner, actionable, map[string]bool{}, nil, discardLogger())
		if len(rerunner.calls) != 0 {
			t.Fatal("re-ran a human-authored PR")
		}
	})
	t.Run("untriaged red PR", func(t *testing.T) {
		newTestEscalationStore(t)
		cfg := escalationTestConfig()
		actionable := actionableWith(redPR("widgets", 7, "hive-agent", "abc"))
		recordRedStaleness(cfg, actionable)
		rerunner := &fakeTransientRerunner{out: github.TransientCIRerunOutcome{Settled: true}}
		rerunTransientTriagedPRs(context.Background(), cfg, rerunner, actionable, map[string]bool{}, nil, discardLogger())
		if len(rerunner.calls) != 0 {
			t.Fatal("re-ran a PR nobody triaged as transient")
		}
	})
	t.Run("listing error is retried next pass", func(t *testing.T) {
		store, _ := newTestEscalationStore(t)
		cfg := escalationTestConfig()
		actionable := actionableWith(transientRedPR())
		recordRedStaleness(cfg, actionable)
		rerunner := &fakeTransientRerunner{err: errors.New("502")}
		rerunTransientTriagedPRs(context.Background(), cfg, rerunner, actionable, map[string]bool{}, nil, discardLogger())
		if !store.TransientRerunOwed("acme/widgets", 7, "abc") {
			t.Fatal("an API error spent the head's re-run")
		}
		rerunner.err, rerunner.out = nil, github.TransientCIRerunOutcome{Settled: true, Skipped: "repo-mode-off"}
		rerunTransientTriagedPRs(context.Background(), cfg, rerunner, actionable, map[string]bool{}, nil, discardLogger())
		if len(rerunner.calls) != 2 || store.TransientRerunOwed("acme/widgets", 7, "abc") {
			t.Fatalf("a settled refusal must spend the re-run: calls=%d", len(rerunner.calls))
		}
	})
}

// The fix kick must tell the agent how to make an infra verdict visible to
// the hive (the transient marker) and forbid a retrigger push; on
// projectbluefin/common#1647 the prose-only verdict was invisible and a lane
// pushed an empty "retrigger CI" commit.
func TestRedPRFixKickInfraVerdictUsesTransientMarker(t *testing.T) {
	got := redPRFixKick(escalation.Observation{Repo: "projectbluefin/common", Number: 1647, HeadSHA: "6772623"}, false)
	for _, want := range []string{
		"do NOT push anything",
		"\"retrigger CI\" commit",
		"transient_marker",
		"hive-baseline-check.sh projectbluefin/common '<failing check>' 1647 --json",
		"Hive\nre-runs the failed jobs itself, once.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("fix kick missing %q in:\n%s", want, got)
		}
	}
}

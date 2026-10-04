package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/github"
)

// escalationFixture is a ledger on a test clock plus a gate whose timeline
// reads and escalations are recorded instead of hitting GitHub.
type escalationFixture struct {
	t         *testing.T
	ledger    *claims.Ledger
	now       time.Time
	gate      *claimEscalationGate
	timeline  []github.IssueEvent // returned for every read, filtered by since
	readErr   error
	reads     int
	escalated []claims.Stall
}

func newEscalationFixture(t *testing.T) *escalationFixture {
	t.Helper()
	f := &escalationFixture{t: t, now: time.Date(2026, 10, 4, 15, 50, 0, 0, time.UTC)}
	l, err := claims.New(filepath.Join(t.TempDir(), "claims.json"), claims.DefaultPolicy(), claims.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	l.SetNow(func() time.Time { return f.now })
	f.ledger = l
	f.gate = &claimEscalationGate{
		ctx: context.Background(), ledger: l, org: "myorg", threshold: 2,
		events: func(_ context.Context, repo string, issue int, since time.Time) ([]github.IssueEvent, error) {
			f.reads++
			if repo != "myorg/repo" || issue != 10512 {
				t.Fatalf("timeline read for %s#%d", repo, issue)
			}
			if f.readErr != nil {
				return nil, f.readErr
			}
			var out []github.IssueEvent
			for _, e := range f.timeline {
				if e.At.After(since) {
					out = append(out, e)
				}
			}
			return out, nil
		},
		escalate: func(s claims.Stall) { f.escalated = append(f.escalated, s) },
		now:      func() time.Time { return f.now },
	}
	return f
}

// kickClaim records the claim an agent kick naming the issue makes, then
// lets it lapse with nothing done — the #10512 pattern.
func (f *escalationFixture) kickClaim(agent string) {
	f.t.Helper()
	res, err := f.ledger.Claim(claims.Request{Repo: "myorg/repo", Issue: 10512, Holder: agent, HolderID: agent, Kind: claims.KindAgent})
	if err != nil || res.Outcome != claims.OutcomeClaimed {
		f.t.Fatalf("claim by %s: outcome=%s err=%v", agent, res.Outcome, err)
	}
	f.now = f.now.Add(claims.DefaultAgentTTL + 7*time.Minute)
}

func (f *escalationFixture) event(e github.IssueEvent) {
	e.At = f.now.Add(-time.Minute)
	f.timeline = append(f.timeline, e)
}

// lookup asks the gate about the issue the way the scheduler does, with the
// bare repo name a default config's enumeration carries.
func (f *escalationFixture) lookup() (string, bool) {
	return f.gate.lookup(github.Issue{Repo: "repo", Number: 10512})
}

func TestClaimEscalationRefusesThirdNoProgressClaim(t *testing.T) {
	f := newEscalationFixture(t)
	f.kickClaim("scanner")
	if _, held := f.lookup(); held || f.reads != 0 {
		t.Fatalf("one prior claim: held=%v reads=%d, want free without a timeline read", held, f.reads)
	}
	f.kickClaim("scanner")
	// The claim machinery's own label churn and comments are not progress.
	f.event(github.IssueEvent{Event: "unlabeled", Label: claims.LabelClaimed})
	f.event(github.IssueEvent{Event: "commented"})

	holder, held := f.lookup()
	if !held || !strings.Contains(holder, "needs-human") || !strings.Contains(holder, "scanner") {
		t.Fatalf("third no-progress claim: held=%v holder=%q, want withheld and escalated", held, holder)
	}
	if len(f.escalated) != 1 || f.escalated[0].Holder != "scanner" || len(f.escalated[0].Attempts) != 2 {
		t.Fatalf("escalations=%+v, want one for scanner's two claims", f.escalated)
	}

	// The scheduler asks several times per cycle, and the stale enumeration
	// keeps offering the issue until needs-human shows up in it: one
	// escalation, one timeline read per recheck window.
	f.event(github.IssueEvent{Event: "labeled", Label: "needs-human"})
	for range 3 {
		if _, held := f.lookup(); !held {
			t.Fatal("escalated issue offered again")
		}
	}
	f.now = f.now.Add(claimProgressRecheck)
	if _, held := f.lookup(); !held {
		t.Fatal("the gate's own needs-human label counted as progress")
	}
	if len(f.escalated) != 1 || f.reads != 2 {
		t.Fatalf("escalations=%d reads=%d, want 1 escalation and 2 reads", len(f.escalated), f.reads)
	}
	if _, ok := f.ledger.Lookup("myorg/repo", 10512); ok {
		t.Fatal("refused claim left a live claim behind")
	}

	// A person removing needs-human is progress: the next claim goes ahead.
	f.now = f.now.Add(claimProgressRecheck)
	f.event(github.IssueEvent{Event: "unlabeled", Label: "needs-human"})
	if _, held := f.lookup(); held {
		t.Fatal("issue still withheld after needs-human was removed")
	}
}

func TestClaimEscalationAllowsClaimsWithAPullRequestBetween(t *testing.T) {
	f := newEscalationFixture(t)
	f.kickClaim("scanner")
	f.event(github.IssueEvent{Event: "cross-referenced", SourcePR: 10530})
	f.kickClaim("scanner")

	if holder, held := f.lookup(); held {
		t.Fatalf("claims with a PR between withheld: %q", holder)
	}
	if len(f.escalated) != 0 {
		t.Fatalf("escalated despite a PR: %+v", f.escalated)
	}
	// The verdict holds for the run: no second read.
	if _, held := f.lookup(); held || f.reads != 1 {
		t.Fatalf("held=%v reads=%d, want free on one read", held, f.reads)
	}

	// Two more claims with nothing between them form a new run, which
	// escalates: the PR before it does not excuse it forever.
	f.kickClaim("scanner")
	f.kickClaim("scanner")
	if _, held := f.lookup(); !held || len(f.escalated) != 1 {
		t.Fatalf("held=%v escalations=%d, want the new no-progress run escalated", held, len(f.escalated))
	}
}

func TestClaimEscalationSkipsWhileClaimedAndAcrossAgents(t *testing.T) {
	f := newEscalationFixture(t)
	f.kickClaim("ci-maintainer")
	f.kickClaim("scanner")
	if _, held := f.lookup(); held || f.reads != 0 {
		t.Fatalf("one claim each by two agents: held=%v reads=%d", held, f.reads)
	}
	if _, err := f.ledger.Claim(claims.Request{Repo: "myorg/repo", Issue: 10512, Holder: "scanner", Kind: claims.KindAgent}); err != nil {
		t.Fatal(err)
	}
	if _, held := f.lookup(); held || f.reads != 0 {
		t.Fatalf("live claim: held=%v reads=%d — the claims lookup owns a held issue", held, f.reads)
	}
}

func TestClaimEscalationFailsOpenOnTimelineError(t *testing.T) {
	f := newEscalationFixture(t)
	f.kickClaim("scanner")
	f.kickClaim("scanner")
	f.readErr = errors.New("502 bad gateway")
	if _, held := f.lookup(); held || len(f.escalated) != 0 {
		t.Fatalf("timeline error: held=%v escalations=%d, want the claim allowed", held, len(f.escalated))
	}
	if _, held := f.lookup(); held || f.reads != 1 {
		t.Fatalf("held=%v reads=%d, want the failed read reused within the recheck window", held, f.reads)
	}
	f.readErr = nil
	f.now = f.now.Add(claimProgressRecheck)
	if _, held := f.lookup(); !held || len(f.escalated) != 1 {
		t.Fatalf("after recovery: held=%v escalations=%d, want escalated", held, len(f.escalated))
	}
}

func TestClaimProgress(t *testing.T) {
	cases := []struct {
		name  string
		event github.IssueEvent
		moved bool
	}{
		{"linked pr", github.IssueEvent{Event: "cross-referenced", SourcePR: 7}, true},
		{"issue cross-reference", github.IssueEvent{Event: "cross-referenced"}, false},
		{"development link", github.IssueEvent{Event: "connected"}, true},
		{"commit", github.IssueEvent{Event: "referenced", CommitID: "abc"}, true},
		{"assignee", github.IssueEvent{Event: "assigned"}, true},
		{"closed", github.IssueEvent{Event: "closed"}, true},
		{"other label", github.IssueEvent{Event: "labeled", Label: "priority/high"}, true},
		{"claimed label", github.IssueEvent{Event: "labeled", Label: "claimed"}, false},
		{"preempted label", github.IssueEvent{Event: "unlabeled", Label: "preempted:alice"}, false},
		{"needs-human added", github.IssueEvent{Event: "labeled", Label: "Needs-Human"}, false},
		{"needs-human removed", github.IssueEvent{Event: "unlabeled", Label: "needs-human"}, true},
		{"comment", github.IssueEvent{Event: "commented"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, moved := claimProgress([]github.IssueEvent{tc.event}); moved != tc.moved {
				t.Fatalf("claimProgress(%+v) moved=%v, want %v", tc.event, moved, tc.moved)
			}
		})
	}
}

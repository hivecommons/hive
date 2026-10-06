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

// verifyOnceFixture is a ledger on a test clock plus a verify-once gate whose
// timeline reads are served from memory.
type verifyOnceFixture struct {
	t        *testing.T
	ledger   *claims.Ledger
	now      time.Time
	mergedAt time.Time
	gate     *verifyOnceGate
	timeline []github.IssueEvent
	readErr  error
	reads    int
}

func newVerifyOnceFixture(t *testing.T) *verifyOnceFixture {
	t.Helper()
	f := &verifyOnceFixture{t: t, now: time.Date(2026, 10, 4, 22, 30, 0, 0, time.UTC)}
	f.mergedAt = f.now.Add(-2 * time.Hour)
	l, err := claims.New(filepath.Join(t.TempDir(), "claims.json"), claims.DefaultPolicy(), claims.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	l.SetNow(func() time.Time { return f.now })
	f.ledger = l
	f.gate = &verifyOnceGate{
		ctx: context.Background(), ledger: l, org: "myorg",
		events: func(_ context.Context, repo string, issue int, since time.Time) ([]github.IssueEvent, error) {
			f.reads++
			if repo != "myorg/repo" || issue != 10509 {
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
		now: func() time.Time { return f.now },
	}
	return f
}

// verify is one kick listing the issue followed by the agent's verification
// comment (its start signal), after which the claim lapses.
func (f *verifyOnceFixture) verify(agent string) {
	f.t.Helper()
	f.ledger.MarkListed("myorg/repo", 10509, agent)
	if _, started, err := f.ledger.ClaimOnStart("myorg/repo", 10509, agent); !started || err != nil {
		f.t.Fatalf("start signal by %s: started=%v err=%v", agent, started, err)
	}
	f.now = f.now.Add(claims.DefaultAgentTTL + time.Minute)
}

func (f *verifyOnceFixture) event(e github.IssueEvent) {
	e.At = f.now.Add(-time.Minute)
	f.timeline = append(f.timeline, e)
}

func (f *verifyOnceFixture) issue(labels ...string) github.Issue {
	return github.Issue{Repo: "repo", Number: 10509, Labels: labels, ClaimContext: &github.IssueClaimContext{
		PRRepo: "myorg/repo", PRNumber: 10511, MergedPR: true, MergedAt: f.mergedAt, Reference: true,
	}}
}

func (f *verifyOnceFixture) lookup(labels ...string) (string, bool) {
	return f.gate.lookup(f.issue(labels...))
}

func TestVerifyOnceWithholdsAfterTheFirstVerification(t *testing.T) {
	f := newVerifyOnceFixture(t)
	if _, held := f.lookup(github.LikelyDoneLabel); held || f.reads != 0 {
		t.Fatalf("unverified likely-done issue: held=%v reads=%d, want offered once", held, f.reads)
	}
	f.verify("scanner")
	// The hive's own verification and reporter-confirmation comments are the
	// one nudge; they are not a person acting.
	f.event(github.IssueEvent{Event: "commented", Actor: "hivecommons-hive[bot]", ActorIsBot: true})
	f.event(github.IssueEvent{Event: "labeled", Label: github.LikelyDoneLabel, Actor: "hivecommons-hive[bot]", ActorIsBot: true})

	holder, held := f.lookup(github.LikelyDoneLabel)
	if !held || !strings.Contains(holder, "verified once by scanner") || !strings.Contains(holder, "#10511") {
		t.Fatalf("second kick after verification: held=%v holder=%q, want withheld", held, holder)
	}
	for range 3 {
		if _, held := f.lookup(github.LikelyDoneLabel); !held {
			t.Fatal("verified issue offered again")
		}
	}
	if f.reads != 1 {
		t.Fatalf("reads=%d, want the withhold verdict reused within the recheck window", f.reads)
	}
	f.now = f.now.Add(8 * time.Hour)
	if _, held := f.lookup(github.LikelyDoneLabel); !held {
		t.Fatal("verified issue offered again with no one acting")
	}

	// The reporter replying is a person acting: the next kick may look again.
	f.event(github.IssueEvent{Event: "commented", Actor: "clubanderson"})
	f.now = f.now.Add(claimProgressRecheck)
	if _, held := f.lookup(github.LikelyDoneLabel); held {
		t.Fatal("issue still withheld after the reporter replied")
	}
	// ... and that next verification starts a new silence.
	f.verify("scanner")
	if _, held := f.lookup(github.LikelyDoneLabel); !held {
		t.Fatal("second verification not followed by silence")
	}
}

func TestVerifyOnceIgnoresOtherIssues(t *testing.T) {
	f := newVerifyOnceFixture(t)
	f.verify("scanner")
	if _, held := f.lookup(github.VerifiedOpenLabel); held {
		t.Fatal("hive/verified-open issue withheld: its remainder is real work")
	}
	plain := github.Issue{Repo: "repo", Number: 10509}
	if _, held := f.gate.lookup(plain); held {
		t.Fatal("issue without merged-PR context withheld")
	}
	// A claim from before the merge is not a verification of it.
	f.mergedAt = f.now
	if _, held := f.lookup(github.LikelyDoneLabel); held {
		t.Fatal("claim that predates the merge treated as a verification")
	}
	if f.reads != 0 {
		t.Fatalf("reads=%d, want none for issues the gate does not cover", f.reads)
	}
}

func TestVerifyOnceFailsOpenOnTimelineError(t *testing.T) {
	f := newVerifyOnceFixture(t)
	f.verify("scanner")
	f.readErr = errors.New("502 bad gateway")
	if _, held := f.lookup(github.LikelyDoneLabel); held {
		t.Fatal("timeline error withheld the issue")
	}
	f.readErr = nil
	f.now = f.now.Add(claimProgressRecheck)
	if _, held := f.lookup(github.LikelyDoneLabel); !held {
		t.Fatal("after recovery the verified issue was not withheld")
	}
}

func TestPersonActed(t *testing.T) {
	cases := []struct {
		name  string
		event github.IssueEvent
		acted bool
	}{
		{"reporter comment", github.IssueEvent{Event: "commented", Actor: "alice"}, true},
		{"maintainer label", github.IssueEvent{Event: "labeled", Label: "hive: reporter-confirmed", Actor: "bob"}, true},
		{"maintainer close", github.IssueEvent{Event: "closed", Actor: "bob"}, true},
		{"hive comment", github.IssueEvent{Event: "commented", Actor: "hivecommons-hive[bot]", ActorIsBot: true}, false},
		{"actorless", github.IssueEvent{Event: "commented"}, false},
		{"cross-reference", github.IssueEvent{Event: "cross-referenced", Actor: "alice", SourcePR: 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, acted := personActed([]github.IssueEvent{tc.event}); acted != tc.acted {
				t.Fatalf("personActed(%+v)=%v, want %v", tc.event, acted, tc.acted)
			}
		})
	}
}

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/forgestate"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/scheduler"
)

func freshKickMsgs(refs ...[]string) []scheduler.KickMessage {
	out := make([]scheduler.KickMessage, 0, len(refs))
	for i, r := range refs {
		out = append(out, scheduler.KickMessage{Agent: fmt.Sprintf("agent-%d", i), IssueRefs: r})
	}
	return out
}

// A stale repo's refs are withheld; a message whose refs all failed is
// dropped; a message with no refs passes through; the guard is consulted
// through the PR endpoint for refs the snapshot classified as PRs.
func TestFreshenKickMessages_WithholdsStaleAndClosed(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	actionable := &github.ActionableResult{
		PRs: github.PRResult{Items: []github.PullRequest{{Repo: "acme/web", Number: 9}}},
	}
	var prChecks, issueChecks atomic.Int64
	guard := github.IrreversibleGuard(func(_ context.Context, repo string, number int, isPR bool) error {
		if isPR {
			prChecks.Add(1)
		} else {
			issueChecks.Add(1)
		}
		switch {
		case repo == "acme/stale":
			return fmt.Errorf("%s#%d: %w", repo, number, github.ErrSnapshotStaleForDecision)
		case number == 5:
			return fmt.Errorf("%s#%d: %w", repo, number, github.ErrItemNotOpen)
		}
		return nil
	})
	in := freshKickMsgs(
		[]string{"acme/web#1", "acme/web#5", "acme/web#9"},
		[]string{"acme/stale#2", "acme/stale#3"},
		nil,
		[]string{"acme/web!ENG-7"},
	)
	out := freshenKickMessages(context.Background(), guard, actionable, in, logger)
	if len(out) != 3 {
		t.Fatalf("got %d messages, want 3 (stale-only message withheld): %+v", len(out), out)
	}
	if got := out[0].IssueRefs; len(got) != 2 || got[0] != "acme/web#1" || got[1] != "acme/web#9" {
		t.Fatalf("message 0 refs = %v, want closed #5 dropped", got)
	}
	if out[1].Agent != "agent-2" || len(out[1].IssueRefs) != 0 {
		t.Fatalf("ref-less message not passed through: %+v", out[1])
	}
	if out[2].Agent != "agent-3" || out[2].IssueRefs[0] != "acme/web!ENG-7" {
		t.Fatalf("external ref not passed through: %+v", out[2])
	}
	if prChecks.Load() != 1 || issueChecks.Load() != 4 {
		t.Fatalf("guard calls = %d pr / %d issue, want 1 / 4", prChecks.Load(), issueChecks.Load())
	}
}

// A refusal from the API budget or rate-limit caution window is the designed
// back-off, not a fault: it is withheld but logged below WARN so the fleet
// self-report feed does not file it as an unclassified repeated error
// (#15465). Any other refusal still logs at WARN.
func TestFreshenKickMessages_DeferredReadLogsBelowWarn(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantWarn bool
	}{
		{"budget critical", github.ErrAPIBudgetCritical, false},
		{"caution window", github.ErrRateLimitCaution, false},
		{"stale snapshot", github.ErrSnapshotStaleForDecision, true},
		{"item closed", github.ErrItemNotOpen, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
			guard := github.IrreversibleGuard(func(_ context.Context, repo string, number int, _ bool) error {
				return fmt.Errorf("confirming %s#%d before irreversible action: %w", repo, number, tc.err)
			})
			out := freshenKickMessages(context.Background(), guard, nil, freshKickMsgs([]string{"acme/web#1"}), logger)
			if len(out) != 0 {
				t.Fatalf("message not withheld: %+v", out)
			}
			log := buf.String()
			if !strings.Contains(log, "kick ref withheld") {
				t.Fatalf("withheld ref not logged: %q", log)
			}
			if got := strings.Contains(log, "level=WARN"); got != tc.wantWarn {
				t.Fatalf("WARN logged = %v, want %v: %q", got, tc.wantWarn, log)
			}
		})
	}
}

// Without a guard (no snapshot to bound against) the messages are untouched.
func TestFreshenKickMessages_NilGuardPassesThrough(t *testing.T) {
	in := freshKickMsgs([]string{"acme/web#1"})
	if out := freshenKickMessages(context.Background(), nil, nil, in, nil); len(out) != 1 || out[0].IssueRefs[0] != "acme/web#1" {
		t.Fatalf("nil guard altered messages: %+v", out)
	}
}

// The real guard built from a snapshot: a fresh copy costs no call, a stale
// repo refuses, and an old copy is confirmed with one GET.
func TestFreshenKickMessages_SnapshotGuard(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Now()
	actionable := &github.ActionableResult{
		GeneratedAt: now,
		StaleRepos:  []string{"acme/stale"},
		ItemPatches: map[string]github.ItemPatchMeta{
			github.ItemPatchKey("acme/web", 1, false): {FetchedAt: now},
		},
	}
	var c *github.Client
	guard := c.SnapshotFreshnessGuard(actionable, func() time.Time { return now })
	if guard != nil {
		t.Fatal("nil client built a guard")
	}
	out := freshenKickMessages(context.Background(), guard, actionable, freshKickMsgs([]string{"acme/stale#2"}), logger)
	if len(out) != 1 {
		t.Fatalf("nil guard must not withhold: %+v", out)
	}
}

// The startup gate: no store returns at once; a store that becomes ready
// releases the gate; a cancelled context releases it with ctx.Err().
func TestWaitForgeStateReady(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := waitForgeStateReady(context.Background(), nil, logger, time.Millisecond); err != nil {
		t.Fatalf("nil store = %v, want nil", err)
	}

	fetcher := &gateFetcher{}
	store, err := forgestate.New(forgestate.Options{Repos: []string{"acme/web"}, Fetcher: fetcher, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := waitForgeStateReady(ctx, store, logger, 5*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("gate before the first reconcile = %v, want ctx deadline", err)
	}

	if err := store.Reconcile(context.Background(), "acme/web"); err != nil {
		t.Fatal(err)
	}
	if err := waitForgeStateReady(context.Background(), store, logger, time.Millisecond); err != nil {
		t.Fatalf("gate after the first reconcile = %v, want nil", err)
	}
}

type gateFetcher struct{}

func (gateFetcher) List(context.Context, string, string) (forgestate.ListResult, error) {
	return forgestate.ListResult{}, nil
}
func (gateFetcher) Issue(context.Context, string, int) (forgestate.Item, error) {
	return forgestate.Item{}, forgestate.ErrNotFound
}
func (gateFetcher) PR(context.Context, string, int) (forgestate.Item, error) {
	return forgestate.Item{}, forgestate.ErrNotFound
}

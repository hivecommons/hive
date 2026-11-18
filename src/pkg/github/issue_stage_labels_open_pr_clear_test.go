package github

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/bluefin-bling#82 and projectbluefin/common#1656: the
// open-PR guard put `has-open-pr` on an issue it refused to dispatch, the
// linking PR then closed or merged, and the label stayed for good next to
// stage/010 or stage/045, still saying an open PR links the issue. The
// issue sweep now takes it off in the stage write once its own open-PR
// listing shows no PR links the issue.
func TestSyncIssueStageLabels_HasOpenPRClearedOnlyOnEvidence(t *testing.T) {
	const n = 82
	issue := func() *ActionableResult {
		result := &ActionableResult{}
		result.Issues = IssueResultFromItems([]Issue{{
			Repo: "acme/widgets", Number: n, Author: "alice", AuthorIsHuman: true,
			Labels: []string{"kind/bug", HasOpenPRLabel, stagelabels.Actionable.Label()},
		}})
		return result
	}
	openPRRemovals := func(fake *issueStageServer) int {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		count := 0
		for _, l := range fake.removed[n] {
			if strings.EqualFold(l, HasOpenPRLabel) {
				count++
			}
		}
		return count
	}
	linking := []map[string]any{openPRFixture(128, "Fixes #82", "mendezr", stageNow().Add(-24*time.Hour))}

	t.Run("no PR links the issue: cleared, silently", func(t *testing.T) {
		fake := newIssueStageServer(t)
		c := issueStageTestClient(t, fake.srv, true)
		store := stagelabels.NewStore("", stageNow)
		syncSeeded(t, fake, c, issue(), store, stageNow())
		if got := openPRRemovals(fake); got != 1 {
			t.Fatalf("%s removals = %d, want 1: removed=%v", HasOpenPRLabel, got, fake.removed[n])
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if slices.Contains(fake.removed[n], "kind/bug") || len(fake.added[n]) != 0 {
			t.Fatalf("only the stale label may change: added=%v removed=%v", fake.added[n], fake.removed[n])
		}
		if fake.comments[n] != 0 {
			t.Fatalf("the label is silent: %d comments posted", fake.comments[n])
		}
		if rec, _ := store.Get(stagelabels.Key("acme/widgets", n)); rec.OpenPRLabelClearedAt.IsZero() {
			t.Fatalf("removal must be booked on the record: %+v", rec)
		}
	})

	t.Run("an open PR still links it: kept", func(t *testing.T) {
		fake := newIssueStageServer(t)
		fake.openPRs = linking
		c := issueStageTestClient(t, fake.srv, true)
		syncSeeded(t, fake, c, issue(), stagelabels.NewStore("", stageNow), stageNow())
		if got := openPRRemovals(fake); got != 0 {
			t.Fatalf("label removed while PR #128 still links the issue: removed=%v", fake.removed[n])
		}
	})

	t.Run("listing fails: kept", func(t *testing.T) {
		fake := newIssueStageServer(t)
		fake.openPRsFail = true
		c := issueStageTestClient(t, fake.srv, true)
		syncSeeded(t, fake, c, issue(), stagelabels.NewStore("", stageNow), stageNow())
		if got := openPRRemovals(fake); got != 0 {
			t.Fatalf("a failed listing must never remove the label: removed=%v", fake.removed[n])
		}
	})

	t.Run("guard off: kept, no listing", func(t *testing.T) {
		fake := newIssueStageServer(t)
		c := issueStageTestClient(t, fake.srv, true)
		c.SetSkipIssuesWithOpenPR(false)
		syncSeeded(t, fake, c, issue(), stagelabels.NewStore("", stageNow), stageNow())
		if got := openPRRemovals(fake); got != 0 {
			t.Fatalf("scanner.skip_issues_with_open_pr: false must leave the label: removed=%v", fake.removed[n])
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.pullsCalls != 0 {
			t.Fatalf("open-PR listings = %d, want 0 with the guard off", fake.pullsCalls)
		}
	})

	t.Run("re-added with no PR: removed once, then yielded", func(t *testing.T) {
		fake := newIssueStageServer(t)
		c := issueStageTestClient(t, fake.srv, true)
		store := stagelabels.NewStore("", stageNow)
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, nil))
		pass := func() {
			t.Helper()
			result := issue()
			fake.seed(result)
			ResetRateLimitPacingForTest()
			resetRESTAccountingForTest(t, stageNow())
			c.InvalidateOpenPRLinks("")
			SyncIssueStageLabels(context.Background(), c, result, store, stageNow(), logger)
		}
		key := stagelabels.Key("acme/widgets", n)
		pass()
		pass() // another writer put it back: yield
		pass() // still yielded: no removal, no second log
		if got := openPRRemovals(fake); got != 1 {
			t.Fatalf("%s removals = %d over three passes, want exactly 1: removed=%v", HasOpenPRLabel, got, fake.removed[n])
		}
		if rec, _ := store.Get(key); !rec.OpenPRLabelClearYielded {
			t.Fatalf("label back with no PR must book the yield: %+v", rec)
		}
		if got := strings.Count(logBuf.String(), "came back after the hive removed it"); got != 1 {
			t.Fatalf("yield logged %d times, want once:\n%s", got, logBuf.String())
		}

		// A new PR links the issue (the guard re-adds the label for it):
		// the label is accurate again, so the once-only memory starts over
		// and the label comes off once more after that PR closes.
		fake.mu.Lock()
		fake.openPRs = linking
		fake.mu.Unlock()
		pass()
		if rec, _ := store.Get(key); rec.OpenPRLabelClearYielded || !rec.OpenPRLabelClearedAt.IsZero() {
			t.Fatalf("a linking PR beside the label must reset the once-only state: %+v", rec)
		}
		fake.mu.Lock()
		fake.openPRs = nil
		fake.mu.Unlock()
		pass()
		if got := openPRRemovals(fake); got != 2 {
			t.Fatalf("after the new PR closed the label must come off once more: %d removals, removed=%v", got, fake.removed[n])
		}
	})
}

// The once-only rule on its own, mirroring triageClearAllowed.
func TestOpenPRLabelClearAllowed(t *testing.T) {
	cases := []struct {
		name           string
		rec            stagelabels.Record
		remove, yieldN bool
	}{
		{"never removed", stagelabels.Record{}, true, false},
		{"removed once and back", stagelabels.Record{OpenPRLabelClearedAt: stageNow()}, false, true},
		{"already yielded", stagelabels.Record{OpenPRLabelClearedAt: stageNow(), OpenPRLabelClearYielded: true}, false, false},
	}
	for _, tc := range cases {
		if remove, yieldNow := openPRLabelClearAllowed(tc.rec); remove != tc.remove || yieldNow != tc.yieldN {
			t.Errorf("%s: remove=%v yieldNow=%v, want %v/%v", tc.name, remove, yieldNow, tc.remove, tc.yieldN)
		}
	}
}

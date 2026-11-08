package github

import (
	"context"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// The un-park sweep reuses an item's comment and label-timeline reads while
// the item's updated_at in the issue list has not moved
// (issue_unpark_read_cache.go). On the knuckle hive these per-item reads
// were the largest charged consumer: 878 comment and 363 timeline reads
// charged in 50 minutes, nearly all of items that had not changed.

// unparkCacheFixture is one repo with three items the sweep reads every
// pass: a label-approval issue (comments + timeline), a PR at 085 (approve
// read + full comment read) and a plain PR with comments (approve read).
// Nothing in it makes the sweep write.
func unparkCacheFixture(t *testing.T, updated time.Time) (*Client, *unparkServer, []unparkWireIssue) {
	t.Helper()
	issue := laIssue(40, "triage/accepted", "needs-human")
	issue.UpdatedAt = updated.UTC().Format(time.RFC3339)
	waiting := unparkWireIssue{
		Number: 41, Title: "waiting PR", User: wireUser{Login: "contributor"},
		Labels:      []wireLabel{{Name: PRNeedsDecisionStageLabel}},
		PullRequest: &unparkWirePRLink{URL: "https://example.test/pull/41"},
		Comments:    []unparkWireComment{humanComment(4101, "contributor", "thanks for the review")},
		UpdatedAt:   updated.UTC().Format(time.RFC3339), CommentCount: 1,
	}
	plain := waiting
	plain.Number, plain.Labels = 42, nil
	plain.PullRequest = &unparkWirePRLink{URL: "https://example.test/pull/42"}
	plain.Comments = []unparkWireComment{humanComment(4201, "contributor", "rebased")}
	issues := []unparkWireIssue{issue, waiting, plain}
	c, rec := laServer(t, issues)
	// The acceptance label was added by someone the policy does not trust,
	// so the label-approval path reads the timeline and does nothing.
	rec.timelines[40] = []unparkWireTimelineEvent{labeledAt(1, laReader, "triage/accepted", updated)}
	return c, rec, issues
}

func unparkCacheSweep(t *testing.T, c *Client, now time.Time) *IssueUnparkSweepResult {
	t.Helper()
	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{
		LabelDriven:    func(string) bool { return true },
		LabelApproval:  laPolicyFn(laPolicy(), true),
		PRApproveReply: func(context.Context, string, int, *gh.IssueComment) (string, error) { return "", nil },
		Now:            func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return result
}

type unparkReadCounts struct {
	comments map[int]int
	timeline int
}

func unparkReads(rec *unparkServer) unparkReadCounts {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := unparkReadCounts{comments: map[int]int{}, timeline: rec.timelineReads}
	for n, k := range rec.commentReads {
		out.comments[n] = k
	}
	return out
}

func TestUnparkSweepReusesReadsWhileUpdatedAtUnchanged(t *testing.T) {
	now := time.Now().UTC()
	c, rec, _ := unparkCacheFixture(t, now.Add(-time.Hour))

	unparkCacheSweep(t, c, now)
	first := unparkReads(rec)
	// The 085 PR is read twice in a pass (approve read, then the full list).
	if first.comments[40] != 1 || first.comments[41] != 2 || first.comments[42] != 1 || first.timeline != 1 {
		t.Fatalf("first pass reads = %+v, want comments 40:1 41:2 42:1 and one timeline read", first)
	}

	unparkCacheSweep(t, c, now.Add(5*time.Minute))
	second := unparkReads(rec)
	for _, n := range []int{40, 41, 42} {
		if second.comments[n] != first.comments[n] {
			t.Errorf("item %d: comments re-read with updated_at unchanged (%d -> %d)", n, first.comments[n], second.comments[n])
		}
	}
	if second.timeline != first.timeline {
		t.Errorf("timeline re-read with updated_at unchanged (%d -> %d)", first.timeline, second.timeline)
	}
	if got := len(rec.posted) + len(rec.edited) + len(rec.added) + len(rec.removed); got != 0 {
		t.Fatalf("fixture must not write, got posted=%v edited=%v added=%v removed=%v", rec.posted, rec.edited, rec.added, rec.removed)
	}
}

func TestUnparkSweepRereadsWhenUpdatedAtMoves(t *testing.T) {
	now := time.Now().UTC()
	c, rec, issues := unparkCacheFixture(t, now.Add(-time.Hour))

	unparkCacheSweep(t, c, now)
	first := unparkReads(rec)

	// A comment or label change moves updated_at; issues backs the list.
	rec.mu.Lock()
	moved := now.Add(-10 * time.Minute).Format(time.RFC3339)
	issues[0].UpdatedAt, issues[2].UpdatedAt = moved, moved
	rec.mu.Unlock()

	unparkCacheSweep(t, c, now.Add(5*time.Minute))
	second := unparkReads(rec)
	if second.comments[40] != first.comments[40]+1 || second.timeline != first.timeline+1 {
		t.Errorf("issue 40 moved: want one more comment and timeline read, got %+v -> %+v", first, second)
	}
	if second.comments[42] != first.comments[42]+1 {
		t.Errorf("PR 42 moved: want one more comment read, got %d -> %d", first.comments[42], second.comments[42])
	}
	if second.comments[41] != first.comments[41] {
		t.Errorf("PR 41 did not move but was re-read (%d -> %d)", first.comments[41], second.comments[41])
	}
}

func TestUnparkSweepTTLBackstopRereads(t *testing.T) {
	now := time.Now().UTC()
	c, rec, _ := unparkCacheFixture(t, now.Add(-time.Hour))

	unparkCacheSweep(t, c, now)
	first := unparkReads(rec)
	unparkCacheSweep(t, c, now.Add(5*time.Hour))
	if mid := unparkReads(rec); mid.comments[40] != first.comments[40] || mid.timeline != first.timeline {
		t.Fatalf("inside the TTL the first read is reused, got %+v -> %+v", first, mid)
	}
	// Seven hours is past the six-hour unparkReadTTL.
	unparkCacheSweep(t, c, now.Add(7*time.Hour))
	second := unparkReads(rec)
	if second.comments[40] != first.comments[40]+1 || second.comments[42] != first.comments[42]+1 || second.timeline != first.timeline+1 {
		t.Fatalf("past the TTL every item must be read again, got %+v -> %+v", first, second)
	}
}

// A read taken moments after the item's updated_at may predate the change
// the list already shows; it is not trusted for the next pass.
func TestUnparkSweepRereadsJustUpdatedItem(t *testing.T) {
	now := time.Now().UTC()
	c, rec, _ := unparkCacheFixture(t, now.Add(-30*time.Second))

	unparkCacheSweep(t, c, now)
	first := unparkReads(rec)
	unparkCacheSweep(t, c, now.Add(5*time.Minute))
	second := unparkReads(rec)
	if second.comments[40] != first.comments[40]+1 || second.timeline != first.timeline+1 {
		t.Fatalf("a read inside the settle window must not be reused, got %+v -> %+v", first, second)
	}
}

// Reused comments still get every live and clock-based check on every pass:
// a maintainer granted write access after the first pass is answered on the
// second without a new comment read, and a command that slid out of the
// seven-day window on the cached data is not.
func TestUnparkSweepDecidesFromCachedCommentsWithCurrentState(t *testing.T) {
	now := time.Now().UTC()
	updated := now.Add(-time.Hour)
	run := func(t *testing.T, commandAge time.Duration, second time.Time) (*unparkServer, *IssueUnparkSweepResult) {
		t.Helper()
		cmd := humanComment(501, "maintainer", "/hive approve")
		cmd.CreatedAt = now.Add(-commandAge).Format(time.RFC3339)
		cmd.UpdatedAt = cmd.CreatedAt
		issue := parkedIssue(50, cmd)
		issue.UpdatedAt = updated.Format(time.RFC3339)
		server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"maintainer": "read"})
		c := newTestClient(t, server, "org", []string{"repo"})
		opts := func(at time.Time) IssueUnparkSweepOptions {
			return IssueUnparkSweepOptions{LabelDriven: func(string) bool { return true }, Now: func() time.Time { return at }}
		}
		if res, err := c.SweepIssueUnparkCommands(context.Background(), opts(now)); err != nil || res.Replies != 0 {
			t.Fatalf("first pass: a read-only commenter must get nothing, replies=%v err=%v", res, err)
		}
		rec.mu.Lock()
		rec.permissions["maintainer"] = "admin"
		rec.mu.Unlock()
		res, err := c.SweepIssueUnparkCommands(context.Background(), opts(second))
		if err != nil {
			t.Fatalf("second pass: %v", err)
		}
		if got := unparkReads(rec).comments[50]; got != 1 {
			t.Fatalf("comments read %d times, want once (second pass from cache)", got)
		}
		return rec, res
	}

	t.Run("permission granted later is answered", func(t *testing.T) {
		rec, res := run(t, time.Hour, now.Add(5*time.Minute))
		if res.Replies != 1 || len(rec.posted[50]) != 1 {
			t.Fatalf("want one reply from cached comments, replies=%d posted=%v", res.Replies, rec.posted[50])
		}
	})
	t.Run("command outside the window on the current clock is ignored", func(t *testing.T) {
		rec, res := run(t, unparkCommandScanWindow-time.Minute, now.Add(5*time.Minute))
		if res.Replies != 0 || len(rec.posted[50]) != 0 {
			t.Fatalf("a command older than the window must not be answered, replies=%d posted=%v", res.Replies, rec.posted[50])
		}
	})
}

package github

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"
)

// Per-issue bot comment budget (#12733).
//
// Every automated comment on an issue is a notification to every subscriber.
// A tracker that collects folded findings, a verification an agent re-posts
// each kick, and the claim that follows it added up to 47 comments on one
// downstream issue in four days. No single producer was wrong; the sum was.
// So the producers that run unattended — the relay's `comment` kind, the
// finding folds and the agent claim mark — share one cap: at most
// issueCommentBudgetPerDay() comments per issue per rolling 24h from this
// process. Operator- and reviewer-driven comments (PR verdicts, dashboard
// actions) are not budgeted; they answer a person.
//
// The budget is in-memory and per process. A restart forgets it, which errs
// toward posting — the cap bounds a storm, it is not a ledger.

const (
	// defaultIssueCommentBudgetPerDay is the cap when
	// HIVE_ISSUE_COMMENT_BUDGET_PER_DAY is unset or invalid.
	defaultIssueCommentBudgetPerDay = 5
	// issueCommentBudgetEnv overrides the daily cap; 0 or less disables it.
	issueCommentBudgetEnv = "HIVE_ISSUE_COMMENT_BUDGET_PER_DAY"
	// issueCommentBudgetWindow is the rolling window the cap applies over.
	issueCommentBudgetWindow = 24 * time.Hour
)

// ErrIssueCommentBudget is returned when the per-issue daily cap would be
// exceeded. Callers treat it as a deliberate suppression, never a retry.
var ErrIssueCommentBudget = errors.New("per-issue bot comment budget exhausted for today")

// issueCommentBudgetPerDay reads the cap once per process.
var issueCommentBudgetPerDay = sync.OnceValue(func() int {
	raw := os.Getenv(issueCommentBudgetEnv)
	if raw == "" {
		return defaultIssueCommentBudgetPerDay
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultIssueCommentBudgetPerDay
	}
	return n
})

// issueCommentBudget tracks automated comment timestamps per repo#number.
// The zero value is ready to use.
type issueCommentBudget struct {
	mu     sync.Mutex
	posted map[string][]time.Time
	// perDay and now are test seams; zero means the process defaults.
	perDay int
	now    func() time.Time
}

func (b *issueCommentBudget) cap() int {
	if b.perDay != 0 {
		return b.perDay
	}
	return issueCommentBudgetPerDay()
}

// allow records one comment on key and reports whether it is within budget.
// It returns the number already posted in the window so the caller can log
// it.
func (b *issueCommentBudget) allow(key string) (ok bool, used int) {
	limit := b.cap()
	if limit <= 0 {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if b.now != nil {
		now = b.now()
	}
	cutoff := now.Add(-issueCommentBudgetWindow)
	kept := b.posted[key][:0]
	for _, at := range b.posted[key] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if b.posted == nil {
		b.posted = map[string][]time.Time{}
	}
	if len(kept) >= limit {
		b.posted[key] = kept
		return false, len(kept)
	}
	b.posted[key] = append(kept, now)
	return true, len(kept)
}

// CreateIssueCommentBudgeted posts an automated comment on an issue unless
// the per-issue daily cap is reached, in which case it logs and returns
// ErrIssueCommentBudget without calling GitHub. A failed post is not charged
// against the budget.
func (c *Client) CreateIssueCommentBudgeted(ctx context.Context, repo string, number int, body string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	key := foldedTrackerKey(owner+"/"+repoName, number)
	ok, used := c.commentBudget.allow(key)
	if !ok {
		c.logger.Warn("issue comment suppressed: per-issue bot comment budget reached",
			slog.String("repo", owner+"/"+repoName), slog.Int("number", number),
			slog.Int("posted_24h", used), slog.Int("cap", c.commentBudget.cap()))
		return ErrIssueCommentBudget
	}
	if err := c.CreateIssueComment(ctx, owner+"/"+repoName, number, body); err != nil {
		c.commentBudget.refund(key)
		return err
	}
	return nil
}

// refund drops the most recent charge on key after a failed post.
func (b *issueCommentBudget) refund(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n := len(b.posted[key]); n > 0 {
		b.posted[key] = b.posted[key][:n-1]
	}
}

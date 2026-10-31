package github

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// notificationTallyLogEvery is how often the client logs its notification
// counters. One INFO line an hour is enough to chart the effect of
// github.notifications.mode without a metrics pipeline.
const notificationTallyLogEvery = time.Hour

// notificationTally counts what the hive did to GitHub comments: created
// (a new comment — an email to the author and every watcher), edited (an
// in-place rewrite — no email) and suppressed (a note the hive decided not
// to post as its own comment: folded into the status comment, found
// already present, or dropped as noise). Counters are cumulative since
// process start; the hourly log line prints them so the delta between two
// lines is the hour's traffic.
type notificationTally struct {
	created    atomic.Int64
	edited     atomic.Int64
	suppressed atomic.Int64

	mu      sync.Mutex
	lastLog time.Time
}

// notificationTallyNow is the tally's clock, replaced by tests.
var notificationTallyNow = time.Now

func (t *notificationTally) bump(counter *atomic.Int64, logger *slog.Logger) {
	if t == nil {
		return
	}
	counter.Add(1)
	t.maybeLog(logger)
}

// maybeLog writes the hourly line when at least notificationTallyLogEvery
// has passed since the last one. The first bump after start logs at once
// so a fresh process reports its baseline.
func (t *notificationTally) maybeLog(logger *slog.Logger) {
	if logger == nil {
		return
	}
	now := notificationTallyNow()
	t.mu.Lock()
	due := t.lastLog.IsZero() || now.Sub(t.lastLog) >= notificationTallyLogEvery
	if due {
		t.lastLog = now
	}
	t.mu.Unlock()
	if !due {
		return
	}
	logger.Info("github notifications",
		slog.Int64("created", t.created.Load()),
		slog.Int64("edited", t.edited.Load()),
		slog.Int64("suppressed", t.suppressed.Load()))
}

// noteCommentCreated records a new issue/PR comment.
func (c *Client) noteCommentCreated() {
	if c == nil {
		return
	}
	c.notifications.bump(&c.notifications.created, c.logger)
}

// noteCommentEdited records an in-place comment edit.
func (c *Client) noteCommentEdited() {
	if c == nil {
		return
	}
	c.notifications.bump(&c.notifications.edited, c.logger)
}

// noteCommentSuppressed records a comment the hive chose not to create.
func (c *Client) noteCommentSuppressed() {
	if c == nil {
		return
	}
	c.notifications.bump(&c.notifications.suppressed, c.logger)
}

// NotificationCounts returns the cumulative created/edited/suppressed
// comment counts, for tests and diagnostics.
func (c *Client) NotificationCounts() (created, edited, suppressed int64) {
	if c == nil {
		return 0, 0, 0
	}
	return c.notifications.created.Load(), c.notifications.edited.Load(), c.notifications.suppressed.Load()
}

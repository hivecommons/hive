package github

import (
	"log/slog"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ReviewRequestWebhookFallbackInterval is how often the review-request
// watcher scans its drop box while webhooks are healthy (hivecommons/hive
// #13378). Between scans it is woken by the request-dir change notification
// and by the review-shaped webhook events (reviewRequestWakeEvent), so the
// scan is a reconcile, not the delivery path.
const ReviewRequestWebhookFallbackInterval = 5 * time.Minute

// reviewRequestFallbackInterval is ReviewRequestWebhookFallbackInterval as a
// var so tests can shorten it.
var reviewRequestFallbackInterval = ReviewRequestWebhookFallbackInterval

// reviewRequestWaker is a coalescing wake signal: any number of wake calls
// between two watcher passes produce exactly one extra pass.
type reviewRequestWaker struct {
	once sync.Once
	ch   chan struct{}
}

func (w *reviewRequestWaker) c() chan struct{} {
	w.once.Do(func() { w.ch = make(chan struct{}, 1) })
	return w.ch
}

func (w *reviewRequestWaker) wake() {
	select {
	case w.c() <- struct{}{}:
	default:
	}
}

// WakeReviewRequestWatcher asks the review-request watcher to scan its drop
// box now rather than at its next scheduled pass.
func (c *Client) WakeReviewRequestWatcher() {
	if c == nil {
		return
	}
	c.reviewWake.wake()
}

// reviewRequestWakeEvent reports whether a webhook delivery is one the
// review-request watcher should wake for: review activity on a PR, or a
// label change on an issue. These are the events #13373 names for this
// watcher; a request that was backed off or waiting on review state gets
// its next attempt when the state it depends on moves.
func reviewRequestWakeEvent(event string, p webhookEventPayload) bool {
	switch event {
	case "issue_comment", "pull_request_review", "pull_request_review_comment":
		return true
	case "issues":
		return p.Action == "labeled" || p.Action == "unlabeled"
	}
	return false
}

// reviewRequestScanInterval picks the delay until the next unprompted scan.
// With a working request-dir notification and healthy webhooks the scan is
// a slow reconcile; otherwise the legacy poll interval is the delivery path
// and must stay short. poll and fallback are captured once by the caller.
func reviewRequestScanInterval(dirNotify bool, poll, fallback time.Duration) time.Duration {
	if !dirNotify || !WebhookHealthSnapshot().Healthy {
		return poll
	}
	return fallback
}

// watchReviewRequestDir starts an fsnotify watch on dir and returns a channel
// that receives once per batch of create/rename/write events plus a stop
// function. notify is nil when the watch could not be established, in which
// case the caller must keep polling for new request files.
func watchReviewRequestDir(dir string, logger *slog.Logger) (notify <-chan struct{}, stop func()) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Warn("review-request watcher: request-dir notification unavailable, keeping poll cadence",
			slog.String("dir", dir), slog.String("error", err.Error()))
		return nil, func() {}
	}
	if err := fsw.Add(dir); err != nil {
		_ = fsw.Close()
		logger.Warn("review-request watcher: request-dir notification unavailable, keeping poll cadence",
			slog.String("dir", dir), slog.String("error", err.Error()))
		return nil, func() {}
	}
	ch := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case ev, ok := <-fsw.Events:
				if !ok {
					return
				}
				if ev.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Write) == 0 {
					continue
				}
				select {
				case ch <- struct{}{}:
				default:
				}
			case _, ok := <-fsw.Errors:
				if !ok {
					return
				}
			}
		}
	}()
	return ch, func() { _ = fsw.Close() }
}

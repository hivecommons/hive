package github

import (
	"path/filepath"
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/fleetreport"
)

// Stuck thresholds for the issue-request queue. A request that has failed
// this many attempts, or has been retrying for this long, is work the hive
// is silently not doing; the watcher's own give-up horizon
// (issueRequestMaxAge) is 24h, far too late for an operator to learn about a
// clogged queue from the self-report alone.
const (
	issueRequestStuckAttempts = 3
	issueRequestStuckAge      = 30 * time.Minute
)

// issueRequestStuckKey scopes the self-report condition to one request file;
// the file name is already an opaque id (agent-chosen, no secrets).
func issueRequestStuckKey(path string) string { return filepath.Base(path) }

// observeIssueRequestStuck feeds fleetreport.ClassIssueRequestStuck once a
// retried request crosses either stuck threshold. It reads the retry state
// the caller just updated via issueNoteFailure; before the threshold it is a
// no-op so a single transient failure never becomes a report.
func (c *Client) observeIssueRequestStuck(path, repo, kind string, err error, now time.Time) {
	attempts, since := c.issueRetrySnapshot(path)
	if attempts == 0 {
		return
	}
	age := now.Sub(since)
	if attempts < issueRequestStuckAttempts && age < issueRequestStuckAge {
		return
	}
	msg := "issue-request stuck: " + kind + " request failed " + strconv.Itoa(attempts) + " attempts over " + age.Round(time.Minute).String()
	diag := "file=" + issueRequestStuckKey(path) + " kind=" + kind + " attempts=" + strconv.Itoa(attempts) + " age=" + age.Round(time.Second).String()
	if err != nil {
		diag += " error=" + err.Error()
	}
	fleetreport.ObserveItem(fleetreport.ClassIssueRequestStuck, issueRequestStuckKey(path), msg, diag, repo, "")
}

// clearIssueRequestStuck is called whenever the retry state for path is
// dropped (completed, quarantined), so the condition clears with it.
func clearIssueRequestStuck(path string) {
	fleetreport.Clear(fleetreport.ClassIssueRequestStuck, issueRequestStuckKey(path))
}

// issueRetrySnapshot reports the attempt count and first-failure time
// recorded for path; zero when nothing is tracked.
func (c *Client) issueRetrySnapshot(path string) (int, time.Time) {
	c.issueRetryMu.Lock()
	defer c.issueRetryMu.Unlock()
	st := c.issueRetries[path]
	if st == nil {
		return 0, time.Time{}
	}
	return st.attempts, st.firstTry
}

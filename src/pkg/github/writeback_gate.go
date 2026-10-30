package github

import (
	"errors"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/writeback"
)

// SetWritebackGate installs the unsolicited write-back gate. cmd/hive builds
// one at boot from writeback.Config and passes it here so every sweep in
// this package reaches the same policy; nil (the default in tests that
// predate the gate) allows everything, like SetRepoPausedFunc.
func (c *Client) SetWritebackGate(g *writeback.Gate) {
	if c == nil {
		return
	}
	c.reposMu.Lock()
	defer c.reposMu.Unlock()
	c.writeback = g
}

// WritebackGate returns the installed gate (nil when none).
func (c *Client) WritebackGate() *writeback.Gate {
	if c == nil {
		return nil
	}
	c.reposMu.RLock()
	defer c.reposMu.RUnlock()
	return c.writeback
}

// WritebackItemForPR builds the gate's view of a PR from the enumerated
// record.
func (c *Client) WritebackItemForPR(pr PullRequest) writeback.Item {
	it := writeback.Item{Number: pr.Number, IsPR: true, Author: pr.Author, Labels: pr.Labels, CreatedAt: pr.CreatedAt}
	it.HiveAuthored = pr.AppAuthored || pr.HiveAttributed || (c != nil && pr.Author != "" && c.IsHiveAuthorLogin(pr.Author))
	return it
}

// WritebackItemForIssue is WritebackItemForPR for an issue.
func (c *Client) WritebackItemForIssue(is Issue) writeback.Item {
	it := writeback.Item{Number: is.Number, Author: is.Author, Labels: is.Labels, CreatedAt: is.CreatedAt}
	it.HiveAuthored = !is.AuthorIsHuman && (c != nil && is.Author != "" && c.IsHiveAuthorLogin(is.Author))
	return it
}

// WritebackAllowed is the one call every unsolicited write in this package
// makes before touching GitHub. It returns the decision so the caller can
// record a suppressed finding for the dashboard instead of writing.
func (c *Client) WritebackAllowed(repo string, item writeback.Item, kind writeback.Kind, preview string) writeback.Decision {
	return c.WritebackGate().AllowedPreview(repo, item, kind, preview)
}

// noteWritebackOutcome feeds a real write's HTTP status to the gate so it
// can detect the App gaining write on a repo (first 2xx after 403/404s).
func (c *Client) noteWritebackOutcome(repo string, resp *gh.Response, err error) {
	g := c.WritebackGate()
	if g == nil {
		return
	}
	status := 0
	if resp != nil && resp.Response != nil {
		status = resp.StatusCode
	}
	if status == 0 && err != nil {
		var ghErr *gh.ErrorResponse
		if errors.As(err, &ghErr) && ghErr.Response != nil {
			status = ghErr.Response.StatusCode
		}
	}
	if status == 0 && err == nil {
		status = 200
	}
	if status != 0 {
		g.NoteWriteOutcome(repo, status)
	}
}

// writebackSweepSummary logs the per-repo backfill summary after a sweep.
func (c *Client) writebackSweepSummary(repo string) {
	c.WritebackGate().SweepSummary(repo)
}

// RepoPausedError is returned by a periodic writer that fell silent because
// its target repo is operator-paused (#6203). Callers treat it as a healthy
// skip, not a failure: the quiet is deliberate.
type RepoPausedError struct {
	Repo   string
	By     string
	Reason string
}

func (e *RepoPausedError) Error() string {
	return "writeback: " + writeback.RepoPausedDetail(e.Repo, writeback.RepoPauseInfo{By: e.By, Reason: e.Reason})
}

// repoPausedError combines the two pause sources a client may carry: the
// gate's provenance-bearing lookup (config.RepoPauseFor) and the bare
// SetRepoPausedFunc predicate. Either one pausing the repo pauses it; the
// provenance is filled in when the gate knows it.
func (c *Client) repoPausedError(repo string) (*RepoPausedError, bool) {
	if c == nil {
		return nil, false
	}
	if info, paused := c.WritebackGate().RepoPaused(repo); paused {
		return &RepoPausedError{Repo: repo, By: info.By, Reason: info.Reason}, true
	}
	if c.RepoIsPaused(repo) {
		return &RepoPausedError{Repo: repo}, true
	}
	return nil, false
}

// WritebackSuppressedSince returns gate suppressions newer than since for
// the dashboard drill-down (nil gate → nil).
func (c *Client) WritebackSuppressedSince(since time.Time) []writeback.Suppressed {
	var out []writeback.Suppressed
	for _, s := range c.WritebackGate().Recent() {
		if s.At.After(since) {
			out = append(out, s)
		}
	}
	return out
}

package github

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

// ReviewEventRecordVerdict is the relay event for "I judged this PR and have
// nothing to post about it". It performs no GitHub write.
const ReviewEventRecordVerdict = "record_verdict"

// recordReviewVerdict is the missing half of the reviewer's output path.
//
// The reviewer produces TWO artifacts: a PR comment (posted through this same
// relay) and a structured verdict that the routing chain consumes —
// review.Collect -> review-verdicts.json -> Aggregate -> dispatchFix ->
// State.Human -> the human-decision label.
//
// The comment half worked. The verdict half had no transport at all. Nothing in
// the tree ever wrote the review-report-*.json files review.Collect reads, and
// the agents could not have written them anyway: /var/run/hive-metrics is
// 0755 dev:node and every agent runs as its own non-dev uid, so an agent-side
// write is EACCES by construction. Measured on a bluefin spoke: 117
// agent_pr_reviewed audit entries, a populated review-links.json, and
// review-verdicts.json still {"items": []} with 38 orphaned pending_reviews and
// zero human-decision labels ever applied.
//
// The relay is the correct place to close that gap. The watcher already runs
// server-side as the dir's owner, so it can write where the agent cannot, and
// the verdict rides the same authorized, forge-resistant drop the comment does
// instead of needing a second privileged channel.
//
// Failures here are logged and swallowed. The review is already posted by the
// time this runs; losing the verdict must never turn a successful review into a
// retry that posts the comment a second time.
func (c *Client) recordReviewVerdict(req ReviewRequest, dir string) {
	raw := strings.TrimSpace(req.Report)
	if raw == "" {
		return
	}
	if dir == "" {
		dir = outputschema.AgentReportDir
	}
	// Validate BEFORE the report lands where the collector reads. review.Collect
	// fails the whole collection on the first unparseable file, so one malformed
	// agent verdict would otherwise take down routing for every PR in the hive.
	report, err := review.ValidateReport([]byte(raw))
	if err != nil {
		c.logger.Warn("review-request watcher: rejected malformed verdict",
			slog.String("repo", req.Repo), slog.Int("number", req.Number),
			slog.String("agent", req.Agent), slog.String("error", err.Error()))
		return
	}
	// The verdict must be about the PR that was actually reviewed. Without this
	// an agent authorized to comment on one PR could record a binding
	// requires_human/reject verdict against any other PR in the fleet.
	if !strings.EqualFold(strings.TrimSpace(report.Repo), strings.TrimSpace(req.Repo)) || report.Number != req.Number {
		c.logger.Warn("review-request watcher: verdict target does not match reviewed PR, discarded",
			slog.String("reviewed", fmt.Sprintf("%s#%d", req.Repo, req.Number)),
			slog.String("claimed", fmt.Sprintf("%s#%d", report.Repo, report.Number)),
			slog.String("agent", req.Agent))
		return
	}

	name := review.ReviewReportFilePrefix +
		verdictSlug(report.Repo) + "-" +
		strconv.Itoa(report.Number) + "-" +
		verdictSlug(string(report.Perspective)) +
		review.ReviewReportFileSuffix
	path := filepath.Join(dir, name)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.logger.Warn("review-request watcher: could not create report dir",
			slog.String("dir", dir), slog.String("error", err.Error()))
		return
	}
	// Write-then-rename: review.Collect scans this dir on its own schedule and
	// must never observe a half-written report.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(raw), 0o644); err != nil {
		c.logger.Warn("review-request watcher: could not write verdict",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		c.logger.Warn("review-request watcher: could not commit verdict",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	c.logger.Info("review-request watcher: verdict recorded",
		slog.String("repo", report.Repo), slog.Int("number", report.Number),
		slog.String("perspective", string(report.Perspective)),
		slog.String("verdict", string(report.Verdict)),
		slog.String("path", path))
}

// verdictSlug reduces an identifier to a filename-safe token. report.Repo is
// agent-supplied, so "../../etc/cron.d/x" must not become a path: everything
// outside the allowlist collapses to "-", which leaves no separators and no
// dot-dot for the join to walk.
func verdictSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	return out
}

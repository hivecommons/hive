package github

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

// ReviewEventRecordVerdict is the relay event for "I judged this PR and have
// nothing to post about it". It performs no GitHub write.
const ReviewEventRecordVerdict = "record_verdict"

const reviewVerdictAuditLookback = 90 * 24 * time.Hour

var reviewVerdictAuditPath = "/data/audit.jsonl"

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
	dir = review.ReportDir(dir)
	// Validate BEFORE the report lands where the collector reads. review.Collect
	// fails the whole collection on the first unparseable file, so one malformed
	// agent verdict would otherwise take down routing for every PR in the hive.
	//
	// A combined review judges every perspective in one session and delivers
	// them as one array, so this accepts either shape: a bare object is still
	// exactly what it was.
	reports, err := review.ValidateReportsFor([]byte(raw), c.perspectives)
	if err != nil {
		c.logger.Warn("review-request watcher: rejected malformed verdict",
			slog.String("repo", req.Repo), slog.Int("number", req.Number),
			slog.String("agent", req.Agent), slog.String("error", err.Error()))
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.logger.Warn("review-request watcher: could not create report dir",
			slog.String("dir", dir), slog.String("error", err.Error()))
		return
	}
	for _, report := range reports {
		c.writeOneVerdict(req, report, dir)
	}
}

// writeOneVerdict commits a single perspective's verdict where review.Collect
// will find it. One file per perspective, which is what the collector has
// always read — a combined review changes how verdicts arrive, not how they
// are stored, so nothing downstream needs to know the difference.
func (c *Client) writeOneVerdict(req ReviewRequest, report review.PerspectiveReport, dir string) {
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
	// Bind to a real dispatch record. A combined review answers one kick with
	// several verdicts, so each element is checked on its own: the dispatch is
	// recorded per perspective, and a perspective nothing dispatched is refused
	// here exactly as it would be for a single-object verdict.
	ok, reason, dispatchHead := verdictDispatchAuthorized(report, req)
	if !ok {
		c.logger.Warn("review-request watcher: verdict does not match a review dispatch, discarded",
			slog.String("reviewed", fmt.Sprintf("%s#%d", req.Repo, req.Number)),
			slog.String("perspective", string(report.Perspective)),
			slog.String("agent", req.Agent),
			slog.String("reason", reason))
		return
	}
	if strings.TrimSpace(report.HeadSHA) == "" && strings.TrimSpace(dispatchHead) != "" {
		report.HeadSHA = strings.TrimSpace(dispatchHead)
	}

	// Re-marshalled from the validated struct rather than written through from
	// the request. One array element is not a standalone document, and the
	// collector reads each file as one report; re-marshalling also drops the
	// descriptive extra keys models add, which ValidateReport tolerates but
	// nothing downstream reads, and carries the bound head SHA above.
	raw, err := json.Marshal(report)
	if err != nil {
		c.logger.Warn("review-request watcher: could not serialize verdict",
			slog.String("perspective", string(report.Perspective)), slog.String("error", err.Error()))
		return
	}

	name := review.ReviewReportFilePrefix +
		verdictSlug(report.Repo) + "-" +
		strconv.Itoa(report.Number) + "-" +
		verdictSlug(string(report.Perspective)) +
		review.ReviewReportFileSuffix
	path := filepath.Join(dir, name)

	// Write-then-rename: review.Collect scans this dir on its own schedule and
	// must never observe a half-written report.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
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

func verdictDispatchAuthorized(report review.PerspectiveReport, req ReviewRequest) (bool, string, string) {
	state, err := review.LoadDispatchState("")
	if err != nil {
		return false, "dispatch_state_unavailable", ""
	}
	now := time.Now().UTC()
	for _, p := range state.Pending {
		if !pendingReviewMatchesVerdict(p, report, req) {
			continue
		}
		if sameVerdictAuthor(verdictAuthorAgent(p.AuthorAgent, report), req.Agent) {
			return false, "author_self_approval", ""
		}
		return true, "", p.HeadSHA
	}
	cutoff := now.Add(-review.RecentDispatchTTL)
	for _, r := range state.Recent {
		if !recentReviewMatchesVerdict(r, report, req) {
			continue
		}
		if !r.Confirmed.IsZero() && r.Confirmed.Before(cutoff) {
			continue
		}
		if sameVerdictAuthor(verdictAuthorAgent(r.AuthorAgent, report), req.Agent) {
			return false, "author_self_approval", ""
		}
		return true, "", r.HeadSHA
	}
	return false, "no_matching_dispatch", ""
}

func pendingReviewMatchesVerdict(p review.PendingReview, report review.PerspectiveReport, req ReviewRequest) bool {
	return reviewDispatchFieldsMatch(p.Repo, p.Number, p.HeadSHA, p.Perspective, p.Agent, report, req)
}

func recentReviewMatchesVerdict(r review.RecentReview, report review.PerspectiveReport, req ReviewRequest) bool {
	return reviewDispatchFieldsMatch(r.Repo, r.Number, r.HeadSHA, r.Perspective, r.Agent, report, req)
}

func reviewDispatchFieldsMatch(repo string, number int, headSHA string, perspective review.Perspective, agent string, report review.PerspectiveReport, req ReviewRequest) bool {
	if !strings.EqualFold(strings.TrimSpace(repo), strings.TrimSpace(report.Repo)) {
		return false
	}
	if number != report.Number || perspective != report.Perspective || strings.TrimSpace(agent) != strings.TrimSpace(req.Agent) {
		return false
	}
	if strings.TrimSpace(headSHA) != "" && strings.TrimSpace(report.HeadSHA) != "" && strings.TrimSpace(headSHA) != strings.TrimSpace(report.HeadSHA) {
		return false
	}
	return true
}

func sameVerdictAuthor(authorAgent, agent string) bool {
	authorAgent = strings.TrimSpace(authorAgent)
	agent = strings.TrimSpace(agent)
	return authorAgent != "" && agent != "" && authorAgent == agent
}

func verdictAuthorAgent(stateAuthor string, report review.PerspectiveReport) string {
	if strings.TrimSpace(stateAuthor) != "" {
		return stateAuthor
	}
	return auditAuthorAgentForPR(report.Repo, report.Number, reviewVerdictAuditPath, time.Now().UTC().Add(-reviewVerdictAuditLookback))
}

type verdictAuditEntry struct {
	Timestamp string `json:"ts"`
	Action    string `json:"action"`
	Detail    string `json:"detail,omitempty"`
	Agent     string `json:"agent,omitempty"`
}

func auditAuthorAgentForPR(repo string, number int, path string, since time.Time) string {
	var authorAgent string
	exactMatch := false
	for _, auditFile := range verdictAuditLogFiles(path) {
		data, err := readVerdictAuditLogFile(auditFile)
		if err != nil {
			continue
		}
		for _, line := range bytes.Split(data, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var entry verdictAuditEntry
			if json.Unmarshal(line, &entry) != nil || entry.Action != AuditActionAgentPRCreated || strings.TrimSpace(entry.Agent) == "" {
				continue
			}
			if !since.IsZero() {
				ts, err := time.Parse(time.RFC3339, entry.Timestamp)
				if err != nil || ts.Before(since) {
					continue
				}
			}
			entryRepo, entryNumber := auditPRDetailTarget(entry.Detail)
			if entryNumber != number {
				continue
			}
			switch verdictRepoMatchRank(entryRepo, repo) {
			case 2:
				authorAgent = entry.Agent
				exactMatch = true
			case 1:
				if !exactMatch {
					authorAgent = entry.Agent
				}
			}
		}
	}
	return authorAgent
}

func verdictAuditLogFiles(filePath string) []string {
	dir := filepath.Dir(filePath)
	base := filepath.Base(filePath)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext)
	patterns := []string{
		filePath,
		filePath + ".*",
		filepath.Join(dir, prefix+"-*"+ext),
		filepath.Join(dir, prefix+"-*"+ext+".gz"),
	}
	seen := map[string]bool{}
	var files []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, p := range matches {
			if seen[p] {
				continue
			}
			seen[p] = true
			files = append(files, p)
		}
	}
	sort.Strings(files)
	return files
}

const maxVerdictAuditFileReadBytes = 64 << 20

func readVerdictAuditLogFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}
	return io.ReadAll(io.LimitReader(r, maxVerdictAuditFileReadBytes))
}

func auditPRDetailTarget(detail string) (string, int) {
	var repo string
	var number int
	for _, part := range strings.Split(detail, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "repo":
			repo = strings.TrimSpace(v)
		case "number":
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err == nil {
				number = n
			}
		}
	}
	return repo, number
}

func verdictRepoMatchRank(a, b string) int {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if strings.EqualFold(a, b) {
		return 2
	}
	if !strings.Contains(a, "/") && strings.EqualFold(a, verdictBareRepo(b)) {
		return 1
	}
	return 0
}

func verdictBareRepo(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
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

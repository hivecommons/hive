package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

// reviewBacklogFooter ends every backlog item body and re-raise comment so a
// reader in any tracker can tell where the item came from.
const reviewBacklogFooter = "filed by Hive review backlog"

const reviewBacklogReRaiseMarker = "<!-- hive:review-backlog-reraise "

// ReviewBacklogRouting is the live review.severity / review.backlog view the
// backlog filer reads on every pass (hivecommons/hive#11089).
type ReviewBacklogRouting struct {
	// Severity decides which in-scope findings fall below the blocking line.
	Severity config.ReviewSeverityConfig
	// Backlog is the configured destination, labels and per-PR daily cap.
	Backlog config.ReviewBacklogConfig
	// Sink files and updates items. Nil means GitHub issues in the PR's repo.
	Sink ReviewBacklogSink
}

// ReviewBacklogItem is one new backlog item.
type ReviewBacklogItem struct {
	Repo   string
	Title  string
	Body   string
	Labels []string
}

// ReviewBacklogRef identifies a filed backlog item in its destination.
type ReviewBacklogRef struct {
	// ID is the destination-native id used for updates: the GitHub issue
	// number, the Linear issue id or the Jira issue key.
	ID string
	// Key is the human-facing reference: "#101", "ENG-12".
	Key string
	// Number is the GitHub issue number; 0 for non-GitHub destinations.
	Number int
	URL    string
	// Reused is true when the destination returned an existing open item
	// instead of creating one.
	Reused bool
}

// ReviewBacklogSink is a backlog destination. The GitHub issue sink lives
// here; the GitHub Projects, Linear and Jira sinks are built by
// pkg/worksource on top of the work-source clients.
type ReviewBacklogSink interface {
	// Destination is the review.backlog.destination value this sink serves.
	Destination() string
	CreateItem(ctx context.Context, item ReviewBacklogItem) (ReviewBacklogRef, error)
	// AppendToItem adds comment to an item previously returned by CreateItem.
	AppendToItem(ctx context.Context, repo string, ref ReviewBacklogRef, comment string) error
}

// SetReviewBacklogRouting configures where review backlog items go and which
// in-scope findings count as below the blocking line. Read through a func so
// live config edits apply without rebuilding the client.
func (c *Client) SetReviewBacklogRouting(fn func() ReviewBacklogRouting) {
	if c == nil {
		return
	}
	c.reviewBacklogRouting = fn
}

func (c *Client) reviewBacklogRoutingConfig() (ReviewBacklogRouting, bool) {
	if c == nil || c.reviewBacklogRouting == nil {
		return ReviewBacklogRouting{}, false
	}
	return c.reviewBacklogRouting(), true
}

// ReviewBacklogIssueSink returns the github_issue backlog sink: issues in the
// reviewed PR's repo, created through CreateIssue.
func (c *Client) ReviewBacklogIssueSink() ReviewBacklogSink {
	return githubIssueBacklogSink{c: c}
}

type githubIssueBacklogSink struct{ c *Client }

func (githubIssueBacklogSink) Destination() string { return config.ReviewBacklogGitHubIssue }

func (s githubIssueBacklogSink) CreateItem(ctx context.Context, item ReviewBacklogItem) (ReviewBacklogRef, error) {
	res, err := s.c.CreateIssue(ctx, item.Repo, item.Title, item.Body, item.Labels)
	if err != nil {
		return ReviewBacklogRef{}, err
	}
	if res.Number <= 0 {
		return ReviewBacklogRef{}, nil
	}
	if res.AlreadyExisted {
		if err := s.c.ensureReviewBacklogLabels(ctx, item.Repo, res.Number, item.Labels); err != nil {
			return ReviewBacklogRef{}, err
		}
	}
	return ReviewBacklogRef{
		ID:     strconv.Itoa(res.Number),
		Key:    fmt.Sprintf("#%d", res.Number),
		Number: res.Number,
		URL:    res.URL,
		Reused: res.AlreadyExisted,
	}, nil
}

func (s githubIssueBacklogSink) AppendToItem(ctx context.Context, repo string, ref ReviewBacklogRef, comment string) error {
	if ref.Number <= 0 {
		return fmt.Errorf("review backlog: GitHub item %q has no issue number", ref.ID)
	}
	return s.c.CreateIssueComment(ctx, repo, ref.Number, comment)
}

// reviewBacklogTarget resolves the sink and labels for one filing pass. When
// the configured destination could not be built (its work source is not
// active or not configured) the sink is the GitHub issue fallback, and the
// from-review label is always applied so the fallback items stay findable.
func (c *Client) reviewBacklogTarget(r ReviewBacklogRouting) (ReviewBacklogSink, []string) {
	labels := append([]string(nil), r.Backlog.EffectiveLabels()...)
	sink := r.Sink
	if sink == nil {
		sink = c.ReviewBacklogIssueSink()
	}
	want, ok := config.NormalizeReviewBacklogDestination(r.Backlog.Destination)
	if !ok || sink.Destination() != want {
		labels = appendLabelOnce(labels, reviewBacklogLabel)
	}
	return sink, labels
}

func appendLabelOnce(labels []string, label string) []string {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), label) {
			return labels
		}
	}
	return append(labels, label)
}

// reviewBacklogDailyCap is review.backlog.max_per_pr_per_day when routing is
// configured, else the legacy per-PR cap.
func reviewBacklogDailyCap(r ReviewBacklogRouting, routed bool, legacyCap int) int {
	if routed {
		return r.Backlog.EffectiveMaxPerPRPerDay()
	}
	if legacyCap <= 0 {
		return DefaultReviewBacklogIssueCap
	}
	return legacyCap
}

// belowLineBacklogEnabled reports whether in-scope findings below
// review.severity.block_at are filed to the backlog.
func (r ReviewBacklogRouting) belowLineBacklogEnabled() bool {
	b, ok := config.NormalizeReviewSeverityBlockAt(r.Severity.BlockAt)
	return ok && b != "" && r.Severity.BacklogBelowEnabled()
}

// ReviewFindingPriority maps a finding severity onto the P0–P3 scale used by
// review.severity.block_at: critical → 0, high → 1, medium → 2, low/info → 3.
func ReviewFindingPriority(sev outputschema.Severity) int {
	switch strings.ToLower(strings.TrimSpace(string(sev))) {
	case string(outputschema.SeverityCritical):
		return 0
	case string(outputschema.SeverityHigh):
		return 1
	case string(outputschema.SeverityMedium):
		return 2
	default:
		return 3
	}
}

// ReviewFindingBelowLine reports whether f is below the blocking line
// blockAt (P1–P3). An empty or invalid blockAt means there is no line, so
// nothing is below it.
func ReviewFindingBelowLine(f outputschema.Finding, blockAt string) bool {
	b, ok := config.NormalizeReviewSeverityBlockAt(blockAt)
	if !ok || b == "" {
		return false
	}
	line, err := strconv.Atoi(strings.TrimPrefix(b, "P"))
	if err != nil {
		return false
	}
	return ReviewFindingPriority(f.Severity) > line
}

// ReviewFindingFingerprint is sha256(rule | path | snippet) for a finding:
// the finding title is the rule, the cited file the path, and the summary
// the snippet. Text is lower-cased with whitespace runs collapsed and paths
// are cleaned, so re-wrapping or re-indenting the same finding, or citing it
// at a shifted line, yields the same fingerprint. Line numbers are excluded
// on purpose.
func ReviewFindingFingerprint(f outputschema.Finding) string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		normaliseFingerprintText(f.Title),
		normaliseFingerprintPath(f.File),
		normaliseFingerprintText(f.Summary),
	}, "|")))
	return hex.EncodeToString(h[:])
}

func normaliseFingerprintText(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func normaliseFingerprintPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return ""
	}
	return strings.TrimPrefix(path.Clean(p), "/")
}

// reviewBacklogFingerprintRecord is one per-repo ledger entry: the item a
// fingerprint was filed as, and every PR that raised it.
type reviewBacklogFingerprintRecord struct {
	Destination string    `json:"destination"`
	ItemID      string    `json:"item_id"`
	ItemKey     string    `json:"item_key,omitempty"`
	ItemNumber  int       `json:"item_number,omitempty"`
	ItemURL     string    `json:"item_url,omitempty"`
	PRs         []int     `json:"prs"`
	FiledAt     time.Time `json:"filed_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (r reviewBacklogFingerprintRecord) ref() ReviewBacklogRef {
	return ReviewBacklogRef{ID: r.ItemID, Key: r.ItemKey, Number: r.ItemNumber, URL: r.ItemURL}
}

func (r reviewBacklogFingerprintRecord) hasPR(n int) bool {
	for _, p := range r.PRs {
		if p == n {
			return true
		}
	}
	return false
}

func reviewBacklogRepoKey(repo string) string {
	return strings.ToLower(strings.TrimSpace(repo))
}

func (l *reviewBacklogLedger) fingerprint(repo, fp string) (reviewBacklogFingerprintRecord, bool) {
	rec, ok := l.Fingerprints[reviewBacklogRepoKey(repo)][fp]
	return rec, ok
}

func (l *reviewBacklogLedger) setFingerprint(repo, fp string, rec reviewBacklogFingerprintRecord) {
	key := reviewBacklogRepoKey(repo)
	if l.Fingerprints[key] == nil {
		l.Fingerprints[key] = map[string]reviewBacklogFingerprintRecord{}
	}
	l.Fingerprints[key][fp] = rec
}

const reviewBacklogDayLayout = "2006-01-02"

func reviewBacklogDayKey(prKey string, now time.Time) string {
	return prKey + "@" + now.UTC().Format(reviewBacklogDayLayout)
}

// pruneDailyCounts drops per-day counters older than yesterday so the ledger
// does not grow without bound.
func (l *reviewBacklogLedger) pruneDailyCounts(now time.Time) {
	cutoff := now.UTC().AddDate(0, 0, -1).Format(reviewBacklogDayLayout)
	for k := range l.PRDailyCount {
		i := strings.LastIndex(k, "@")
		if i < 0 || k[i+1:] < cutoff {
			delete(l.PRDailyCount, k)
		}
	}
}

func reviewPRURL(owner, repo string, number int) string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, number)
}

// reviewFindingPermalink is the file:line link at the reviewed head SHA, or
// "" when the head SHA is unknown.
func reviewFindingPermalink(owner, repo, headSHA string, f outputschema.Finding) string {
	headSHA = strings.TrimSpace(headSHA)
	file := normaliseFingerprintPath(f.File)
	if headSHA == "" || file == "" || owner == "" || repo == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s#L%d", owner, repo, headSHA, file, f.Line)
}

func reviewFindingSeverityLine(f outputschema.Finding) string {
	return fmt.Sprintf("%s (P%d)", f.Severity, ReviewFindingPriority(f.Severity))
}

// reviewBacklogReason is the one-line why for an item body.
func reviewBacklogReason(f outputschema.Finding, blockAt string) string {
	if !review.InScopeFinding(f) {
		return "an out-of-scope finding"
	}
	b, _ := config.NormalizeReviewSeverityBlockAt(blockAt)
	return fmt.Sprintf("a finding below the blocking line (%s)", b)
}

func (c *Client) reviewBacklogIssueBody(req ReviewRequest, p review.Perspective, f outputschema.Finding, headSHA, blockAt, fp string) string {
	owner, repoName := c.splitRepo(req.Repo)
	var b strings.Builder
	fmt.Fprintf(&b, "Filed from %s in the Hive review of PR #%d.\n\n", reviewBacklogReason(f, blockAt), req.Number)
	fmt.Fprintf(&b, "PR: #%d (%s)\n", req.Number, reviewPRURL(owner, repoName, req.Number))
	fmt.Fprintf(&b, "Perspective: %s\n", p)
	fmt.Fprintf(&b, "Severity: %s\n", reviewFindingSeverityLine(f))
	fmt.Fprintf(&b, "Evidence: `%s:%d`", strings.TrimSpace(f.File), f.Line)
	if link := reviewFindingPermalink(owner, repoName, headSHA, f); link != "" {
		fmt.Fprintf(&b, " (%s)", link)
	}
	fmt.Fprintf(&b, "\nFinding: %s\n\n%s\n\nFingerprint: `%s`\n\n%s\n", strings.TrimSpace(f.Title), strings.TrimSpace(f.Summary), fp, reviewBacklogFooter)
	return b.String()
}

func (c *Client) reviewBacklogReRaiseComment(req ReviewRequest, p review.Perspective, f outputschema.Finding, headSHA, fp string) string {
	owner, repoName := c.splitRepo(req.Repo)
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s -->\n", reviewBacklogReRaiseMarker, fp)
	fmt.Fprintf(&b, "Raised again in the Hive review of PR #%d (%s).\n\n", req.Number, reviewPRURL(owner, repoName, req.Number))
	fmt.Fprintf(&b, "Perspective: %s\n", p)
	fmt.Fprintf(&b, "Severity: %s\n", reviewFindingSeverityLine(f))
	fmt.Fprintf(&b, "Evidence: `%s:%d`", strings.TrimSpace(f.File), f.Line)
	if link := reviewFindingPermalink(owner, repoName, headSHA, f); link != "" {
		fmt.Fprintf(&b, " (%s)", link)
	}
	fmt.Fprintf(&b, "\n\n%s\n", reviewBacklogFooter)
	return b.String()
}

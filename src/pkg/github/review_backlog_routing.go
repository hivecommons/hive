package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
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

const reviewBacklogSameFileMarker = "<!-- hive:review-backlog-same-file "

// reviewBacklogFoldLineWindow is how far apart two citations of one file may
// be and still count as the same spot for folding (#13906).
const reviewBacklogFoldLineWindow = 10

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
	// Perspective and Finding are the review finding the item files, so a
	// sink that consolidates into open twins folds only on a matching
	// finding, never on a shared file alone (#13906).
	Perspective review.Perspective
	Finding     outputschema.Finding
	// SplitFrom is the closed host this item is being split out of by the
	// review-backlog split sweep (#13914); 0 for a fresh filing. A sink
	// that suppresses findings a maintainer rejected must not count the
	// host as the rejection: its decision is already applied by
	// reviewBacklogSplitEligible.
	SplitFrom int
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
	// SameFileAs is the GitHub issue number of an open item that cites the
	// same file but was not folded into because it describes a different
	// finding (#13906); 0 when there is none.
	SameFileAs int
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
// reviewed PR's repo, created through createIssue with open-twin
// consolidation narrowed by reviewBacklogFoldMatches, so a review finding
// folds into an open issue only when it is the same finding, not merely the
// same file (#13906).
func (c *Client) ReviewBacklogIssueSink() ReviewBacklogSink {
	return githubIssueBacklogSink{c: c}
}

type githubIssueBacklogSink struct{ c *Client }

func (githubIssueBacklogSink) Destination() string { return config.ReviewBacklogGitHubIssue }

func (s githubIssueBacklogSink) CreateItem(ctx context.Context, item ReviewBacklogItem) (ReviewBacklogRef, error) {
	foldMatch := func(title, body string) bool {
		return reviewBacklogFoldMatches(item.Perspective, item.Finding, title, body)
	}
	res, err := s.c.createIssue(ctx, CreateIssueRequest{Repo: item.Repo, Title: item.Title, Body: item.Body, Labels: item.Labels, Origin: config.OriginSourceReviewer, ConsolidateOpen: true, FoldMatch: foldMatch, IgnoreClosedTwin: item.SplitFrom})
	if err != nil {
		return ReviewBacklogRef{}, err
	}
	if res.Number <= 0 {
		return ReviewBacklogRef{}, nil
	}
	if res.RejectedTwin {
		// Nothing was filed: a maintainer recently closed an agent issue
		// over the same file set as not-planned/duplicate. Number names
		// that closed issue, not a backlog item, so it must not be handed
		// back as one. For a split out of a closed host the host itself is
		// excluded above (IgnoreClosedTwin), so this is some other
		// rejection and the finding stays suppressed.
		return ReviewBacklogRef{}, nil
	}
	if res.AlreadyExisted {
		// hivecommons/hive#13364: the twin's own state governs. A blocked
		// filing folded into an open twin keeps its labels but loses its
		// body, so propagating `blocked` would park the twin with no
		// blocked-on marker and nothing would ever lift it.
		if err := s.c.ensureReviewBacklogLabels(ctx, item.Repo, res.Number, reusedTwinLabels(item.Labels)); err != nil {
			return ReviewBacklogRef{}, err
		}
	}
	return ReviewBacklogRef{
		ID:         strconv.Itoa(res.Number),
		Key:        fmt.Sprintf("#%d", res.Number),
		Number:     res.Number,
		URL:        res.URL,
		Reused:     res.AlreadyExisted,
		SameFileAs: res.SameFileAs,
	}, nil
}

var (
	// The optional leading `[<agent>]` tag keeps items filed before titles
	// carried the filer in the same category as their prefixed successors.
	reviewBacklogTitleCategoryRE = regexp.MustCompile(`^\s*(?:\[[^\]\s]+\]\s*)?Review backlog \(([^)]+)\):`)
	reviewBacklogPerspectiveRE   = regexp.MustCompile(`(?m)^\s*Perspective:\s*(\S+)`)
	reviewBacklogFileLineRE      = regexp.MustCompile(`([A-Za-z0-9_./-]+):(\d+)\b`)
	reviewBacklogFileNameRE      = regexp.MustCompile(`[A-Za-z0-9_./-]*[A-Za-z0-9_-]\.[a-z]{1,5}\b`)
	reviewBacklogBacktickRE      = regexp.MustCompile("`([^`]+)`")
	reviewBacklogIdentRE         = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(\()?`)
)

// reviewBacklogNonSymbols are words that read like identifiers but name
// products or common literals, so sharing one says nothing about sharing a
// symbol.
var reviewBacklogNonSymbols = map[string]bool{
	"github": true, "gitlab": true, "javascript": true, "typescript": true,
	"macos": true, "postgresql": true, "youtube": true,
	"true": true, "false": true, "null": true, "error": true, "string": true,
}

// reviewBacklogFoldMatches reports whether a review finding is the same
// finding as an open issue (title, body) that cites the same file, so it may
// be folded in rather than filed (#13906). Both must be in the same category
// (review perspective), and the finding must cite the issue's file within
// reviewBacklogFoldLineWindow lines or name a symbol the issue's title names.
// An issue with no recognisable category never matches.
func reviewBacklogFoldMatches(p review.Perspective, f outputschema.Finding, title, body string) bool {
	want := strings.ToLower(strings.TrimSpace(string(p)))
	if want == "" {
		return false
	}
	if cat := reviewBacklogItemCategory(title, body); cat != want {
		// Another perspective's item folds only when it is the same
		// finding verbatim: same title once per-pass wrapping is stripped,
		// cited at the same place (hivecommons/hive#15627).
		n := reviewBacklogNormalisedTitle(f.Title)
		return cat != "" && n != "" && n == reviewBacklogNormalisedTitle(title) && reviewBacklogNearbyCitation(f, title+"\n"+body)
	}
	return reviewBacklogNearbyCitation(f, title+"\n"+body) || reviewBacklogSharesSymbol(f.Title, title)
}

// reviewBacklogItemCategory is the review perspective an item was filed
// under, from its "Review backlog (<perspective>):" title or its
// "Perspective:" line, lower-cased; "" when it names none.
func reviewBacklogItemCategory(title, body string) string {
	if m := reviewBacklogTitleCategoryRE.FindStringSubmatch(title); m != nil {
		return strings.ToLower(strings.TrimSpace(m[1]))
	}
	if m := reviewBacklogPerspectiveRE.FindStringSubmatch(body); m != nil {
		return strings.ToLower(strings.TrimSpace(m[1]))
	}
	return ""
}

func reviewBacklogNearbyCitation(f outputschema.Finding, text string) bool {
	file := normaliseFingerprintPath(f.File)
	if file == "" || f.Line <= 0 {
		return false
	}
	for _, m := range reviewBacklogFileLineRE.FindAllStringSubmatch(text, -1) {
		if normaliseFingerprintPath(m[1]) != file {
			continue
		}
		line, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		if d := line - f.Line; d >= -reviewBacklogFoldLineWindow && d <= reviewBacklogFoldLineWindow {
			return true
		}
	}
	return false
}

func reviewBacklogSharesSymbol(a, b string) bool {
	want := reviewBacklogSymbols(a)
	if len(want) == 0 {
		return false
	}
	for s := range reviewBacklogSymbols(b) {
		if want[s] {
			return true
		}
	}
	return false
}

// reviewBacklogSymbols is the set of code identifiers a title names:
// backticked identifiers, calls ("name("), camelCase/PascalCase words with an
// inner capital, and snake_case words. File names are dropped first so a
// shared file is never mistaken for a shared symbol.
func reviewBacklogSymbols(s string) map[string]bool {
	out := map[string]bool{}
	add := func(tok string) {
		if len(tok) >= 4 && !reviewBacklogNonSymbols[strings.ToLower(tok)] {
			out[tok] = true
		}
	}
	for _, m := range reviewBacklogBacktickRE.FindAllStringSubmatch(s, -1) {
		for _, tok := range reviewBacklogIdentRE.FindAllStringSubmatch(reviewBacklogFileNameRE.ReplaceAllString(m[1], " "), -1) {
			add(tok[0][:len(tok[0])-len(tok[1])])
		}
	}
	for _, tok := range reviewBacklogIdentRE.FindAllStringSubmatch(reviewBacklogFileNameRE.ReplaceAllString(s, " "), -1) {
		name := tok[0][:len(tok[0])-len(tok[1])]
		if tok[1] != "" || strings.Contains(strings.Trim(name, "_"), "_") || hasInnerCapital(name) {
			add(name)
		}
	}
	return out
}

func hasInnerCapital(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' && s[i-1] >= 'a' && s[i-1] <= 'z' {
			return true
		}
	}
	return false
}

func (s githubIssueBacklogSink) AppendToItem(ctx context.Context, repo string, ref ReviewBacklogRef, comment string) error {
	if ref.Number <= 0 {
		return fmt.Errorf("review backlog: GitHub item %q has no issue number", ref.ID)
	}
	return s.c.CreateIssueComment(ctx, repo, ref.Number, comment)
}

// ReviewBacklogStateChecker is an optional sink capability: sinks that can
// tell whether a filed item is still open let the filer avoid appending
// re-raised findings to closed items.
type ReviewBacklogStateChecker interface {
	ItemOpen(ctx context.Context, repo string, ref ReviewBacklogRef) (bool, error)
}

// ItemOpen reports whether the ledger's GitHub issue is still open.
func (s githubIssueBacklogSink) ItemOpen(ctx context.Context, repo string, ref ReviewBacklogRef) (bool, error) {
	if ref.Number <= 0 {
		return false, fmt.Errorf("review backlog: GitHub item %q has no issue number", ref.ID)
	}
	if s.c == nil || s.c.client == nil {
		return false, ErrNoGitHubClient
	}
	owner, name := s.c.splitRepo(repo)
	issue, _, err := s.c.client.Issues.Get(ctx, owner, name, ref.Number)
	if err != nil {
		return false, err
	}
	return !strings.EqualFold(issue.GetState(), "closed"), nil
}

// reviewBacklogTarget resolves the sink and labels for one filing pass. When
// the configured destination could not be built (its work source is not
// active or not configured) the sink is the GitHub issue fallback, and the
// agent/reviewer label is always applied so the fallback items stay findable.
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

// reusedTwinLabels is the label set applied to an already-open twin a
// filing folds into: the filing's labels minus `blocked`. The marker that
// names the blocking PR lives in the discarded body, and the twin's earlier
// evidence stands on its own (hivecommons/hive#13364).
func reusedTwinLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), BlockedLabel) {
			continue
		}
		out = append(out, l)
	}
	return out
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
	// BlockedOnPR is the reviewed PR the item was filed from and waits on
	// (hivecommons/hive#13833); 0 for items filed before blocking applied to
	// every item, and for filings folded into an existing twin.
	BlockedOnPR int `json:"blocked_on_pr,omitempty"`
	// BlockState is blocked, lifted or retriage; ReconcileReviewBacklogBlocks
	// moves it on when BlockedOnPR merges or closes.
	BlockState string `json:"block_state,omitempty"`
	// File and Title are the finding's evidence file (repo-relative) and
	// title, kept so the block sweep can check whether the finding still
	// applies on the base once BlockedOnPR merges (hivecommons/hive#15627).
	File  string `json:"file,omitempty"`
	Title string `json:"title,omitempty"`
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

// reviewBacklogFoldedComment carries a whole finding onto an existing open
// item the sink reused instead of creating one (hivecommons/hive#13604). The
// GitHub sink reuses a twin only on the same subject, or on the same file
// with a matching symbol or nearby line and the same category (#13906); the
// whole finding is still carried so nothing is lost if the match was wrong
// and the twin closes. The hive:review-backlog-link marker keyed on the
// finding key is kept so the ledger and comment dedup are unchanged.
// parseReviewBacklogFoldedComment (review_backlog_split.go) reads this
// layout back when the host closes; keep the two in sync.
func (c *Client) reviewBacklogFoldedComment(req ReviewRequest, p review.Perspective, f outputschema.Finding, headSHA, fp, key string) string {
	owner, repoName := c.splitRepo(req.Repo)
	file := strings.TrimSpace(f.File)
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s -->\n", reviewBacklogLinkMarker, key)
	fmt.Fprintf(&b, "**Additional finding (folded in — same finding area and category).** Raised in the Hive review of PR #%d (%s) and folded into this issue because it is a %s finding at the same symbol or lines of `%s` as this issue. Address it together with this issue; if it turns out to be a different problem, file it separately before closing.\n\n", req.Number, reviewPRURL(owner, repoName, req.Number), p, file)
	fmt.Fprintf(&b, "### %s\n\n", reviewBacklogIssueTitle(p, f))
	fmt.Fprintf(&b, "- [ ] %s (`%s:%d`)\n\n", strings.TrimSpace(f.Title), file, f.Line)
	fmt.Fprintf(&b, "Perspective: %s\n", p)
	fmt.Fprintf(&b, "Severity: %s\n", reviewFindingSeverityLine(f))
	fmt.Fprintf(&b, "Evidence: `%s:%d`", file, f.Line)
	if link := reviewFindingPermalink(owner, repoName, headSHA, f); link != "" {
		fmt.Fprintf(&b, " (%s)", link)
	}
	fmt.Fprintf(&b, "\nFinding: %s\n\n%s\n\nFingerprint: `%s`\n\n%s\n", strings.TrimSpace(f.Title), strings.TrimSpace(f.Summary), fp, reviewBacklogFooter)
	return b.String()
}

// reviewBacklogSameFileComment cross-links a newly filed item to the open
// issue that cites the same file but was not folded into because it is a
// different finding (#13906). GitHub backlinks the reference on that issue.
func reviewBacklogSameFileComment(f outputschema.Finding, sameFileAs int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s%d -->\n", reviewBacklogSameFileMarker, sameFileAs)
	fmt.Fprintf(&b, "Same file as #%d: both cite `%s`, but this finding does not match that issue's symbol, line range or category, so it was filed separately instead of folded in.\n\n%s\n", sameFileAs, strings.TrimSpace(f.File), reviewBacklogFooter)
	return b.String()
}

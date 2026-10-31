package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/hivecommons/hive/pkg/prstatus"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/issueshape"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

const (
	DefaultReviewBacklogIssueCap = 3
	reviewBacklogFile            = "review-backlog-issues.json"
	reviewBacklogLabel           = config.DefaultReviewBacklogLabel
	reviewBacklogSummaryMarker   = "<!-- hive:review-backlog-summary v1 -->"
	reviewBacklogLinkMarker      = "<!-- hive:review-backlog-link "
)

var ReviewBacklogPath = filepath.Join(review.DefaultDispatchStateDir, reviewBacklogFile)

type reviewBacklogLedger struct {
	GeneratedAt       time.Time                      `json:"generated_at"`
	Items             map[string]reviewBacklogRecord `json:"items"`
	SummaryCommented  map[string]bool                `json:"summary_commented,omitempty"`
	PRFiledIssueCount map[string]int                 `json:"pr_filed_issue_count,omitempty"`
	// Fingerprints maps repo → finding fingerprint → filed item, so a
	// finding re-raised on any PR updates its existing item.
	Fingerprints map[string]map[string]reviewBacklogFingerprintRecord `json:"fingerprints,omitempty"`
	// PRDailyCount counts items filed or updated per "<pr key>@<UTC day>".
	PRDailyCount map[string]int `json:"pr_daily_count,omitempty"`
	// SplitKeys maps a folded finding's link-marker key to every closed
	// host issue it has been split out of (hivecommons/hive#13914). A
	// finding can be split more than once when the split lands in an open
	// twin that later closes, so this is a set per key: a re-run of the
	// split sweep never files the same finding twice from any one host.
	SplitKeys map[string][]int `json:"split_keys,omitempty"`
	// SplitBackfilled records that BackfillReviewBacklogSplits walked every
	// host in the ledger once (hivecommons/hive#13947), so later passes
	// skip it and leave closed hosts to the windowed split sweep.
	SplitBackfilled bool `json:"split_backfilled,omitempty"`
	// PRFindings maps "<repo>#<pr>" → the findings filed or tracked from
	// that PR, so a later perspective or head reporting the same finding is
	// tracked against the existing item instead of filed again
	// (hivecommons/hive#15627).
	PRFindings map[string][]reviewBacklogPRFinding `json:"pr_findings,omitempty"`
	// StillPresentHeads maps "<repo>#<pr>:<item id>" → the last head a "still
	// present" note was posted for, so each item gets at most one per head.
	StillPresentHeads map[string]string `json:"still_present_heads,omitempty"`
}

// reviewBacklogLedgerMu serialises the load→mutate→save cycles of the
// ledger writers (fileOutOfScopeReviewBacklog, SweepReviewBacklogSplits and
// ReconcileReviewBacklogBlocks), so a concurrent save cannot drop another
// writer's update. They hold it across their CreateItem/AppendToItem calls on purpose: the ledger records
// the issue each call creates, so releasing it between the call and the
// save would reopen the lost-update window. The cost is that the run
// loop's split sweep waits while a review's backlog filing is in flight,
// bounded by the review's per-PR daily cap and the sweep's per-pass cap.
var reviewBacklogLedgerMu sync.Mutex

// splitFrom reports whether key was already split out of host.
func (l *reviewBacklogLedger) splitFrom(key string, host int) bool {
	for _, h := range l.SplitKeys[key] {
		if h == host {
			return true
		}
	}
	return false
}

// markSplitFrom adds host to the set of hosts key was split out of.
func (l *reviewBacklogLedger) markSplitFrom(key string, host int) {
	if l.splitFrom(key, host) {
		return
	}
	l.SplitKeys[key] = append(l.SplitKeys[key], host)
}

type reviewBacklogRecord struct {
	Repo        string    `json:"repo"`
	PRNumber    int       `json:"pr_number"`
	HeadSHA     string    `json:"head_sha,omitempty"`
	Perspective string    `json:"perspective"`
	IssueNumber int       `json:"issue_number"`
	IssueURL    string    `json:"issue_url,omitempty"`
	FindingKey  string    `json:"finding_key"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Destination string    `json:"destination,omitempty"`
	ItemKey     string    `json:"item_key,omitempty"`
	RecordedAt  time.Time `json:"recorded_at"`
}

type reviewBacklogFiled struct {
	number int
	key    string
	url    string
	title  string
	// folded is true when the finding was appended to an existing open item
	// instead of becoming its own (hivecommons/hive#13604).
	folded bool
	// status is how this pass touched the item, for the decided review's
	// Follow-ups section (hivecommons/hive#13833).
	status string
}

// How a review's finding maps to a backlog item, as listed in the decided
// review's Follow-ups section.
const (
	reviewBacklogListedFiled   = "filed"
	reviewBacklogListedUpdated = "re-raised"
	reviewBacklogListedTracked = "already tracked"
)

func newReviewBacklogLedger() *reviewBacklogLedger {
	return &reviewBacklogLedger{
		Items:             map[string]reviewBacklogRecord{},
		SummaryCommented:  map[string]bool{},
		PRFiledIssueCount: map[string]int{},
		Fingerprints:      map[string]map[string]reviewBacklogFingerprintRecord{},
		PRDailyCount:      map[string]int{},
		SplitKeys:         map[string][]int{},
		PRFindings:        map[string][]reviewBacklogPRFinding{},
		StillPresentHeads: map[string]string{},
	}
}

func loadReviewBacklog(path string) (*reviewBacklogLedger, error) {
	if path == "" {
		path = ReviewBacklogPath
	}
	l := newReviewBacklogLedger()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return l, err
	}
	if err := json.Unmarshal(data, l); err != nil {
		return newReviewBacklogLedger(), err
	}
	if l.Items == nil {
		l.Items = map[string]reviewBacklogRecord{}
	}
	if l.SummaryCommented == nil {
		l.SummaryCommented = map[string]bool{}
	}
	if l.PRFiledIssueCount == nil {
		l.PRFiledIssueCount = map[string]int{}
	}
	if l.Fingerprints == nil {
		l.Fingerprints = map[string]map[string]reviewBacklogFingerprintRecord{}
	}
	if l.PRDailyCount == nil {
		l.PRDailyCount = map[string]int{}
	}
	if l.SplitKeys == nil {
		l.SplitKeys = map[string][]int{}
	}
	if l.PRFindings == nil {
		l.PRFindings = map[string][]reviewBacklogPRFinding{}
	}
	if l.StillPresentHeads == nil {
		l.StillPresentHeads = map[string]string{}
	}
	return l, nil
}

func (l *reviewBacklogLedger) save(path string, now time.Time) error {
	if path == "" {
		path = ReviewBacklogPath
	}
	l.GeneratedAt = now.UTC()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// fileOutOfScopeReviewBacklog files review findings that should not block the
// PR as follow-up items: cited out-of-scope findings (bounded by the legacy
// per-PR cap) and, when review.severity.block_at is set with backlog_below,
// cited in-scope findings below the blocking line. Items go to the
// review.backlog destination (hivecommons/hive#11089) and are de-duplicated
// per repo by ReviewFindingFingerprint: a finding another PR already filed is
// appended to the existing item instead of filed again. Every item filed or
// updated counts toward the per-PR daily cap, and each pass that wrote
// anything leaves one batch audit entry.
//
// Findings in files changed by the PR or missing on its base are parked on
// that PR; findings in untouched files can be worked immediately (#14473).
// ReconcileReviewBacklogBlocks lifts the
// block when the PR merges, and unblocks the item for re-triage (never closes
// it) when the PR closes unmerged.
func (c *Client) fileOutOfScopeReviewBacklog(ctx context.Context, req ReviewRequest, now time.Time) error {
	return c.fileReviewBacklog(ctx, req, now, nil)
}

// fileReviewBacklog is fileOutOfScopeReviewBacklog with the review the relay
// just posted for this request. In heads mode that review is the decision
// step's, and the items filed (or found already tracked) are listed in it,
// edited in place, instead of in a once-per-PR summary comment.
func (c *Client) fileReviewBacklog(ctx context.Context, req ReviewRequest, now time.Time, posted *gh.PullRequestReview) error {
	outOfScopeEnabled, outOfScopeCap := c.reviewBacklogConfig()
	routing, routed := c.reviewBacklogRoutingConfig()
	belowLine := routing.belowLineBacklogEnabled()
	if (!outOfScopeEnabled || outOfScopeCap <= 0) && !belowLine {
		return nil
	}
	if strings.TrimSpace(req.Report) == "" {
		return nil
	}
	reports, err := review.ValidateReportsFor([]byte(strings.TrimSpace(req.Report)), c.perspectives)
	if err != nil {
		return nil
	}
	reports, decided := c.acceptedReviewBacklogReports(req, reports)
	if len(reports) == 0 {
		return nil
	}
	reviewBacklogLedgerMu.Lock()
	defer reviewBacklogLedgerMu.Unlock()
	ledger, err := loadReviewBacklog("")
	if err != nil {
		return err
	}
	prKey := ReviewLinkKey(req.Repo, req.Number)
	dayKey := reviewBacklogDayKey(prKey, now)
	dailyCap := reviewBacklogDailyCap(routing, routed, outOfScopeCap)
	dailyRemaining := dailyCap - ledger.PRDailyCount[dayKey]
	outOfScopeRemaining := outOfScopeCap - ledger.PRFiledIssueCount[prKey]
	sink, labels := c.reviewBacklogTarget(routing)
	destination := sink.Destination()
	headSHA := reqHeadSHAFromReports(reports)
	// hivecommons/hive#13093: a finding whose evidence file the PR introduces
	// is parked on the PR, not offered as work.
	baseProbe := c.newReviewBacklogBaseProbe(req.Repo, req.Number)
	changedFiles := c.newReviewBacklogChangedFilesProbe(req.Repo, req.Number)
	var filedIssues []reviewBacklogFiled
	// listed is every item this pass's findings map to — filed, re-raised
	// onto an existing item, or already tracked for this PR — for the
	// decided review's Follow-ups section.
	var listed []reviewBacklogFiled
	var updated, capped int
	for _, report := range reports {
		for _, finding := range report.Findings {
			if !findingHasEvidence(finding) {
				continue
			}
			outOfScope := !review.InScopeFinding(finding)
			if outOfScope && !outOfScopeEnabled {
				continue
			}
			if !outOfScope && (!belowLine || !ReviewFindingBelowLine(finding, routing.Severity.BlockAt)) {
				continue
			}
			key := reviewBacklogKey(req.Repo, req.Number, report.Perspective, finding)
			if prev, ok := ledger.Items[key]; ok {
				listed = append(listed, reviewBacklogFiled{number: prev.IssueNumber, key: prev.ItemKey, url: prev.IssueURL, title: reviewBacklogIssueTitle(report.Perspective, finding), status: reviewBacklogListedTracked})
				// The same perspective reporting it again at a new head
				// leaves the once-per-head "still present" note.
				if tracked, isTracked := ledger.prFinding(req.Repo, req.Number, finding); isTracked && dailyRemaining > 0 && ledger.stillPresentDue(req.Repo, req.Number, tracked.ref(), headSHA) {
					if err := sink.AppendToItem(ctx, req.Repo, tracked.ref(), reviewBacklogStillPresentComment(headSHA, []review.Perspective{report.Perspective})); err != nil {
						return err
					}
					ledger.PRDailyCount[dayKey]++
					dailyRemaining--
				}
				continue
			}
			fp := ReviewFindingFingerprint(finding)
			record := reviewBacklogRecord{
				Repo:        req.Repo,
				PRNumber:    req.Number,
				HeadSHA:     headSHA,
				Perspective: string(report.Perspective),
				FindingKey:  key,
				Fingerprint: fp,
				Destination: destination,
				RecordedAt:  now.UTC(),
			}
			known, ok := ledger.fingerprint(req.Repo, fp)
			// One item per finding per PR (hivecommons/hive#15627): a
			// report that names a finding already tracked for this PR —
			// another perspective's, or an earlier head's carried over —
			// is a reference, not a filing. A carry-over that refers to
			// nothing tracked here is dropped outright.
			carryOver := reviewBacklogCarryOver(finding)
			tracked, isTracked := ledger.prFinding(req.Repo, req.Number, finding)
			if !isTracked {
				for _, n := range reviewRefersTo(finding) {
					if tracked, isTracked = ledger.prFindingByItem(req.Repo, req.Number, n); isTracked {
						break
					}
				}
			}
			if isTracked {
				record.IssueNumber, record.IssueURL, record.ItemKey = tracked.ItemNumber, tracked.ItemURL, tracked.ItemKey
				ledger.Items[key] = record
				listed = append(listed, reviewBacklogFiled{number: tracked.ItemNumber, key: tracked.ItemKey, url: tracked.ItemURL, title: reviewBacklogIssueTitle(report.Perspective, finding), status: reviewBacklogListedTracked})
				if dailyRemaining > 0 && ledger.stillPresentDue(req.Repo, req.Number, tracked.ref(), headSHA) {
					if err := sink.AppendToItem(ctx, req.Repo, tracked.ref(), reviewBacklogStillPresentComment(headSHA, []review.Perspective{report.Perspective})); err != nil {
						return err
					}
					ledger.PRDailyCount[dayKey]++
					dailyRemaining--
				}
				continue
			}
			if carryOver {
				c.logger.Info("review backlog: carried-over note is a reference, not filed",
					slog.String("repo", req.Repo), slog.Int("pr", req.Number),
					slog.String("perspective", string(report.Perspective)), slog.String("title", strings.TrimSpace(finding.Title)))
				continue
			}
			if ok && known.Destination == destination && known.ItemID != "" {
				// A closed item must not collect new re-raises; drop the
				// ledger entry so the finding is re-filed (the create path
				// still folds into any open twin). A failed lookup keeps the
				// old append behaviour.
				if checker, can := sink.(ReviewBacklogStateChecker); can && !known.hasPR(req.Number) {
					if open, err := checker.ItemOpen(ctx, req.Repo, known.ref()); err == nil && !open {
						delete(ledger.Fingerprints[reviewBacklogRepoKey(req.Repo)], fp)
						ok = false
					}
				}
			}
			if ok && known.Destination == destination && known.ItemID != "" {
				record.IssueNumber, record.IssueURL, record.ItemKey = known.ItemNumber, known.ItemURL, known.ItemKey
				if known.hasPR(req.Number) {
					ledger.Items[key] = record
					listed = append(listed, reviewBacklogFiled{number: known.ItemNumber, key: known.ItemKey, url: known.ItemURL, title: reviewBacklogIssueTitle(report.Perspective, finding), status: reviewBacklogListedTracked})
					continue
				}
				if dailyRemaining <= 0 {
					capped++
					continue
				}
				if err := sink.AppendToItem(ctx, req.Repo, known.ref(), c.reviewBacklogReRaiseComment(req, report.Perspective, finding, headSHA, fp)); err != nil {
					return err
				}
				known.PRs = append(known.PRs, req.Number)
				known.UpdatedAt = now.UTC()
				ledger.setFingerprint(req.Repo, fp, known)
				ledger.Items[key] = record
				ledger.addPRFinding(req.Repo, req.Number, finding, known.ref(), headSHA)
				ledger.PRDailyCount[dayKey]++
				dailyRemaining--
				updated++
				listed = append(listed, reviewBacklogFiled{number: known.ItemNumber, key: known.ItemKey, url: known.ItemURL, title: reviewBacklogIssueTitle(report.Perspective, finding), status: reviewBacklogListedUpdated})
				continue
			}
			if dailyRemaining <= 0 || (outOfScope && outOfScopeRemaining <= 0) {
				capped++
				continue
			}
			title := reviewBacklogIssueTitle(report.Perspective, finding)
			missing := baseProbe.FileMissingOnBase(ctx, finding.File)
			touched, filesKnown := false, true
			if !missing {
				touched, filesKnown = changedFiles(ctx, finding.File)
			}
			blocked := missing || touched || !filesKnown
			itemLabels := labels
			if blocked {
				itemLabels = blockedBacklogLabels(labels)
			}
			body := c.reviewBacklogIssueBody(req, report.Perspective, finding, headSHA, routing.Severity.BlockAt, fp)
			if missing {
				body = blockedBacklogNote(req.Repo, req.Number, finding.File, baseProbe.BaseRef(ctx)) + body
			} else if blocked {
				note := blockedOnReviewedPRNote(req.Repo, req.Number)
				if !filesKnown {
					note = strings.Replace(note, "is about code that PR is changing", "could not be checked against the PR's changed files", 1)
				}
				body = note + body
			}
			ref, err := sink.CreateItem(ctx, ReviewBacklogItem{
				Repo:        req.Repo,
				Title:       title,
				Body:        body,
				Labels:      itemLabels,
				Perspective: report.Perspective,
				Finding:     finding,
			})
			if err != nil && ref.ID == "" {
				return err
			}
			createErr := err
			if ref.ID == "" {
				continue
			}
			if ref.Reused && createErr == nil {
				// hivecommons/hive#13604: the comment must carry the whole
				// finding or it is lost when the twin closes.
				if err := sink.AppendToItem(ctx, req.Repo, ref, c.reviewBacklogFoldedComment(req, report.Perspective, finding, headSHA, fp, key)); err != nil {
					return err
				}
			}
			if !ref.Reused && ref.SameFileAs > 0 && createErr == nil {
				// hivecommons/hive#13906: the item exists, so a failed
				// cross-link must not fail the pass and re-file it.
				if err := sink.AppendToItem(ctx, req.Repo, ref, reviewBacklogSameFileComment(finding, ref.SameFileAs)); err != nil {
					c.logger.Warn("review backlog: same-file cross-link comment failed",
						slog.String("repo", req.Repo), slog.String("item", ref.Key),
						slog.Int("same_file_as", ref.SameFileAs), slog.String("error", err.Error()))
				}
			}
			if ref.Number > 0 {
				c.recordWriteAudit(AuditActionReviewBacklogIssueFiled, hiveWriteMeta(),
					WriteTarget{Repo: req.Repo, Number: ref.Number},
					"pr", strconv.Itoa(req.Number),
					"perspective", string(report.Perspective),
					"reused", strconv.FormatBool(ref.Reused),
					"blocked", strconv.FormatBool(blocked),
					"destination", destination,
					"review_agent", req.Agent)
			}
			record.IssueNumber, record.IssueURL, record.ItemKey = ref.Number, ref.URL, ref.Key
			ledger.Items[key] = record
			ledger.addPRFinding(req.Repo, req.Number, finding, ref, headSHA)
			fpRecord := reviewBacklogFingerprintRecord{
				Destination: destination,
				ItemID:      ref.ID,
				ItemKey:     ref.Key,
				ItemNumber:  ref.Number,
				ItemURL:     ref.URL,
				PRs:         []int{req.Number},
				FiledAt:     now.UTC(),
				UpdatedAt:   now.UTC(),
				File:        normaliseFingerprintPath(finding.File),
				Title:       strings.TrimSpace(finding.Title),
			}
			if !ref.Reused {
				// A filing folded into an open twin keeps the twin's own
				// state (#13364). Keep source provenance for new items,
				// but only park findings related to the PR.
				fpRecord.BlockedOnPR = req.Number
				if blocked {
					fpRecord.BlockState = reviewBacklogBlockBlocked
				}
			}
			ledger.setFingerprint(req.Repo, fp, fpRecord)
			if outOfScope {
				ledger.PRFiledIssueCount[prKey]++
				outOfScopeRemaining--
			}
			ledger.PRDailyCount[dayKey]++
			dailyRemaining--
			filed := reviewBacklogFiled{number: ref.Number, key: ref.Key, url: ref.URL, title: title, folded: ref.Reused, status: reviewBacklogListedFiled}
			filedIssues = append(filedIssues, filed)
			listed = append(listed, filed)
			if createErr != nil {
				// The item exists but a follow-up step (Projects column,
				// Jira status) failed: keep the ledger entry so a retry
				// does not file it twice.
				if err := ledger.save("", now); err != nil {
					return err
				}
				return createErr
			}
		}
	}
	if len(filedIssues) > 0 || updated > 0 {
		c.recordWriteAudit(AuditActionReviewBacklogBatchRouted, hiveWriteMeta(),
			WriteTarget{Repo: req.Repo, Number: req.Number},
			"destination", destination,
			"filed", strconv.Itoa(len(filedIssues)),
			"updated", strconv.Itoa(updated),
			"capped", strconv.Itoa(capped),
			"review_agent", req.Agent)
	}
	ledger.pruneDailyCounts(now)
	if decided {
		// Heads mode (#13833): list what this head's findings map to in the
		// decided review itself, every head, instead of a once-per-PR
		// summary comment that later heads never update.
		if err := ledger.save("", now); err != nil {
			return err
		}
		if len(listed) > 0 {
			c.appendReviewFollowUps(ctx, req, posted, listed, capped)
		}
		return nil
	}
	if len(filedIssues) == 0 {
		return ledger.save("", now)
	}
	sort.SliceStable(filedIssues, func(i, j int) bool {
		if filedIssues[i].number != filedIssues[j].number {
			return filedIssues[i].number < filedIssues[j].number
		}
		return filedIssues[i].key < filedIssues[j].key
	})
	if !ledger.SummaryCommented[prKey] {
		if _, err := c.PostNoteOrFold(ctx, req.Repo, req.Number, prstatus.KeyReviewBacklog, reviewBacklogSummaryComment(filedIssues, dailyCap)); err != nil {
			return err
		}
		c.recordWriteAudit(AuditActionReviewBacklogSummaryPosted, hiveWriteMeta(),
			WriteTarget{Repo: req.Repo, Number: req.Number},
			"issues", strconv.Itoa(len(filedIssues)),
			"review_agent", req.Agent)
		ledger.SummaryCommented[prKey] = true
	}
	return ledger.save("", now)
}

// ensureReviewBacklogLabels adds labels to an existing issue the backlog
// reused, creating any that are missing on the repo.
func (c *Client) ensureReviewBacklogLabels(ctx context.Context, repo string, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	owner, repoName := c.splitRepo(repo)
	for _, l := range labels {
		if err := c.ensureCreateIssueLabel(ctx, owner, repoName, l); err != nil {
			return err
		}
	}
	_, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repoName, number, labels)
	return err
}

// acceptedReviewBacklogReports keeps the reports the relay would record and
// reports whether any is a review-heads decided report. A raw head report
// (hivecommons/hive#13542) is dropped: in heads mode only the decision step's
// reconciled findings become backlog items, otherwise each head files its own
// wording of the same finding, the fingerprint misses the near-duplicates, and
// the duplicates spend the PR's daily cap (hivecommons/hive#13833).
func (c *Client) acceptedReviewBacklogReports(req ReviewRequest, reports []review.PerspectiveReport) ([]review.PerspectiveReport, bool) {
	accepted := make([]review.PerspectiveReport, 0, len(reports))
	decided := false
	for _, report := range reports {
		if !strings.EqualFold(strings.TrimSpace(report.Repo), strings.TrimSpace(req.Repo)) || report.Number != req.Number {
			continue
		}
		auth := c.verdictDispatchAuthorized(report, req)
		if !auth.OK {
			continue
		}
		if auth.ReviewHead != "" && !auth.Decided {
			continue
		}
		if auth.Decided {
			decided = true
		}
		accepted = append(accepted, report)
	}
	return accepted, decided
}

func findingHasEvidence(f outputschema.Finding) bool {
	return strings.TrimSpace(f.File) != "" && f.Line > 0
}

func reviewBacklogKey(repo string, pr int, perspective review.Perspective, f outputschema.Finding) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.Join([]string{
		strings.TrimSpace(repo),
		strconv.Itoa(pr),
		string(perspective),
		strings.TrimSpace(f.Title),
		strings.TrimSpace(f.File),
		strconv.Itoa(f.Line),
	}, "\x00"))))
	return hex.EncodeToString(h[:])
}

// reviewBacklogIssueTitle is the title of a review backlog item:
// `[reviewer] Review backlog (<perspective>): <finding>`. The leading tag
// names the filing agent, as every hive-filed issue title does.
func reviewBacklogIssueTitle(p review.Perspective, f outputschema.Finding) string {
	title := strings.TrimSpace(f.Title)
	if title == "" {
		title = "out-of-scope review finding"
	}
	return issueshape.AgentIssueTitle(config.OriginSourceReviewer, fmt.Sprintf("Review backlog (%s): %s", p, title))
}

func reviewBacklogSummaryComment(issues []reviewBacklogFiled, cap int) string {
	var b strings.Builder
	b.WriteString(reviewBacklogSummaryMarker)
	b.WriteString("\nHive filed review findings that do not block this PR as backlog items:\n")
	for _, issue := range issues {
		ref := issue.key
		if ref == "" {
			ref = fmt.Sprintf("#%d", issue.number)
		}
		if issue.folded {
			// The item's own title describes another finding; say so
			// rather than imply it is titled like this one (#13604).
			ref += " (folded into an existing issue)"
		}
		if issue.url != "" {
			fmt.Fprintf(&b, "- %s — %s (%s)\n", ref, issue.title, issue.url)
		} else {
			fmt.Fprintf(&b, "- %s — %s\n", ref, issue.title)
		}
	}
	fmt.Fprintf(&b, "\nCap: %d backlog item(s) per PR per day.", cap)
	return b.String()
}

func reqHeadSHAFromReports(reports []review.PerspectiveReport) string {
	for _, r := range reports {
		if strings.TrimSpace(r.HeadSHA) != "" {
			return strings.TrimSpace(r.HeadSHA)
		}
	}
	return ""
}

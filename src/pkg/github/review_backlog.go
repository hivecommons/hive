package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

const (
	DefaultReviewBacklogIssueCap = 3
	reviewBacklogFile            = "review-backlog-issues.json"
	reviewBacklogLabel           = "from-review"
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
}

func newReviewBacklogLedger() *reviewBacklogLedger {
	return &reviewBacklogLedger{
		Items:             map[string]reviewBacklogRecord{},
		SummaryCommented:  map[string]bool{},
		PRFiledIssueCount: map[string]int{},
		Fingerprints:      map[string]map[string]reviewBacklogFingerprintRecord{},
		PRDailyCount:      map[string]int{},
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
func (c *Client) fileOutOfScopeReviewBacklog(ctx context.Context, req ReviewRequest, now time.Time) error {
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
	reports = c.acceptedReviewBacklogReports(req, reports)
	if len(reports) == 0 {
		return nil
	}
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
	var filedIssues []reviewBacklogFiled
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
			if _, ok := ledger.Items[key]; ok {
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
			if known, ok := ledger.fingerprint(req.Repo, fp); ok && known.Destination == destination && known.ItemID != "" {
				record.IssueNumber, record.IssueURL, record.ItemKey = known.ItemNumber, known.ItemURL, known.ItemKey
				if known.hasPR(req.Number) {
					ledger.Items[key] = record
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
				ledger.PRDailyCount[dayKey]++
				dailyRemaining--
				updated++
				continue
			}
			if dailyRemaining <= 0 || (outOfScope && outOfScopeRemaining <= 0) {
				capped++
				continue
			}
			title := reviewBacklogIssueTitle(report.Perspective, finding)
			ref, err := sink.CreateItem(ctx, ReviewBacklogItem{
				Repo:   req.Repo,
				Title:  title,
				Body:   c.reviewBacklogIssueBody(req, report.Perspective, finding, headSHA, routing.Severity.BlockAt, fp),
				Labels: labels,
			})
			if err != nil && ref.ID == "" {
				return err
			}
			createErr := err
			if ref.ID == "" {
				continue
			}
			if ref.Reused && createErr == nil {
				if err := sink.AppendToItem(ctx, req.Repo, ref, reviewBacklogExistingLinkComment(req, key)); err != nil {
					return err
				}
			}
			if ref.Number > 0 {
				c.recordWriteAudit(AuditActionReviewBacklogIssueFiled, hiveWriteMeta(),
					WriteTarget{Repo: req.Repo, Number: ref.Number},
					"pr", strconv.Itoa(req.Number),
					"perspective", string(report.Perspective),
					"reused", strconv.FormatBool(ref.Reused),
					"destination", destination,
					"review_agent", req.Agent)
			}
			record.IssueNumber, record.IssueURL, record.ItemKey = ref.Number, ref.URL, ref.Key
			ledger.Items[key] = record
			ledger.setFingerprint(req.Repo, fp, reviewBacklogFingerprintRecord{
				Destination: destination,
				ItemID:      ref.ID,
				ItemKey:     ref.Key,
				ItemNumber:  ref.Number,
				ItemURL:     ref.URL,
				PRs:         []int{req.Number},
				FiledAt:     now.UTC(),
				UpdatedAt:   now.UTC(),
			})
			if outOfScope {
				ledger.PRFiledIssueCount[prKey]++
				outOfScopeRemaining--
			}
			ledger.PRDailyCount[dayKey]++
			dailyRemaining--
			filedIssues = append(filedIssues, reviewBacklogFiled{number: ref.Number, key: ref.Key, url: ref.URL, title: title})
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
		if err := c.CreateIssueComment(ctx, req.Repo, req.Number, reviewBacklogSummaryComment(filedIssues, dailyCap)); err != nil {
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

func (c *Client) acceptedReviewBacklogReports(req ReviewRequest, reports []review.PerspectiveReport) []review.PerspectiveReport {
	accepted := make([]review.PerspectiveReport, 0, len(reports))
	for _, report := range reports {
		if !strings.EqualFold(strings.TrimSpace(report.Repo), strings.TrimSpace(req.Repo)) || report.Number != req.Number {
			continue
		}
		ok, _, _ := c.verdictDispatchAuthorized(report, req)
		if !ok {
			continue
		}
		accepted = append(accepted, report)
	}
	return accepted
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

func reviewBacklogIssueTitle(p review.Perspective, f outputschema.Finding) string {
	title := strings.TrimSpace(f.Title)
	if title == "" {
		title = "out-of-scope review finding"
	}
	return fmt.Sprintf("Review backlog (%s): %s", p, title)
}

func reviewBacklogExistingLinkComment(req ReviewRequest, key string) string {
	return fmt.Sprintf("%s%s -->\nLinked again from the Hive review of PR #%d.", reviewBacklogLinkMarker, key, req.Number)
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

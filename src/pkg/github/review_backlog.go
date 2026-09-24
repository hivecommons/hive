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
}

type reviewBacklogRecord struct {
	Repo        string    `json:"repo"`
	PRNumber    int       `json:"pr_number"`
	HeadSHA     string    `json:"head_sha,omitempty"`
	Perspective string    `json:"perspective"`
	IssueNumber int       `json:"issue_number"`
	IssueURL    string    `json:"issue_url,omitempty"`
	FindingKey  string    `json:"finding_key"`
	RecordedAt  time.Time `json:"recorded_at"`
}

type reviewBacklogFiled struct {
	number int
	url    string
	title  string
}

func loadReviewBacklog(path string) (*reviewBacklogLedger, error) {
	if path == "" {
		path = ReviewBacklogPath
	}
	l := &reviewBacklogLedger{
		Items:             map[string]reviewBacklogRecord{},
		SummaryCommented:  map[string]bool{},
		PRFiledIssueCount: map[string]int{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return l, err
	}
	if err := json.Unmarshal(data, l); err != nil {
		return &reviewBacklogLedger{
			Items:             map[string]reviewBacklogRecord{},
			SummaryCommented:  map[string]bool{},
			PRFiledIssueCount: map[string]int{},
		}, err
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

func (c *Client) fileOutOfScopeReviewBacklog(ctx context.Context, req ReviewRequest, now time.Time) error {
	enabled, cap := c.reviewBacklogConfig()
	if !enabled || cap <= 0 || strings.TrimSpace(req.Report) == "" {
		return nil
	}
	reports, err := review.ValidateReportsFor([]byte(strings.TrimSpace(req.Report)), c.perspectives)
	if err != nil {
		return nil
	}
	reports = acceptedReviewBacklogReports(req, reports)
	if len(reports) == 0 {
		return nil
	}
	ledger, err := loadReviewBacklog("")
	if err != nil {
		return err
	}
	prKey := ReviewLinkKey(req.Repo, req.Number)
	remaining := cap - ledger.PRFiledIssueCount[prKey]
	if remaining <= 0 {
		return nil
	}
	var filedIssues []reviewBacklogFiled
	for _, report := range reports {
		for _, finding := range report.Findings {
			if review.InScopeFinding(finding) || !findingHasEvidence(finding) {
				continue
			}
			key := reviewBacklogKey(req.Repo, req.Number, report.Perspective, finding)
			if _, ok := ledger.Items[key]; ok {
				continue
			}
			if remaining <= 0 {
				break
			}
			title := reviewBacklogIssueTitle(report.Perspective, finding)
			res, err := c.CreateIssue(ctx, req.Repo, title, reviewBacklogIssueBody(req, report.Perspective, finding), []string{reviewBacklogLabel})
			if err != nil {
				return err
			}
			if res.Number <= 0 {
				continue
			}
			if res.AlreadyExisted {
				if err := c.ensureReviewBacklogLabel(ctx, req.Repo, res.Number); err != nil {
					return err
				}
				if err := c.CreateIssueComment(ctx, req.Repo, res.Number, reviewBacklogExistingLinkComment(req, key)); err != nil {
					return err
				}
			}
			ledger.Items[key] = reviewBacklogRecord{
				Repo:        req.Repo,
				PRNumber:    req.Number,
				HeadSHA:     reqHeadSHAFromReports(reports),
				Perspective: string(report.Perspective),
				IssueNumber: res.Number,
				IssueURL:    res.URL,
				FindingKey:  key,
				RecordedAt:  now.UTC(),
			}
			ledger.PRFiledIssueCount[prKey]++
			remaining--
			filedIssues = append(filedIssues, reviewBacklogFiled{number: res.Number, url: res.URL, title: title})
		}
	}
	if len(filedIssues) == 0 {
		return ledger.save("", now)
	}
	sort.Slice(filedIssues, func(i, j int) bool { return filedIssues[i].number < filedIssues[j].number })
	if !ledger.SummaryCommented[prKey] {
		if err := c.CreateIssueComment(ctx, req.Repo, req.Number, reviewBacklogSummaryComment(filedIssues, cap)); err != nil {
			return err
		}
		ledger.SummaryCommented[prKey] = true
	}
	return ledger.save("", now)
}

func (c *Client) ensureReviewBacklogLabel(ctx context.Context, repo string, number int) error {
	owner, repoName := c.splitRepo(repo)
	if err := c.ensureCreateIssueLabel(ctx, owner, repoName, reviewBacklogLabel); err != nil {
		return err
	}
	_, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repoName, number, []string{reviewBacklogLabel})
	return err
}

func acceptedReviewBacklogReports(req ReviewRequest, reports []review.PerspectiveReport) []review.PerspectiveReport {
	accepted := make([]review.PerspectiveReport, 0, len(reports))
	for _, report := range reports {
		if !strings.EqualFold(strings.TrimSpace(report.Repo), strings.TrimSpace(req.Repo)) || report.Number != req.Number {
			continue
		}
		ok, _, _ := verdictDispatchAuthorized(report, req)
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

func reviewBacklogIssueBody(req ReviewRequest, p review.Perspective, f outputschema.Finding) string {
	return fmt.Sprintf("Filed from an out-of-scope finding in the Hive review of PR #%d.\n\nPR: #%d\nPerspective: %s\nEvidence: `%s:%d`\nSeverity: %s\n\n%s\n",
		req.Number, req.Number, p, strings.TrimSpace(f.File), f.Line, f.Severity, strings.TrimSpace(f.Summary))
}

func reviewBacklogExistingLinkComment(req ReviewRequest, key string) string {
	return fmt.Sprintf("%s%s -->\nLinked again from the Hive review of PR #%d.", reviewBacklogLinkMarker, key, req.Number)
}

func reviewBacklogSummaryComment(issues []reviewBacklogFiled, cap int) string {
	var b strings.Builder
	b.WriteString(reviewBacklogSummaryMarker)
	b.WriteString("\nHive filed out-of-scope review findings as backlog issues so they do not block this PR:\n")
	for _, issue := range issues {
		if issue.url != "" {
			fmt.Fprintf(&b, "- #%d — %s (%s)\n", issue.number, issue.title, issue.url)
		} else {
			fmt.Fprintf(&b, "- #%d — %s\n", issue.number, issue.title)
		}
	}
	fmt.Fprintf(&b, "\nCap: %d backlog issue(s) per PR.", cap)
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

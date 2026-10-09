package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/sentinel"
)

// Sentinel sweep: evaluates every open PR in the watched repos with
// pkg/sentinel and, on a finding, posts one marker-stamped comment naming the
// finding(s) and reports the event for auditing. Untrusted authors also get
// the configured alert label, which is a hard hold for Hive approvals and
// auto-merge lanes. Trusted authors default to an informational notice without
// the label unless the operator opts into blocking them.
const (
	// SentinelMarker stamps the alert comment so later ticks find it and do
	// not repost for the same head.
	SentinelMarker = "<!-- hive-sentinel -->"
	// SentinelConfigKey is the config block named in the comment.
	SentinelConfigKey = "sentinel"
)

// SentinelSweepOptions parameterizes one pass.
type SentinelSweepOptions struct {
	Label            string
	LabelColor       string
	LabelDescription string
	Evaluator        sentinel.Config
	// TrustedAuthor classifies authors with the same predicate used by the
	// auto-merge exception lanes: the Hive App, trusted bots and owner-tier
	// authorized users. nil means no author is trusted.
	TrustedAuthor       func(repo, author string) SentinelAuthorTrust
	TrustedAuthorsBlock bool
	// RepoAllowed restricts the sweep; nil means every watched repo.
	RepoAllowed func(repo string) bool
	MaxActions  int
	// Audit receives one event per PR with a finding this pass.
	Audit func(SentinelSweepEvent)
}

// SentinelAuthorTrust records whether the PR author is in a trusted lane and
// the human-readable reason included in the informational notice.
type SentinelAuthorTrust struct {
	Trusted bool
	Reason  string
}

// SentinelDecision is the single block-vs-notify decision for a finding.
type SentinelDecision struct {
	Label         string
	Block         bool
	TrustedReason string
}

// DecideSentinelAction resolves whether a finding should add the alert label
// (block) or only notify. Trusted authors default to notify-only; setting
// trustedAuthorsBlock opts them back into the blocking label.
func DecideSentinelAction(label string, trust SentinelAuthorTrust, trustedAuthorsBlock bool) SentinelDecision {
	label = strings.TrimSpace(label)
	if label == "" {
		label = "sentinel-alert"
	}
	reason := strings.TrimSpace(trust.Reason)
	return SentinelDecision{
		Label:         label,
		Block:         !trust.Trusted || trustedAuthorsBlock,
		TrustedReason: reason,
	}
}

// SentinelSweepEvent is one PR with a sentinel finding.
type SentinelSweepEvent struct {
	Repo          string
	Number        int
	Author        string
	Title         string
	HeadSHA       string
	Findings      []sentinel.Finding
	Blocked       bool
	TrustedReason string
}

// Rules lists the triggered rule names.
func (e SentinelSweepEvent) Rules() []string { return sentinel.Rules(e.Findings) }

// SentinelSweepResult summarizes one pass.
type SentinelSweepResult struct {
	Seen       int
	Flagged    []SentinelSweepEvent
	Notified   []SentinelSweepEvent
	Remediated []SentinelSweepEvent
	Skipped    int
	Errors     []string
	ReposHit   map[string]int
}

// sentinelHeadsSeen remembers the head SHA each PR was last evaluated at so
// an unchanged PR costs one list call per pass, not a files fetch. A
// maintainer removing the label is respected: nothing is re-applied until a
// new push changes the head.
type sentinelHeadsSeen struct {
	mu   sync.Mutex
	seen map[string]string
}

func (s *sentinelHeadsSeen) unchanged(key, head string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]string{}
	}
	if prev, ok := s.seen[key]; ok && prev == head && head != "" {
		return true
	}
	s.seen[key] = head
	return false
}

// SweepSentinel runs one sentinel pass over the watched repos.
func (c *Client) SweepSentinel(ctx context.Context, opts SentinelSweepOptions) (*SentinelSweepResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	label := strings.TrimSpace(opts.Label)
	if label == "" {
		return nil, fmt.Errorf("sentinel sweep: label is required")
	}
	maxActions := opts.MaxActions
	if maxActions <= 0 {
		maxActions = 20
	}
	result := &SentinelSweepResult{ReposHit: map[string]int{}}
	for _, repo := range c.getRepos() {
		if opts.RepoAllowed != nil && !opts.RepoAllowed(repo) {
			continue
		}
		if len(result.Flagged)+len(result.Notified)+len(result.Remediated) >= maxActions {
			break
		}
		owner, repoName := c.splitRepo(repo)
		prs, err := c.listOpenPRsForSupersessionSweep(ctx, owner, repoName)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: list PRs: %v", repo, err))
			continue
		}
		for _, pr := range prs {
			if len(result.Flagged)+len(result.Notified)+len(result.Remediated) >= maxActions {
				break
			}
			if pr == nil {
				continue
			}
			result.Seen++
			event, acted, err := c.trySentinelPR(ctx, repo, owner, repoName, pr, label, opts)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s#%d: %v", repo, pr.GetNumber(), err))
				continue
			}
			if !acted {
				result.Skipped++
				continue
			}
			event.Repo = repo
			c.recordReviewEvidenceSentinel(repo, pr, event.Findings, time.Now())
			if event.Blocked {
				result.Flagged = append(result.Flagged, event)
			} else if prHasLabel(pr, label) {
				result.Remediated = append(result.Remediated, event)
			} else {
				result.Notified = append(result.Notified, event)
			}
			result.ReposHit[repo]++
			if opts.Audit != nil {
				opts.Audit(event)
			}
		}
	}
	return result, nil
}

func (c *Client) trySentinelPR(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, label string, opts SentinelSweepOptions) (SentinelSweepEvent, bool, error) {
	number := pr.GetNumber()
	head := pr.GetHead().GetSHA()
	author := pr.GetUser().GetLogin()
	trust := SentinelAuthorTrust{}
	if opts.TrustedAuthor != nil {
		trust = opts.TrustedAuthor(displayRepo, author)
	}
	decision := DecideSentinelAction(label, trust, opts.TrustedAuthorsBlock)
	hasAlertLabel := prHasLabel(pr, label)
	key := strings.ToLower(owner+"/"+repo) + "#" + fmt.Sprint(number)
	if !hasAlertLabel && c.sentinelHeads.unchanged(key, head) {
		return SentinelSweepEvent{}, false, nil
	}
	if opts.Evaluator.Exempt(author) {
		return SentinelSweepEvent{}, false, nil
	}
	if hasAlertLabel && decision.Block {
		// Already alerted on; a maintainer has not cleared it yet.
		return SentinelSweepEvent{}, false, nil
	}
	files, err := listPRFilesWithPatch(ctx, c.client, owner, repo, pr)
	if err != nil {
		return SentinelSweepEvent{}, false, err
	}
	findings := sentinel.Evaluate(sentinel.PR{Author: author, Title: pr.GetTitle(), Files: files}, opts.Evaluator)
	if len(findings) == 0 {
		return SentinelSweepEvent{}, false, nil
	}
	event := SentinelSweepEvent{Number: number, Author: author, Title: pr.GetTitle(), HeadSHA: head, Findings: findings, Blocked: decision.Block, TrustedReason: decision.TrustedReason}
	marker := SentinelMarker + " " + head
	comment, posted, err := findMarkerComment(ctx, c.client, owner, repo, number, marker)
	if err != nil {
		return SentinelSweepEvent{}, false, err
	}
	if !decision.Block {
		body := SentinelComment(decision, head, author, findings)
		if hasAlertLabel {
			if !posted {
				return SentinelSweepEvent{}, false, nil
			}
			if _, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, label); err != nil {
				return SentinelSweepEvent{}, false, fmt.Errorf("removing %s label from trusted author PR: %w", label, err)
			}
			if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, comment.GetID(), &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
				return SentinelSweepEvent{}, false, fmt.Errorf("updating sentinel notice: %w", err)
			}
			return event, true, nil
		}
		if !posted {
			if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
				return SentinelSweepEvent{}, false, fmt.Errorf("posting sentinel notice: %w", err)
			}
		}
		return event, true, nil
	}
	if err := c.EnsureIssueLabel(ctx, owner+"/"+repo, label, opts.LabelColor, opts.LabelDescription); err != nil {
		return SentinelSweepEvent{}, false, fmt.Errorf("ensuring %s label: %w", label, err)
	}
	if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{label}); err != nil {
		return SentinelSweepEvent{}, false, fmt.Errorf("adding %s label: %w", label, err)
	}
	body := SentinelComment(decision, head, author, findings)
	if posted {
		if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, comment.GetID(), &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			return SentinelSweepEvent{}, false, fmt.Errorf("updating sentinel alert: %w", err)
		}
	} else {
		if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			return SentinelSweepEvent{}, false, fmt.Errorf("posting sentinel comment: %w", err)
		}
	}
	return event, true, nil
}

func prHasLabel(pr *gh.PullRequest, label string) bool {
	for _, l := range pr.GetLabels() {
		if strings.EqualFold(l.GetName(), label) {
			return true
		}
	}
	return false
}

// listPRFilesWithPatch is ListPRChangedFiles plus the unified-diff patch the
// content rules inspect. It fails closed on an incomplete list for the same
// reason: a rule evaluated on a partial file set could miss the one file
// that matters.
func listPRFilesWithPatch(ctx context.Context, client *gh.Client, owner, repo string, pr *gh.PullRequest) ([]sentinel.File, error) {
	if client == nil || pr == nil {
		return nil, fmt.Errorf("listing PR files: no client or PR")
	}
	var files []sentinel.File
	fileOpts := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := client.PullRequests.ListFiles(ctx, owner, repo, pr.GetNumber(), fileOpts)
		if err != nil {
			return nil, fmt.Errorf("listing PR files: %w", err)
		}
		for _, f := range page {
			files = append(files, sentinel.File{
				Path:      f.GetFilename(),
				Status:    f.GetStatus(),
				Additions: f.GetAdditions(),
				Deletions: f.GetDeletions(),
				Patch:     f.GetPatch(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		fileOpts.Page = resp.NextPage
	}
	if reported := pr.GetChangedFiles(); reported > len(files) {
		return nil, fmt.Errorf("incomplete PR file list: GitHub reported %d changed files but API returned %d", reported, len(files))
	}
	return files, nil
}

func findMarkerComment(ctx context.Context, client *gh.Client, owner, repo string, number int, marker string) (*gh.IssueComment, bool, error) {
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := client.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, false, fmt.Errorf("listing comments for sentinel marker: %w", err)
		}
		for _, comment := range comments {
			if strings.Contains(comment.GetBody(), marker) {
				return comment, true, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return nil, false, nil
		}
		opts.Page = resp.NextPage
	}
}

func hasMarkerComment(ctx context.Context, client *gh.Client, owner, repo string, number int, marker string) (bool, error) {
	_, found, err := findMarkerComment(ctx, client, owner, repo, number, marker)
	return found, err
}

// SentinelComment is the body of the alert/notice comment. The marker carries
// the head SHA so a new push gets a fresh comment while an unchanged head never
// gets a duplicate.
func SentinelComment(decision SentinelDecision, head, author string, findings []sentinel.Finding) string {
	var b strings.Builder
	b.WriteString(SentinelMarker + " " + head + "\n")
	if decision.Block {
		b.WriteString("## ⚠️ Sentinel alert — maintainer review required\n\n")
	} else {
		reason := decision.TrustedReason
		if strings.TrimSpace(reason) == "" {
			reason = "trusted author"
		}
		fmt.Fprintf(&b, "## ℹ️ Sentinel notice — informational; author is trusted (%s), merge is not blocked\n\n", reason)
	}
	fmt.Fprintf(&b, "Hive flagged this PR (author @%s, head `%s`) because it matches behaviors that can override security controls, escalate privileges or damage the codebase. This is a heuristic, not an accusation — a maintainer should confirm the change is intended before it merges.\n\n", author, shortSHA(head))
	for _, f := range findings {
		fmt.Fprintf(&b, "- **%s** — %s", f.Rule, f.Summary)
		if desc := sentinel.RuleDescriptions[f.Rule]; desc != "" {
			fmt.Fprintf(&b, " _(%s)_", desc)
		}
		b.WriteString("\n")
		paths := append([]string(nil), f.Paths...)
		sort.Strings(paths)
		for _, p := range paths {
			fmt.Fprintf(&b, "  - `%s`\n", p)
		}
	}
	if decision.Block {
		fmt.Fprintf(&b, "\nHive added the `%s` label. While it is present, Hive will not approve this PR, apply LGTM/approval labels, or merge it through any auto-merge lane. Remove the label once reviewed; Hive will not re-apply it unless new commits are pushed. Tune paths and behaviors under `%s` in hive.yaml or the dashboard Security tab.\n", decision.Label, SentinelConfigKey)
	} else {
		fmt.Fprintf(&b, "\nHive did not add the `%s` label because this author is trusted. The finding remains recorded for audit and the dashboard Security tab; set `%s.trusted_authors_block: true` to require the blocking label for trusted authors too.\n", decision.Label, SentinelConfigKey)
	}
	return b.String()
}

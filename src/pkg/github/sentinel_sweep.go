package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/sentinel"
)

// Sentinel sweep: evaluates every open PR in the watched repos with
// pkg/sentinel and, on a finding, ensures + applies the alert label, posts
// one marker-stamped comment naming the finding(s) and reports the event for
// auditing. It is observe-and-alert only — it never merges, closes, holds or
// removes labels. A maintainer clears the alert by removing the label; the
// sweep will not re-apply it for the same head SHA.
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
	// RepoAllowed restricts the sweep; nil means every watched repo.
	RepoAllowed func(repo string) bool
	MaxActions  int
	// Audit receives one event per PR labelled this pass.
	Audit func(SentinelSweepEvent)
}

// SentinelSweepEvent is one labelled PR.
type SentinelSweepEvent struct {
	Repo     string
	Number   int
	Author   string
	Title    string
	HeadSHA  string
	Findings []sentinel.Finding
}

// Rules lists the triggered rule names.
func (e SentinelSweepEvent) Rules() []string { return sentinel.Rules(e.Findings) }

// SentinelSweepResult summarizes one pass.
type SentinelSweepResult struct {
	Seen     int
	Flagged  []SentinelSweepEvent
	Skipped  int
	Errors   []string
	ReposHit map[string]int
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
		if len(result.Flagged) >= maxActions {
			break
		}
		owner, repoName := c.splitRepo(repo)
		prs, err := c.listOpenPRsForSupersessionSweep(ctx, owner, repoName)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: list PRs: %v", repo, err))
			continue
		}
		for _, pr := range prs {
			if len(result.Flagged) >= maxActions {
				break
			}
			if pr == nil {
				continue
			}
			result.Seen++
			event, flagged, err := c.trySentinelPR(ctx, owner, repoName, pr, label, opts)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s#%d: %v", repo, pr.GetNumber(), err))
				continue
			}
			if !flagged {
				result.Skipped++
				continue
			}
			event.Repo = repo
			result.Flagged = append(result.Flagged, event)
			result.ReposHit[repo]++
			if opts.Audit != nil {
				opts.Audit(event)
			}
		}
	}
	return result, nil
}

func (c *Client) trySentinelPR(ctx context.Context, owner, repo string, pr *gh.PullRequest, label string, opts SentinelSweepOptions) (SentinelSweepEvent, bool, error) {
	number := pr.GetNumber()
	head := pr.GetHead().GetSHA()
	key := strings.ToLower(owner+"/"+repo) + "#" + fmt.Sprint(number)
	if c.sentinelHeads.unchanged(key, head) {
		return SentinelSweepEvent{}, false, nil
	}
	author := pr.GetUser().GetLogin()
	if opts.Evaluator.Exempt(author) {
		return SentinelSweepEvent{}, false, nil
	}
	for _, l := range pr.Labels {
		if strings.EqualFold(l.GetName(), label) {
			// Already alerted on; a maintainer has not cleared it yet.
			return SentinelSweepEvent{}, false, nil
		}
	}
	files, err := listPRFilesWithPatch(ctx, c.client, owner, repo, pr)
	if err != nil {
		return SentinelSweepEvent{}, false, err
	}
	findings := sentinel.Evaluate(sentinel.PR{Author: author, Title: pr.GetTitle(), Files: files}, opts.Evaluator)
	if len(findings) == 0 {
		return SentinelSweepEvent{}, false, nil
	}
	if err := c.EnsureIssueLabel(ctx, owner+"/"+repo, label, opts.LabelColor, opts.LabelDescription); err != nil {
		return SentinelSweepEvent{}, false, fmt.Errorf("ensuring %s label: %w", label, err)
	}
	if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{label}); err != nil {
		return SentinelSweepEvent{}, false, fmt.Errorf("adding %s label: %w", label, err)
	}
	posted, err := hasMarkerComment(ctx, c.client, owner, repo, number, SentinelMarker+" "+head)
	if err != nil {
		return SentinelSweepEvent{}, false, err
	}
	if !posted {
		body := SentinelComment(label, head, author, findings)
		if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			return SentinelSweepEvent{}, false, fmt.Errorf("posting sentinel comment: %w", err)
		}
	}
	return SentinelSweepEvent{Number: number, Author: author, Title: pr.GetTitle(), HeadSHA: head, Findings: findings}, true, nil
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

func hasMarkerComment(ctx context.Context, client *gh.Client, owner, repo string, number int, marker string) (bool, error) {
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := client.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return false, fmt.Errorf("listing comments for sentinel marker: %w", err)
		}
		for _, comment := range comments {
			if strings.Contains(comment.GetBody(), marker) {
				return true, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return false, nil
		}
		opts.Page = resp.NextPage
	}
}

// SentinelComment is the body of the alert comment. The marker carries the
// head SHA so a new push gets a fresh comment while an unchanged head never
// gets a duplicate.
func SentinelComment(label, head, author string, findings []sentinel.Finding) string {
	var b strings.Builder
	b.WriteString(SentinelMarker + " " + head + "\n")
	b.WriteString("## ⚠️ Sentinel alert — maintainer review required\n\n")
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
	fmt.Fprintf(&b, "\nHive added the `%s` label. Automated reviewers route this PR to a human and will not approve it. Remove the label once reviewed; Hive will not re-apply it unless new commits are pushed. Tune paths and behaviors under `%s` in hive.yaml or the dashboard Security tab.\n", label, SentinelConfigKey)
	return b.String()
}

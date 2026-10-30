package github

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// Declared supersession (hivecommons/hive#15208). The merged-closer rule
// needs every issue a hive PR closes to be closed; #15208 said `Closes
// #15203` (a child issue still open) and `Part of #15188`, so when #15206
// merged with "## Supersedes #15208" in its body and closed #15188, the
// rule read #15208 as live work, the reviewer swarm answered
// `requires_human` and the PR sat at 085 asking why hive created it. A
// merged PR of the same repo that states `Supersedes #N` is the
// maintainer's answer: #N has nothing left to deliver.

// issueStateReasonNotPlanned is GitHub's state_reason for an issue closed
// as not planned (wontfix).
const issueStateReasonNotPlanned = "not_planned"

// supersedesLinePattern finds a "Supersedes …" statement; the PR numbers
// are read from the rest of its line up to a separator (`·`, `;`, `(`),
// so "Supersedes #15208 · layers on #15209" names only #15208.
var supersedesLinePattern = regexp.MustCompile(`(?i)\bsupersedes\b:?([^\n]*)`)

var supersedesNumberPattern = regexp.MustCompile(`#(\d+)`)

// SupersedesDeclared returns the PR numbers body states it supersedes
// (`Supersedes #N`), in order of first appearance, de-duplicated. An
// adoption copy's body names its original this way.
func SupersedesDeclared(body string) []int {
	seen := map[int]bool{}
	var out []int
	for _, m := range supersedesLinePattern.FindAllStringSubmatch(body, -1) {
		rest := m[1]
		if i := strings.IndexAny(rest, "·;("); i >= 0 {
			rest = rest[:i]
		}
		for _, n := range supersedesNumberPattern.FindAllStringSubmatch(rest, -1) {
			if v, err := strconv.Atoi(n[1]); err == nil && v > 0 && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}

// bodyDeclaresSupersedes reports whether body states that it supersedes
// PR number.
func bodyDeclaresSupersedes(body string, number int) bool {
	if number <= 0 {
		return false
	}
	for _, v := range SupersedesDeclared(body) {
		if v == number {
			return true
		}
	}
	return false
}

// detectDeclaredSupersession walks pr's timeline for a cross-reference
// from a merged pull request of the same repository whose body declares
// `Supersedes #<pr>`. It returns that PR as the closer.
func (c *Client) detectDeclaredSupersession(ctx context.Context, owner, repo string, pr *gh.PullRequest) (bool, supersessionCloser, error) {
	if c == nil || c.client == nil || pr == nil {
		return false, supersessionCloser{}, nil
	}
	number := pr.GetNumber()
	full := owner + "/" + repo
	opts := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := c.client.Issues.ListIssueTimeline(ctx, owner, repo, number, opts)
		if err != nil {
			return false, supersessionCloser{}, err
		}
		for _, t := range page {
			src := t.GetSource()
			if t.GetEvent() != "cross-referenced" || src == nil || src.Issue == nil || !src.Issue.IsPullRequest() {
				continue
			}
			ref := src.Issue
			if ref.GetNumber() == number || !timelineSourceInRepo(ref, full) {
				continue
			}
			mergedAt := ref.GetPullRequestLinks().GetMergedAt()
			if mergedAt.IsZero() || !bodyDeclaresSupersedes(ref.GetBody(), number) {
				continue
			}
			return true, supersessionCloser{Repo: full, Number: ref.GetNumber(), URL: ref.GetHTMLURL(), MergedAt: mergedAt.Time}, nil
		}
		if resp == nil || resp.NextPage == 0 {
			return false, supersessionCloser{}, nil
		}
		opts.Page = resp.NextPage
	}
}

// timelineSourceInRepo reports whether a cross-reference source issue
// lives in full ("owner/name"); a source naming no repository is taken as
// same-repo (GitHub omits it for same-repository references).
func timelineSourceInRepo(issue *gh.Issue, full string) bool {
	if name := issue.GetRepository().GetFullName(); name != "" {
		return strings.EqualFold(name, full)
	}
	if u := issue.GetRepositoryURL(); u != "" {
		return strings.HasSuffix(strings.ToLower(u), "/repos/"+strings.ToLower(full))
	}
	return true
}

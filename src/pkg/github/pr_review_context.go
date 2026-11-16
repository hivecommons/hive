package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/crux"
)

// The PR snapshot a sandboxed reviewer reads instead of `gh pr view` and
// `gh pr diff` (hivecommons/hive#18513). The sandbox Job holds no GitHub
// credential, so the hive reads the PR with its own client and the executor
// writes the result into the workspace as untrusted data.

const (
	// prReviewContextMaxFiles bounds the changed-file list. GitHub's list
	// endpoint itself stops at 3000 files.
	prReviewContextMaxFiles = 3000
	// prReviewContextCommentRunes bounds each comment body carried into the
	// workspace. Longer than the follow-up router's cap: a reviewer needs the
	// whole earlier review, not a notification-sized excerpt.
	prReviewContextCommentRunes = 8000
	// Comment kinds in crux.PRReviewComment.Kind.
	prReviewKindConversation = "conversation"
	prReviewKindReview       = "review"
	prReviewKindInline       = "inline"
)

var _ crux.PRReviewContextReader = (*Client)(nil)

// PRReviewContext reads repo#number for a sandboxed reviewer: the PR itself,
// its changed files, its unified diff and its conversation. A failure to read
// the PR or its files is an error; a diff GitHub refuses to render (very
// large PRs) is recorded in DiffUnavailable instead, because the reviewer can
// still diff the checked-out refs itself. Comments are not filtered by
// author: the reviewer sees what a person opening the PR would see, and the
// executor marks all of it untrusted.
func (c *Client) PRReviewContext(ctx context.Context, repo string, number int) (crux.PRReviewContext, error) {
	if c == nil || c.client == nil {
		return crux.PRReviewContext{}, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:sandbox_review_pr"), owner, name, number)
	if err != nil {
		return crux.PRReviewContext{}, fmt.Errorf("reading %s#%d: %w", repo, number, err)
	}
	out := crux.PRReviewContext{
		Number:   pr.GetNumber(),
		URL:      pr.GetHTMLURL(),
		Title:    pr.GetTitle(),
		Body:     pr.GetBody(),
		Author:   pr.GetUser().GetLogin(),
		State:    pr.GetState(),
		Draft:    pr.GetDraft(),
		BaseRef:  pr.GetBase().GetRef(),
		BaseSHA:  pr.GetBase().GetSHA(),
		HeadRef:  pr.GetHead().GetRef(),
		HeadSHA:  pr.GetHead().GetSHA(),
		HeadRepo: pr.GetHead().GetRepo().GetFullName(),
		Labels:   []string{},
	}
	if out.Number == 0 {
		out.Number = number
	}
	baseRepo := pr.GetBase().GetRepo().GetFullName()
	out.Fork = out.HeadRepo != "" && baseRepo != "" && !strings.EqualFold(out.HeadRepo, baseRepo)
	for _, l := range pr.Labels {
		if n := l.GetName(); n != "" {
			out.Labels = append(out.Labels, n)
		}
	}
	files, truncated, err := c.prReviewFiles(ctx, owner, name, number)
	if err != nil {
		return crux.PRReviewContext{}, fmt.Errorf("listing %s#%d files: %w", repo, number, err)
	}
	out.Files, out.FilesTruncated = files, truncated
	diff, _, err := c.client.PullRequests.GetRaw(WithRESTCaller(ctx, "hive:sandbox_review_pr_diff"), owner, name, number, gh.RawOptions{Type: gh.Diff})
	if err != nil {
		out.DiffUnavailable = "GitHub did not return the diff: " + err.Error()
	} else {
		out.Diff = diff
	}
	comments, err := c.prReviewComments(ctx, owner, name, number)
	if err != nil {
		return crux.PRReviewContext{}, fmt.Errorf("reading %s#%d comments: %w", repo, number, err)
	}
	out.Comments = comments
	return out, nil
}

func (c *Client) prReviewFiles(ctx context.Context, owner, name string, number int) ([]crux.PRChangedFile, bool, error) {
	files := []crux.PRChangedFile{}
	opts := &gh.ListOptions{PerPage: listPerPage}
	for {
		page, resp, err := c.client.PullRequests.ListFiles(WithRESTCaller(ctx, "hive:sandbox_review_pr_files"), owner, name, number, opts)
		if err != nil {
			return nil, false, err
		}
		for _, f := range page {
			if len(files) >= prReviewContextMaxFiles {
				return files, true, nil
			}
			files = append(files, crux.PRChangedFile{
				Path:         f.GetFilename(),
				PreviousPath: f.GetPreviousFilename(),
				Status:       f.GetStatus(),
				Additions:    f.GetAdditions(),
				Deletions:    f.GetDeletions(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			return files, false, nil
		}
		opts.Page = resp.NextPage
	}
}

func (c *Client) prReviewComments(ctx context.Context, owner, name string, number int) ([]crux.PRReviewComment, error) {
	var data struct {
		Repository struct {
			PullRequest *rawPRConversation `json:"pullRequest"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": owner, "repo": name, "number": number}
	if err := c.graphQL(ctx, prConversationQuery, vars, &data); err != nil {
		return nil, err
	}
	if data.Repository.PullRequest == nil {
		return nil, fmt.Errorf("%s/%s#%d: pull request not found", owner, name, number)
	}
	return prReviewCommentsFrom(*data.Repository.PullRequest), nil
}

// prReviewCommentsFrom flattens a PR conversation into the snapshot's comment
// list: conversation comments, then reviews, then inline threads (numbered
// from 1 so a reviewer can tell replies in one thread from another).
// Conversation and inline comments with no text are dropped; a review is
// kept even with an empty body, because its state is the point.
func prReviewCommentsFrom(raw rawPRConversation) []crux.PRReviewComment {
	out := []crux.PRReviewComment{}
	conv := func(kind string, rc rawPRComment) crux.PRReviewComment {
		author := ""
		if rc.Author != nil {
			author = rc.Author.Login
		}
		return crux.PRReviewComment{
			Kind:              kind,
			Author:            author,
			AuthorAssociation: rc.AuthorAssociation,
			Body:              truncateReviewContextBody(rc.Body),
			State:             rc.State,
			URL:               rc.URL,
			CreatedAt:         rc.CreatedAt,
		}
	}
	for _, rc := range raw.Comments.Nodes {
		if strings.TrimSpace(rc.Body) != "" {
			out = append(out, conv(prReviewKindConversation, rc))
		}
	}
	for _, rc := range raw.Reviews.Nodes {
		out = append(out, conv(prReviewKindReview, rc))
	}
	for i, th := range raw.ReviewThreads.Nodes {
		line := 0
		if th.Line != nil {
			line = *th.Line
		}
		for _, rc := range th.Comments.Nodes {
			if strings.TrimSpace(rc.Body) == "" {
				continue
			}
			cm := conv(prReviewKindInline, rc)
			cm.Path, cm.Line, cm.Thread = th.Path, line, i+1
			cm.Resolved, cm.Outdated = th.IsResolved, th.IsOutdated
			out = append(out, cm)
		}
	}
	return out
}

func truncateReviewContextBody(body string) string {
	body = strings.TrimSpace(body)
	if runes := []rune(body); len(runes) > prReviewContextCommentRunes {
		return string(runes[:prReviewContextCommentRunes]) + "... [truncated by hive]"
	}
	return body
}

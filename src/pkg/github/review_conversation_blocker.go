package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/writeback"
)

const reviewConversationStatusSection = "review-conversation-blocker"

// ReviewConversationBlockReason names a confirmed conversation blocker without
// implying it is the only unsatisfied merge rule.
func (p PullRequest) ReviewConversationBlockReason() string {
	if p.Protection == nil || !strings.EqualFold(p.MergeableState, "blocked") {
		return ""
	}
	switch n := p.Protection.UnresolvedReviewThreads; {
	case n == 1:
		return "1 unresolved review thread"
	case n > 1:
		return strconv.Itoa(n) + " unresolved review threads"
	default:
		return ""
	}
}

func (pc *protectionCollector) reviewConversationBlocker(ctx context.Context, pr *PullRequest) (int, []uint64) {
	if pr.Draft || pr.BaseRef == "" || pr.HeadSHA == "" || !strings.EqualFold(pr.MergeableState, "blocked") {
		return 0, nil
	}
	owner, name := pc.c.splitRepo(pr.Repo)
	key := strings.ToLower(owner+"/"+name) + "@" + pr.BaseRef
	required, ok := pc.conversationRequired[key]
	if !ok {
		required = pc.c.requiresReviewConversationResolution(ctx, owner, name, pr.BaseRef)
		if pc.conversationRequired == nil {
			pc.conversationRequired = make(map[string]bool)
		}
		pc.conversationRequired[key] = required
	}
	if !required {
		return 0, nil
	}
	return pc.c.unresolvedReviewConversations(ctx, owner, name, pr)
}

// A positive result from either active rulesets or classic protection is
// enough. An unreadable rule is not evidence that a thread blocks merging.
func (c *Client) requiresReviewConversationResolution(ctx context.Context, owner, repo, branch string) bool {
	path := fmt.Sprintf("repos/%s/%s/rules/branches/%s", owner, repo, url.PathEscape(branch))
	for page := 1; page != 0; {
		var rules []struct {
			Type       string `json:"type"`
			Parameters struct {
				Required bool `json:"required_review_thread_resolution"`
			} `json:"parameters"`
		}
		req, err := c.client.NewRequest(http.MethodGet, fmt.Sprintf("%s?per_page=100&page=%d", path, page), nil)
		if err != nil {
			break
		}
		resp, err := c.client.Do(ctx, req, &rules)
		if err != nil {
			break
		}
		for _, r := range rules {
			if r.Type == "pull_request" && r.Parameters.Required {
				return true
			}
		}
		page = resp.NextPage
	}
	protection, _, err := c.client.Repositories.GetBranchProtection(ctx, owner, repo, url.PathEscape(branch))
	return err == nil && protection != nil && protection.RequiredConversationResolution != nil && protection.RequiredConversationResolution.Enabled
}

const unresolvedConversationsQuery = `query($owner:String!, $repo:String!, $number:Int!, $after:String) {
  repository(owner:$owner,name:$repo) {
    pullRequest(number:$number) {
      headRefOid baseRefName mergeStateStatus
      reviewThreads(first:100,after:$after) {
        pageInfo { hasNextPage endCursor }
        nodes { isResolved comments(first:1) { nodes { url } } }
      }
    }
  }
}`

func (c *Client) unresolvedReviewConversations(ctx context.Context, owner, repo string, pr *PullRequest) (int, []uint64) {
	const maxNoticeLinks = 10
	vars := map[string]any{"owner": owner, "repo": repo, "number": pr.Number}
	var count int
	var links []uint64
	for cursor := ""; ; {
		var data struct {
			Repository struct {
				PullRequest *struct {
					Head    string `json:"headRefOid"`
					Base    string `json:"baseRefName"`
					State   string `json:"mergeStateStatus"`
					Threads struct {
						PageInfo struct {
							More bool   `json:"hasNextPage"`
							End  string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							Resolved bool `json:"isResolved"`
							Comments struct {
								Nodes []struct{ URL string } `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.graphQL(ctx, unresolvedConversationsQuery, vars, &data); err != nil {
			return 0, nil
		}
		pull := data.Repository.PullRequest
		if pull == nil || pull.Head != pr.HeadSHA || pull.Base != pr.BaseRef || pull.State != "BLOCKED" {
			return 0, nil
		}
		for _, th := range pull.Threads.Nodes {
			if th.Resolved {
				continue
			}
			count++ // An outdated thread can still be unresolved.
			if len(links) < maxNoticeLinks && len(th.Comments.Nodes) > 0 {
				if _, id, ok := strings.Cut(th.Comments.Nodes[0].URL, "#discussion_r"); ok {
					if n, err := strconv.ParseUint(id, 10, 64); err == nil && n > 0 {
						links = append(links, n)
					}
				}
			}
		}
		if !pull.Threads.PageInfo.More {
			return count, links
		}
		end := pull.Threads.PageInfo.End
		if end == "" || end == cursor {
			return 0, nil // Never present a partial count as exact.
		}
		cursor = end
		vars["after"] = cursor
	}
}

// PostReviewConversationBlockers records observations in a single section of
// the hive's PR status comment. This doesn't resolve threads or change holds.
func (c *Client) PostReviewConversationBlockers(ctx context.Context, prs []PullRequest) {
	if c == nil || c.appBotLogin == "" {
		return
	}
	for i := range prs {
		pr := &prs[i]
		reason := pr.ReviewConversationBlockReason()
		if reason == "" || !c.IsHiveAuthorLogin(pr.Author) {
			continue
		}
		owner, name := c.splitRepo(pr.Repo)
		repo := owner + "/" + name
		if _, paused := c.repoPausedError(repo); paused {
			continue
		}
		var text strings.Builder
		fmt.Fprintf(&text, "GitHub reported this PR blocked with **%s** on a base branch that requires conversation resolution.\n\n", reason)
		for i, id := range pr.Protection.reviewThreadCommentIDs {
			fmt.Fprintf(&text, "- [Review thread %d](/%s/pull/%d#discussion_r%d)\n", i+1, repo, pr.Number, id)
		}
		if linked := len(pr.Protection.reviewThreadCommentIDs); linked < pr.Protection.UnresolvedReviewThreads {
			text.WriteString("\nSee Files changed for unresolved threads not linked here.\n")
		}
		fmt.Fprintf(&text, "\nCheck the [current conversations](/%s/pull/%d/files) and resolve any that are still open. Hive v5 does not automatically resolve its own reviewer's notes.", repo, pr.Number)
		body := text.String()
		if !c.WritebackAllowed(repo, c.WritebackItemForPR(*pr), writeback.KindReviewConversationNotice, body).Allowed {
			continue
		}
		_, err := c.UpsertPRStatusSection(ctx, repo, pr.Number, reviewConversationStatusSection, body)
		c.noteWritebackOutcome(repo, nil, err)
		if err != nil {
			c.logger.Warn("review conversations: could not update the PR status comment", "repo", repo, "pr", pr.Number, "error", err)
		}
	}
}

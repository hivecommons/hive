package github

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
)

const graphQLPRBatchQuery = `
query($owner: String!, $name: String!, $first: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(states: OPEN, first: $first, after: $cursor, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        headRefOid
        updatedAt
        isDraft
        mergeable
        mergeStateStatus
        reviewDecision
        labels(first: 30) { nodes { name } }
        author { login }
        baseRefName
        headRefName
        title
        commits(last: 1) {
          nodes {
            commit {
              statusCheckRollup {
                state
                contexts(first: 50) {
                  nodes {
                    __typename
                    ... on CheckRun { name conclusion status detailsUrl }
                    ... on StatusContext { context state targetUrl }
                  }
                }
              }
            }
          }
        }
        closingIssuesReferences(first: 10) { nodes { number } }
      }
    }
  }
  rateLimit { cost remaining resetAt }
}`

type prBatchCheckRunKey struct {
	repo    string
	number  int
	headSHA string
}

type graphQLPRBatchResponse struct {
	Repository struct {
		PullRequests struct {
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []graphQLPRBatchNode `json:"nodes"`
		} `json:"pullRequests"`
	} `json:"repository"`
	RateLimit struct {
		Cost      int       `json:"cost"`
		Remaining int       `json:"remaining"`
		ResetAt   time.Time `json:"resetAt"`
	} `json:"rateLimit"`
}

type graphQLPRBatchNode struct {
	Number           int       `json:"number"`
	HeadRefOID       string    `json:"headRefOid"`
	UpdatedAt        time.Time `json:"updatedAt"`
	IsDraft          bool      `json:"isDraft"`
	Mergeable        string    `json:"mergeable"`
	MergeStateStatus string    `json:"mergeStateStatus"`
	ReviewDecision   string    `json:"reviewDecision"`
	Labels           struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	BaseRefName string `json:"baseRefName"`
	HeadRefName string `json:"headRefName"`
	Title       string `json:"title"`
	Commits     struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State    string `json:"state"`
					Contexts struct {
						Nodes []graphQLPRBatchContext `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	ClosingIssuesReferences struct {
		Nodes []struct {
			Number int `json:"number"`
		} `json:"nodes"`
	} `json:"closingIssuesReferences"`
}

type graphQLPRBatchContext struct {
	TypeName   string `json:"__typename"`
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	Status     string `json:"status"`
	DetailsURL string `json:"detailsUrl"`
	Context    string `json:"context"`
	State      string `json:"state"`
	TargetURL  string `json:"targetUrl"`
}

func (c *Client) graphQLPRBatchEnabledEffective() bool {
	if c != nil && c.graphQLPRBatchEnabled != nil {
		return c.graphQLPRBatchEnabled()
	}
	return false
}

func (c *Client) graphQLPRBatchPageSizeEffective() int {
	if c != nil && c.graphQLPRBatchPageSize != nil {
		return config.NormalizeGraphQLPRBatchPageSize(c.graphQLPRBatchPageSize())
	}
	return config.DefaultGraphQLPRBatchPageSize
}

func (c *Client) prefetchOpenPRDetailsGraphQL(ctx context.Context, owner, repoName, fullRepo string) {
	if c == nil || !c.graphQLPRBatchEnabledEffective() {
		if c != nil {
			c.clearGraphQLPRBatchRepo(fullRepo)
			c.recordGraphQLPRBatchFallback(0)
		}
		return
	}
	// With healthy webhooks and nothing marked dirty since the last batch,
	// the cached batch still describes every open PR (#11177).
	if sharedWebhookTracker.repoCleanAndHealthy(fullRepo) && c.hasGraphQLPRBatchRepo(fullRepo) {
		c.recordGraphQLPRBatchWebhookSkip()
		return
	}
	started := sharedWebhookTracker.now()
	c.clearGraphQLPRBatchRepo(fullRepo)
	pageSize := c.graphQLPRBatchPageSizeEffective()
	var cursor string
	repoPRs, pages := 0, 0
	for {
		var out graphQLPRBatchResponse
		vars := map[string]any{"owner": owner, "name": repoName, "first": pageSize}
		if cursor != "" {
			vars["cursor"] = cursor
		}

		if err := c.graphQL(WithRESTCaller(ctx, "hive:pr_batch"), graphQLPRBatchQuery, vars, &out); err != nil {
			c.recordGraphQLPRBatchError()
			if c.logger != nil {
				c.logger.Warn("github GraphQL PR batch failed; falling back to REST enrichment", "repo", fullRepo, "error", err)
			}
			return
		}
		pages++
		c.recordGraphQLPRBatchRate(out.RateLimit.Cost, out.RateLimit.Remaining, out.RateLimit.ResetAt)
		for _, n := range out.Repository.PullRequests.Nodes {
			repoPRs++
			c.storeGraphQLPRBatchNode(fullRepo, n)
		}
		if !out.Repository.PullRequests.PageInfo.HasNextPage {
			break
		}
		cursor = out.Repository.PullRequests.PageInfo.EndCursor
		if cursor == "" {
			break
		}
	}
	c.recordGraphQLPRBatchSuccess(repoPRs, pages)
	sharedWebhookTracker.clearDirtyRepoBefore(fullRepo, started)
}

func (c *Client) hasGraphQLPRBatchRepo(repo string) bool {
	c.prBatchMu.Lock()
	defer c.prBatchMu.Unlock()
	_, ok := c.prBatchReviews[canonicalPRDetailRepo(repo)]
	return ok
}

// invalidateGraphQLPRBatchPR drops the batch check-run and review entries for
// one PR so its next read misses and re-enriches (#11177).
func (c *Client) invalidateGraphQLPRBatchPR(repo string, number int) {
	if c == nil {
		return
	}
	key := canonicalPRDetailRepo(repo)
	c.prBatchMu.Lock()
	defer c.prBatchMu.Unlock()
	delete(c.prBatchReviews[key], number)
	for k := range c.prBatchCheckRuns {
		if k.repo == key && k.number == number {
			delete(c.prBatchCheckRuns, k)
		}
	}
}

func (c *Client) clearGraphQLPRBatchRepo(repo string) {
	c.prBatchMu.Lock()
	defer c.prBatchMu.Unlock()
	key := canonicalPRDetailRepo(repo)
	if c.prBatchReviews != nil {
		delete(c.prBatchReviews, key)
	}
	if c.prBatchCheckRuns != nil {
		for k := range c.prBatchCheckRuns {
			if k.repo == key {
				delete(c.prBatchCheckRuns, k)
			}
		}
	}
}

func (c *Client) storeGraphQLPRBatchNode(repo string, n graphQLPRBatchNode) {
	state := graphQLMergeStateStatusToREST(n.MergeStateStatus)
	mergeable := graphQLMergeable(n.Mergeable)
	pr := &gh.PullRequest{
		Number:         gh.Ptr(n.Number),
		State:          gh.Ptr("open"),
		Title:          gh.Ptr(n.Title),
		UpdatedAt:      &gh.Timestamp{Time: n.UpdatedAt},
		User:           &gh.User{Login: gh.Ptr(n.Author.Login)},
		Draft:          gh.Ptr(n.IsDraft),
		Mergeable:      mergeable,
		MergeableState: gh.Ptr(state),
		Head:           &gh.PullRequestBranch{SHA: gh.Ptr(n.HeadRefOID), Ref: gh.Ptr(n.HeadRefName)},
		Base:           &gh.PullRequestBranch{Ref: gh.Ptr(n.BaseRefName)},
	}
	for _, l := range n.Labels.Nodes {
		if strings.TrimSpace(l.Name) != "" {
			pr.Labels = append(pr.Labels, &gh.Label{Name: gh.Ptr(l.Name)})
		}
	}
	c.storePRDetail(repo, n.Number, pr)
	if checks, ok := graphQLCheckRuns(n); ok {
		c.prBatchMu.Lock()
		if c.prBatchCheckRuns == nil {
			c.prBatchCheckRuns = map[prBatchCheckRunKey][]*gh.CheckRun{}
		}
		key := prBatchCheckRunKey{repo: canonicalPRDetailRepo(repo), number: n.Number, headSHA: n.HeadRefOID}
		c.prBatchCheckRuns[key] = cloneCheckRuns(checks)
		c.prBatchMu.Unlock()
	}
	c.storeGraphQLPRBatchReview(repo, n)
	if strings.EqualFold(strings.TrimSpace(n.Mergeable), "UNKNOWN") {
		c.recordGraphQLPRBatchFallback(1)
	}
}

func graphQLMergeable(raw string) *bool {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "MERGEABLE":
		return gh.Ptr(true)
	case "CONFLICTING":
		return gh.Ptr(false)
	default:
		return nil
	}
}

func graphQLCheckRuns(n graphQLPRBatchNode) ([]*gh.CheckRun, bool) {
	if len(n.Commits.Nodes) == 0 || n.Commits.Nodes[0].Commit.StatusCheckRollup == nil {
		return nil, false
	}
	var out []*gh.CheckRun
	for i, ctx := range n.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts.Nodes {
		name, status, conclusion, url := "", "", "", ""
		switch ctx.TypeName {
		case "CheckRun":
			name, status, conclusion, url = ctx.Name, strings.ToLower(ctx.Status), strings.ToLower(ctx.Conclusion), ctx.DetailsURL
		case "StatusContext":
			name, status, conclusion, url = ctx.Context, statusFromStatusContext(ctx.State), conclusionFromStatusContext(ctx.State), ctx.TargetURL
		default:
			continue
		}
		id := int64(i + 1)
		out = append(out, &gh.CheckRun{
			ID:         gh.Ptr(id),
			Name:       gh.Ptr(name),
			Status:     gh.Ptr(status),
			Conclusion: gh.Ptr(conclusion),
			DetailsURL: gh.Ptr(url),
		})
	}
	return out, true
}

func statusFromStatusContext(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUCCESS", "FAILURE", "ERROR":
		return "completed"
	default:
		return "in_progress"
	}
}

func conclusionFromStatusContext(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUCCESS":
		return "success"
	case "FAILURE", "ERROR":
		return "failure"
	default:
		return ""
	}
}

func (c *Client) graphQLBatchCheckRuns(repo string, number int, headSHA string) ([]*gh.CheckRun, bool) {
	if c == nil || strings.TrimSpace(headSHA) == "" {
		return nil, false
	}
	key := prBatchCheckRunKey{repo: canonicalPRDetailRepo(repo), number: number, headSHA: headSHA}
	c.prBatchMu.Lock()
	defer c.prBatchMu.Unlock()
	checks, ok := c.prBatchCheckRuns[key]
	if !ok {
		return nil, false
	}
	return cloneCheckRuns(checks), true
}

func (c *Client) storeGraphQLPRBatchReview(repo string, n graphQLPRBatchNode) {
	c.prBatchMu.Lock()
	defer c.prBatchMu.Unlock()
	if c.prBatchReviews == nil {
		c.prBatchReviews = map[string]map[int]prReviewState{}
	}
	key := canonicalPRDetailRepo(repo)
	byNumber := c.prBatchReviews[key]
	if byNumber == nil {
		byNumber = map[int]prReviewState{}
		c.prBatchReviews[key] = byNumber
	}
	st := prReviewState{decision: ReviewDecision(strings.ToUpper(strings.TrimSpace(n.ReviewDecision)))}
	for _, li := range n.ClosingIssuesReferences.Nodes {
		if li.Number > 0 {
			st.linkedIssues = append(st.linkedIssues, PRLinkedIssue{Number: li.Number})
		}
	}
	byNumber[n.Number] = st
}

func (c *Client) graphQLBatchReviewStates(repo string) (map[int]prReviewState, bool) {
	if c == nil {
		return nil, false
	}
	c.prBatchMu.Lock()
	defer c.prBatchMu.Unlock()
	byNumber, ok := c.prBatchReviews[canonicalPRDetailRepo(repo)]
	if !ok {
		return nil, false
	}
	out := make(map[int]prReviewState, len(byNumber))
	for k, v := range byNumber {
		if len(v.linkedIssues) > 0 {
			v.linkedIssues = append([]PRLinkedIssue(nil), v.linkedIssues...)
		}
		if len(v.changesRequestedBy) > 0 {
			v.changesRequestedBy = append([]string(nil), v.changesRequestedBy...)
		}
		out[k] = v
	}
	return out, true
}

func cloneCheckRuns(in []*gh.CheckRun) []*gh.CheckRun {
	out := make([]*gh.CheckRun, 0, len(in))
	for _, cr := range in {
		if cr == nil {
			continue
		}
		out = append(out, &gh.CheckRun{
			ID:         clonePtr(cr.ID),
			Name:       clonePtr(cr.Name),
			Status:     clonePtr(cr.Status),
			Conclusion: clonePtr(cr.Conclusion),
			DetailsURL: clonePtr(cr.DetailsURL),
			StartedAt:  cloneTimestamp(cr.StartedAt),
			App:        cr.App,
		})
	}
	return out
}

func (c *Client) recordGraphQLPRBatchRate(cost, remaining int, reset time.Time) {
	c.prBatchMu.Lock()
	c.prBatchStats.LastCost = cost
	c.prBatchGraphQLRate = RateLimitEntry{Limit: 5000, Remaining: remaining, Reset: reset, ObservedAt: time.Now()}
	c.prBatchMu.Unlock()
}

func (c *Client) recordGraphQLPRBatchSuccess(prs, pages int) {
	c.prBatchMu.Lock()
	c.prBatchStats.Repos++
	c.prBatchStats.PRs += prs
	c.prBatchStats.Pages += pages
	c.prBatchMu.Unlock()
}

func (c *Client) recordGraphQLPRBatchWebhookSkip() {
	c.prBatchMu.Lock()
	c.prBatchStats.WebhookSkips++
	c.prBatchMu.Unlock()
}

func (c *Client) recordGraphQLPRBatchError() {
	c.prBatchMu.Lock()
	c.prBatchStats.Errors++
	c.prBatchMu.Unlock()
}

func (c *Client) recordGraphQLPRBatchFallback(n int) {
	if n <= 0 {
		return
	}
	c.prBatchMu.Lock()
	c.prBatchStats.Fallbacks += n
	c.prBatchMu.Unlock()
}

func graphQLMergeStateStatusToREST(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

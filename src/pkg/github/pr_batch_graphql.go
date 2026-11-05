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
        baseRefOid
        updatedAt
        isDraft
        mergeable
        mergeStateStatus
        reviewDecision
        latestOpinionatedReviews(first:50){nodes{state submittedAt authorAssociation author{__typename login}}}
        comments(first:1){totalCount}
        reviewThreads(first:1){totalCount}
        labels(first: 30) { nodes { name } }
        author { login }
        baseRefName
        headRefName
        title
        commits(last: 1) {
          nodes {
            commit {
              oid
              statusCheckRollup {
                state
                contexts(first: 100) {
                  totalCount
                  nodes {
                    __typename
                    ... on CheckRun { id databaseId name conclusion status detailsUrl startedAt completedAt checkSuite { app { databaseId } commit { oid } } }
                    ... on StatusContext { id context state targetUrl createdAt }
                  }
                }
              }
            }
          }
        }
        closingIssuesReferences(first: 10) { nodes { number title state stateReason url repository { nameWithOwner } } }
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
	BaseRefOID       string    `json:"baseRefOid"`
	UpdatedAt        time.Time `json:"updatedAt"`
	IsDraft          bool      `json:"isDraft"`
	Mergeable        string    `json:"mergeable"`
	MergeStateStatus string    `json:"mergeStateStatus"`
	reviewDecisionNode
	Labels struct {
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
				OID               string `json:"oid"`
				StatusCheckRollup *struct {
					State    string `json:"state"`
					Contexts struct {
						TotalCount int                     `json:"totalCount"`
						Nodes      []graphQLPRBatchContext `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

type graphQLPRBatchContext struct {
	TypeName    string     `json:"__typename"`
	Name        string     `json:"name"`
	Conclusion  string     `json:"conclusion"`
	Status      string     `json:"status"`
	DetailsURL  string     `json:"detailsUrl"`
	Context     string     `json:"context"`
	State       string     `json:"state"`
	TargetURL   string     `json:"targetUrl"`
	ID          string     `json:"id"`
	DatabaseID  *int64     `json:"databaseId"`
	StartedAt   *time.Time `json:"startedAt"`
	CompletedAt *time.Time `json:"completedAt"`
	CreatedAt   *time.Time `json:"createdAt"`
	CheckSuite  *struct {
		App *struct {
			DatabaseID *int64 `json:"databaseId"`
		} `json:"app"`
		Commit *struct {
			OID string `json:"oid"`
		} `json:"commit"`
	} `json:"checkSuite"`
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
	if sharedWebhookTracker().repoCleanAndHealthy(fullRepo) && c.hasGraphQLPRBatchRepo(fullRepo) {
		c.recordGraphQLPRBatchWebhookSkip()
		return
	}
	started := sharedWebhookTracker().now()
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
	sharedWebhookTracker().clearDirtyRepoBefore(fullRepo, started)
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
			delete(c.prBatchCheckObservedAt, k)
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
				delete(c.prBatchCheckObservedAt, k)
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
		Base:           &gh.PullRequestBranch{SHA: gh.Ptr(n.BaseRefOID), Ref: gh.Ptr(n.BaseRefName)},
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
		if c.prBatchCheckObservedAt == nil {
			c.prBatchCheckObservedAt = map[prBatchCheckRunKey]time.Time{}
		}
		c.prBatchCheckObservedAt[key] = time.Now().UTC()
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

// graphQLCheckRuns converts a PR node's statusCheckRollup into check runs. It
// reports ok=false when the rollup is absent OR when GitHub truncated the
// contexts page: a head with more contexts than one page holds (a 70-job
// matrix plus statuses) would otherwise be cached as a partial listing, and
// enrichPRCI would read the missing required checks as "never reported"
// (hivecommons/hive#16615 showed `CI 7/11 · required check "test" hasn't
// reported` while every check was green). The caller falls back to the
// paginated REST listing instead.
func graphQLCheckRuns(n graphQLPRBatchNode) ([]*gh.CheckRun, bool) {
	if len(n.Commits.Nodes) == 0 || n.Commits.Nodes[0].Commit.StatusCheckRollup == nil {
		return nil, false
	}
	if graphQLContextsTruncated(n) {
		return nil, false
	}
	var out []*gh.CheckRun
	for _, ctx := range n.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts.Nodes {
		name, status, conclusion, url := "", "", "", ""
		switch ctx.TypeName {
		case "CheckRun":
			name, status, conclusion, url = ctx.Name, strings.ToLower(ctx.Status), strings.ToLower(ctx.Conclusion), ctx.DetailsURL
		case "StatusContext":
			name, status, conclusion, url = ctx.Context, statusFromStatusContext(ctx.State), conclusionFromStatusContext(ctx.State), ctx.TargetURL
		default:
			continue
		}
		run := &gh.CheckRun{
			ID:         clonePtr(ctx.DatabaseID),
			NodeID:     gh.Ptr(ctx.ID),
			HeadSHA:    gh.Ptr(n.HeadRefOID),
			Name:       gh.Ptr(name),
			Status:     gh.Ptr(status),
			Conclusion: gh.Ptr(conclusion),
			DetailsURL: gh.Ptr(url),
		}
		if oid := n.Commits.Nodes[0].Commit.OID; oid != "" {
			run.HeadSHA = gh.Ptr(oid)
		}
		if ctx.TypeName == "CheckRun" {
			if ctx.CheckSuite != nil && ctx.CheckSuite.Commit != nil && ctx.CheckSuite.Commit.OID != "" {
				run.HeadSHA = gh.Ptr(ctx.CheckSuite.Commit.OID)
			}
			if ctx.CheckSuite != nil && ctx.CheckSuite.App != nil {
				run.App = &gh.App{ID: clonePtr(ctx.CheckSuite.App.DatabaseID)}
			}
			if ctx.StartedAt != nil {
				run.StartedAt = &gh.Timestamp{Time: *ctx.StartedAt}
			}
			if ctx.CompletedAt != nil {
				run.CompletedAt = &gh.Timestamp{Time: *ctx.CompletedAt}
			}
		} else {
			// Legacy statuses have a real node ID and creation time, but no
			// numeric check-run or producer App identity.
			if ctx.CreatedAt != nil {
				run.StartedAt = &gh.Timestamp{Time: *ctx.CreatedAt}
			}
		}
		out = append(out, run)
	}
	return out, true
}

// graphQLContextsTruncated reports whether the rollup's contexts connection
// holds fewer nodes than GitHub says exist for the head.
func graphQLContextsTruncated(n graphQLPRBatchNode) bool {
	if len(n.Commits.Nodes) == 0 || n.Commits.Nodes[0].Commit.StatusCheckRollup == nil {
		return false
	}
	contexts := n.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts
	return contexts.TotalCount > len(contexts.Nodes)
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
	checks, _, ok := c.graphQLBatchCheckRunsObservation(repo, number, headSHA)
	return checks, ok
}

func (c *Client) graphQLBatchCheckRunsObservation(repo string, number int, headSHA string) ([]*gh.CheckRun, *time.Time, bool) {
	if c == nil || strings.TrimSpace(headSHA) == "" {
		return nil, nil, false
	}

	key := prBatchCheckRunKey{repo: canonicalPRDetailRepo(repo), number: number, headSHA: headSHA}
	c.prBatchMu.Lock()
	defer c.prBatchMu.Unlock()
	checks, ok := c.prBatchCheckRuns[key]
	if !ok {
		return nil, nil, false
	}
	at := c.prBatchCheckObservedAt[key]
	if at.IsZero() {
		return cloneCheckRuns(checks), nil, true
	}
	return cloneCheckRuns(checks), &at, true
}

func (c *Client) graphQLBatchCheckObservedAt(repo string, number int, headSHA string) *time.Time {
	_, at, _ := c.graphQLBatchCheckRunsObservation(repo, number, headSHA)
	return at
}

func (c *Client) storeGraphQLPRBatchReview(repo string, n graphQLPRBatchNode) {
	st := c.reviewStateFromNode(n.reviewDecisionNode)
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
			ID:          clonePtr(cr.ID),
			NodeID:      clonePtr(cr.NodeID),
			HeadSHA:     clonePtr(cr.HeadSHA),
			Name:        clonePtr(cr.Name),
			Status:      clonePtr(cr.Status),
			Conclusion:  clonePtr(cr.Conclusion),
			DetailsURL:  clonePtr(cr.DetailsURL),
			StartedAt:   cloneTimestamp(cr.StartedAt),
			CompletedAt: cloneTimestamp(cr.CompletedAt),
		})
		if cr.App != nil {
			app := *cr.App
			app.ID = clonePtr(cr.App.ID)
			out[len(out)-1].App = &app
		}
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

package github

import (
	"context"
	"strings"
)

// protectionCollector gathers, once per enrichment pass, the per-repository
// data needed to name the branch-protection rule behind a "blocked" PR
// (hivecommons/hive#7515, step 2).
//
// Cost discipline: the required-check set comes from the operator's
// auto_merge.required_checks config when one is installed — zero API calls,
// the primary path — and falls back to the branch-protection API once per
// repo+branch. Review decisions come from ONE GraphQL query per repository,
// paginated 100 PRs at a time, not one per PR. Both are memoised for the
// lifetime of the pass. Nothing is fetched on a dashboard hover.
type protectionCollector struct {
	c *Client
	// required is keyed "owner/repo@branch".
	required map[string]requiredSet
	// reviews is keyed "owner/repo"; a nil map value records a repo whose
	// GraphQL query failed, so it is attempted only once.
	reviews map[string]map[int]prReviewState
}

type requiredSet struct {
	set   map[string]bool
	known bool
}

// prReviewState is GitHub's review verdict for one PR.
type prReviewState struct {
	decision           ReviewDecision
	changesRequestedBy []string
	approvals          int
}

func newProtectionCollector(c *Client) *protectionCollector {
	return &protectionCollector{
		c:        c,
		required: make(map[string]requiredSet),
		reviews:  make(map[string]map[int]prReviewState),
	}
}

// attach populates pr.Protection from the facts on hand. reported is the set
// of check-run names observed on the PR's head commit, or nil when no
// check-run listing was obtained — the distinction matters, because a missing
// listing is not evidence that a required check never ran.
//
// pr.Protection is left nil when nothing at all was determined, so that
// BranchProtectionBlockReason declines to guess rather than inventing a rule.
func (pc *protectionCollector) attach(ctx context.Context, pr *PullRequest, reported map[string]bool) {
	if pc == nil || pc.c == nil || pr == nil {
		return
	}
	var f ProtectionFacts

	if rs, ok := pc.reviewState(ctx, pr.Repo, pr.Number); ok {
		f.ReviewDecision = rs.decision
		f.ChangesRequestedBy = rs.changesRequestedBy
		f.ApprovalsGiven = rs.approvals
	}

	if reported != nil {
		req := pc.requiredChecks(ctx, pr.Repo, pr.BaseRef)
		if req.known {
			f.RequiredChecksKnown = true
			failing := make(map[string]bool, len(pr.FailingChecks))
			for _, n := range pr.FailingChecks {
				failing[n] = true
			}
			// observedRequired proves this repository reports its required
			// contexts as CHECK RUNS. Without that proof a required context
			// absent from the check-run listing may simply be a commit
			// status, which this pass does not fetch; claiming it "has not
			// reported" would be confidently wrong for every required
			// context on such a repo.
			observedRequired := 0
			var missing, red []string
			for name := range req.set {
				switch {
				case failing[name]:
					observedRequired++
					red = append(red, name)
				case reported[name]:
					observedRequired++
				default:
					missing = append(missing, name)
				}
			}
			f.FailingRequiredChecks = red
			if observedRequired > 0 {
				f.MissingRequiredChecks = missing
			}
		}
	}

	if f.ReviewDecision == ReviewDecisionNone && !f.RequiredChecksKnown {
		return
	}
	pr.Protection = &f
}

func (pc *protectionCollector) requiredChecks(ctx context.Context, repo, branch string) requiredSet {
	key := repo + "@" + branch
	if rs, ok := pc.required[key]; ok {
		return rs
	}
	owner, name := pc.c.splitRepo(repo)
	set, known := pc.c.requiredStatusCheckContexts(ctx, owner, name, branch)
	rs := requiredSet{set: set, known: known}
	pc.required[key] = rs
	return rs
}

func (pc *protectionCollector) reviewState(ctx context.Context, repo string, number int) (prReviewState, bool) {
	byNumber, ok := pc.reviews[repo]
	if !ok {
		byNumber = pc.c.fetchReviewDecisions(ctx, repo)
		pc.reviews[repo] = byNumber
	}
	if byNumber == nil {
		return prReviewState{}, false
	}
	rs, ok := byNumber[number]
	return rs, ok
}

// reviewDecisionQuery asks for every open PR's review verdict in one request.
// latestOpinionatedReviews is GitHub's own per-author deduplication: one
// entry per reviewer, carrying that reviewer's current position, which is
// exactly the "0 approvals given" / "changes requested by @x" the operator
// needs and is not derivable from a raw review list.
const reviewDecisionQuery = `query($owner:String!,$name:String!,$cursor:String){
  repository(owner:$owner,name:$name){
    pullRequests(states:OPEN,first:100,after:$cursor){
      pageInfo{hasNextPage endCursor}
      nodes{
        number
        reviewDecision
        latestOpinionatedReviews(first:50){nodes{state author{login}}}
      }
    }
  }
}`

type reviewDecisionResponse struct {
	Repository struct {
		PullRequests struct {
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []struct {
				Number                   int    `json:"number"`
				ReviewDecision           string `json:"reviewDecision"`
				LatestOpinionatedReviews struct {
					Nodes []struct {
						State  string `json:"state"`
						Author *struct {
							Login string `json:"login"`
						} `json:"author"`
					} `json:"nodes"`
				} `json:"latestOpinionatedReviews"`
			} `json:"nodes"`
		} `json:"pullRequests"`
	} `json:"repository"`
}

// fetchReviewDecisions returns GitHub's review verdict for every open PR in
// repo, or nil when the query could not be answered (no GraphQL scope, a GHE
// that does not serve the field, a transport error). nil means "not
// determined" and is never read as "approved".
func (c *Client) fetchReviewDecisions(ctx context.Context, repo string) map[int]prReviewState {
	if c == nil || c.client == nil {
		return nil
	}
	owner, name := c.splitRepo(repo)
	if owner == "" || name == "" {
		return nil
	}
	out := make(map[int]prReviewState)
	vars := map[string]any{"owner": owner, "name": name}
	for page := 0; page < 20; page++ {
		var resp reviewDecisionResponse
		if err := c.graphQL(ctx, reviewDecisionQuery, vars, &resp); err != nil {
			c.logger.Warn("failed to fetch PR review decisions", "repo", repo, "error", err)
			return nil
		}
		for _, n := range resp.Repository.PullRequests.Nodes {
			st := prReviewState{decision: ReviewDecision(strings.ToUpper(strings.TrimSpace(n.ReviewDecision)))}
			for _, r := range n.LatestOpinionatedReviews.Nodes {
				login := ""
				if r.Author != nil {
					login = r.Author.Login
				}
				switch strings.ToUpper(strings.TrimSpace(r.State)) {
				case "APPROVED":
					st.approvals++
				case "CHANGES_REQUESTED":
					if login != "" {
						st.changesRequestedBy = append(st.changesRequestedBy, login)
					}
				}
			}
			out[n.Number] = st
		}
		if !resp.Repository.PullRequests.PageInfo.HasNextPage || resp.Repository.PullRequests.PageInfo.EndCursor == "" {
			break
		}
		vars["cursor"] = resp.Repository.PullRequests.PageInfo.EndCursor
	}
	return out
}

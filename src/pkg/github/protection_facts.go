package github

import (
	"context"
	"errors"
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

// prReviewState is GitHub's review verdict for one PR, plus the display-only
// triage signals that ride the same query (hivecommons/hive#8968): comment
// and review-thread totals and the issues the PR would close.
type prReviewState struct {
	decision           ReviewDecision
	changesRequestedBy []string
	approvals          int
	comments           int
	reviewThreads      int
	linkedIssues       []PRLinkedIssue
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
		// Triage signals go on the PR itself, not under Protection: they
		// are not branch-protection facts, and they are wanted even when
		// GitHub returned no review decision at all (#8968).
		pr.CommentCount = rs.comments
		pr.ReviewThreadCount = rs.reviewThreads
		pr.LinkedIssues = rs.linkedIssues
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

	// Keep the review opinions even without a decision: GitHub reports a
	// null reviewDecision on a base branch with no review rule, and the
	// dashboard still wants "changes requested by @x" / "2 approvals" on
	// such a PR (#8968). The decision itself stays ReviewDecisionNone —
	// unknown, never inferred — exactly as the contract above requires.
	if f.ReviewDecision == ReviewDecisionNone && !f.RequiredChecksKnown && f.ApprovalsGiven == 0 && len(f.ChangesRequestedBy) == 0 {
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

// reviewSignalsQuery is reviewDecisionQuery plus the repo-card triage
// signals (#8968): comment and review-thread totals (totalCount only — no
// thread nodes) and the first 20 closingIssuesReferences behind the PR
// pill's 🔗 badge. It is tried first; if the forge rejects it (a GHE that
// does not serve one of the added fields, a scope that denies it), the
// fetch falls back to reviewDecisionQuery so the review decision — which
// the merge-block wording depends on — is never lost to a display field.
const reviewSignalsQuery = `query($owner:String!,$name:String!,$cursor:String){
  repository(owner:$owner,name:$name){
    pullRequests(states:OPEN,first:100,after:$cursor){
      pageInfo{hasNextPage endCursor}
      nodes{
        number
        reviewDecision
        latestOpinionatedReviews(first:50){nodes{state author{login}}}
        comments(first:1){totalCount}
        reviewThreads(first:1){totalCount}
        closingIssuesReferences(first:20){nodes{number state url repository{nameWithOwner}}}
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
				Comments struct {
					TotalCount int `json:"totalCount"`
				} `json:"comments"`
				ReviewThreads struct {
					TotalCount int `json:"totalCount"`
				} `json:"reviewThreads"`
				ClosingIssuesReferences struct {
					Nodes []struct {
						Number     int    `json:"number"`
						State      string `json:"state"`
						URL        string `json:"url"`
						Repository struct {
							NameWithOwner string `json:"nameWithOwner"`
						} `json:"repository"`
					} `json:"nodes"`
				} `json:"closingIssuesReferences"`
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
	query := reviewSignalsQuery
	for page := 0; page < 20; page++ {
		var resp reviewDecisionResponse
		err := c.graphQL(ctx, query, vars, &resp)
		// Fall back only when the forge answered and rejected the document
		// (*graphQLErrors: an unknown field on an older GHE, a field this
		// token may not read). A transport or scope failure would fail the
		// decision-only query just the same, and that one is asked once.
		var ge *graphQLErrors
		if err != nil && query == reviewSignalsQuery && page == 0 && errors.As(err, &ge) {
			c.logger.Debug("PR review signals query rejected; falling back to review decisions only", "repo", repo, "error", err)
			query = reviewDecisionQuery
			resp = reviewDecisionResponse{}
			err = c.graphQL(ctx, query, vars, &resp)
		}
		if err != nil {
			c.logger.Warn("failed to fetch PR review decisions", "repo", repo, "error", err)
			return nil
		}
		for _, n := range resp.Repository.PullRequests.Nodes {
			st := prReviewState{
				decision:      ReviewDecision(strings.ToUpper(strings.TrimSpace(n.ReviewDecision))),
				comments:      n.Comments.TotalCount,
				reviewThreads: n.ReviewThreads.TotalCount,
			}
			for _, li := range n.ClosingIssuesReferences.Nodes {
				if li.Number <= 0 {
					continue
				}
				st.linkedIssues = append(st.linkedIssues, PRLinkedIssue{
					Number: li.Number,
					Repo:   li.Repository.NameWithOwner,
					State:  strings.ToLower(strings.TrimSpace(li.State)),
					URL:    li.URL,
				})
			}
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

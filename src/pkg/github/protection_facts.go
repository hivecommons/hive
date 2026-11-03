package github

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
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
	// codeOwner is keyed "owner/repo@branch" (hivecommons/hive#12953).
	codeOwner map[string]codeOwnerRule
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
	latestHumanState   ReviewDecision
	latestHumanBy      string
	latestHumanAt      time.Time
	comments           int
	reviewThreads      int
	linkedIssues       []PRLinkedIssue
}

func newProtectionCollector(c *Client) *protectionCollector {
	return &protectionCollector{
		c:         c,
		required:  make(map[string]requiredSet),
		reviews:   make(map[string]map[int]prReviewState),
		codeOwner: make(map[string]codeOwnerRule),
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

	rs, reviewKnown := pc.reviewState(ctx, pr.Repo, pr.Number)
	if reviewKnown {
		f.ReviewDecision = rs.decision
		f.ChangesRequestedBy = rs.changesRequestedBy
		f.ApprovalsGiven = rs.approvals
		f.LatestHumanReviewState = rs.latestHumanState
		f.LatestHumanReviewBy = rs.latestHumanBy
		f.LatestHumanReviewSubmittedAt = rs.latestHumanAt
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
			f.RequiredChecksAllReported = len(req.set) > 0 && len(missing) == 0
			switch {
			case observedRequired > 0:
				f.MissingRequiredChecks = missing
			case len(missing) > 0 && strings.EqualFold(pr.MergeableState, "blocked"):
				// Nothing proves check-run reporting, but GitHub says the head is
				// blocked: look at commit statuses before deciding. A context
				// absent from both listings was never created, and must not be
				// reported as merge-eligible (#11892).
				if statuses, ok := pc.c.commitStatusContexts(ctx, pr.Repo, pr.HeadSHA); ok {
					var never []string
					for _, name := range missing {
						if !statuses[name] {
							never = append(never, name)
						}
					}
					sort.Strings(never)
					f.MissingRequiredChecks = never
				}
			}
		}
	}

	// A null review decision on a blocked PR may hide an outstanding
	// code-owner review (#12953). The rule is looked up only then — and only
	// when the decision was actually fetched — once per repo+branch.
	if reviewKnown && f.ReviewDecision == ReviewDecisionNone && strings.EqualFold(pr.MergeableState, "blocked") {
		if rule := pc.codeOwnerRule(ctx, pr.Repo, pr.BaseRef); rule.required {
			f.CodeOwnerReviewRequired = true
			f.CodeOwnerRuleSource = rule.source
		}
	}

	// Keep the review opinions even without a decision: GitHub reports a
	// null reviewDecision on a base branch with no review rule, and the
	// dashboard still wants "changes requested by @x" / "2 approvals" on
	// such a PR (#8968). The decision itself stays ReviewDecisionNone —
	// unknown, never inferred — exactly as the contract above requires.
	if f.ReviewDecision == ReviewDecisionNone && !f.RequiredChecksKnown && f.ApprovalsGiven == 0 && len(f.ChangesRequestedBy) == 0 && !f.CodeOwnerReviewRequired {
		return
	}
	pr.Protection = &f
}

// commitStatusContexts returns the contexts of every commit status on sha, or
// ok=false when the listing could not be obtained.
func (c *Client) commitStatusContexts(ctx context.Context, repo, sha string) (map[string]bool, bool) {
	if sha == "" {
		return nil, false
	}
	owner, name := c.splitRepo(repo)
	contexts := map[string]bool{}
	opts := &gh.ListOptions{PerPage: 100}
	for {
		status, resp, err := c.client.Repositories.GetCombinedStatus(ctx, owner, name, sha, opts)
		if err != nil {
			return nil, false
		}
		for _, st := range status.Statuses {
			contexts[st.GetContext()] = true
		}
		if resp == nil || resp.NextPage == 0 {
			return contexts, true
		}
		opts.Page = resp.NextPage
	}
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

func (pc *protectionCollector) codeOwnerRule(ctx context.Context, repo, branch string) codeOwnerRule {
	key := repo + "@" + branch
	if rule, ok := pc.codeOwner[key]; ok {
		return rule
	}
	owner, name := pc.c.splitRepo(repo)
	rule := pc.c.codeOwnerReviewRule(ctx, owner, name, branch)
	pc.codeOwner[key] = rule
	return rule
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
        latestOpinionatedReviews(first:50){nodes{state submittedAt authorAssociation author{__typename login}}}
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
        latestOpinionatedReviews(first:50){nodes{state submittedAt authorAssociation author{__typename login}}}
        comments(first:1){totalCount}
        reviewThreads(first:1){totalCount}
        closingIssuesReferences(first:20){nodes{number title state stateReason url repository{nameWithOwner}}}
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
						State             string    `json:"state"`
						SubmittedAt       time.Time `json:"submittedAt"`
						AuthorAssociation string    `json:"authorAssociation"`
						Author            *struct {
							Typename string `json:"__typename"`
							Login    string `json:"login"`
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
						Number      int    `json:"number"`
						Title       string `json:"title"`
						State       string `json:"state"`
						StateReason string `json:"stateReason"`
						URL         string `json:"url"`
						Repository  struct {
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
	if batch, ok := c.graphQLBatchReviewStates(repo); ok {
		return batch
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
			// Transient forge outages leave reviews unknown until the next poll.
			// Keep them out of the WARN-fed fleet defect reporter while retaining
			// warnings for permission, schema and other persistent failures.
			if isTransientEnumerationError(err) || errors.Is(err, context.Canceled) {
				c.logger.Debug("failed to fetch PR review decisions", "repo", repo, "error", err)
			} else {
				c.logger.Warn("failed to fetch PR review decisions", "repo", repo, "error", err)
			}
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
					Number:      li.Number,
					Title:       li.Title,
					Repo:        li.Repository.NameWithOwner,
					State:       strings.ToLower(strings.TrimSpace(li.State)),
					StateReason: strings.ToLower(strings.TrimSpace(li.StateReason)),
					URL:         li.URL,
				})
			}
			for _, r := range n.LatestOpinionatedReviews.Nodes {
				login := ""
				typename := ""
				if r.Author != nil {
					login = r.Author.Login
					typename = r.Author.Typename
				}
				state := ReviewDecision(strings.ToUpper(strings.TrimSpace(r.State)))
				switch state {
				case ReviewDecisionApproved:
					st.approvals++
				case ReviewDecisionChangesRequested:
					if login != "" {
						st.changesRequestedBy = append(st.changesRequestedBy, login)
					}
				}
				if c.isTrustedHumanReviewAuthor(login, typename, r.AuthorAssociation) && (state == ReviewDecisionApproved || state == ReviewDecisionChangesRequested) && r.SubmittedAt.After(st.latestHumanAt) {
					st.latestHumanState = state
					st.latestHumanBy = login
					st.latestHumanAt = r.SubmittedAt
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

func (c *Client) isTrustedHumanReviewAuthor(login, typename, association string) bool {
	login = strings.TrimSpace(login)
	if login == "" || strings.EqualFold(strings.TrimSpace(typename), "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return false
	}
	if !trustedCommentAssociations[strings.ToUpper(strings.TrimSpace(association))] {
		return false
	}
	if c != nil && c.isHiveLogin(login) {
		return false
	}
	if c != nil && c.getReviewBots().IsBot(login) {
		return false
	}
	return true
}

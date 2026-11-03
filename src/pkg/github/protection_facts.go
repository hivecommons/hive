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
}

type requiredSet struct {
	set      map[string]bool
	known    bool
	bindings []PRRequiredCheck
	source   string
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
		if pr.CIActivity != nil {
			pr.CIActivity.setRequired(req.set, req.known)
			pr.CIActivity.RequiredChecks = req.bindings
			pr.CIActivity.RequiredSource = req.source
			pr.CIActivity.RequiredKnown = req.known && req.source != "config" && observedRequiredBindings(req.set, req.bindings)
		}
		if req.known {
			f.RequiredChecksKnown = true
			needsStatuses := false
			if pr.CIActivity != nil {
				needsStatuses = pr.CIActivity.NeedsCommitStatusEvidence()
			}
			for name := range req.set {
				present := reported[name]
				if pr.CIActivity != nil && len(req.bindings) > 0 {
					present = len(pr.CIActivity.ChecksForRequired(name)) > 0
				}
				needsStatuses = needsStatuses || !present
			}
			statusesKnown := false
			statuses := map[string]string{}
			if needsStatuses {
				collected, known := pc.c.commitStatuses(ctx, pr.Repo, pr.HeadSHA)
				statusesKnown = known
				if known {
					for _, status := range collected {
						statuses[status.GetContext()] = status.GetState()
					}
					if pr.CIActivity != nil {
						pr.CIActivity.addCommitStatuses(collected, time.Now().UTC())
					}
				}
			}
			failing := make(map[string]bool, len(pr.FailingChecks))
			for _, n := range pr.FailingChecks {
				failing[n] = true
			}
			// A check-run listing alone cannot prove a context is missing:
			// legacy commit statuses may provide it. Failed status reads
			// leave absence unknown, including for App-bound policy.
			observedRequired := 0
			var missing, red []string
			for name := range req.set {
				// Canonical facts retain their name-set contract. App-aware
				// display ambiguity must not become a new merge policy.
				_, statusReported := statuses[name]
				switch {
				case failing[name] || statuses[name] == "failure" || statuses[name] == "error":
					observedRequired++
					red = append(red, name)
				case reported[name] || statusReported:
					observedRequired++
				default:
					if statusesKnown {
						missing = append(missing, name)
					}
				}
			}
			f.FailingRequiredChecks = red
			if observedRequired > 0 || strings.EqualFold(pr.MergeableState, "blocked") {
				sort.Strings(missing)
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

// commitStatuses retains state and identity, not just context presence.
// A failed, incomplete or explicitly wrong-head listing is unknown.
func (c *Client) commitStatuses(ctx context.Context, repo, sha string) ([]*gh.RepoStatus, bool) {
	if sha == "" {
		return nil, false
	}
	owner, name := c.splitRepo(repo)
	var statuses []*gh.RepoStatus
	opts := &gh.ListOptions{PerPage: 100}
	for {
		status, resp, err := c.client.Repositories.GetCombinedStatus(ctx, owner, name, sha, opts)
		if err != nil || status == nil || status.Statuses == nil || (status.GetSHA() != "" && !strings.EqualFold(status.GetSHA(), sha)) {
			return nil, false
		}
		for _, st := range status.Statuses {
			if st == nil || strings.TrimSpace(st.GetContext()) == "" {
				return nil, false
			}
			statuses = append(statuses, st)
		}
		if resp == nil || resp.NextPage == 0 {
			return statuses, true
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
	configSet, configKnown := pc.c.configRequiredChecksForRepo(owner + "/" + name)
	set, known, _, _, source, bindings := RequiredStatusCheckPolicy(ctx, pc.c.client, owner, name, branch, configSet, configKnown)
	rs := requiredSet{set: set, known: known, bindings: bindings, source: source}
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

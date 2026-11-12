package github

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// PRRef identifies a pull request parsed out of a GitHub PR URL.
type PRRef struct {
	Owner  string
	Repo   string
	Number int
}

// FullName returns "owner/repo".
func (r PRRef) FullName() string { return r.Owner + "/" + r.Repo }

// prURLPattern matches the "…/{owner}/{repo}/pull/{number}" tail of a GitHub PR
// URL. It is deliberately host-agnostic (works for github.com AND GHE hosts) and
// anchored on the "/pull/" segment so trailing paths (/files, #discussion) and
// query strings do not defeat it. Only the FIRST such match in the string is
// used by ParsePRURL — the reported URL is expected to be a single PR link.
var prURLPattern = regexp.MustCompile(`([^/\s]+)/([^/\s]+)/pull/(\d+)`)

// ParsePRURL extracts owner, repo and PR number from a GitHub pull-request URL.
// It accepts full URLs (https://github.com/owner/repo/pull/123, GHE hosts, with
// or without trailing path/fragment/query) and is intentionally strict about the
// "/pull/<number>" shape so an arbitrary non-PR URL does not parse. It returns an
// error when no PR reference can be found.
func ParsePRURL(prURL string) (PRRef, error) {
	s := strings.TrimSpace(prURL)
	if s == "" {
		return PRRef{}, fmt.Errorf("empty PR URL")
	}
	m := prURLPattern.FindStringSubmatch(s)
	if m == nil {
		return PRRef{}, fmt.Errorf("not a recognizable GitHub pull request URL: %q", prURL)
	}
	num, err := strconv.Atoi(m[3])
	if err != nil || num <= 0 {
		return PRRef{}, fmt.Errorf("invalid PR number in URL %q", prURL)
	}
	return PRRef{Owner: m[1], Repo: m[2], Number: num}, nil
}

// PRVerification is the result of VerifyReportedPR. Verified is true only when
// the PR exists, its base repo matches the expected repo, and its author matches
// the expected contributor. When Verified is false, Reason explains why (used for
// logging/audit) and Err is set when the failure was a GitHub API error (as
// opposed to a clean "PR does not match" negative).
type PRVerification struct {
	Verified bool
	Reason   string
	Err      error
	// Author is the resolved PR author login (best-effort, for logging).
	Author string
	// BaseRepo is the resolved PR base repo "owner/repo" (best-effort, for logging).
	BaseRepo string
	// Merged reports whether GitHub says the PR is MERGED (not merely open or
	// closed). Verification deliberately does not require it — a reported PR
	// that exists, targets the right repo and has the right author still earns
	// the completion cooldown while review is pending — so callers that need
	// "a fix actually landed" must check this field explicitly.
	Merged bool
	// State is GitHub's pull request state ("open" or "closed"). Merged remains
	// separate because a merged pull request is also closed.
	State string
	// MergedAt is GitHub's merge timestamp for merged PRs. It lets callers date
	// effects of the merge to when the fix landed instead of when they observed it.
	MergedAt time.Time
	// Title is the PR's own title as GitHub reports it. Callers must use this
	// rather than the assignment's issue title when they mean "what did this PR
	// say it does".
	Title string
}

// VerifyReportedPR checks a client-reported PR URL against GitHub server-side
// before the hub is willing to trust it (see kubestellar/hive#2565). The
// contributor relay scrapes a PR URL out of tmux output and reports it on
// task_complete; that value is entirely client-supplied, so on its own it must
// not drive the long completion cooldown or trust promotion. This resolves the
// URL via the GitHub API and confirms three things:
//
//  1. Exists      — the PR resolves via GET /repos/{owner}/{repo}/pulls/{number}.
//  2. Assigned    — the PR's BASE repo equals expectedRepo (the repo of the
//     assignment). This closes the relay's "first PR URL mentioned anywhere in
//     the tmux tail" fallback: an unrelated PR link no longer counts.
//  3. Author      — the PR author login equals expectedAuthor (the connected
//     contributor's GitHub identity, which the hub already knows).
//
// expectedRepo may be a bare repo name ("repo") or "owner/repo"; only the repo
// component is compared when no owner is supplied, so callers that track just the
// short repo name still match. Comparisons are case-insensitive (GitHub logins
// and repo names are).
//
// The method never panics and never returns an error to abort a caller: a
// GitHub API failure (rate limit, transient network, 404) yields
// Verified=false with Err set, so the completion handler can degrade to the
// short cooldown / no trust credit rather than crashing or stranding the
// contributor. A nil client (hive without GitHub credentials) is treated the
// same way — it cannot verify, so it must not grant trust.
//
// One failure is retried through another door. When the REST read is refused
// because the hive's PRIMARY (hourly) REST budget is spent, the same pull
// request is read once through GraphQL, which GitHub meters in a separate
// bucket, and the same checks run on what it returns (see
// verifyReportedPRViaGraphQL). If that read fails too, or cannot be mapped to
// the REST form with certainty, the result is the REST failure unchanged.
func (c *Client) VerifyReportedPR(ctx context.Context, expectedRepo, prURL, expectedAuthor string) PRVerification {
	if c == nil || c.client == nil {
		return PRVerification{Reason: "no github client configured", Err: ErrNoGitHubClient}
	}

	ref, err := ParsePRURL(prURL)
	if err != nil {
		// A malformed / non-PR URL is a clean negative, not an API error: there
		// is nothing to look up. No Err → caller treats it as "no verified PR".
		return PRVerification{Reason: "unparseable PR URL: " + err.Error()}
	}

	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:pullrequest_verify"), ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		if prVerifyPrimaryRateLimited(err) {
			// The REST budget for the hour is spent, so the PR itself was
			// never looked at. GraphQL is a separate bucket: ask there once.
			// Anything short of a settled lookup falls through to the REST
			// failure below.
			if v, ok := c.verifyReportedPRViaGraphQL(ctx, ref, expectedRepo, expectedAuthor, err); ok {
				return v
			}
		}
		// Transient/permission/404 — fail CLOSED on trust, but surface Err so the
		// caller logs it and treats the completion as unverified rather than
		// aborting. We do NOT distinguish 404 here: a PR the App cannot see is,
		// for trust purposes, indistinguishable from one that does not exist.
		return PRVerification{Reason: "github lookup failed", Err: err}
	}
	if pr == nil {
		return PRVerification{Reason: "github returned no pull request"}
	}

	author := safeGetLogin(pr.GetUser())
	baseRepo := pr.GetBase().GetRepo().GetFullName() // "owner/repo"
	// Merged/Title are recorded even on the negative paths below: they describe
	// the PR that was looked up, not the verdict.
	v := PRVerification{Author: author, BaseRepo: baseRepo, Merged: pr.GetMerged(), State: pr.GetState(), MergedAt: pr.GetMergedAt().Time, Title: pr.GetTitle()}
	return prVerifyAssignedAndAuthor(v, expectedRepo, expectedAuthor)
}

// prVerifyAssignedAndAuthor runs checks 2 and 3 of VerifyReportedPR on a pull
// request that was looked up (v.BaseRepo and v.Author filled in) and returns v
// with the verdict. It is the one place those checks and their reason texts
// live, so the REST read and the GraphQL fallback cannot drift apart.
func prVerifyAssignedAndAuthor(v PRVerification, expectedRepo, expectedAuthor string) PRVerification {
	if !prBaseRepoMatches(v.BaseRepo, expectedRepo) {
		v.Reason = fmt.Sprintf("PR base repo %q does not match assigned repo %q", v.BaseRepo, expectedRepo)
		return v
	}
	if expectedAuthor == "" || v.Author == "" || !strings.EqualFold(v.Author, expectedAuthor) {
		v.Reason = fmt.Sprintf("PR author %q does not match contributor %q", v.Author, expectedAuthor)
		return v
	}

	v.Verified = true
	return v
}

// prVerifyPrimaryRateLimited reports whether a failed REST read was refused
// for the PRIMARY (hourly) rate limit: go-github's *RateLimitError, which it
// returns both when GitHub answers 403 with X-RateLimit-Remaining: 0 and when
// the library refuses locally until the recorded reset ("not making remote
// request"). That is the only failure the GraphQL fallback answers.
//
// A secondary (abuse) limit is excluded on purpose: there GitHub is asking the
// hive to slow down, and another request is the wrong reply. go-github types a
// 403 as *RateLimitError whenever X-RateLimit-Remaining is 0, BEFORE it looks
// for the secondary-limit markers, so a secondary refusal that also reports an
// empty budget arrives as *RateLimitError too. The two markers the pacer uses
// to tell them apart (slowstart.go: a Retry-After header, or the "secondary
// rate limit" phrase) are therefore checked here as well.
func prVerifyPrimaryRateLimited(err error) bool {
	var abuse *gh.AbuseRateLimitError
	if errors.As(err, &abuse) {
		return false
	}
	var rl *gh.RateLimitError
	if !errors.As(err, &rl) || rl == nil {
		return false
	}
	if rl.Response != nil && strings.TrimSpace(rl.Response.Header.Get("Retry-After")) != "" {
		return false
	}
	return !strings.Contains(strings.ToLower(rl.Message), "secondary rate limit")
}

// prVerifyGraphQLFallbackTimeout bounds the GraphQL fallback read. The
// completion handler awaits VerifyReportedPR on the contributor's message
// loop, and while the rate-limit pacer is cautious a request may wait for a
// slot; a deadline makes the pacer refuse at once when the slot is further
// away than this instead of queueing behind it.
const prVerifyGraphQLFallbackTimeout = 10 * time.Second

// prVerifyGraphQLQuery reads the fields VerifyReportedPR takes from the REST
// pull request. Owner, name and number travel as variables, never in the text.
const prVerifyGraphQLQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      state
      merged
      mergedAt
      title
      author { __typename login }
      baseRepository { nameWithOwner }
    }
  }
}`

// prVerifyGraphQLResponse is the data object prVerifyGraphQLQuery returns.
// Every level is a pointer because GraphQL answers null for a repository or
// pull request it cannot resolve and for a deleted author.
type prVerifyGraphQLResponse struct {
	Repository *struct {
		PullRequest *struct {
			State    string     `json:"state"`
			Merged   bool       `json:"merged"`
			MergedAt *time.Time `json:"mergedAt"`
			Title    string     `json:"title"`
			Author   *struct {
				TypeName string `json:"__typename"`
				Login    string `json:"login"`
			} `json:"author"`
			BaseRepository *struct {
				NameWithOwner string `json:"nameWithOwner"`
			} `json:"baseRepository"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

// prVerificationFromGraphQL maps a GraphQL pull request onto the fields the
// REST read fills, in the REST form, or returns an error when it cannot do so
// with certainty:
//
//   - State: GraphQL says OPEN, CLOSED or MERGED; REST says "open" or
//     "closed", and a merged pull request is "closed" with Merged true.
//   - BaseRepo: baseRepository.nameWithOwner is REST's base.repo.full_name.
//   - Author: only a User author is accepted. For a Bot, GraphQL returns the
//     login without the "[bot]" suffix REST carries, and nothing guarantees
//     that adding it back gives the REST login for every bot; the other actor
//     types (Mannequin, Organization, EnterpriseUserAccount) and a deleted
//     author (null) are not mapped either. Those pull requests stay unverified
//     through the fallback, exactly as they were before it existed.
func prVerificationFromGraphQL(out prVerifyGraphQLResponse) (PRVerification, error) {
	if out.Repository == nil || out.Repository.PullRequest == nil {
		return PRVerification{}, errors.New("graphql returned no pull request")
	}
	pr := out.Repository.PullRequest
	if pr.Author == nil {
		return PRVerification{}, errors.New("graphql returned no author for the pull request")
	}
	if pr.Author.TypeName != "User" {
		return PRVerification{}, fmt.Errorf("pull request author is a %q account; its REST login cannot be derived with certainty", pr.Author.TypeName)
	}
	v := PRVerification{Author: pr.Author.Login, Merged: pr.Merged, Title: pr.Title}
	switch pr.State {
	case "OPEN":
		v.State = "open"
	case "CLOSED", "MERGED":
		v.State = "closed"
	default:
		return PRVerification{}, fmt.Errorf("unrecognized pull request state %q", pr.State)
	}
	if pr.BaseRepository != nil {
		v.BaseRepo = pr.BaseRepository.NameWithOwner
	}
	if pr.MergedAt != nil {
		v.MergedAt = *pr.MergedAt
	}
	return v, nil
}

// verifyReportedPRViaGraphQL is VerifyReportedPR's fallback for a REST read
// refused by the primary rate limit (restErr). It reads the same pull request
// once through GraphQL and runs the same base-repo and author checks. ok is
// false when the read failed, returned no pull request, or could not be mapped
// to the REST form; the caller then returns the REST failure it already has,
// so nothing downstream sees a new kind of result. The fallback's own error is
// only logged.
//
// The request carries the same caller tag as the REST read, the convention for
// a component that uses both APIs (pr_contributor_link.go), so API-usage
// accounting lists it under hive:pullrequest_verify on endpoint /graphql.
//
// It helps only while the GraphQL bucket has room. On a host whose GraphQL
// path is not exactly "/graphql" (graphQLEndpoint maps a ".../api/v3" REST
// base to "/api/graphql"), go-github files the request under the core
// category, so its local refusal covers this request too and the result is
// the REST failure, as before.
func (c *Client) verifyReportedPRViaGraphQL(ctx context.Context, ref PRRef, expectedRepo, expectedAuthor string, restErr error) (PRVerification, bool) {
	pr := ref.FullName() + "#" + strconv.Itoa(ref.Number)
	fctx, cancel := context.WithTimeout(WithRESTCaller(ctx, "hive:pullrequest_verify"), prVerifyGraphQLFallbackTimeout)
	defer cancel()

	var out prVerifyGraphQLResponse
	vars := map[string]any{"owner": ref.Owner, "name": ref.Repo, "number": ref.Number}
	err := c.graphQL(fctx, prVerifyGraphQLQuery, vars, &out)
	var v PRVerification
	if err == nil {
		v, err = prVerificationFromGraphQL(out)
	}
	if err != nil {
		if c.logger != nil {
			// "error" is the REST rate-limit refusal that started this, so the
			// line is classed with the rest of the rate-limit episode.
			c.logger.Warn("PR verification: REST read was rate limited and the GraphQL fallback did not settle it; result is the REST failure",
				"pr", pr, "error", restErr.Error(), "fallback_error", err.Error())
		}
		return PRVerification{}, false
	}

	v = prVerifyAssignedAndAuthor(v, expectedRepo, expectedAuthor)
	if c.logger != nil {
		c.logger.Info("PR verification: REST read was rate limited; pull request read through GraphQL instead",
			"pr", pr, "verified", v.Verified, "reason", v.Reason, "author", v.Author, "base_repo", v.BaseRepo, "state", v.State, "merged", v.Merged)
	}
	return v, true
}

// prBaseRepoMatches reports whether a PR's base repo ("owner/repo", from the API)
// satisfies the expected repo. expected may be bare ("repo") or "owner/repo".
// When expected carries an owner, both owner and repo must match; when it is a
// bare name, only the repo component is compared (callers that track only the
// short repo name still match). All comparisons are case-insensitive.
func prBaseRepoMatches(baseFullName, expected string) bool {
	baseFullName = strings.TrimSpace(baseFullName)
	expected = strings.TrimSpace(expected)
	if baseFullName == "" || expected == "" {
		return false
	}
	baseOwner, baseRepo := splitRepo(baseFullName)
	if expOwner, expRepo := splitRepo(expected); expOwner != "" {
		return strings.EqualFold(expOwner, baseOwner) && strings.EqualFold(expRepo, baseRepo)
	} else {
		return strings.EqualFold(expRepo, baseRepo)
	}
}

// splitRepo splits "owner/repo" into its parts. A bare "repo" yields an empty
// owner. Anything with more than one slash keeps only the last two segments.
func splitRepo(full string) (owner, repo string) {
	full = strings.Trim(strings.TrimSpace(full), "/")
	if full == "" {
		return "", ""
	}
	parts := strings.Split(full, "/")
	if len(parts) == 1 {
		return "", parts[0]
	}
	return parts[len(parts)-2], parts[len(parts)-1]
}

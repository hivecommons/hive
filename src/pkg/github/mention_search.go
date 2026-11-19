package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// The Customers accept-time search (hivecommons/hive#18884). One request asks
// GitHub's issue search for the issues and pull requests that mention a login
// or that the login commented on, scoped to the hive's own repositories. It
// goes through the shared transport, so the rate-limit tracker, the REST
// accounting (caller mentionSearchCaller) and the slow-start pacer all see
// it. It reads no text: only the repository, number, kind and update time of
// each hit are returned.
const (
	// mentionSearchCaller labels the request in /api/gh-rate-limits.
	mentionSearchCaller = "customers_mention_search"
	// MentionSearchKindMentions and MentionSearchKindCommenter are the two
	// search qualifiers the call accepts.
	MentionSearchKindMentions  = "mentions"
	MentionSearchKindCommenter = "commenter"
	// mentionSearchMaxPerPage is GitHub's page size limit.
	mentionSearchMaxPerPage = 100
	// mentionSearchLowRemaining is the search-bucket remaining at or below
	// which optional searches stand down. GitHub allows 30 search requests a
	// minute, so a reading this low means other callers are using it.
	mentionSearchLowRemaining = 5
)

// MentionHit is one issue or pull request found by SearchMentions.
type MentionHit struct {
	// Repo is "owner/name".
	Repo      string
	Number    int
	IsPR      bool
	UpdatedAt time.Time
}

// OptionalSearchStandDown returns a short reason when optional work should
// not spend GitHub quota now, and "" when it may: the API budget mode is not
// normal, the secondary-limit caution window is engaged, or the core or
// search bucket is low. The reasons are fixed words, safe to log.
func (c *Client) OptionalSearchStandDown() string {
	if c == nil {
		return "no_client"
	}
	if mode, _ := c.APIBudgetMode(); mode != APIBudgetNormal {
		return "api_budget_" + mode.String()
	}
	if RateLimitCautionEngaged() {
		return "caution_window"
	}
	if !LowValueRESTWorkAllowed() {
		return "core_low"
	}
	if remaining, _, ok := RateLimitBudget("search"); ok && remaining <= mentionSearchLowRemaining {
		return "search_low"
	}
	return ""
}

// mentionSearchQuery builds "<kind>:<login> repo:o/r ...", adding repositories
// until GitHub's query length limit. It returns "" when no repository fits.
func mentionSearchQuery(kind, login string, repos []string) string {
	q := kind + ":" + login
	added := 0
	seen := map[string]bool{}
	for _, r := range repos {
		r = strings.TrimSpace(r)
		key := strings.ToLower(r)
		if r == "" || !strings.Contains(r, "/") || seen[key] {
			continue
		}
		seen[key] = true
		next := q + " repo:" + r
		if len(next) > searchQueryMaxLen {
			break
		}
		q = next
		added++
	}
	if added == 0 {
		return ""
	}
	return q
}

// SearchMentions makes ONE search request (one page of at most limit hits)
// for the issues and pull requests that mention login (kind
// MentionSearchKindMentions) or that login commented on (kind
// MentionSearchKindCommenter), in repos. repos entries are "owner/name"; a
// bare name is qualified with the client's organisation. The request is
// tagged low priority, so the pacer sheds it first under caution.
func (c *Client) SearchMentions(ctx context.Context, kind, login string, repos []string, limit int) ([]MentionHit, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	if kind != MentionSearchKindMentions && kind != MentionSearchKindCommenter {
		return nil, fmt.Errorf("mention search: unknown kind")
	}
	login = strings.TrimPrefix(strings.TrimSpace(login), "@")
	if login == "" || strings.ContainsAny(login, " \t\r\n:") {
		return nil, fmt.Errorf("mention search: invalid login")
	}
	if limit <= 0 || limit > mentionSearchMaxPerPage {
		limit = mentionSearchMaxPerPage
	}
	qualified := make([]string, 0, len(repos))
	for _, r := range repos {
		if owner, name := c.splitRepo(strings.TrimSpace(r)); owner != "" && name != "" {
			qualified = append(qualified, owner+"/"+name)
		}
	}
	query := mentionSearchQuery(kind, login, qualified)
	if query == "" {
		return nil, nil
	}
	ctx = WithPacingClass(WithRESTCaller(ctx, mentionSearchCaller), PacingLow)
	result, _, err := c.client.Search.Issues(ctx, query, &gh.SearchOptions{
		Sort: "updated", Order: "desc", ListOptions: gh.ListOptions{PerPage: limit},
	})
	if err != nil {
		return nil, classifyMentionSearchError(ctx, err)
	}
	out := make([]MentionHit, 0, len(result.Issues))
	for _, is := range result.Issues {
		if is == nil || is.GetNumber() <= 0 {
			continue
		}
		o, r := blockerRepoFromURLs(is.GetRepositoryURL(), is.GetHTMLURL())
		if o == "" || r == "" {
			continue
		}
		out = append(out, MentionHit{Repo: o + "/" + r, Number: is.GetNumber(), IsPR: is.IsPullRequest(), UpdatedAt: is.GetUpdatedAt().Time})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ErrMentionSearchRateLimited is returned when GitHub refused the search for
// a rate-limit reason (primary or secondary) or the pacer shed it.
var ErrMentionSearchRateLimited = errors.New("mention search: rate limited")

// classifyMentionSearchError reduces a transport error to a class. go-github
// errors and URL errors echo the request URL, which holds the login being
// searched, so the original text is never returned.
func classifyMentionSearchError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var rl *gh.RateLimitError
	var abuse *gh.AbuseRateLimitError
	if errors.Is(err, ErrPacingShed) || errors.As(err, &rl) || errors.As(err, &abuse) {
		return ErrMentionSearchRateLimited
	}
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		return fmt.Errorf("mention search failed: HTTP %d", ghErr.Response.StatusCode)
	}
	return errors.New("mention search failed")
}

// profileLookupCaller labels the request in /api/gh-rate-limits.
const profileLookupCaller = "customers_profile_lookup"

// PublicProfile is the part of a person's public GitHub profile a Customers
// profile keeps: the name, company and location they chose to publish. The
// bio, email and everything else are dropped.
type PublicProfile struct {
	Name     string
	Company  string
	Location string
}

// GetPublicProfile makes ONE request for the public profile of a login. Like
// SearchMentions it goes through the shared transport at low priority, and
// it never returns the original error text, which echoes the login.
func (c *Client) GetPublicProfile(ctx context.Context, login string) (PublicProfile, error) {
	if c == nil || c.client == nil {
		return PublicProfile{}, ErrNoGitHubClient
	}
	login = strings.TrimPrefix(strings.TrimSpace(login), "@")
	if login == "" || strings.ContainsAny(login, " \t\r\n:/") {
		return PublicProfile{}, fmt.Errorf("profile lookup: invalid login")
	}
	ctx = WithPacingClass(WithRESTCaller(ctx, profileLookupCaller), PacingLow)
	u, _, err := c.client.Users.Get(ctx, login)
	if err != nil {
		return PublicProfile{}, classifyMentionSearchError(ctx, err)
	}
	return PublicProfile{Name: u.GetName(), Company: u.GetCompany(), Location: u.GetLocation()}, nil
}

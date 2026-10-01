package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/effects"
)

// GitHub "blocked by" issue dependencies (hivecommons/hive#9839).
//
// When an agent splits an issue it often knows the children must land in a
// certain order, and until now that order lived only in a comment: the kick
// list ranked the children oldest-first and an agent could start on one whose
// prerequisite was not done. GitHub has a native relation for exactly this —
// "blocked by" — and this file is both halves of using it:
//
//   - WRITE: the issue-request relay accepts `blocked_by` on an issue create
//     and links the new issue to each blocker (AddBlockedBy), so a split's
//     order is recorded where every reader, human or hive, can see it.
//   - READ: enumeration decodes the `issue_dependencies_summary` GitHub puts
//     on every issue payload and, only for issues that report an open blocker
//     count, fetches the blocker list into Issue.DependsOn. Downstream, the
//     kick list keeps an issue with an open blocker out of the ready work and
//     contributor admission (observeExternalDependencies) refuses it, exactly
//     as it already does for Linear/Jira dependency edges. When the blocker
//     closes the next enumeration sees Resolved=true and the child is simply
//     ready — nobody relabels or reorders anything.
//
// go-github v72 has no typed wrapper for either the summary or the
// dependencies endpoints, so the requests are built through the same
// NewRequest/Do escape hatch sub_issues.go uses.

// issueDependenciesSummary is the per-issue count block GitHub includes on
// issue payloads. Only blocked_by is read: it is the number of OPEN blockers,
// which is the signal that decides whether a per-issue fetch is worth making.
// total_blocked_by counts closed blockers too and is kept only for logging.
type issueDependenciesSummary struct {
	BlockedBy      int `json:"blocked_by"`
	TotalBlockedBy int `json:"total_blocked_by"`
}

// issueWithDependencies is gh.Issue plus the summary block go-github drops.
// gh.Issue has no custom UnmarshalJSON, so embedding it decodes every field
// go-github knows about and this struct adds the one it does not.
type issueWithDependencies struct {
	gh.Issue
	DependenciesSummary *issueDependenciesSummary `json:"issue_dependencies_summary,omitempty"`
}

// listOpenIssuesWithBlockedCounts is Issues.ListByRepo(state=open) with the
// dependency summary preserved. It returns the issues go-github would have
// and, keyed by issue number, how many OPEN blockers each one reports.
func (c *Client) listOpenIssuesWithBlockedCounts(ctx context.Context, owner, repoName string) ([]*gh.Issue, map[int]int, error) {
	var all []*gh.Issue
	blocked := map[int]int{}
	page := 1
	for {
		q := url.Values{}
		q.Set("state", "open")
		q.Set("per_page", "100")
		q.Set("page", strconv.Itoa(page))
		path := fmt.Sprintf("repos/%s/%s/issues?%s", owner, repoName, q.Encode())
		req, err := c.client.NewRequest(http.MethodGet, path, nil)
		if err != nil {
			return nil, nil, err
		}
		var issues []*issueWithDependencies
		resp, err := c.client.Do(ctx, req, &issues)
		if err != nil {
			return nil, nil, err
		}
		for _, is := range issues {
			if is == nil {
				continue
			}
			issue := is.Issue
			all = append(all, &issue)
			if is.DependenciesSummary != nil && is.DependenciesSummary.BlockedBy > 0 {
				blocked[issue.GetNumber()] = is.DependenciesSummary.BlockedBy
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	return all, blocked, nil
}

// blockerRef is one entry of GET .../issues/{n}/dependencies/blocked_by.
// Blockers may live in another repository, so the repo is taken from the
// payload rather than assumed.
type blockerRef struct {
	Owner  string
	Repo   string
	Number int
	State  string
}

// blockedByPayloadIssue is the subset of the blocker payload that is read.
type blockedByPayloadIssue struct {
	Number        int    `json:"number"`
	State         string `json:"state"`
	HTMLURL       string `json:"html_url"`
	RepositoryURL string `json:"repository_url"`
}

// fetchBlockedBy lists the issues that block owner/repoName#number.
func (c *Client) fetchBlockedBy(ctx context.Context, owner, repoName string, number int) ([]blockerRef, error) {
	path := fmt.Sprintf("repos/%s/%s/issues/%d/dependencies/blocked_by?per_page=100", owner, repoName, number)
	req, err := c.client.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var payload []blockedByPayloadIssue
	if _, err := c.client.Do(ctx, req, &payload); err != nil {
		return nil, err
	}
	out := make([]blockerRef, 0, len(payload))
	for _, p := range payload {
		if p.Number <= 0 {
			continue
		}
		bOwner, bRepo := blockerRepoFromURLs(p.RepositoryURL, p.HTMLURL)
		if bOwner == "" || bRepo == "" {
			// A blocker whose repository cannot be named cannot be keyed;
			// fall back to the same repo, which is the overwhelmingly common
			// case and the only one the relay can create.
			bOwner, bRepo = owner, repoName
		}
		out = append(out, blockerRef{Owner: bOwner, Repo: bRepo, Number: p.Number, State: strings.ToLower(p.State)})
	}
	return out, nil
}

// blockerRepoFromURLs derives owner/repo from a repository_url
// (".../repos/{owner}/{repo}") or, failing that, an html_url
// ("https://host/{owner}/{repo}/issues/{n}").
func blockerRepoFromURLs(repositoryURL, htmlURL string) (string, string) {
	if u, err := url.Parse(repositoryURL); err == nil && u.Path != "" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		// Either "repos/{owner}/{repo}" or "api/v3/repos/{owner}/{repo}".
		for i := 0; i+2 < len(parts); i++ {
			if parts[i] == "repos" {
				return parts[i+1], parts[i+2]
			}
		}
	}
	if u, err := url.Parse(htmlURL); err == nil && u.Path != "" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) >= 4 && parts[2] == "issues" {
			return parts[0], parts[1]
		}
	}
	return "", ""
}

// githubBlockedByDependencies turns the blockers of owner/repoName#number into
// Issue.DependsOn edges. repoLabel is the spelling Issue.Repo carries for this
// repository (the configured name, which may be the short form); a same-repo
// blocker is keyed with it so the key matches the blocker's own Issue.Repo#N
// identity everywhere the hive persists one. Cross-repo blockers are keyed by
// their full owner/repo. A blocker is Resolved when GitHub reports it closed.
func (c *Client) githubBlockedByDependencies(ctx context.Context, owner, repoName, repoLabel string, number int) ([]IssueDependency, error) {
	blockers, err := c.fetchBlockedBy(ctx, owner, repoName, number)
	if err != nil {
		return nil, err
	}
	deps := make([]IssueDependency, 0, len(blockers))
	for _, b := range blockers {
		repo := b.Owner + "/" + b.Repo
		if strings.EqualFold(b.Owner, owner) && strings.EqualFold(b.Repo, repoName) {
			repo = repoLabel
		}
		deps = append(deps, IssueDependency{
			Key:      repo + "#" + strconv.Itoa(b.Number),
			Resolved: b.State == "closed",
		})
	}
	return deps, nil
}

// OpenBlockers returns the keys ("repo#N") of this issue's unresolved
// dependencies, sorted, or nil when the issue is ready.
func (i Issue) OpenBlockers() []string {
	var out []string
	for _, d := range i.DependsOn {
		if !d.Resolved && d.Key != "" {
			out = append(out, d.Key)
		}
	}
	sort.Strings(out)
	return out
}

// IsBlocked reports whether the issue has an unresolved dependency and so must
// not be offered as ready work.
func (i Issue) IsBlocked() bool {
	for _, d := range i.DependsOn {
		if !d.Resolved && d.Key != "" {
			return true
		}
	}
	return false
}

// dropMutualBlocks removes dependency edges that form a two-issue cycle within
// the snapshot (A blocked by B, B blocked by A, both open). Left alone, such a
// pair would never be ready and the work would vanish from every kick list
// forever. The edges are dropped — fail visible, not silent — with one warning
// naming both issues so a person can break the cycle on GitHub.
func dropMutualBlocks(issues []Issue, logger *slog.Logger) {
	index := make(map[string]int, len(issues))
	for i, is := range issues {
		index[is.Repo+"#"+strconv.Itoa(is.Number)] = i
	}
	blockedBy := func(i int, key string) bool {
		for _, d := range issues[i].DependsOn {
			if !d.Resolved && d.Key == key {
				return true
			}
		}
		return false
	}
	for i := range issues {
		self := issues[i].Repo + "#" + strconv.Itoa(issues[i].Number)
		for _, d := range issues[i].DependsOn {
			if d.Resolved {
				continue
			}
			j, ok := index[d.Key]
			if !ok || j == i || !blockedBy(j, self) {
				continue
			}
			if logger != nil {
				logger.Warn("issue dependencies: mutual \"blocked by\" cycle; ignoring both edges so neither issue is hidden forever — break the cycle on GitHub",
					slog.String("issue", self), slog.String("blocker", d.Key))
			}
			issues[i].DependsOn = removeDependency(issues[i].DependsOn, d.Key)
			issues[j].DependsOn = removeDependency(issues[j].DependsOn, self)
		}
	}
}

func removeDependency(deps []IssueDependency, key string) []IssueDependency {
	out := deps[:0]
	for _, d := range deps {
		if d.Key != key {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AddBlockedBy records on GitHub that repo#issueNumber is blocked by the issue
// whose database id is blockerID (POST .../issues/{n}/dependencies/blocked_by).
// Like AddSubIssue it takes the blocker's numeric `id`, not its number.
func (c *Client) AddBlockedBy(ctx context.Context, repo string, issueNumber int, blockerID int64) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	if issueNumber <= 0 || blockerID <= 0 {
		return fmt.Errorf("AddBlockedBy: issue number and blocker id are required")
	}
	owner, repoName := c.splitRepo(repo)
	if err := validateRepoRef(owner, repoName); err != nil {
		return fmt.Errorf("AddBlockedBy: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s/issues/%d/dependencies/blocked_by", owner, repoName, issueNumber)
	payload := struct {
		IssueID int64 `json:"issue_id"`
	}{IssueID: blockerID}

	_, err := effects.Execute(ctx, c.mutationBoundary(), effects.Claim{
		Repo:   owner + "/" + repoName,
		Kind:   effects.KindDependencyLink,
		Target: strconv.Itoa(issueNumber),
		Inputs: map[string]string{"blocker_issue_id": strconv.FormatInt(blockerID, 10)},
	}, func(ctx context.Context) (effects.Result, error) {
		req, reqErr := c.client.NewRequest(http.MethodPost, path, payload)
		if reqErr != nil {
			return effects.Result{}, reqErr
		}
		if _, doErr := c.client.Do(ctx, req, nil); doErr != nil {
			return effects.Result{}, doErr
		}
		return effects.Result{Provenance: fmt.Sprintf("%s/%s#%d blocked_by_id=%d", owner, repoName, issueNumber, blockerID)}, nil
	})
	if err != nil {
		return fmt.Errorf("linking %s/%s#%d as blocked by issue id %d: %w", owner, repoName, issueNumber, blockerID, err)
	}
	return nil
}

// linkBlockedBy resolves each blocker number to its database id and links it
// as a blocker of child. Best-effort per blocker: one failure (blocker
// missing, closed, cross-repo, API error) is reported and the rest proceed,
// and the caller never turns a successful create into a failure over it.
func (c *Client) linkBlockedBy(ctx context.Context, repo string, child int, blockers []int) (linked []int, errs []string) {
	owner, repoName := c.splitRepo(repo)
	seen := map[int]bool{}
	for _, n := range blockers {
		if n <= 0 || n == child || seen[n] {
			continue
		}
		seen[n] = true
		blocker, _, err := c.client.Issues.Get(ctx, owner, repoName, n)
		if err != nil {
			errs = append(errs, fmt.Sprintf("#%d: %v", n, err))
			continue
		}
		if err := c.AddBlockedBy(ctx, repo, child, blocker.GetID()); err != nil {
			errs = append(errs, fmt.Sprintf("#%d: %v", n, err))
			continue
		}
		linked = append(linked, n)
	}
	return linked, errs
}

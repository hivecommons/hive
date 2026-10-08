package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// PostureMergedPR is one merged PR as the compliance posture checks see it
// (hivecommons/hive#11079): who authored it, who merged it, and who reviewed
// it.
type PostureMergedPR struct {
	Repo        string
	Number      int
	URL         string
	Author      string
	MergedBy    string
	MergedByBot bool
	MergedAt    time.Time
	// Reviewers are the logins with a submitted review (APPROVED,
	// CHANGES_REQUESTED or COMMENTED); pending and dismissed reviews do not
	// count as review evidence.
	Reviewers []string
}

// postureSearchPageSize and postureSearchMaxPages bound one repo's search:
// GitHub search returns at most 1000 results, so 20 pages of 50 is the whole
// reachable set.
const (
	postureSearchPageSize = 50
	postureSearchMaxPages = 20
	postureLabelMaxPages  = 10
)

const postureMergedPRsQuery = `query($q: String!, $first: Int!, $after: String) {
  search(query: $q, type: ISSUE, first: $first, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes {
      ... on PullRequest {
        number
        url
        mergedAt
        author { login }
        mergedBy { __typename login }
        reviews(first: 100) { nodes { state author { login } } }
      }
    }
  }
}`

type postureSearchResponse struct {
	Search struct {
		PageInfo struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
		Nodes []struct {
			Number   int        `json:"number"`
			URL      string     `json:"url"`
			MergedAt *time.Time `json:"mergedAt"`
			Author   *struct {
				Login string `json:"login"`
			} `json:"author"`
			MergedBy *struct {
				Typename string `json:"__typename"`
				Login    string `json:"login"`
			} `json:"mergedBy"`
			Reviews struct {
				Nodes []struct {
					State  string `json:"state"`
					Author *struct {
						Login string `json:"login"`
					} `json:"author"`
				} `json:"nodes"`
			} `json:"reviews"`
		} `json:"nodes"`
	} `json:"search"`
}

// PostureMergedPRsSince lists the PRs merged into repo ("owner/name" or a
// bare name in the client's org) since the given time, with their authors,
// mergers and reviewers, in one GraphQL search per 50 PRs.
func (c *Client) PostureMergedPRsSince(ctx context.Context, repo string, since time.Time) ([]PostureMergedPR, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	full := owner + "/" + name
	q := fmt.Sprintf("repo:%s is:pr is:merged merged:>=%s", full, since.UTC().Format("2006-01-02"))
	var out []PostureMergedPR
	var after any
	for page := 0; page < postureSearchMaxPages; page++ {
		var resp postureSearchResponse
		vars := map[string]any{"q": q, "first": postureSearchPageSize, "after": after}
		if err := c.graphQL(ctx, postureMergedPRsQuery, vars, &resp); err != nil {
			return nil, fmt.Errorf("searching merged PRs for %s: %w", full, err)
		}
		for _, n := range resp.Search.Nodes {
			if n.Number == 0 || n.MergedAt == nil {
				continue
			}
			pr := PostureMergedPR{Repo: full, Number: n.Number, URL: n.URL, MergedAt: n.MergedAt.UTC()}
			if n.Author != nil {
				pr.Author = n.Author.Login
			}
			if n.MergedBy != nil {
				pr.MergedBy = n.MergedBy.Login
				pr.MergedByBot = n.MergedBy.Typename == "Bot"
			}
			seen := map[string]bool{}
			for _, r := range n.Reviews.Nodes {
				if r.Author == nil || r.Author.Login == "" {
					continue
				}
				switch strings.ToUpper(r.State) {
				case "APPROVED", "CHANGES_REQUESTED", "COMMENTED":
				default:
					continue
				}
				if key := strings.ToLower(r.Author.Login); !seen[key] {
					seen[key] = true
					pr.Reviewers = append(pr.Reviewers, r.Author.Login)
				}
			}
			out = append(out, pr)
		}
		if !resp.Search.PageInfo.HasNextPage || resp.Search.PageInfo.EndCursor == "" {
			break
		}
		after = resp.Search.PageInfo.EndCursor
	}
	return out, nil
}

// RepoLabelNames lists the names of every label defined on repo.
func (c *Client) RepoLabelNames(ctx context.Context, repo string) ([]string, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	opts := &gh.ListOptions{PerPage: listPerPage}
	var out []string
	for page := 0; page < postureLabelMaxPages; page++ {
		labels, resp, err := c.client.Issues.ListLabels(WithRESTCaller(ctx, "hive:compliance_posture"), owner, name, opts)
		if err != nil {
			return nil, fmt.Errorf("listing labels for %s/%s: %w", owner, name, err)
		}
		for _, l := range labels {
			out = append(out, l.GetName())
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

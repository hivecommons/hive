package github

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Reading a PR for the analysis fact-check sweep (hivecommons/hive#19074).
//
// The sweep needs, in one call per PR: whether it is open and a draft, its
// head commit and labels, the hive's own reviews (to read the fact-check
// markers it left), and whether a person has reviewed. Nothing here
// interprets comment text; the marker parser and the decisions live in
// pkg/factcheck.

const (
	factCheckStateOpen = "open"
	factCheckLabels    = 50
	factCheckReviews   = 50
	factCheckInline    = 50
	factCheckComments  = 50
	graphQLUserType    = "User"
)

// FactCheckInline is one line-anchored comment of a review.
type FactCheckInline struct {
	Path string
	Line int
	Body string
	URL  string
}

// FactCheckReview is one review submission.
type FactCheckReview struct {
	URL    string
	Body   string
	Author string
	// Hive is true when the review is the hive's own posting identity.
	Hive bool
	// Human is true for a person: a User account that is not the hive.
	Human  bool
	At     time.Time
	Commit string
	Inline []FactCheckInline
}

// FactCheckComment is one conversation comment.
type FactCheckComment struct {
	URL    string
	Body   string
	Author string
	Hive   bool
	At     time.Time
}

// FactCheckPR is what the sweep reads of one PR.
type FactCheckPR struct {
	// NodeID is the GraphQL id the ready-for-review mutation needs.
	NodeID string
	// State is "open", "closed" or "merged".
	State    string
	Draft    bool
	HeadSHA  string
	HeadRef  string
	Labels   []string
	Reviews  []FactCheckReview
	Comments []FactCheckComment
}

// Open reports whether the PR can still take a revision.
func (p FactCheckPR) Open() bool { return p.State == factCheckStateOpen }

type rawFactCheckAuthor struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
}

type rawFactCheckComment struct {
	URL       string              `json:"url"`
	Body      string              `json:"body"`
	CreatedAt time.Time           `json:"createdAt"`
	Author    *rawFactCheckAuthor `json:"author"`
}

type rawFactCheckInline struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
	URL  string `json:"url"`
}

type rawFactCheckReview struct {
	URL         string              `json:"url"`
	Body        string              `json:"body"`
	State       string              `json:"state"`
	SubmittedAt time.Time           `json:"submittedAt"`
	Author      *rawFactCheckAuthor `json:"author"`
	Commit      *struct {
		Oid string `json:"oid"`
	} `json:"commit"`
	Comments struct {
		Nodes []rawFactCheckInline `json:"nodes"`
	} `json:"comments"`
}

type rawFactCheckPR struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	IsDraft     bool   `json:"isDraft"`
	Merged      bool   `json:"merged"`
	HeadRefName string `json:"headRefName"`
	HeadRefOid  string `json:"headRefOid"`
	Labels      struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Reviews struct {
		Nodes []rawFactCheckReview `json:"nodes"`
	} `json:"reviews"`
	Comments struct {
		Nodes []rawFactCheckComment `json:"nodes"`
	} `json:"comments"`
}

var factCheckQuery = fmt.Sprintf(`query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      id state isDraft merged headRefName headRefOid
      labels(first: %d) { nodes { name } }
      reviews(last: %d) {
        nodes {
          url body state submittedAt author { __typename login } commit { oid }
          comments(first: %d) { nodes { path line body url } }
        }
      }
      comments(last: %d) { nodes { url body createdAt author { __typename login } } }
    }
  }
}`, factCheckLabels, factCheckReviews, factCheckInline, factCheckComments)

const markPRReadyMutation = `mutation($id: ID!) {
  markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { isDraft } }
}`

// FetchPRFactCheck reads one PR for the fact-check sweep.
func (c *Client) FetchPRFactCheck(ctx context.Context, repo string, number int) (FactCheckPR, error) {
	if c == nil {
		return FactCheckPR{}, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	var data struct {
		Repository struct {
			PullRequest *rawFactCheckPR `json:"pullRequest"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": owner, "repo": name, "number": number}
	if err := c.graphQL(ctx, factCheckQuery, vars, &data); err != nil {
		return FactCheckPR{}, err
	}
	if data.Repository.PullRequest == nil {
		return FactCheckPR{}, fmt.Errorf("%s/%s#%d: pull request not found", owner, name, number)
	}
	return buildFactCheckPR(*data.Repository.PullRequest, c.factCheckIsHive), nil
}

// MarkPRReadyForReview moves a draft PR to ready for review. nodeID is
// FactCheckPR.NodeID.
func (c *Client) MarkPRReadyForReview(ctx context.Context, nodeID string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	if strings.TrimSpace(nodeID) == "" {
		return fmt.Errorf("mark ready: empty pull request id")
	}
	return c.graphQL(ctx, markPRReadyMutation, map[string]any{"id": nodeID}, nil)
}

// factCheckIsHive reports whether a GraphQL author is this hive's own posting
// identity. GraphQL names an App bot without the "[bot]" suffix REST uses, so
// both spellings are tried.
func (c *Client) factCheckIsHive(login string) bool {
	login = strings.TrimSpace(login)
	if login == "" {
		return false
	}
	return c.isHiveLogin(login) || c.isHiveLogin(strings.TrimSuffix(login, "[bot]")+"[bot]")
}

func factCheckActor(a *rawFactCheckAuthor, isHive func(string) bool) (login string, hive, human bool) {
	if a == nil {
		return "", false, false
	}
	hive = isHive != nil && isHive(a.Login)
	human = !hive && a.Typename == graphQLUserType && !strings.HasSuffix(a.Login, "[bot]")
	return a.Login, hive, human
}

// buildFactCheckPR classifies a raw read. isHive says whether a login is the
// hive's own; everything else that is a User account counts as a person.
func buildFactCheckPR(raw rawFactCheckPR, isHive func(string) bool) FactCheckPR {
	state := factCheckStateOpen
	switch {
	case raw.Merged || strings.EqualFold(raw.State, "MERGED"):
		state = "merged"
	case strings.EqualFold(raw.State, "CLOSED"):
		state = "closed"
	}
	out := FactCheckPR{NodeID: raw.ID, State: state, Draft: raw.IsDraft, HeadSHA: raw.HeadRefOid, HeadRef: raw.HeadRefName}
	for _, l := range raw.Labels.Nodes {
		out.Labels = append(out.Labels, l.Name)
	}
	for _, r := range raw.Reviews.Nodes {
		if strings.EqualFold(r.State, "PENDING") {
			continue
		}
		login, hive, human := factCheckActor(r.Author, isHive)
		rv := FactCheckReview{URL: r.URL, Body: r.Body, Author: login, Hive: hive, Human: human, At: r.SubmittedAt}
		if r.Commit != nil {
			rv.Commit = r.Commit.Oid
		}
		for _, in := range r.Comments.Nodes {
			rv.Inline = append(rv.Inline, FactCheckInline{Path: in.Path, Line: in.Line, Body: in.Body, URL: in.URL})
		}
		out.Reviews = append(out.Reviews, rv)
	}
	for _, cm := range raw.Comments.Nodes {
		login, hive, _ := factCheckActor(cm.Author, isHive)
		out.Comments = append(out.Comments, FactCheckComment{URL: cm.URL, Body: cm.Body, Author: login, Hive: hive, At: cm.CreatedAt})
	}
	return out
}

package github

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Human feedback on a PR the hive opened (hivecommons/hive#9583, phase 2).
//
// The review-thread monitor (review_threads.go) only follows threads a
// configured review BOT opened. A person's feedback - a conversation comment,
// a review body, or an inline review comment - had no route back to the agent
// that authored the PR. FetchHumanPRComments returns exactly that feedback,
// with per-comment authorship so the hive's own replies and every bot are
// excluded: a follow-up must never be triggered by the agent answering
// itself, or by one bot answering another.

// PRCommentKind says where on the PR a human comment was left.
type PRCommentKind string

const (
	// PRCommentConversation is a comment on the PR's conversation tab.
	PRCommentConversation PRCommentKind = "conversation"
	// PRCommentReview is the body of a submitted review.
	PRCommentReview PRCommentKind = "review"
	// PRCommentInline is a comment in an inline review thread.
	PRCommentInline PRCommentKind = "inline"
)

// PRComment is one human comment on a PR. ID is stable per comment and unique
// across kinds, so a consumer can dedupe on it.
type PRComment struct {
	ID        string        `json:"id"`
	Kind      PRCommentKind `json:"kind"`
	Author    string        `json:"author"`
	Body      string        `json:"body"`
	Path      string        `json:"path,omitempty"`
	Line      int           `json:"line,omitempty"`
	URL       string        `json:"url,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
}

const (
	// prCommentBodyRunes bounds each comment body carried out of GitHub.
	prCommentBodyRunes = 1000
	// prCommentsConversationWindow, prCommentsReviewWindow,
	// prCommentsThreadWindow and prCommentsPerThreadWindow bound how much of
	// a PR's recent history one fetch reads. Follow-ups care about what is
	// NEW; anything older than these windows was seen on an earlier pass.
	prCommentsConversationWindow = 50
	prCommentsReviewWindow       = 30
	prCommentsThreadWindow       = 50
	prCommentsPerThreadWindow    = 20
)

// trustedCommentAssociations are the GitHub author associations whose
// comments may be fed to an agent as review feedback. A public repository
// accepts comments from anyone; typing an arbitrary stranger's text into an
// agent session would be a prompt-injection channel, so only people with a
// standing relationship to the repository are routed. Everyone else still
// reaches the agent the way they did before (generic review cycles).
var trustedCommentAssociations = map[string]bool{
	"OWNER":        true,
	"MEMBER":       true,
	"COLLABORATOR": true,
}

// Review states whose body is feedback worth routing. APPROVED bodies are
// usually a sign-off, PENDING reviews are not visible yet, DISMISSED ones
// were withdrawn.
var routedReviewStates = map[string]bool{
	"COMMENTED":         true,
	"CHANGES_REQUESTED": true,
}

type rawPRCommentAuthor struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
}

type rawPRComment struct {
	DatabaseID        int64               `json:"databaseId"`
	URL               string              `json:"url"`
	Body              string              `json:"body"`
	CreatedAt         time.Time           `json:"createdAt"`
	AuthorAssociation string              `json:"authorAssociation"`
	State             string              `json:"state,omitempty"`
	Author            *rawPRCommentAuthor `json:"author"`
}

type rawPRCommentThread struct {
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Path       string `json:"path"`
	Line       *int   `json:"line"`
	Comments   struct {
		Nodes []rawPRComment `json:"nodes"`
	} `json:"comments"`
}

type rawPRConversation struct {
	Comments struct {
		Nodes []rawPRComment `json:"nodes"`
	} `json:"comments"`
	Reviews struct {
		Nodes []rawPRComment `json:"nodes"`
	} `json:"reviews"`
	ReviewThreads struct {
		Nodes []rawPRCommentThread `json:"nodes"`
	} `json:"reviewThreads"`
}

var prConversationQuery = fmt.Sprintf(`query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      comments(last: %d) { nodes { databaseId url body createdAt authorAssociation author { __typename login } } }
      reviews(last: %d) { nodes { databaseId url body createdAt authorAssociation state author { __typename login } } }
      reviewThreads(last: %d) {
        nodes {
          isResolved isOutdated path line
          comments(last: %d) { nodes { databaseId url body createdAt authorAssociation author { __typename login } } }
        }
      }
    }
  }
}`, prCommentsConversationWindow, prCommentsReviewWindow, prCommentsThreadWindow, prCommentsPerThreadWindow)

// FetchHumanPRComments returns the human feedback left on repo#number at or
// after since, oldest first. See filterHumanPRComments for what counts as
// human.
func (c *Client) FetchHumanPRComments(ctx context.Context, repo string, number int, since time.Time) ([]PRComment, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	var data struct {
		Repository struct {
			PullRequest *rawPRConversation `json:"pullRequest"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": owner, "repo": name, "number": number}
	if err := c.graphQL(ctx, prConversationQuery, vars, &data); err != nil {
		return nil, err
	}
	if data.Repository.PullRequest == nil {
		return nil, fmt.Errorf("%s/%s#%d: pull request not found", owner, name, number)
	}
	return filterHumanPRComments(*data.Repository.PullRequest, since, c.isHumanFeedbackAuthor), nil
}

// isHumanFeedbackAuthor is the per-comment authorship test. It reuses the
// hive's own identity (the App bot login and project.ai_author, via
// isHiveLogin) rather than a separate list, and treats every bot account -
// GitHub's Bot type, a "[bot]" login, or a configured review bot - as not
// human.
func (c *Client) isHumanFeedbackAuthor(author *rawPRCommentAuthor) bool {
	if author == nil {
		return false // deleted account ("ghost"): nobody to answer
	}
	login := strings.TrimSpace(author.Login)
	if login == "" || strings.EqualFold(author.Typename, "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return false
	}
	if c.isHiveLogin(login) {
		return false
	}
	return !c.getReviewBots().IsBot(login)
}

// filterHumanPRComments keeps a comment only when it is non-empty, created at
// or after since, written by a human (isHuman) whose association with the
// repository is trusted, and does not carry the hive attribution trailer (an
// agent running on a person's credentials signs what it posts). Inline
// comments in resolved or outdated threads are dropped: the conversation
// there has already moved on.
func filterHumanPRComments(raw rawPRConversation, since time.Time, isHuman func(*rawPRCommentAuthor) bool) []PRComment {
	var out []PRComment
	keep := func(rc rawPRComment) bool {
		if rc.Author == nil || strings.TrimSpace(rc.Body) == "" || rc.CreatedAt.Before(since) {
			return false
		}
		if !trustedCommentAssociations[strings.ToUpper(rc.AuthorAssociation)] {
			return false
		}
		if HasAttributionTrailer(rc.Body) {
			return false
		}
		return isHuman != nil && isHuman(rc.Author)
	}
	add := func(kind PRCommentKind, rc rawPRComment, path string, line int) {
		out = append(out, PRComment{
			ID:        string(kind) + ":" + strconv.FormatInt(rc.DatabaseID, 10),
			Kind:      kind,
			Author:    rc.Author.Login,
			Body:      truncateCommentBody(rc.Body),
			Path:      path,
			Line:      line,
			URL:       rc.URL,
			CreatedAt: rc.CreatedAt,
		})
	}
	for _, rc := range raw.Comments.Nodes {
		if keep(rc) {
			add(PRCommentConversation, rc, "", 0)
		}
	}
	for _, rc := range raw.Reviews.Nodes {
		if routedReviewStates[strings.ToUpper(rc.State)] && keep(rc) {
			add(PRCommentReview, rc, "", 0)
		}
	}
	for _, th := range raw.ReviewThreads.Nodes {
		if th.IsResolved || th.IsOutdated {
			continue
		}
		line := 0
		if th.Line != nil {
			line = *th.Line
		}
		for _, rc := range th.Comments.Nodes {
			if keep(rc) {
				add(PRCommentInline, rc, th.Path, line)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func truncateCommentBody(body string) string {
	body = strings.TrimSpace(body)
	if runes := []rune(body); len(runes) > prCommentBodyRunes {
		return string(runes[:prCommentBodyRunes]) + "..."
	}
	return body
}

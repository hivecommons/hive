package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// ReviewThreadsPath is the pre-kick artifact listing every unresolved
// external-review-bot thread on an open hive-mediated PR (isHiveMediatedPR;
// hivecommons/hive#7360, #7638). The scheduler reads it to prepend a
// fix-before-new block to the kick of the agent that opened each PR, the
// same way ci-failing.json drives the red-CI block. It is a var only so tests
// can redirect it.
var ReviewThreadsPath = "/var/run/hive-metrics/review-threads.json"

// ReviewThreadsReport is the JSON shape of ReviewThreadsPath.
type ReviewThreadsReport struct {
	GeneratedAt string `json:"generated_at"`
	// Enabled is false when classification.review_bots names no login — the
	// file is still written (empty) so a consumer can tell "off" from "stale".
	Enabled      bool             `json:"enabled"`
	TotalThreads int              `json:"total_threads"`
	PRs          []ReviewThreadPR `json:"prs"`
}

// ReviewThreadPR is one open hive-mediated PR that has (or had) review-bot
// threads. Threads holds only the ones still needing a hive pass; a PR whose
// bot threads are all resolved or already at the attempt cap is listed with
// zero threads so the file shows the reconciler has nothing left to do there.
type ReviewThreadPR struct {
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
	Title   string `json:"title,omitempty"`
	HeadRef string `json:"head_ref"`
	// Agent is the hive agent whose relay request opened this PR (from the
	// audit trail, resolved by the caller). Empty means unattributed; the kick
	// builder defaults it to scanner, exactly as it does for ci-failing.json.
	// A PR opened on a person's credentials has no App-bot audit entry, so it
	// lands here empty and reaches the scanner that way (#7638).
	Agent string `json:"agent,omitempty"`
	// Escalated marks a PR handed to a human (needs-human). The kick builder
	// never lists escalated PRs, same as the red-CI block.
	Escalated bool           `json:"escalated,omitempty"`
	Threads   []ReviewThread `json:"threads"`
}

// ReviewThread is one unresolved, non-outdated inline thread whose first
// comment is from a configured review bot and that has fewer than
// max_attempts_per_thread replies from the hive.
type ReviewThread struct {
	ThreadID string `json:"thread_id"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Author   string `json:"author"`
	Body     string `json:"body"`
	// CommentID is the first comment's REST id, for callers that prefer the
	// REST reply endpoint over the GraphQL thread mutation.
	CommentID int64 `json:"comment_id,omitempty"`
	// HiveReplies is how many replies in the thread are already from the
	// hive — the attempt counter. Always < max_attempts_per_thread here.
	HiveReplies int `json:"hive_replies"`
}

// reviewThreadBodyRunes bounds the body excerpt stored per thread. The kick
// builder truncates again for the prompt; this bound keeps the artifact
// itself from ballooning on a chatty bot.
const reviewThreadBodyRunes = 1000

// rawReviewThread is the GraphQL projection of one pullRequest.reviewThreads
// node. It is the input to filterReviewThreads, which is what the unit tests
// exercise; the network fetch just fills it in.
type rawReviewThread struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Path       string `json:"path"`
	Line       *int   `json:"line"`
	Comments   struct {
		Nodes []rawReviewComment `json:"nodes"`
	} `json:"comments"`
}

type rawReviewComment struct {
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	Body       string `json:"body"`
	DatabaseID int64  `json:"databaseId"`
}

// SetReviewBots installs classification.review_bots. Nil-receiver safe and
// guarded like the other config-reload setters: the monitor and the
// review-request watcher read it from their own goroutines.
func (c *Client) SetReviewBots(rb config.ReviewBotsConfig) {
	if c == nil {
		return
	}
	c.reviewBotsMu.Lock()
	defer c.reviewBotsMu.Unlock()
	c.reviewBots = rb
}

func (c *Client) getReviewBots() config.ReviewBotsConfig {
	c.reviewBotsMu.RLock()
	defer c.reviewBotsMu.RUnlock()
	return c.reviewBots
}

// isHiveLogin reports whether login is one of this hive's own accounts: the
// App bot, or project.ai_author when configured. It is the "hive-authored"
// test for PRs and the "our reply" test for the per-thread attempt counter.
func (c *Client) isHiveLogin(login string) bool {
	login = strings.TrimSpace(login)
	if login == "" {
		return false
	}
	if c.appBotLogin != "" && strings.EqualFold(login, c.appBotLogin) {
		return true
	}
	return c.getHiveIdentity().Matches(login)
}

// filterReviewThreads applies the #7360 thread filter to one PR's threads:
// keep a thread only when it is unresolved, not outdated, its FIRST comment
// is from a configured review bot, and it has fewer than maxAttempts replies
// from the hive (isHive). Human-opened threads never pass — that is the
// property the watcher's guard re-checks server-side, so an agent cannot be
// prompted into a human's conversation even by a wrong kick.
func filterReviewThreads(threads []rawReviewThread, bots config.ReviewBotsConfig, isHive func(string) bool) []ReviewThread {
	if !bots.Enabled() {
		return nil
	}
	maxAttempts := bots.MaxAttempts()
	var out []ReviewThread
	for _, t := range threads {
		if t.IsResolved || t.IsOutdated || len(t.Comments.Nodes) == 0 {
			continue
		}
		first := t.Comments.Nodes[0]
		if !bots.IsBot(first.Author.Login) {
			continue
		}
		replies := 0
		for _, cmt := range t.Comments.Nodes[1:] {
			if isHive != nil && isHive(cmt.Author.Login) {
				replies++
			}
		}
		if replies >= maxAttempts {
			continue
		}
		body := strings.TrimSpace(first.Body)
		if runes := []rune(body); len(runes) > reviewThreadBodyRunes {
			body = string(runes[:reviewThreadBodyRunes]) + "…"
		}
		line := 0
		if t.Line != nil {
			line = *t.Line
		}
		out = append(out, ReviewThread{
			ThreadID:    t.ID,
			Path:        t.Path,
			Line:        line,
			Author:      first.Author.Login,
			Body:        body,
			CommentID:   first.DatabaseID,
			HiveReplies: replies,
		})
	}
	return out
}

// hasBlockedLabel reports whether the PR carries the "blocked" label
// (case-insensitive), the exclusion bin/enumerate-actionable.sh applies to
// PRs in addition to hold and do-not-merge.
func hasBlockedLabel(labels []string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), "blocked") {
			return true
		}
	}
	return false
}

// hasBotThread reports whether any thread (resolved or not) was opened by a
// configured bot — the "this PR is one the reconciler cares about" test that
// decides whether a PR is listed at all.
func hasBotThread(threads []rawReviewThread, bots config.ReviewBotsConfig) bool {
	for _, t := range threads {
		if len(t.Comments.Nodes) > 0 && bots.IsBot(t.Comments.Nodes[0].Author.Login) {
			return true
		}
	}
	return false
}

// reviewThreadsQuery is the per-PR GraphQL query. comments(first: 50) — not
// the sketch's 10 — so a thread the hive has already replied in a few times
// still shows every hive reply to the attempt counter.
const reviewThreadsQuery = `query($owner: String!, $repo: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      headRefName
      reviewThreads(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id isResolved isOutdated path line
          comments(first: 50) { nodes { author { login } body databaseId } }
        }
      }
    }
  }
}`

// fetchReviewThreads returns every review thread on owner/repo#number plus
// the PR's head ref, following reviewThreads pagination.
func (c *Client) fetchReviewThreads(ctx context.Context, owner, repo string, number int) (headRef string, threads []rawReviewThread, err error) {
	var after *string
	for {
		vars := map[string]any{"owner": owner, "repo": repo, "number": number}
		if after != nil {
			vars["after"] = *after
		}
		var data struct {
			Repository struct {
				PullRequest *struct {
					HeadRefName   string `json:"headRefName"`
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []rawReviewThread `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.graphQL(ctx, reviewThreadsQuery, vars, &data); err != nil {
			return "", nil, err
		}
		pr := data.Repository.PullRequest
		if pr == nil {
			return "", nil, fmt.Errorf("%s/%s#%d: pull request not found", owner, repo, number)
		}
		headRef = pr.HeadRefName
		threads = append(threads, pr.ReviewThreads.Nodes...)
		if !pr.ReviewThreads.PageInfo.HasNextPage || pr.ReviewThreads.PageInfo.EndCursor == "" {
			return headRef, threads, nil
		}
		cursor := pr.ReviewThreads.PageInfo.EndCursor
		after = &cursor
	}
}

// isHiveMediatedPR is the "this PR is the hive's to follow up on" test for the
// review-thread reconciler. Either signal suffices:
//
//  1. the author is a hive login (App bot or project.ai_author) — the PRs the
//     PR-request watcher opens; or
//  2. the body carries the `— hive:` attribution trailer — the PRs a hive
//     agent opened on a PERSON's credentials (a contributor relay, or an
//     operator running agents under their own GitHub auth). GitHub shows the
//     person as author, so rule 1 alone dropped every one of them before the
//     thread query ran (hivecommons/hive#7638) — and those are precisely the
//     PRs external review bots review, since Codex skips bot authors.
//
// The trailer is the same rule the task-list sweep (isHiveFiledIssue) uses to
// decide an issue is the hive's to close. It is spoofable — a person can paste
// a `— hive:` line into a hand-written PR — and that is accepted here as it was
// there: the consequence is bounded to the hive replying in and resolving
// threads a CONFIGURED REVIEW BOT opened on that PR (the watcher-side guard
// still refuses a human's thread, and still keys on the thread's first
// author, not the PR's), which the PR's own author could do by hand anyway.
// Setting project.ai_author to the person's login is NOT the answer: that
// would make every PR they write by hand hive-authored too.
func (c *Client) isHiveMediatedPR(pr PullRequest) bool {
	return c.isHiveLogin(pr.Author) || pr.HiveAttributed
}

// CollectReviewThreads builds the review-threads report for the given open
// PRs. Callers pass the governor's already-enumerated actionable PR list, so
// the hold / do-not-merge / exempt-label and draft exclusions are exactly the
// ones every other kick input already has (fetchPRs applies them). This
// function additionally keeps only HIVE-MEDIATED PRs (isHiveMediatedPR: a
// hive login as author, or the attribution trailer in the body) and skips the
// GraphQL round trip entirely when review_bots is off. A PR whose thread
// fetch fails is logged and omitted (omission means "no attempt this pass",
// the safe direction); it is not an error for the report as a whole.
func (c *Client) CollectReviewThreads(ctx context.Context, prs []PullRequest, now time.Time) ReviewThreadsReport {
	report := ReviewThreadsReport{GeneratedAt: now.UTC().Format(time.RFC3339), PRs: []ReviewThreadPR{}}
	if c == nil {
		return report
	}
	bots := c.getReviewBots()
	report.Enabled = bots.Enabled()
	if !report.Enabled {
		return report
	}
	for _, pr := range prs {
		if pr.Draft || !c.isHiveMediatedPR(pr) {
			continue
		}
		if isHeld(pr.Labels) || c.isExempt(pr.Labels) || hasBlockedLabel(pr.Labels) {
			// hold / do-not-merge / exempt: defense in depth, fetchPRs already
			// dropped these. "blocked" is enumerate-actionable.sh's extra PR
			// exclusion (#7360 asks for the same set), which the Go
			// enumerator does not apply, so it is checked here.
			continue
		}
		owner, repoName := c.splitRepo(pr.Repo)
		headRef, raw, err := c.fetchReviewThreads(ctx, owner, repoName, pr.Number)
		if err != nil {
			c.warn("review-thread monitor: thread fetch failed, PR skipped this pass",
				"repo", pr.Repo, "number", pr.Number, "error", err)
			if isRateLimited(err) {
				c.refreshRateLimitCache(ctx)
				break // the rest of the pass would only burn budget
			}
			continue
		}
		if !hasBotThread(raw, bots) {
			continue
		}
		threads := filterReviewThreads(raw, bots, c.isHiveLogin)
		if threads == nil {
			threads = []ReviewThread{}
		}
		report.PRs = append(report.PRs, ReviewThreadPR{
			Repo:    owner + "/" + repoName,
			Number:  pr.Number,
			Title:   pr.Title,
			HeadRef: headRef,
			Threads: threads,
		})
		report.TotalThreads += len(threads)
	}
	sort.SliceStable(report.PRs, func(i, j int) bool {
		if report.PRs[i].Repo != report.PRs[j].Repo {
			return report.PRs[i].Repo < report.PRs[j].Repo
		}
		return report.PRs[i].Number < report.PRs[j].Number
	})
	return report
}

// WriteReviewThreadsReport writes the report atomically to path ("" =
// ReviewThreadsPath). Written even when empty: the scheduler treats a
// missing file as "nothing to do", but an operator reading the directory
// should be able to see the reconciler ran.
func WriteReviewThreadsReport(path string, report ReviewThreadsReport) error {
	if path == "" {
		path = ReviewThreadsPath
	}
	if report.PRs == nil {
		report.PRs = []ReviewThreadPR{}
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeRequestFile(path, data)
}

// ReadReviewThreadsReport reads a report written by WriteReviewThreadsReport.
func ReadReviewThreadsReport(path string) (ReviewThreadsReport, error) {
	if path == "" {
		path = ReviewThreadsPath
	}
	var report ReviewThreadsReport
	data, err := os.ReadFile(path)
	if err != nil {
		return report, err
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return report, err
	}
	return report, nil
}

// reviewThreadNode is the guard's re-fetch of one thread by node id: the
// facts the review-request watcher must re-verify server-side before it
// replies in or resolves a thread on an agent's behalf.
type reviewThreadNode struct {
	ID          string
	IsResolved  bool
	FirstAuthor string
	HiveReplies int
	Repo        string // "owner/name"
	Number      int
}

const reviewThreadNodeQuery = `query($id: ID!) {
  node(id: $id) {
    ... on PullRequestReviewThread {
      id isResolved
      comments(first: 50) { nodes { author { login } } }
      pullRequest { number repository { nameWithOwner } }
    }
  }
}`

// fetchReviewThreadNode re-reads a thread by id. A nil node with a nil error
// means the id resolved to nothing this token can see (or to a node of
// another type).
func (c *Client) fetchReviewThreadNode(ctx context.Context, threadID string) (*reviewThreadNode, error) {
	var data struct {
		Node *struct {
			ID         string `json:"id"`
			IsResolved bool   `json:"isResolved"`
			Comments   struct {
				Nodes []rawReviewComment `json:"nodes"`
			} `json:"comments"`
			PullRequest *struct {
				Number     int `json:"number"`
				Repository struct {
					NameWithOwner string `json:"nameWithOwner"`
				} `json:"repository"`
			} `json:"pullRequest"`
		} `json:"node"`
	}
	if err := c.graphQL(ctx, reviewThreadNodeQuery, map[string]any{"id": threadID}, &data); err != nil {
		if isGraphQLNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	n := data.Node
	if n == nil || n.ID == "" {
		return nil, nil
	}
	out := &reviewThreadNode{ID: n.ID, IsResolved: n.IsResolved}
	if len(n.Comments.Nodes) > 0 {
		out.FirstAuthor = n.Comments.Nodes[0].Author.Login
		for _, cmt := range n.Comments.Nodes[1:] {
			if c.isHiveLogin(cmt.Author.Login) {
				out.HiveReplies++
			}
		}
	}
	if n.PullRequest != nil {
		out.Number = n.PullRequest.Number
		out.Repo = n.PullRequest.Repository.NameWithOwner
	}
	return out, nil
}

// resolveReviewThread runs the resolveReviewThread mutation.
func (c *Client) resolveReviewThread(ctx context.Context, threadID string) error {
	const q = `mutation($id: ID!) {
  resolveReviewThread(input: {threadId: $id}) { thread { id isResolved } }
}`
	var data struct {
		ResolveReviewThread struct {
			Thread struct {
				IsResolved bool `json:"isResolved"`
			} `json:"thread"`
		} `json:"resolveReviewThread"`
	}
	if err := c.graphQL(ctx, q, map[string]any{"id": threadID}, &data); err != nil {
		return err
	}
	if !data.ResolveReviewThread.Thread.IsResolved {
		return fmt.Errorf("resolveReviewThread: thread %s still unresolved after mutation", threadID)
	}
	return nil
}

// replyToReviewThread runs the addPullRequestReviewThreadReply mutation.
func (c *Client) replyToReviewThread(ctx context.Context, threadID, body string) error {
	const q = `mutation($id: ID!, $body: String!) {
  addPullRequestReviewThreadReply(input: {pullRequestReviewThreadId: $id, body: $body}) { comment { id } }
}`
	return c.graphQL(ctx, q, map[string]any{"id": threadID, "body": body}, nil)
}

package github

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// The newest comment a person left on an untrusted reporter's issue (the
// reporter_triage bucket), read for the dashboard's your-turn decision only:
// a maintainer's reply that is still the newest word from a person shows the
// issue as waiting on someone else instead of Admit. Display only: nothing
// that admits, filters, queues, labels or routes an issue reads it.
//
// Cost: when the reporter-trust wait notice scan already listed the issue's
// comments this pass (markReporterTrustAwaiting), the comment is taken from
// that list and costs nothing. Otherwise no call when the issue listing says
// the issue has no comments, and one ListComments call (the last page, where
// the newest comments are) on first sight, then none until the issue's
// updated_at or comment count moves. At most issuePersonCommentReadsPerRepo
// such reads run per repository per pass; a row past the budget keeps its
// cached comment.

// IssuePersonComment is the newest comment by a person on an issue: who
// wrote it, their GitHub association with the repository, when, and the
// logins it @-mentions. The body itself is never kept.
type IssuePersonComment struct {
	Login             string    `json:"login"`
	AuthorAssociation string    `json:"author_association,omitempty"`
	CreatedAt         time.Time `json:"created_at,omitzero"`
	// Mentions are the distinct @-mentioned logins (or org/team slugs),
	// lower-cased, in order of first appearance, outside code and quotes
	// and without bots (personCommentMentions).
	Mentions []string `json:"mentions,omitempty"`
}

// issuePersonCommentPageSize is the page size of the one comments read; the
// page asked for is the last one, so the newest comments are on it.
const issuePersonCommentPageSize = 100

// issuePersonCommentReadsPerRepo caps the comment reads one enumeration pass
// spends per repository, like citedMaxChecksPerRepo. Lists the wait notice
// scan already made do not count.
const issuePersonCommentReadsPerRepo = 10

// issuePersonCommentCacheMax bounds the cache. Past it the cache is dropped
// wholesale, as issueClaimCache is: the next enumeration refills it for the
// live reporter_triage set, the only set it serves.
const issuePersonCommentCacheMax = 4096

type issuePersonCommentCacheEntry struct {
	updatedAt time.Time
	count     int
	comment   *IssuePersonComment
}

// issuePersonComments is the client's cache of latestPersonComment reads,
// keyed "owner/repo#number".
type issuePersonComments struct {
	mu      sync.Mutex
	entries map[string]issuePersonCommentCacheEntry
}

// personCommentPass is one repository's share of an enumeration pass: the
// reads left and whether a read failure was already logged.
type personCommentPass struct {
	reads  int
	warned bool
}

func newPersonCommentPass() *personCommentPass {
	return &personCommentPass{reads: issuePersonCommentReadsPerRepo}
}

// latestPersonComment returns the newest comment a person left on issue, or
// nil when there is none or it could not be read. listed is the issue's
// full comment list when the wait notice scan read it this pass (ok true);
// it is used instead of a read. A failed or over-budget read keeps the
// cached comment, if any: a stale answer beats flipping the row to Admit.
func (c *Client) latestPersonComment(ctx context.Context, owner, repo string, issue *gh.Issue, listed []*gh.IssueComment, ok bool, pass *personCommentPass) *IssuePersonComment {
	if c == nil || issue == nil {
		return nil
	}
	key := owner + "/" + repo + "#" + strconv.Itoa(issue.GetNumber())
	updatedAt := issue.GetUpdatedAt().Time
	count := issue.GetComments()
	if ok {
		comment := c.newestPersonComment(listed)
		c.storePersonComment(key, issuePersonCommentCacheEntry{updatedAt: updatedAt, count: count, comment: comment})
		return comment
	}
	if count <= 0 || c.client == nil {
		return nil
	}
	c.personComments.mu.Lock()
	entry, hit := c.personComments.entries[key]
	c.personComments.mu.Unlock()
	if hit && !updatedAt.IsZero() && entry.updatedAt.Equal(updatedAt) && entry.count == count {
		return entry.comment
	}
	if pass != nil {
		if pass.reads <= 0 {
			return entry.comment
		}
		pass.reads--
	}

	comments, cacheable, err := c.lastCommentPage(ctx, owner, repo, issue.GetNumber(), count)
	if err != nil {
		if c.logger != nil && (pass == nil || !pass.warned) {
			c.logger.Warn("reporter triage: comment read failed; the issue keeps its last known comment this cycle",
				slog.String("repo", owner+"/"+repo), slog.Int("number", issue.GetNumber()), slog.String("error", err.Error()))
		}
		if pass != nil {
			pass.warned = true
		}
		return entry.comment
	}
	comment := c.newestPersonComment(comments)
	if cacheable {
		c.storePersonComment(key, issuePersonCommentCacheEntry{updatedAt: updatedAt, count: count, comment: comment})
	}
	return comment
}

// lastCommentPage reads the page the issue's newest comments are on. The
// page is worked out from the listed count; when the count is stale the
// response says so: a later LastPage is read instead, and an empty page
// past the first restarts from page 1 and its LastPage. cacheable is false
// when the result is still an empty page past the first, so a count that
// ran ahead of GitHub never caches as "no person comment".
func (c *Client) lastCommentPage(ctx context.Context, owner, repo string, number, count int) ([]*gh.IssueComment, bool, error) {
	ctx = WithRESTCaller(ctx, "hive:reporter_triage_comment")
	list := func(page int) ([]*gh.IssueComment, *gh.Response, error) {
		return c.client.Issues.ListComments(ctx, owner, repo, number,
			&gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{Page: page, PerPage: issuePersonCommentPageSize}})
	}
	page := (count-1)/issuePersonCommentPageSize + 1
	comments, resp, err := list(page)
	if err != nil {
		return nil, false, err
	}
	switch {
	case resp != nil && resp.LastPage > page:
		page = resp.LastPage
		comments, _, err = list(page)
	case len(comments) == 0 && page > 1:
		page = 1
		comments, resp, err = list(page)
		if err == nil && resp != nil && resp.LastPage > page {
			page = resp.LastPage
			comments, _, err = list(page)
		}
	}
	if err != nil {
		return nil, false, err
	}
	return comments, len(comments) > 0 || page == 1, nil
}

func (c *Client) storePersonComment(key string, entry issuePersonCommentCacheEntry) {
	c.personComments.mu.Lock()
	defer c.personComments.mu.Unlock()
	if c.personComments.entries == nil || len(c.personComments.entries) >= issuePersonCommentCacheMax {
		c.personComments.entries = make(map[string]issuePersonCommentCacheEntry)
	}
	c.personComments.entries[key] = entry
}

// newestPersonComment picks the newest comment a person wrote as a reply:
// isHumanAuthor skips bots, logins ending in [bot] and this hive's own
// accounts, and a comment that is a slash command (its first non-blank line
// starts with "/", e.g. /assign or /hive decision) or carries a hive marker
// (the hive's own notes under a PAT identity) is skipped too, so the next
// newest one decides. The newest creation time wins; the order comments
// arrive in is not relied on.
func (c *Client) newestPersonComment(comments []*gh.IssueComment) *IssuePersonComment {
	var newest *gh.IssueComment
	for _, comment := range comments {
		if comment == nil || !c.isHumanAuthor(comment.GetUser()) || !personCommentIsReply(comment.GetBody()) {
			continue
		}
		if newest == nil || !comment.GetCreatedAt().Time.Before(newest.GetCreatedAt().Time) {
			newest = comment
		}
	}
	if newest == nil {
		return nil
	}
	return &IssuePersonComment{
		Login:             strings.TrimSpace(newest.GetUser().GetLogin()),
		AuthorAssociation: strings.ToUpper(strings.TrimSpace(newest.GetAuthorAssociation())),
		CreatedAt:         newest.GetCreatedAt().Time,
		Mentions:          c.personCommentMentions(newest.GetBody()),
	}
}

// personCommentHiveMarker opens every hive marker comment.
const personCommentHiveMarker = "<!-- hive:"

// personCommentIsReply reports whether body reads as a person's reply: not
// a slash command and not a hive marker comment.
func personCommentIsReply(body string) bool {
	if strings.Contains(body, personCommentHiveMarker) {
		return false
	}
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return !strings.HasPrefix(line, "/")
		}
	}
	return true
}

// personCommentInlineCode matches an inline code span.
var personCommentInlineCode = regexp.MustCompile("`[^`\n]*`")

// personCommentBotMentions are bot accounts people @-mention without the
// [bot] suffix; asking one is not waiting on a person.
var personCommentBotMentions = map[string]bool{
	"dependabot": true, "renovate": true, "copilot": true, "github-actions": true,
	"codecov": true, "coderabbitai": true, "mergify": true, "mergeraptor": true,
}

// personCommentMentions are the logins body @-mentions outside fenced code
// blocks, inline code and quoted (">") lines, without bots: a "[bot]" login,
// a known bot name or one of this hive's accounts.
func (c *Client) personCommentMentions(body string) []string {
	var kept []string
	fenced := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced || strings.HasPrefix(trimmed, ">") {
			continue
		}
		kept = append(kept, personCommentInlineCode.ReplaceAllString(line, " "))
	}
	var out []string
	for _, login := range extractMentionLogins(strings.Join(kept, "\n")) {
		if personCommentBotMentions[login] || c.isBotLogin(login) {
			continue
		}
		out = append(out, login)
	}
	return out
}

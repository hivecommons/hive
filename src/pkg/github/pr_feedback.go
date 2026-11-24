package github

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Unanswered human feedback on a PR the hive's own agent opened
// (hivecommons/hive#19027).
//
// pr_comments.go feeds the session-resume router, which skips drafts and
// needs a pointer written under an opt-in flag. A reviewer's COMMENTED review
// with inline threads on a draft, hold-labelled, remote-repo agent PR reached
// nobody. This file is the lane-independent reader: given one PR it returns
// every human comment the hive has not yet answered, whatever the PR's draft
// state, labels or repo lane, so a sweep can kick the authoring agent.
//
// The comment text is data. Nothing here executes or interprets it; the kick
// built from it tells the agent to weigh the claim against evidence.

// FeedbackHandledMarkerPrefix opens the hidden marker the agent puts in its
// reply so a later pass never re-routes the same comments.
const FeedbackHandledMarkerPrefix = "<!-- hive:feedback-handled"

// FeedbackNeedsHumanMarker tags the one-time note the sweep posts when the
// feedback cannot be routed to an agent.
const FeedbackNeedsHumanMarker = "<!-- hive:feedback-needs-human -->"

const (
	feedbackMaxPages  = 5
	feedbackPerPage   = 100
	feedbackStateOpen = "open"
)

// feedbackTrustedAssociations are the author associations whose comments are
// routed. CONTRIBUTOR is included (unlike FetchHumanPRComments): on a repo the
// hive does not manage, a maintainer whose organisation membership is private
// reads as CONTRIBUTOR, and the agent treats the text as data either way.
// NONE and FIRST_TIME* stay excluded.
var feedbackTrustedAssociations = map[string]bool{
	"OWNER":        true,
	"MEMBER":       true,
	"COLLABORATOR": true,
	"CONTRIBUTOR":  true,
}

// FeedbackItem is one unanswered human comment.
type FeedbackItem struct {
	// ID is stable per comment: "inline:<id>", "review:<id>" or
	// "conversation:<id>".
	ID string `json:"id"`
	// ThreadID is the review thread's id for an inline comment, else "".
	ThreadID string `json:"thread_id,omitempty"`
	// Thread is the whole review thread this comment sits in, oldest first,
	// including replies by people and by the hive, so a consumer applies the
	// reviewer's LATEST position rather than the first comment's. Empty for
	// review and conversation comments.
	Thread    []FeedbackMessage `json:"thread,omitempty"`
	Kind      PRCommentKind     `json:"kind"`
	Author    string            `json:"author"`
	Body      string            `json:"body"`
	Path      string            `json:"path,omitempty"`
	Line      int               `json:"line,omitempty"`
	URL       string            `json:"url,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	// AuthorAssociation is GitHub's association of the author with the repo
	// (OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR), upper-case. The rule
	// capture trust gate (pkg/reviewrules) reads it.
	AuthorAssociation string `json:"author_association,omitempty"`
}

// FeedbackMessage is one comment in a review thread.
type FeedbackMessage struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"`
	Hive      bool      `json:"hive,omitempty"`
	Body      string    `json:"body"`
	URL       string    `json:"url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// PRFeedback is what one fetch of a PR's conversation found.
type PRFeedback struct {
	// State is "open", "closed" or "merged".
	State   string
	Draft   bool
	HeadRef string
	// HeadSHA is the PR's head commit, "" when the read did not report it.
	// The sweep compares it across passes to tell whether a kicked agent
	// pushed (#19320).
	HeadSHA string
	// LastHiveReplyAt is when the hive's identity last commented, reviewed or
	// replied in a thread on the PR, ignoring the sweep's own notes (needs
	// human, review-rule notices). Zero when it never has.
	LastHiveReplyAt time.Time
	// Items are the unanswered human comments, oldest first.
	Items []FeedbackItem
	// Source is "graphql" or "rest": which read produced this result.
	Source string
	// HiveReplies are the hive's own comments that carry a review-rules
	// block (the structured statement of the rules a reviewer stated). They
	// are read even when no human comment is unanswered.
	HiveReplies []FeedbackMessage
	// AgentReplies are the hive's comments that carry the handled-ids
	// marker: the kicked agent's replies to feedback.
	AgentReplies []FeedbackMessage
	// RuleCandidates are every human comment on the PR, whatever its handled,
	// resolved or addressed state, oldest first. Rule capture reads these
	// (hivecommons/hive#19136); kick routing reads Items.
	RuleCandidates []FeedbackItem
	// AddressedChangeRequests are the people's standing CHANGES_REQUESTED
	// reviews whose every point the hive has handled
	// (hivecommons/hive#19538): the sweep re-requests those reviewers.
	AddressedChangeRequests []AddressedChangeRequest
}

// Open reports whether the PR can still take a revision.
func (f PRFeedback) Open() bool { return f.State == feedbackStateOpen }

type rawPRFeedback struct {
	State       string `json:"state"`
	IsDraft     bool   `json:"isDraft"`
	Merged      bool   `json:"merged"`
	HeadRefName string `json:"headRefName"`
	HeadRefOid  string `json:"headRefOid"`
	rawPRConversation
}

var prFeedbackQuery = fmt.Sprintf(`query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      state isDraft merged headRefName headRefOid
      comments(last: %d) { nodes { databaseId url body createdAt authorAssociation author { __typename login } } }
      reviews(last: %d) { nodes { databaseId url body createdAt authorAssociation state author { __typename login } } }
      reviewThreads(last: %d) {
        nodes {
          id isResolved isOutdated path line
          comments(last: %d) { nodes { databaseId url body createdAt authorAssociation author { __typename login } } }
        }
      }
    }
  }
}`, prCommentsConversationWindow, prCommentsReviewWindow, prCommentsThreadWindow, prCommentsPerThreadWindow)

// FetchPRFeedback returns the unanswered human feedback on repo#number. It
// reads through GraphQL first (one call, carries thread resolution, and has
// its own rate-limit budget) and falls back to listing reviews, review
// comments and issue comments over REST when GraphQL fails, so a hive with
// either budget exhausted still sees the feedback.
func (c *Client) FetchPRFeedback(ctx context.Context, repo string, number int) (PRFeedback, error) {
	if c == nil {
		return PRFeedback{}, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	var data struct {
		Repository struct {
			PullRequest *rawPRFeedback `json:"pullRequest"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": owner, "repo": name, "number": number}
	gqlErr := c.graphQL(ctx, prFeedbackQuery, vars, &data)
	var raw *rawPRFeedback
	source := "graphql"
	switch {
	case gqlErr == nil && data.Repository.PullRequest != nil:
		raw = data.Repository.PullRequest
	default:
		r, restErr := c.fetchPRFeedbackREST(ctx, owner, name, number)
		if restErr != nil {
			if gqlErr == nil {
				gqlErr = fmt.Errorf("%s/%s#%d: pull request not found", owner, name, number)
			}
			return PRFeedback{}, fmt.Errorf("graphql: %v; rest: %w", gqlErr, restErr)
		}
		raw, source = r, "rest"
	}
	state := feedbackStateOpen
	switch {
	case raw.Merged || strings.EqualFold(raw.State, "MERGED"):
		state = "merged"
	case strings.EqualFold(raw.State, "CLOSED"):
		state = "closed"
	}
	return PRFeedback{
		State:                   state,
		Draft:                   raw.IsDraft,
		HeadRef:                 raw.HeadRefName,
		HeadSHA:                 raw.HeadRefOid,
		LastHiveReplyAt:         LastHiveReplyAt(raw.rawPRConversation, c.isHiveAuthor),
		Items:                   UnansweredFeedback(raw.rawPRConversation, c.isHumanFeedbackAuthor, c.isAgentCommentAuthor, c.isHiveIdentityComment, c.addressesHive),
		Source:                  source,
		HiveReplies:             HiveRuleReplies(raw.rawPRConversation, c.isHiveAuthor),
		AgentReplies:            HiveFeedbackReplies(raw.rawPRConversation, c.isHiveAuthor),
		RuleCandidates:          RuleCandidates(raw.rawPRConversation, c.isHumanFeedbackAuthor),
		AddressedChangeRequests: AddressedChangeRequests(raw.rawPRConversation, raw.HeadRefOid, c.isHumanFeedbackAuthor, c.isHiveIdentityComment),
	}, nil
}

func (c *Client) isHiveAuthor(a *rawPRCommentAuthor) bool {
	return a != nil && c.isHiveLogin(strings.TrimSpace(a.Login))
}

// addressesHive reports whether a conversation comment is aimed at the hive:
// it mentions the App bot or the AI author account, or starts with /hive.
// Inline review comments and review bodies on a hive PR are always aimed at
// the hive; plain conversation comments between people are not.
//
// The App bot is matched with or without its "[bot]" suffix: people type
// "@hivecommons-hive", not "@hivecommons-hive[bot]" (cncf/techdocs#424).
// Mentions inside code spans, fenced code and quoted lines do not count, and
// a login only matches as a whole mention, so "@hive-foo" or an email
// address never addresses "hive".
func (c *Client) addressesHive(body string) bool {
	if strings.HasPrefix(strings.TrimSpace(strings.ToLower(body)), "/hive") {
		return true
	}
	id := c.getHiveIdentity()
	return MentionsAnyLogin(body, hiveMentionLogins(c.appBotLogin, id.AppLogin, id.AIAuthor))
}

// hiveMentionLogins expands the hive's logins into every spelling a person
// may use to mention them: each login as configured plus, for an App bot
// login, its bare slug without the "[bot]" suffix.
func hiveMentionLogins(logins ...string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(l string) {
		l = strings.ToLower(strings.TrimSpace(l))
		if l == "" || seen[l] {
			return
		}
		seen[l] = true
		out = append(out, l)
	}
	for _, l := range logins {
		add(l)
		if slug, ok := strings.CutSuffix(strings.ToLower(strings.TrimSpace(l)), botLoginSuffix); ok {
			add(slug)
		}
	}
	return out
}

// MentionsAnyLogin reports whether text @-mentions any of logins as a
// whole mention, case-insensitively, outside code and quoted lines.
func MentionsAnyLogin(text string, logins []string) bool {
	text = strings.ToLower(stripMentionNoise(text))
	for _, login := range logins {
		login = strings.ToLower(strings.TrimSpace(login))
		if login != "" && mentionsLogin(text, login) {
			return true
		}
	}
	return false
}

// stripMentionNoise drops the parts of a Markdown body where an @login is
// not a mention: fenced code, inline code spans and quoted ("> ") lines.
func stripMentionNoise(text string) string {
	text = fencedCodeBlockPattern.ReplaceAllString(text, " ")
	text = inlineCodeSpanPattern.ReplaceAllString(text, " ")
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), ">") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}

// mentionsLogin reports whether lower (already lower-cased) contains
// "@"+login as a standalone mention: the "@" does not follow a login or
// email character, and the login is not followed by one.
func mentionsLogin(lower, login string) bool {
	needle := "@" + login
	for from := 0; ; {
		i := strings.Index(lower[from:], needle)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(needle)
		before := i == 0 || !isMentionChar(lower[i-1])
		after := end >= len(lower) || !isMentionTailChar(lower[end])
		if before && after {
			return true
		}
		from = i + 1
	}
}

// isMentionChar reports whether b may precede "@" in an email address or a
// longer token, so an "@" after it is not a mention.
func isMentionChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '_' || b == '.' || b == '+' || b == '/'
}

// isMentionTailChar reports whether b continues a login (or a "[bot]"
// suffix), so a match ending before it is a prefix of a longer login.
func isMentionTailChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '_' || b == '['
}

var feedbackHandledIDs = regexp.MustCompile(`<!--\s*hive:feedback-handled\s+ids=([^\s>]*)\s*-->`)

// FeedbackHandledMarker renders the hidden marker naming handled comment ids.
func FeedbackHandledMarker(ids []string) string {
	return FeedbackHandledMarkerPrefix + " ids=" + strings.Join(ids, ",") + " -->"
}

// handledIDsIn returns the comment ids a hive comment body declares handled.
func handledIDsIn(body string) []string {
	var out []string
	for _, m := range feedbackHandledIDs.FindAllStringSubmatch(body, -1) {
		for _, id := range strings.Split(m[1], ",") {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

// UnansweredFeedback derives the human comments on a PR the hive has not
// answered. A comment counts only when it is non-empty, from a human (isHuman)
// with a trusted association, and carries no hive attribution trailer. Then:
//
//   - inline: unanswered while its review thread is unresolved and the hive
//     has not replied in that thread after it;
//   - review body (COMMENTED or CHANGES_REQUESTED): unanswered until a hive
//     comment names it in a handled marker;
//   - conversation: same, and only when addressed to the hive.
//
// A review with an empty body (inline threads only) contributes nothing
// itself; its inline comments are the feedback. The draft state, labels and
// repo lane of the PR play no part. isHive says whether a comment is the
// hive's own (a thread reply); isHiveIdentity says whether a comment was
// posted by the hive's own login, and only such a comment's handled marker
// counts: the marker is a plain hidden comment anyone can paste, and it
// carries the "-- hive:" trailer prefix isHive accepts (hivecommons/hive#19547).
// addressed says whether a conversation comment is aimed at the hive.
func UnansweredFeedback(raw rawPRConversation, isHuman func(*rawPRCommentAuthor) bool, isHive, isHiveIdentity func(*rawPRCommentAuthor, string) bool, addressed func(string) bool) []FeedbackItem {
	handled := map[string]bool{}
	hiveComments := append(append([]rawPRComment{}, raw.Comments.Nodes...), raw.Reviews.Nodes...)
	for _, th := range raw.ReviewThreads.Nodes {
		hiveComments = append(hiveComments, th.Comments.Nodes...)
	}
	for _, rc := range hiveComments {
		if rc.Author != nil && isHiveIdentity != nil && isHiveIdentity(rc.Author, rc.Body) {
			for _, id := range handledIDsIn(rc.Body) {
				handled[id] = true
			}
		}
	}
	human := func(rc rawPRComment) bool { return humanFeedback(rc, isHuman) }
	var out []FeedbackItem
	add := func(kind PRCommentKind, rc rawPRComment, thread, path string, line int, ctxThread []FeedbackMessage) {
		id := string(kind) + ":" + strconv.FormatInt(rc.DatabaseID, 10)
		if handled[id] {
			return
		}
		out = append(out, FeedbackItem{ID: id, ThreadID: thread, Thread: ctxThread, Kind: kind, Author: rc.Author.Login,
			Body: truncateCommentBody(rc.Body), Path: path, Line: line, URL: rc.URL, CreatedAt: rc.CreatedAt,
			AuthorAssociation: strings.ToUpper(rc.AuthorAssociation)})
	}
	for _, rc := range raw.Comments.Nodes {
		if human(rc) && addressed != nil && addressed(rc.Body) {
			add(PRCommentConversation, rc, "", "", 0, nil)
		}
	}
	for _, rc := range raw.Reviews.Nodes {
		if routedReviewStates[strings.ToUpper(rc.State)] && human(rc) {
			add(PRCommentReview, rc, "", "", 0, nil)
		}
	}
	for _, th := range raw.ReviewThreads.Nodes {
		if th.IsResolved {
			continue
		}
		line := 0
		if th.Line != nil {
			line = *th.Line
		}
		var lastHiveReply time.Time
		for _, rc := range th.Comments.Nodes {
			if rc.Author != nil && isHive != nil && isHive(rc.Author, rc.Body) && rc.CreatedAt.After(lastHiveReply) {
				lastHiveReply = rc.CreatedAt
			}
		}
		var whole []FeedbackMessage
		for _, rc := range th.Comments.Nodes {
			if rc.Author == nil {
				continue
			}
			whole = append(whole, FeedbackMessage{ID: string(PRCommentInline) + ":" + strconv.FormatInt(rc.DatabaseID, 10),
				Author: rc.Author.Login, Hive: isHive != nil && isHive(rc.Author, rc.Body), Body: truncateCommentBody(rc.Body),
				URL: rc.URL, CreatedAt: rc.CreatedAt})
		}
		sort.SliceStable(whole, func(i, j int) bool { return whole[i].CreatedAt.Before(whole[j].CreatedAt) })
		for _, rc := range th.Comments.Nodes {
			if !human(rc) || !rc.CreatedAt.After(lastHiveReply) {
				continue
			}
			add(PRCommentInline, rc, th.ID, th.Path, line, whole)
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

// humanFeedback reports whether a comment is a person's: non-empty, from a
// human (isHuman) with a trusted association, and without a hive attribution
// trailer.
func humanFeedback(rc rawPRComment, isHuman func(*rawPRCommentAuthor) bool) bool {
	if rc.Author == nil || strings.TrimSpace(rc.Body) == "" || HasAttributionTrailer(rc.Body) {
		return false
	}
	if !feedbackTrustedAssociations[strings.ToUpper(rc.AuthorAssociation)] {
		return false
	}
	return isHuman != nil && isHuman(rc.Author)
}

// RuleCandidates returns every human comment on a PR for rule capture
// (hivecommons/hive#19136): inline comments, non-empty review bodies of any
// state, and conversation comments, with the same author checks as
// UnansweredFeedback but none of its routing gates. A comment named in a
// handled marker, older than the hive's reply in its thread, in a resolved
// thread, or a conversation comment not addressed to the hive is still a
// candidate, because a reviewer can state a rule in any of them. Which
// candidates were already classified is the rule index's concern.
func RuleCandidates(raw rawPRConversation, isHuman func(*rawPRCommentAuthor) bool) []FeedbackItem {
	var out []FeedbackItem
	add := func(kind PRCommentKind, rc rawPRComment, thread, path string, line int) {
		if !humanFeedback(rc, isHuman) {
			return
		}
		out = append(out, FeedbackItem{ID: string(kind) + ":" + strconv.FormatInt(rc.DatabaseID, 10), ThreadID: thread, Kind: kind,
			Author: rc.Author.Login, Body: truncateCommentBody(rc.Body), Path: path, Line: line, URL: rc.URL, CreatedAt: rc.CreatedAt,
			AuthorAssociation: strings.ToUpper(rc.AuthorAssociation)})
	}
	for _, rc := range raw.Comments.Nodes {
		add(PRCommentConversation, rc, "", "", 0)
	}
	for _, rc := range raw.Reviews.Nodes {
		add(PRCommentReview, rc, "", "", 0)
	}
	for _, th := range raw.ReviewThreads.Nodes {
		line := 0
		if th.Line != nil {
			line = *th.Line
		}
		for _, rc := range th.Comments.Nodes {
			add(PRCommentInline, rc, th.ID, th.Path, line)
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

// fetchPRFeedbackREST assembles the same raw shape from the REST list
// endpoints. Thread resolution is not exposed over REST, so every thread reads
// as unresolved; the sweep's handled-id store keeps that from looping.
func (c *Client) fetchPRFeedbackREST(ctx context.Context, owner, repo string, number int) (*rawPRFeedback, error) {
	ctx = WithRESTCaller(ctx, "hive:pr_feedback")
	pr, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	raw := &rawPRFeedback{State: strings.ToUpper(pr.GetState()), IsDraft: pr.GetDraft(), Merged: pr.GetMerged() || !pr.GetMergedAt().IsZero(), HeadRefName: pr.GetHead().GetRef(), HeadRefOid: pr.GetHead().GetSHA()}
	author := func(u *gh.User) *rawPRCommentAuthor {
		if u == nil {
			return nil
		}
		return &rawPRCommentAuthor{Typename: u.GetType(), Login: u.GetLogin()}
	}

	for page := 1; page > 0 && page <= feedbackMaxPages; {
		cs, resp, err := c.client.Issues.ListComments(ctx, owner, repo, number, &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{Page: page, PerPage: feedbackPerPage}})
		if err != nil {
			return nil, err
		}
		for _, ic := range cs {
			raw.Comments.Nodes = append(raw.Comments.Nodes, rawPRComment{DatabaseID: ic.GetID(), URL: ic.GetHTMLURL(), Body: ic.GetBody(),
				CreatedAt: ic.GetCreatedAt().Time, AuthorAssociation: ic.GetAuthorAssociation(), Author: author(ic.User)})
		}
		page = resp.NextPage
	}
	for page := 1; page > 0 && page <= feedbackMaxPages; {
		rs, resp, err := c.client.PullRequests.ListReviews(ctx, owner, repo, number, &gh.ListOptions{Page: page, PerPage: feedbackPerPage})
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			raw.Reviews.Nodes = append(raw.Reviews.Nodes, rawPRComment{DatabaseID: r.GetID(), URL: r.GetHTMLURL(), Body: r.GetBody(),
				CreatedAt: r.GetSubmittedAt().Time, AuthorAssociation: r.GetAuthorAssociation(), State: r.GetState(), Author: author(r.User)})
		}
		page = resp.NextPage
	}
	var inline []*gh.PullRequestComment
	for page := 1; page > 0 && page <= feedbackMaxPages; {
		cs, resp, err := c.client.PullRequests.ListComments(ctx, owner, repo, number, &gh.PullRequestListCommentsOptions{ListOptions: gh.ListOptions{Page: page, PerPage: feedbackPerPage}})
		if err != nil {
			return nil, err
		}
		inline = append(inline, cs...)
		page = resp.NextPage
	}
	raw.ReviewThreads.Nodes = restThreads(inline)
	return raw, nil
}

// restThreads groups REST review comments into threads by their root comment.
func restThreads(inline []*gh.PullRequestComment) []rawPRCommentThread {
	byRoot := map[int64]*rawPRCommentThread{}
	var order []int64
	for _, c := range inline {
		root := c.GetID()
		if c.InReplyTo != nil && c.GetInReplyTo() != 0 {
			root = c.GetInReplyTo()
		}
		th, ok := byRoot[root]
		if !ok {
			th = &rawPRCommentThread{ID: "rest:" + strconv.FormatInt(root, 10), Path: c.GetPath()}
			if l := c.GetLine(); l > 0 {
				th.Line = &l
			}
			byRoot[root] = th
			order = append(order, root)
		}
		var author *rawPRCommentAuthor
		if c.User != nil {
			author = &rawPRCommentAuthor{Typename: c.User.GetType(), Login: c.User.GetLogin()}
		}
		th.Comments.Nodes = append(th.Comments.Nodes, rawPRComment{DatabaseID: c.GetID(), URL: c.GetHTMLURL(), Body: c.GetBody(),
			CreatedAt: c.GetCreatedAt().Time, AuthorAssociation: c.GetAuthorAssociation(), Author: author})
	}
	out := make([]rawPRCommentThread, 0, len(order))
	for _, root := range order {
		out = append(out, *byRoot[root])
	}
	return out
}

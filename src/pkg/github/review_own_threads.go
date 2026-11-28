package github

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

// The hive's own review threads.
//
// Since inline review comments (hivecommons/hive#18531) every reviewer
// finding that points at a changed line is its own resolvable thread, opened
// by the hive's App. Nothing resolved them: the review-thread reconciler
// (#7360) only answers threads a configured classification.review_bots login
// opened. On a repository whose branch protection or ruleset requires
// conversation resolution, a PR carrying one of the hive's own
// "non-blocking, you can ignore it" notes could then never merge: GitHub
// refused every native-queue enqueue with "All comments must be resolved",
// the effect boundary logged it as an uncertain outcome, and the lane retried
// on every sweep (projectbluefin/gutenprint-printer-app#254,
// projectbluefin/ps-printer-app#221, 2026-11-20).
//
// The hive resolves its own threads at two points, both only on a
// repository that requires conversation resolution:
//
//   - when GitHub refuses a merge or merge-queue enqueue because
//     conversations are unresolved (ResolveOwnReviewThreadsForMerge, called
//     by the automerge lanes), after the lane's merge gate passed;
//   - when the stage pass hands a head to a person at
//     090-ready-for-human-merge (ResolveOwnReviewThreadsForHumanMerge), once
//     per head, after reading that the base requires conversation
//     resolution (conversation_resolution.go). The hive never attempts that
//     merge itself (auto-merge off, a human-merge path), so without it the
//     person's merge was refused for the hive's own notes.
//
// Both resolve the hive's own threads marked non-blocking, and only when
// every unresolved thread on the PR is one of those.
//
// A reply from anyone but the hive used to keep such a thread open for a
// person: the conversation is theirs too, and replying was how a person
// kept a note open. The pull request's author is the one exception. An
// author's agent answers a note with "Addressed in <sha>" after pushing the
// fix, and that reply alone turned a note the hive would have resolved into
// one a person had to resolve by hand. A non-blocking thread whose only
// other replies are the PR author's is resolved too, once one of two
// signals holds: the thread is outdated (the commented lines changed after
// the note), or the note was left on an earlier head and the hive's own
// review of the PR's current head approved it after the author's last
// reply (OwnThreadsEvidence). With neither, the author's reply keeps the
// thread open as before. The reply's text is never read: a reply that
// disagrees is treated like "Addressed" once a signal holds.
//
// Never resolved by this path: a thread whose first comment is not the
// hive's App (a person's or another bot's thread), a hive thread anyone but
// the hive and the PR author has replied in, and a hive finding that is not
// marked non-blocking — outdated or not, answered or not.

// reviewFindingSeverityMarker is the hidden marker the relay appends to an
// inline review comment, naming the severity of the verdict finding it
// matches (stampInlineCommentSeverity). It is the structured answer to "did
// the hive mark this non-blocking", read back by ownFindingNonBlocking.
const reviewFindingSeverityMarker = "<!-- hive-finding-severity:"

// reviewFindingSeverityUnmatched is the marker value for an inline comment
// no verdict finding matched (no report, or no finding on its file and
// line). It is read as blocking: the prose fallback is only for threads
// posted before the marker existed.
const reviewFindingSeverityUnmatched = "unmatched"

// reviewFindingSeveritySecurity is the marker value for an inline comment a
// security-perspective finding is anchored on, whatever severity it was
// filed at. It is read as blocking: a security note is never auto-resolved,
// so a reviewer misled (or prompt-injected) into rating a real security
// problem low cannot unblock the merge by it.
const reviewFindingSeveritySecurity = "security"

// ownThreadsAuditAgent is the actor named on the audit entries.
const ownThreadsAuditAgent = "automerge"

// ownThreadsTrigger is the audit trigger detail.
const ownThreadsTrigger = "merge-refused-unresolved-conversations"

// ownThreadsReadyTrigger is the audit trigger detail when the threads are
// resolved for a person's merge at 090-ready-for-human-merge.
const ownThreadsReadyTrigger = "ready-for-human-merge"

// ownThreadsAuthorReplyRule is the audit "rule" detail on a thread resolved
// under the author-reply rule (ownThreadAuthorAnswered); other resolutions
// carry no rule detail.
const ownThreadsAuthorReplyRule = "pr-author-replied"

// unresolvedConversationsRE matches GitHub's refusal when the base requires
// conversation resolution. GitHub words the same rule more than one way:
//
//   - "All comments must be resolved": the GraphQL enqueuePullRequest
//     UNPROCESSABLE ("Pull request All comments must be resolved.") and the
//     REST merge 405 ("Repository rule violations found\n\nAll comments must
//     be resolved."), the GraphQL one seen 2026-11-20;
//   - "A conversation must be resolved": the GraphQL enqueuePullRequest
//     UNPROCESSABLE ("Pull request A conversation must be resolved before
//     this pull request can be merged."), seen in a production hive's
//     contributor lane from 2026-11-27, where it did not match and the enqueue
//     was retried as an uncertain outcome on every sweep;
//   - "conversations must be resolved": the plural of the same phrase, not
//     yet seen in a log, matched so a rewording to it is still named.
//
// The GraphQL error type (UNPROCESSABLE) does not identify this refusal: it
// is the type of every enqueue refusal, so only the message is matched.
var unresolvedConversationsRE = regexp.MustCompile(`(?i)(?:all comments|a conversation|conversations) must be resolved`)

// IsUnresolvedConversationsError reports whether err is GitHub refusing a
// merge or a merge-queue enqueue because review conversations on the PR are
// unresolved. It is a definite refusal: nothing was merged or queued.
func IsUnresolvedConversationsError(err error) bool {
	if err == nil {
		return false
	}
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && unresolvedConversationsRE.MatchString(ghErr.Message) {
		return true
	}
	var gqlErr *graphQLErrors
	if errors.As(err, &gqlErr) {
		for _, e := range gqlErr.Errors {
			if unresolvedConversationsRE.MatchString(e.Message) {
				return true
			}
		}
	}
	return unresolvedConversationsRE.MatchString(err.Error())
}

// OwnReviewThreadsOutcome is what one pass over a PR's unresolved review
// threads found and did.
type OwnReviewThreadsOutcome struct {
	// Unresolved counts the PR's unresolved threads before this pass.
	Unresolved int `json:"unresolved"`
	// Resolvable counts the hive's own threads this pass may resolve.
	Resolvable int `json:"resolvable"`
	// OwnAuthorAnswered counts, among Resolvable, the threads resolvable
	// only under the author-reply rule: the pull request's author replied,
	// nobody else did, and one of its two signals holds. A pass resolves
	// every Resolvable thread or none, so on a cleared PR it is how many
	// threads that rule resolved.
	OwnAuthorAnswered int `json:"own_author_answered"`
	// Resolved counts threads this pass resolved.
	Resolved int `json:"resolved"`
	// OwnBlocking counts the hive's own threads that are blocking findings
	// (or carry no non-blocking mark) and are not resolvable by this pass.
	OwnBlocking int `json:"own_blocking"`
	// OwnAnswered counts the hive's own threads someone else replied in
	// that this pass may not resolve: a reply from anyone but the pull
	// request's author, the author's reply on a blocking finding or with
	// neither signal of the author-reply rule, or a thread with more
	// comments than were read.
	OwnAnswered int `json:"own_answered"`
	// Others counts threads a person or another bot opened.
	Others int `json:"others"`
	// Failed counts resolvable threads whose mutation failed.
	Failed int `json:"failed"`
	// Skipped names why nothing was attempted ("paused",
	// "not-every-thread-resolvable"); empty when the pass ran.
	Skipped string `json:"skipped,omitempty"`
}

// Remaining is how many unresolved threads are left after the pass.
func (o OwnReviewThreadsOutcome) Remaining() int {
	if r := o.Unresolved - o.Resolved; r > 0 {
		return r
	}
	return 0
}

// Cleared reports whether the pass resolved every unresolved thread on the
// PR, so the next merge attempt is not refused for conversations.
func (o OwnReviewThreadsOutcome) Cleared() bool {
	return o.Resolved > 0 && o.Remaining() == 0
}

type ownThreadClass int

const (
	// ownThreadOther: the first comment is a person's or another bot's.
	ownThreadOther ownThreadClass = iota
	// ownThreadAnswered: the hive's thread, but someone else replied and
	// the author-reply rule does not cover it.
	ownThreadAnswered
	// ownThreadBlocking: the hive's finding with no non-blocking mark,
	// outdated or not.
	ownThreadBlocking
	// ownThreadNonBlocking: the hive's thread it marked non-blocking, with
	// no reply but the hive's own. Resolved by this path.
	ownThreadNonBlocking
	// ownThreadAuthorAnswered: the hive's thread it marked non-blocking,
	// replied in by the pull request's author and nobody else, with the
	// thread outdated or a newer head approved after the reply
	// (ownThreadsPR.actedOn). Resolved by this path and counted separately.
	ownThreadAuthorAnswered
)

// OwnThreadsEvidence is what the caller knows about the head it is merging
// or handing to a person, passed in so this package does not read the
// review store. The zero value claims nothing, and an outdated thread is
// then the only evidence the author-reply rule accepts.
type OwnThreadsEvidence struct {
	// HeadSHA is the head the caller acted on. HeadApprovedAt counts only
	// while it is still the PR's head when the threads are read.
	HeadSHA string
	// HeadApprovedAt is the earliest time a report behind the hive's own
	// approving, merge-eligible verdict for HeadSHA was received: no report
	// for that head arrived before it. Zero when there is no such verdict,
	// when it was carried from an earlier head with no review session, or
	// when a perspective's report time is not known.
	HeadApprovedAt time.Time
}

// ownThreadsPR is what classifyOwnReviewThread knows about the pull request
// a thread is on. The zero value knows no author and no approval: every
// reply that is not the hive's own then keeps a thread answered.
type ownThreadsPR struct {
	// authorLogin and authorType are the PR author's login and GraphQL
	// actor type, from the query that read the threads.
	authorLogin string
	authorType  string
	// head and approvedAt are OwnThreadsEvidence.HeadSHA and HeadApprovedAt
	// once the caller's head is known to be the PR's current head; empty
	// and zero otherwise.
	head       string
	approvedAt time.Time
}

// isAuthor reports whether a review-thread comment is the pull request
// author's: the same actor type and the same login, compared without case.
// Both come from one GraphQL read, so a bot author is spelled the same way
// in each; the type is compared because a User and an App's bot can share a
// login there. A PR whose author GitHub no longer names (a deleted account)
// has no author here.
func (p ownThreadsPR) isAuthor(cmt rawReviewComment) bool {
	typ := strings.TrimSpace(p.authorType)
	login := strings.TrimSuffix(strings.TrimSpace(p.authorLogin), "[bot]")
	got := strings.TrimSuffix(strings.TrimSpace(cmt.Author.Login), "[bot]")
	return typ != "" && login != "" && got != "" &&
		strings.EqualFold(typ, strings.TrimSpace(cmt.Author.Typename)) && strings.EqualFold(login, got)
}

// actedOn reports whether one of the two signals the author-reply rule
// accepts holds for t. Neither reads the reply: a reply that disagrees or
// asks a question is treated like "Addressed" once a signal holds. Either
// is enough:
//
//   - the thread is outdated: GitHub marks it so when the lines the note is
//     anchored on changed in a later push;
//   - the note was left on an earlier head, the hive's own review approved
//     the PR's current head, and no report behind that verdict was received
//     before the author's last reply was published (lastReply; dated is
//     false when a reply's time is unknown, which rules this signal out).
//     The review that left the note never counts, whatever its times say:
//     it read the head the note is on. A note whose commit is unknown is
//     treated as left on the current head.
func (p ownThreadsPR) actedOn(t rawReviewThread, lastReply time.Time, dated bool) bool {
	if t.IsOutdated {
		return true
	}
	if !dated || p.approvedAt.IsZero() || !p.approvedAt.After(lastReply) {
		return false
	}
	noteHead := strings.TrimSpace(t.Comments.Nodes[0].OriginalCommit.OID)
	return noteHead != "" && p.head != "" && !strings.EqualFold(noteHead, p.head)
}

// ownThreadReplyTime is when a reply became visible: its publish time, or
// its creation time when GitHub reports no publish time. A reply drafted
// inside a pending review is created before it is published, and it is the
// publish time an approval has to follow.
func ownThreadReplyTime(cmt rawReviewComment) time.Time {
	if !cmt.PublishedAt.IsZero() {
		return cmt.PublishedAt
	}
	return cmt.CreatedAt
}

// isOwnThreadAuthor reports whether a review-thread comment is the hive's
// own: a Bot-typed author whose login is the hive's App bot. GraphQL names
// an App's bot without the REST "[bot]" suffix, so both spellings compare
// equal. project.ai_author is deliberately not counted: it can be a real
// person's account, and resolving a thread on its strength could close that
// person's own conversation. A hive with no App identity (static-token
// mode, where the hive posts as that token's account) has no own threads
// here and resolves nothing.
func (c *Client) isOwnThreadAuthor(cmt rawReviewComment) bool {
	if c == nil || !strings.EqualFold(cmt.Author.Typename, "Bot") {
		return false
	}
	app := strings.TrimSuffix(strings.TrimSpace(c.appBotLogin), "[bot]")
	login := strings.TrimSuffix(strings.TrimSpace(cmt.Author.Login), "[bot]")
	return app != "" && login != "" && strings.EqualFold(app, login)
}

// classifyOwnReviewThread classifies one unresolved thread. isOwn decides
// authorship; blockAt is review.severity.block_at; pr is what is known of
// the pull request the thread is on. A reply from anyone but the hive and
// the PR author is ownThreadAnswered whatever else holds. So is the
// author's reply on a blocking finding, or on a non-blocking one with
// neither signal (actedOn). So is a hive thread with more comments than
// were read: a reply past the query's first page may be someone else's.
func classifyOwnReviewThread(t rawReviewThread, isOwn func(rawReviewComment) bool, blockAt string, pr ownThreadsPR) ownThreadClass {
	if len(t.Comments.Nodes) == 0 || isOwn == nil || !isOwn(t.Comments.Nodes[0]) {
		return ownThreadOther
	}
	if t.Comments.TotalCount > len(t.Comments.Nodes) {
		return ownThreadAnswered
	}
	authorReplied, dated := false, true
	var lastReply time.Time
	for _, cmt := range t.Comments.Nodes[1:] {
		if isOwn(cmt) {
			continue
		}
		if !pr.isAuthor(cmt) {
			return ownThreadAnswered
		}
		authorReplied = true
		if at := ownThreadReplyTime(cmt); at.IsZero() {
			dated = false
		} else if at.After(lastReply) {
			lastReply = at
		}
	}
	nonBlocking := ownFindingNonBlocking(t.Comments.Nodes[0].Body, blockAt)
	if authorReplied {
		if nonBlocking && pr.actedOn(t, lastReply, dated) {
			return ownThreadAuthorAnswered
		}
		return ownThreadAnswered
	}
	if nonBlocking {
		return ownThreadNonBlocking
	}
	return ownThreadBlocking
}

// ownFindingNonBlocking reports whether the hive marked its own finding
// non-blocking. The structured marker wins when present
// (severityNonBlocking); an "unmatched" or unknown marker is blocking.
// Threads opened before the marker existed carry only the reviewer's prose,
// so a conservative read of the comment's first line is the fallback
// (freeTextNonBlocking), read as a low finding.
func ownFindingNonBlocking(body, blockAt string) bool {
	if sev := findingSeverityMarkerValue(body); sev != "" {
		return severityNonBlocking(outputschema.Severity(strings.ToLower(sev)), blockAt)
	}
	return freeTextNonBlocking(body) && severityNonBlocking(outputschema.SeverityLow, blockAt)
}

// findingSeverityMarkerValue returns the severity named by the LAST
// reviewFindingSeverityMarker line in body, "" when there is none. The relay
// strips any marker an agent wrote before appending its own
// (stampInlineCommentSeverity), so the last one is the relay's.
func findingSeverityMarkerValue(body string) string {
	lines := strings.Split(body, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if v := hiddenMarkerValue(lines[i], reviewFindingSeverityMarker); v != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// stripFindingSeverityMarkers drops every reviewFindingSeverityMarker line
// from body, so an agent-written marker can never pose as the relay's.
func stripFindingSeverityMarkers(body string) string {
	if !strings.Contains(strings.ToLower(body), reviewFindingSeverityMarker) {
		return body
	}
	lines := strings.Split(body, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if hiddenMarkerValue(line, reviewFindingSeverityMarker) != "" {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// severityNonBlocking reports whether a finding of sev is below the
// blocking line. With review.severity.block_at set it is
// ReviewFindingBelowLine (P1: medium, low and info are below it; P2: low and
// info; P3: nothing, which is how a hive opts out of the hive resolving
// its own threads at all). With no block_at — the default, where
// ReviewFindingBelowLine puts nothing below the line — info and low are
// treated as below it, the reviewer's own "non-blocking" tier. Only the five
// known severities qualify: ReviewFindingPriority files any other value
// (required, a typo, "unmatched") as P3, which would read as below every
// line, so they are blocking here.
func severityNonBlocking(sev outputschema.Severity, blockAt string) bool {
	switch sev {
	case outputschema.SeverityInfo, outputschema.SeverityLow, outputschema.SeverityMedium,
		outputschema.SeverityHigh, outputschema.SeverityCritical:
	default:
		return false
	}
	if b, ok := config.NormalizeReviewSeverityBlockAt(blockAt); ok && b != "" {
		return ReviewFindingBelowLine(outputschema.Finding{Severity: sev}, b)
	}
	return sev == outputschema.SeverityInfo || sev == outputschema.SeverityLow
}

var (
	// "non-blocking" / "nonblocking" anywhere in the lead.
	freeTextNonBlockingRE = regexp.MustCompile(`(?i)\bnon-?blocking\b`)
	// "style (low):", "docs-currency (info)": a short label then a low
	// severity in parentheses, at the very start.
	freeTextSeverityParenRE = regexp.MustCompile(`(?i)^[a-z][\w ./-]{0,40}\(\s*(?:info|low|nit|nitpick)\s*\)`)
	// "Low, …", "Info: …", "Nit: …" as the first word.
	freeTextSeverityLeadRE = regexp.MustCompile(`(?i)^(?:info|low|nit|nitpick)\s*[,:.)]`)
	// Words that contradict a non-blocking reading once "non-blocking"
	// itself is removed.
	freeTextBlockingRE = regexp.MustCompile(`(?i)\bblock(?:s|ing|er|ers)?\b|\b(?:medium|high|critical|required|must)\b`)
)

// freeTextLeadRunes bounds how much of the first line is read.
const freeTextLeadRunes = 160

// freeTextNonBlocking is the fallback for a hive thread with no severity
// marker: its first line (emphasis stripped, at most freeTextLeadRunes) must
// open with a low severity label or say "non-blocking", and must not also
// say blocking, medium, high, critical, required or must. It reads only the
// formats the hive's reviewer writes ("**style (low):** …", "Style, info,
// non-blocking. …"); anything else is treated as blocking.
func freeTextNonBlocking(body string) bool {
	lead := strings.TrimSpace(body)
	if i := strings.IndexByte(lead, '\n'); i >= 0 {
		lead = lead[:i]
	}
	lead = strings.NewReplacer("**", "", "__", "", "*", "", "`", "").Replace(lead)
	lead = strings.TrimLeft(lead, " >_#-")
	if r := []rune(lead); len(r) > freeTextLeadRunes {
		lead = string(r[:freeTextLeadRunes])
	}
	if lead == "" {
		return false
	}
	marked := freeTextNonBlockingRE.MatchString(lead) || freeTextSeverityParenRE.MatchString(lead) || freeTextSeverityLeadRE.MatchString(lead)
	if !marked {
		return false
	}
	return !freeTextBlockingRE.MatchString(freeTextNonBlockingRE.ReplaceAllString(lead, ""))
}

// reviewBlockAt is review.severity.block_at as the backlog routing has it.
func (c *Client) reviewBlockAt() string {
	routing, ok := c.reviewBacklogRoutingConfig()
	if !ok {
		return ""
	}
	return routing.Severity.BlockAt
}

// ResolveOwnReviewThreadsForMerge is the merge lanes' answer to GitHub's
// "All comments must be resolved": when every unresolved thread on the PR
// is the hive's own and marked non-blocking, it resolves them all, so the
// next merge attempt can succeed. Otherwise it resolves nothing and the
// outcome counts what a person must look at. evidence is what the lane
// knows about the head it tried to merge, for a thread the PR's author
// replied in. Each resolution is audited like an agent's resolve_thread
// request (agent_pr_reviewed, state=thread_resolved) with
// trigger=merge-refused-unresolved-conversations.
func (c *Client) ResolveOwnReviewThreadsForMerge(ctx context.Context, repo string, number int, evidence OwnThreadsEvidence) (OwnReviewThreadsOutcome, error) {
	return c.resolveOwnReviewThreads(ctx, repo, number, ownThreadsTrigger, evidence, nil)
}

// resolveOwnReviewThreads is the one implementation of the rules above for
// both points: it resolves every unresolved thread on repo#number when each
// is the hive's own and marked non-blocking, and nothing otherwise. trigger
// is the audit trigger detail. evidence feeds the author-reply rule; its
// approval counts only when its head is the PR's head in the same read as
// the threads. beforeResolve, when set, runs for each thread just before
// its resolve call, only once every unresolved thread qualified (the
// merge-refusal path passes nil and posts nothing).
func (c *Client) resolveOwnReviewThreads(ctx context.Context, repo string, number int, trigger string, evidence OwnThreadsEvidence, beforeResolve func(context.Context, rawReviewThread)) (OwnReviewThreadsOutcome, error) {
	var out OwnReviewThreadsOutcome
	if c == nil {
		return out, ErrNoGitHubClient
	}
	if c.RepoIsPaused(repo) {
		out.Skipped = "paused"
		return out, nil
	}
	owner, name := c.splitRepo(repo)
	read, err := c.readReviewThreads(ctx, owner, name, number)
	if err != nil {
		return out, err
	}
	pr := ownThreadsPR{authorLogin: read.AuthorLogin, authorType: read.AuthorType}
	if head := strings.TrimSpace(read.HeadSHA); head != "" && strings.EqualFold(head, strings.TrimSpace(evidence.HeadSHA)) {
		pr.head, pr.approvedAt = head, evidence.HeadApprovedAt
	}
	blockAt := c.reviewBlockAt()
	var resolvable []rawReviewThread
	// byAuthorRule names the resolvable threads the author-reply rule
	// admitted, for the audit entry.
	byAuthorRule := map[string]bool{}
	for _, t := range read.Threads {
		if t.IsResolved {
			continue
		}
		out.Unresolved++
		switch classifyOwnReviewThread(t, c.isOwnThreadAuthor, blockAt, pr) {
		case ownThreadNonBlocking:
			out.Resolvable++
			resolvable = append(resolvable, t)
		case ownThreadAuthorAnswered:
			out.Resolvable++
			out.OwnAuthorAnswered++
			byAuthorRule[t.ID] = true
			resolvable = append(resolvable, t)
		case ownThreadOther:
			out.Others++
		case ownThreadAnswered:
			out.OwnAnswered++
		default:
			out.OwnBlocking++
		}
	}
	if len(resolvable) == 0 {
		return out, nil
	}
	if out.Resolvable < out.Unresolved {
		out.Skipped = "not-every-thread-resolvable"
		return out, nil
	}
	meta := c.attributionMeta(ownThreadsAuditAgent)
	for _, t := range resolvable {
		id := t.ID
		if beforeResolve != nil {
			beforeResolve(ctx, t)
		}
		if err := c.resolveReviewThread(ctx, id); err != nil {
			out.Failed++
			c.logger.Warn("review threads: could not resolve the hive's own thread",
				slog.String("repo", repo), slog.Int("number", number), slog.String("thread", id),
				slog.String("error", err.Error()))
			continue
		}
		out.Resolved++
		detail := []string{"state", reviewStateThreadResolved, "thread", id, "trigger", trigger}
		if byAuthorRule[id] {
			detail = append(detail, "rule", ownThreadsAuthorReplyRule)
		}
		c.recordWriteAudit(AuditActionPRReviewed, meta, WriteTarget{Repo: repo, Number: number}, detail...)
	}
	return out, nil
}

// NeedsPerson reports whether the outcome names a thread only a person can
// clear: another author's thread, a hive thread someone answered (not one
// counted in OwnAuthorAnswered, which the hive resolves), or a blocking
// hive finding. An outcome with none of those (GitHub still
// counting a thread a person just resolved, or only resolve mutations that
// failed) is worth another try rather than a park.
func (o OwnReviewThreadsOutcome) NeedsPerson() bool {
	return o.Others > 0 || o.OwnAnswered > 0 || o.OwnBlocking > 0
}

// stampInlineCommentSeverity appends reviewFindingSeverityMarker to each
// drafted inline comment whose (path, line) matches a finding in the
// request's verdict report, so a later pass can tell a non-blocking note
// from a blocking finding without reading prose. drafts and comments are
// parallel (draftReviewComments builds one draft per comment). When several
// findings match one line, the most severe wins. A comment no finding
// matches (no report, no finding on its file and line) is marked
// "unmatched", which reads as blocking. A marker line the agent wrote itself
// is stripped from every comment first, so only the relay's marker is ever
// read back.
func stampInlineCommentSeverity(drafts []*gh.DraftReviewComment, comments []ReviewInlineComment, reports []review.PerspectiveReport) {
	if len(drafts) != len(comments) {
		return
	}
	for i, rc := range comments {
		if drafts[i] == nil {
			continue
		}
		sev, ok := inlineCommentSeverity(rc, reports)
		if !ok || strings.TrimSpace(string(sev)) == "" {
			sev = reviewFindingSeverityUnmatched
		}
		body := strings.TrimRight(stripFindingSeverityMarkers(drafts[i].GetBody()), "\n")
		drafts[i].Body = gh.Ptr(body + "\n\n" + reviewFindingSeverityMarker + " " + string(sev) + " -->")
	}
}

// inlineCommentSeverity finds the most severe verdict finding anchored where
// rc is (same file, a line inside rc's range). A security-perspective
// finding there wins outright as reviewFindingSeveritySecurity.
func inlineCommentSeverity(rc ReviewInlineComment, reports []review.PerspectiveReport) (outputschema.Severity, bool) {
	path := strings.TrimPrefix(strings.TrimSpace(rc.Path), "./")
	start := rc.Line
	if rc.StartLine > 0 {
		start = rc.StartLine
	}
	var best outputschema.Severity
	bestRank := -1
	for _, r := range reports {
		for _, f := range r.Findings {
			if strings.TrimPrefix(strings.TrimSpace(f.File), "./") != path || f.Line < start || f.Line > rc.Line {
				continue
			}
			if r.Perspective == review.PerspectiveSecurity {
				return reviewFindingSeveritySecurity, true
			}
			sev := outputschema.Severity(strings.ToLower(strings.TrimSpace(string(f.Severity))))
			if rank := inlineSeverityRank(sev); rank > bestRank {
				best, bestRank = sev, rank
			}
		}
	}
	return best, bestRank >= 0
}

// inlineSeverityRank orders severities; an unknown one ranks as blocking.
func inlineSeverityRank(s outputschema.Severity) int {
	switch s {
	case outputschema.SeverityInfo:
		return 0
	case outputschema.SeverityLow:
		return 1
	case outputschema.SeverityMedium:
		return 2
	case outputschema.SeverityHigh:
		return 3
	case outputschema.SeverityCritical:
		return 4
	}
	return 5
}

package github

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"

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
// The hive resolves its own threads at exactly one point: when GitHub
// refuses a merge or merge-queue enqueue because conversations are
// unresolved (ResolveOwnReviewThreadsForMerge, called by the automerge
// lanes). So it only ever happens on a repository that requires
// conversation resolution, after the lane's merge gate passed. It resolves
// the hive's own threads marked non-blocking, and only when every
// unresolved thread on the PR is one of those.
//
// Never resolved by this path: a thread whose first comment is not the
// hive's App (a person's or another bot's thread), a hive thread anyone else
// has replied in, and a hive finding that is not marked non-blocking -
// outdated or not.

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

// unresolvedConversationsRE matches GitHub's refusal when the base requires
// conversation resolution: the GraphQL enqueuePullRequest UNPROCESSABLE
// ("Pull request All comments must be resolved.") and the REST merge 405
// ("Repository rule violations found\n\nAll comments must be resolved.").
var unresolvedConversationsRE = regexp.MustCompile(`(?i)all comments must be resolved`)

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
	// Resolved counts threads this pass resolved.
	Resolved int `json:"resolved"`
	// OwnBlocking counts the hive's own threads that are blocking findings
	// (or carry no non-blocking mark) and are not resolvable by this pass.
	OwnBlocking int `json:"own_blocking"`
	// OwnAnswered counts the hive's own threads someone else replied in.
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
	// ownThreadAnswered: the hive's thread, but someone else replied.
	ownThreadAnswered
	// ownThreadBlocking: the hive's finding with no non-blocking mark,
	// outdated or not.
	ownThreadBlocking
	// ownThreadNonBlocking: the hive's thread it marked non-blocking; the
	// only class this path resolves.
	ownThreadNonBlocking
)

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
// authorship; blockAt is review.severity.block_at.
func classifyOwnReviewThread(t rawReviewThread, isOwn func(rawReviewComment) bool, blockAt string) ownThreadClass {
	if len(t.Comments.Nodes) == 0 || isOwn == nil || !isOwn(t.Comments.Nodes[0]) {
		return ownThreadOther
	}
	for _, cmt := range t.Comments.Nodes[1:] {
		if !isOwn(cmt) {
			return ownThreadAnswered
		}
	}
	if ownFindingNonBlocking(t.Comments.Nodes[0].Body, blockAt) {
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
// its own threads at all). With no block_at - the default, where
// ReviewFindingBelowLine puts nothing below the line - info and low are
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
// outcome counts what a person must look at. Each resolution is audited
// like an agent's resolve_thread request (agent_pr_reviewed,
// state=thread_resolved) with trigger=merge-refused-unresolved-conversations.
func (c *Client) ResolveOwnReviewThreadsForMerge(ctx context.Context, repo string, number int) (OwnReviewThreadsOutcome, error) {
	var out OwnReviewThreadsOutcome
	if c == nil {
		return out, ErrNoGitHubClient
	}
	if c.RepoIsPaused(repo) {
		out.Skipped = "paused"
		return out, nil
	}
	owner, name := c.splitRepo(repo)
	_, threads, err := c.fetchReviewThreads(ctx, owner, name, number)
	if err != nil {
		return out, err
	}
	blockAt := c.reviewBlockAt()
	var resolvable []string
	for _, t := range threads {
		if t.IsResolved {
			continue
		}
		out.Unresolved++
		switch classifyOwnReviewThread(t, c.isOwnThreadAuthor, blockAt) {
		case ownThreadNonBlocking:
			out.Resolvable++
			resolvable = append(resolvable, t.ID)
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
	for _, id := range resolvable {
		if err := c.resolveReviewThread(ctx, id); err != nil {
			out.Failed++
			c.logger.Warn("review threads: could not resolve the hive's own thread",
				slog.String("repo", repo), slog.Int("number", number), slog.String("thread", id),
				slog.String("error", err.Error()))
			continue
		}
		out.Resolved++
		c.recordWriteAudit(AuditActionPRReviewed, meta,
			WriteTarget{Repo: repo, Number: number},
			"state", reviewStateThreadResolved,
			"thread", id,
			"trigger", ownThreadsTrigger)
	}
	return out, nil
}

// NeedsPerson reports whether the outcome names a thread only a person can
// clear: another author's thread, a hive thread someone answered, or a
// blocking hive finding. An outcome with none of those (GitHub still
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

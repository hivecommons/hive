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
	"github.com/hivecommons/hive/pkg/effects"
)

// Addressed human change requests (hivecommons/hive#19538).
//
// A person's CHANGES_REQUESTED review keeps standing after the hive has
// answered every point of it: GitHub only clears it when that reviewer
// reviews again (or it is dismissed), and nothing asked them to. The PR then
// sat on the dashboard as "needs decision" with the feedback already handled
// (#19463, #19349). AddressedChangeRequests finds each such review; the
// review-feedback sweep re-requests the reviewer's review once per head and
// posts one short note listing what changed. A human review is never
// dismissed here: it stands until its reviewer approves, asks for changes
// again, or dismisses it.

// ReviewRerequestedMarkerPrefix opens the hidden marker on the sweep's note,
// one per (reviewer, head), so a later pass or a restarted process does not
// re-request the same reviewer on the same head again.
const ReviewRerequestedMarkerPrefix = "<!-- hive:review-rerequested"

const (
	// reviewStateChangesRequested is the review state this file reads.
	reviewStateChangesRequested = "CHANGES_REQUESTED"
	// rerequestNoteShortSHA is how many characters of the head the note
	// prints.
	rerequestNoteShortSHA = 7
	// rerequestNotePointMax bounds the points the note lists; the rest are
	// counted.
	rerequestNotePointMax = 20
	// rerequestNoteExcerptMax bounds each point's quoted excerpt, in runes.
	rerequestNoteExcerptMax = 100
	// rerequestRESTCaller tags the re-request write.
	rerequestRESTCaller = "hive:review_rerequest"
)

// opinionatedReviewStates are the review states that replace a reviewer's
// standing opinion; COMMENTED does not.
var opinionatedReviewStates = map[string]bool{
	"APPROVED":                  true,
	reviewStateChangesRequested: true,
	"DISMISSED":                 true,
}

var reviewRerequestedPattern = regexp.MustCompile(regexp.QuoteMeta(ReviewRerequestedMarkerPrefix) + `\s+reviewer=(\S+)\s+head=([0-9A-Za-z]+)\s*-->`)

// AddressedChangeRequest is a person's standing CHANGES_REQUESTED review
// whose every point the hive has handled.
type AddressedChangeRequest struct {
	// Reviewer is the person whose review stands.
	Reviewer string
	// ReviewID is the review's feedback id ("review:<id>").
	ReviewID string
	// ReviewURL links the review.
	ReviewURL string
	// Points are what the review asked, with how each was handled.
	Points []AddressedPoint
	// Rerequested is true when the sweep's marker for this reviewer on the
	// current head is already on the PR.
	Rerequested bool
}

// AddressedPoint is one point of a change request and how it was handled.
type AddressedPoint struct {
	// ID is the feedback id ("review:<id>" or "inline:<id>").
	ID      string
	Path    string
	Line    int
	URL     string
	Excerpt string
	// ReplyURL links the hive's reply that handled it, "" when the thread
	// was resolved instead.
	ReplyURL string
}

// AddressedChangeRequests returns each trusted person's standing
// CHANGES_REQUESTED review on the PR whose every point the hive has handled.
// A reviewer's standing review is their latest APPROVED / CHANGES_REQUESTED /
// DISMISSED one. Its points are the review body (when non-empty) and every
// inline comment the reviewer left in a review thread. A point is handled
// when a hive comment's feedback-handled marker names it, or, for an inline
// comment, its thread is resolved or the hive replied in it afterwards. At
// least one point must be named by a feedback-handled marker, so a review
// nobody answered never qualifies. isHive must accept only the hive's own
// identity, never a body-based test (FetchPRFeedback passes
// isHiveIdentityComment). head is the PR's current head; the
// result's Rerequested says whether the sweep already acted on it.
func AddressedChangeRequests(raw rawPRConversation, head string, isHuman func(*rawPRCommentAuthor) bool, isHive func(*rawPRCommentAuthor, string) bool) []AddressedChangeRequest {
	head = strings.ToLower(strings.TrimSpace(head))
	handledBy := map[string]string{}
	rerequested := map[string]bool{}
	hiveComment := func(rc rawPRComment) bool {
		return rc.Author != nil && isHive != nil && isHive(rc.Author, rc.Body)
	}
	note := func(rc rawPRComment) {
		if !hiveComment(rc) {
			return
		}
		for _, id := range handledIDsIn(rc.Body) {
			if _, seen := handledBy[id]; !seen {
				handledBy[id] = rc.URL
			}
		}
		for _, m := range reviewRerequestedPattern.FindAllStringSubmatch(rc.Body, -1) {
			rerequested[strings.ToLower(m[1])+"@"+strings.ToLower(m[2])] = true
		}
	}
	for _, rc := range raw.Comments.Nodes {
		note(rc)
	}
	for _, rc := range raw.Reviews.Nodes {
		note(rc)
	}
	for _, th := range raw.ReviewThreads.Nodes {
		for _, rc := range th.Comments.Nodes {
			note(rc)
		}
	}

	standing := map[string]rawPRComment{}
	for _, rv := range raw.Reviews.Nodes {
		if rv.Author == nil || !opinionatedReviewStates[strings.ToUpper(rv.State)] {
			continue
		}
		login := strings.ToLower(strings.TrimSpace(rv.Author.Login))
		if login == "" {
			continue
		}
		if prev, ok := standing[login]; !ok || !rv.CreatedAt.Before(prev.CreatedAt) {
			standing[login] = rv
		}
	}
	var out []AddressedChangeRequest
	for login, rv := range standing {
		if !strings.EqualFold(rv.State, reviewStateChangesRequested) || isHuman == nil || !isHuman(rv.Author) {
			continue
		}
		if !feedbackTrustedAssociations[strings.ToUpper(rv.AuthorAssociation)] || HasAttributionTrailer(rv.Body) {
			continue
		}
		points, covered := changeRequestPoints(raw, rv, login, handledBy, hiveComment)
		if !covered {
			continue
		}
		out = append(out, AddressedChangeRequest{
			Reviewer:    rv.Author.Login,
			ReviewID:    string(PRCommentReview) + ":" + strconv.FormatInt(rv.DatabaseID, 10),
			ReviewURL:   rv.URL,
			Points:      points,
			Rerequested: head != "" && rerequested[login+"@"+head],
		})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Reviewer) < strings.ToLower(out[j].Reviewer) })
	return out
}

// changeRequestPoints lists the points of review rv by login and reports
// whether every one is handled and at least one by a feedback-handled
// marker.
func changeRequestPoints(raw rawPRConversation, rv rawPRComment, login string, handledBy map[string]string, hiveComment func(rawPRComment) bool) ([]AddressedPoint, bool) {
	var points []AddressedPoint
	marked := false
	if strings.TrimSpace(rv.Body) != "" {
		id := string(PRCommentReview) + ":" + strconv.FormatInt(rv.DatabaseID, 10)
		reply, ok := handledBy[id]
		if !ok {
			return nil, false
		}
		marked = true
		points = append(points, AddressedPoint{ID: id, URL: rv.URL, Excerpt: rerequestExcerpt(rv.Body), ReplyURL: reply})
	}
	for _, th := range raw.ReviewThreads.Nodes {
		line := 0
		if th.Line != nil {
			line = *th.Line
		}
		for _, rc := range th.Comments.Nodes {
			if rc.Author == nil || !strings.EqualFold(strings.TrimSpace(rc.Author.Login), login) || strings.TrimSpace(rc.Body) == "" {
				continue
			}
			id := string(PRCommentInline) + ":" + strconv.FormatInt(rc.DatabaseID, 10)
			p := AddressedPoint{ID: id, Path: th.Path, Line: line, URL: rc.URL, Excerpt: rerequestExcerpt(rc.Body)}
			if reply, ok := handledBy[id]; ok {
				p.ReplyURL, marked = reply, true
			} else if reply := hiveReplyAfter(th, rc.CreatedAt, hiveComment); reply != "" {
				p.ReplyURL = reply
			} else if !th.IsResolved {
				return nil, false
			}
			points = append(points, p)
		}
	}
	return points, marked && len(points) > 0
}

// hiveReplyAfter returns the URL of the hive's first reply in th after at,
// or "".
func hiveReplyAfter(th rawPRCommentThread, at time.Time, hiveComment func(rawPRComment) bool) string {
	var found *rawPRComment
	for i := range th.Comments.Nodes {
		rc := &th.Comments.Nodes[i]
		if !hiveComment(*rc) || !rc.CreatedAt.After(at) {
			continue
		}
		if found == nil || rc.CreatedAt.Before(found.CreatedAt) {
			found = rc
		}
	}
	if found == nil {
		return ""
	}
	return found.URL
}

// rerequestExcerpt is the first line of body, trimmed to
// rerequestNoteExcerptMax runes, with Markdown that would break the note's
// list (backticks) dropped.
func rerequestExcerpt(body string) string {
	line := ""
	for _, l := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "<!--") {
			line = l
			break
		}
	}
	line = strings.ReplaceAll(line, "`", "")
	r := []rune(line)
	if len(r) > rerequestNoteExcerptMax {
		line = strings.TrimSpace(string(r[:rerequestNoteExcerptMax-1])) + "…"
	}
	return line
}

// isHiveIdentityComment reports whether a comment was written by the hive's
// own identity (the App bot or the AI author). Unlike isAgentCommentAuthor
// it ignores the attribution trailer: the trailer prefix ("-- hive:") also
// occurs inside every hidden hive marker, so anyone who pastes a
// feedback-handled marker would otherwise pass as the hive and could
// trigger a re-request.
func (c *Client) isHiveIdentityComment(author *rawPRCommentAuthor, _ string) bool {
	return c.isHiveAuthor(author)
}

// ReviewRerequestedMarker renders the once-per-(reviewer, head) marker.
func ReviewRerequestedMarker(reviewer, head string) string {
	return ReviewRerequestedMarkerPrefix + " reviewer=" + strings.ToLower(strings.TrimSpace(reviewer)) + " head=" + strings.ToLower(strings.TrimSpace(head)) + " -->"
}

// ReviewRerequestedNote is the one short note posted when the hive
// re-requests a reviewer's review: what changed, point by point. The
// reviewer is named in code, not @-mentioned: the re-request itself is the
// notification.
func ReviewRerequestedNote(acr AddressedChangeRequest, head string) string {
	short := strings.TrimSpace(head)
	if len(short) > rerequestNoteShortSHA {
		short = short[:rerequestNoteShortSHA]
	}
	var b strings.Builder
	b.WriteString(ReviewRerequestedMarker(acr.Reviewer, head))
	b.WriteString("\n")
	review := "change request"
	if acr.ReviewURL != "" {
		review = "[change request](" + acr.ReviewURL + ")"
	}
	fmt.Fprintf(&b, "Every point of `%s`'s %s has been addressed on head `%s`, so the hive re-requested their review. What changed:\n\n", acr.Reviewer, review, short)
	for i, p := range acr.Points {
		if i == rerequestNotePointMax {
			fmt.Fprintf(&b, "- and %d more\n", len(acr.Points)-rerequestNotePointMax)
			break
		}
		b.WriteString("- ")
		where := "review comment"
		if p.Path != "" {
			where = "`" + p.Path
			if p.Line > 0 {
				where += ":" + strconv.Itoa(p.Line)
			}
			where += "`"
		}
		if p.URL != "" {
			where = "[" + where + "](" + p.URL + ")"
		}
		b.WriteString(where)
		if p.Excerpt != "" {
			b.WriteString(": " + p.Excerpt)
		}
		if p.ReplyURL != "" {
			b.WriteString(" ([handled](" + p.ReplyURL + "))")
		} else {
			b.WriteString(" (thread resolved)")
		}
		b.WriteString("\n")
	}
	b.WriteString("\nThe review still stands until its reviewer approves or asks for changes again; the hive never dismisses it. An owner can also approve this head.")
	return b.String()
}

// RerequestReview re-requests reviewer's review on repo#number for head
// (POST pulls/{n}/requested_reviewers). The effect claim is keyed on the
// head so a re-request on a new head is a new effect.
func (c *Client) RerequestReview(ctx context.Context, repo string, number int, reviewer, head string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	reviewer = strings.TrimSpace(reviewer)
	if reviewer == "" {
		return nil
	}
	owner, repoName := c.splitRepo(repo)
	ctx = WithRESTCaller(ctx, rerequestRESTCaller)
	_, err := effects.Execute(ctx, c.mutationBoundary(), effects.Claim{
		Repo:   owner + "/" + repoName,
		Kind:   effects.KindReviewRequest,
		Target: strconv.Itoa(number),
		Inputs: map[string]string{"reviewers": reviewer, "head_sha": strings.ToLower(strings.TrimSpace(head)), "reason": "changes-addressed"},
	}, func(ctx context.Context) (effects.Result, error) {
		_, _, apiErr := c.client.PullRequests.RequestReviewers(ctx, owner, repoName, number, gh.ReviewersRequest{Reviewers: []string{reviewer}}) // writeback:solicited the reviewer's own change request on this PR, now addressed
		return effects.Result{Provenance: owner + "/" + repoName + "#" + strconv.Itoa(number)}, apiErr
	})
	c.recordNotificationWrite(ctx, notificationWrite{kind: notificationWriteReviewRequest, repo: owner + "/" + repoName, item: number, requested: []string{reviewer}, class: NotificationClassRequiredReview, err: err})
	return err
}

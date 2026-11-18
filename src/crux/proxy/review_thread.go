package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/agent"
)

// Review-thread replies on the hive's own PRs (hivecommons/hive#19096).
//
// The review-feedback sweep (#19027) kicks the agent that opened a PR when a
// person leaves review comments on it. Answering those comments in place
// needs two REST routes the rule table never had, so both fell through to
// deny-by-default while the issue-comment route passed:
//
//   - POST /repos/{o}/{r}/pulls/[{n}/]comments/{id}/replies  (reply in a thread)
//   - POST /repos/{o}/{r}/pulls/{n}/comments           (PR review comment)
//
// They are allowed here, and only here, when the PR is one the hive's own
// agent opened (the audit trail's agent_pr_created entries, the same
// ownership source the sweep uses) and the caller is the agent that opened
// it. Any other PR, any other agent, and any PR the hive cannot attribute is
// refused.
//
// An agent may also retract its own mistake: DELETE of an issue comment or a
// review comment is allowed only when GitHub says the comment was written by
// the hive's own identity and it is younger than OwnCommentDeleteWindow.
// Anyone else's comment, and any older one, stays refused. A lookup that
// fails refuses (fail closed) and says so.

// DenyReasonReviewThread is the 403 reason code for a refused review-thread
// reply, PR review comment or comment delete (see denial_message.go).
const DenyReasonReviewThread = "review_thread"

// OwnCommentDeleteWindow is how long after creation the hive's own comment may
// be deleted by an agent. Long enough to notice and retract a mistake within
// one run, short enough that it never becomes a way to rewrite history.
const OwnCommentDeleteWindow = 15 * time.Minute

// ownCommentClockSkew tolerates a comment timestamp slightly ahead of this
// host's clock; anything further in the future is treated as unknown.
const ownCommentClockSkew = time.Minute

// reviewDraftBodyLimit bounds the body read from a refused reply so the
// drafted text can be handed to the sweep's needs-human note.
const reviewDraftBodyLimit = 16 * 1024

// Kinds of guarded review-thread write.
const (
	reviewWriteReply     = "reply"
	reviewWritePRComment = "pr_comment"
	reviewWriteDelete    = "delete"
)

var (
	reviewReplyPathRe      = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/pulls/(?:\d+/)?comments/(\d+)/replies$`)
	reviewPRCommentPathRe  = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/pulls/(\d+)/comments$`)
	ownCommentDeletePathRe = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/(issues|pulls)/comments/(\d+)$`)
)

// The mode-table rules for the guarded routes. They only say WHICH modes may:
// the review-thread gate runs first and refuses the reply routes unless the PR
// is the hive's own and the caller opened it, and a delete unless the comment
// is the hive's own and recent. Replies and PR review comments sit at the tier
// of a review (ISSUES_AND_PRS or converse); a delete at ISSUES_ONLY. No other
// rule matches these (method, path) pairs, so appending keeps first-match-wins
// order intact; they live here so rules.go stays inside its size ratchet.
func init() {
	rules = append(rules,
		ProxyRule{PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/(?:\d+/)?comments/\d+/replies$`), Method: http.MethodPost, MinMode: agent.ModeIssuesAndPRs, Capability: agent.AgentCapabilities.CanConverse},
		ProxyRule{PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/\d+/comments$`), Method: http.MethodPost, MinMode: agent.ModeIssuesAndPRs, Capability: agent.AgentCapabilities.CanConverse},
		ProxyRule{PathPattern: regexp.MustCompile(`^/repos/[^/]+/[^/]+/(?:issues|pulls)/comments/\d+$`), Method: http.MethodDelete, MinMode: agent.ModeIssuesOnly},
	)
}

// reviewThreadWrite is one classified guarded request.
type reviewThreadWrite struct {
	kind string
	repo string
	// number is the PR for reviewWritePRComment (from the path).
	number int
	// commentID is the target comment for reviewWriteReply / reviewWriteDelete.
	commentID string
	// commentSurface is "issues" or "pulls" for reviewWriteDelete.
	commentSurface string
}

// classifyReviewThreadWrite reports whether (method, path) is one of the
// guarded routes. Every other request is untouched by this gate.
func classifyReviewThreadWrite(method, path string) (reviewThreadWrite, bool) {
	switch method {
	case http.MethodPost:
		if m := reviewReplyPathRe.FindStringSubmatch(path); m != nil {
			return reviewThreadWrite{kind: reviewWriteReply, repo: m[1], commentID: m[2]}, true
		}
		if m := reviewPRCommentPathRe.FindStringSubmatch(path); m != nil {
			n, err := strconv.Atoi(m[2])
			if err != nil || n <= 0 {
				return reviewThreadWrite{}, false
			}
			return reviewThreadWrite{kind: reviewWritePRComment, repo: m[1], number: n}, true
		}
	case http.MethodDelete:
		if m := ownCommentDeletePathRe.FindStringSubmatch(path); m != nil {
			return reviewThreadWrite{kind: reviewWriteDelete, repo: m[1], commentSurface: m[2], commentID: m[3]}, true
		}
	}
	return reviewThreadWrite{}, false
}

// ReviewWriteRefusal describes a refused reply on a PR the hive's own agent
// opened, so the review-feedback sweep can hand the drafted text to a person.
type ReviewWriteRefusal struct {
	Agent  string
	Repo   string
	Number int
	// InReplyTo is the review comment the reply answered, "" for a PR comment.
	InReplyTo string
	// Body is the drafted reply text (bounded), "" when unreadable.
	Body   string
	Reason string
	At     time.Time
}

// ReviewThreadGuard is what the hive supplies for the gate.
type ReviewThreadGuard struct {
	// OwnPR returns the agent that opened repo#number, from the audit trail,
	// and whether the hive's own agent opened it at all.
	OwnPR func(repo string, number int) (agent string, ok bool)
	// HiveLogins returns the GitHub logins that are the hive's own identity
	// (the App bot). A reference repo's write Login is added by the proxy.
	HiveLogins func() []string
	// OnRefused, when set, receives each refused reply on a hive-owned PR.
	OnRefused func(ReviewWriteRefusal)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// reviewGuardState is the guarded copy the proxy holds.
type reviewGuardState struct {
	mu    sync.RWMutex
	guard ReviewThreadGuard
}

// SetReviewThreadGuard installs the ownership seams for review-thread replies
// and own-comment deletes (#19096). With no guard (or no OwnPR) every guarded
// route is refused, which is exactly the behaviour before this gate existed.
func (p *GitHubProxy) SetReviewThreadGuard(g ReviewThreadGuard) {
	p.reviewGuard.mu.Lock()
	p.reviewGuard.guard = g
	p.reviewGuard.mu.Unlock()
}

func (p *GitHubProxy) reviewThreadGuard() ReviewThreadGuard {
	p.reviewGuard.mu.RLock()
	defer p.reviewGuard.mu.RUnlock()
	return p.reviewGuard.guard
}

func (g ReviewThreadGuard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// reviewThreadRefusalText is the agent-facing 403 body. It tells the agent to
// stop instead of probing other routes: a probe comment on a person's review
// is worse than an unanswered thread.
func reviewThreadRefusalText(why string) string {
	return "review-thread write refused: " + why + ". Replies inside review threads and PR review comments are allowed only on a PR the hive's own agent opened, and only for the agent that opened it; deleting a comment is allowed only for the hive's own comment within " + OwnCommentDeleteWindow.String() + " of posting. Do NOT retry, do NOT post the reply as an issue comment instead, and NEVER post a test or probe comment. Stop here: the hive's review-feedback sweep hands your drafted reply to a person."
}

// reviewThreadRefusal is the per-request gate the MITM loop calls for
// non-internal callers. It returns the refusal reason and true when the
// request must be blocked; a request that is not a guarded route returns
// false and is left to the rest of the chain (mode table, repo filter).
func (p *GitHubProxy) reviewThreadRefusal(req *http.Request, agentName string) (string, bool) {
	w, ok := classifyReviewThreadWrite(req.Method, req.URL.Path)
	if !ok {
		return "", false
	}
	g := p.reviewThreadGuard()
	if agentName == "" {
		return reviewThreadRefusalText("the calling agent could not be identified"), true
	}
	if g.OwnPR == nil {
		return reviewThreadRefusalText("this hive has no record of which PRs its agents opened"), true
	}
	if w.kind == reviewWriteDelete {
		return p.ownCommentDeleteRefusal(req, agentName, w, g)
	}
	number := w.number
	if w.kind == reviewWriteReply {
		n, err := p.reviewCommentPR(req, agentName, w)
		if err != nil {
			return reviewThreadRefusalText(fmt.Sprintf("could not resolve which PR review comment %s belongs to (%v)", w.commentID, err)), true
		}
		number = n
	}
	owner, owned := g.OwnPR(w.repo, number)
	if !owned {
		return reviewThreadRefusalText(fmt.Sprintf("%s#%d was not opened by the hive's own agent", w.repo, number)), true
	}
	if !strings.EqualFold(owner, agentName) {
		return reviewThreadRefusalText(fmt.Sprintf("%s#%d was opened by agent %s, not by %s", w.repo, number, owner, agentName)), true
	}
	return "", false
}

// noteRefusedReviewReply is called for every blocked request, whichever gate
// refused it (this one, a reference-repo rule, the mode table, write-surface
// enforcement). When the request was a reply or PR review comment on a PR the
// hive's own agent opened, the drafted text is handed to the review-feedback
// sweep so it can post its needs-human note with the reply a person should
// place. The request body is left readable for the caller's drain.
func (p *GitHubProxy) noteRefusedReviewReply(req *http.Request, agentName, reason string) {
	w, ok := classifyReviewThreadWrite(req.Method, req.URL.Path)
	if !ok || w.kind == reviewWriteDelete {
		return
	}
	g := p.reviewThreadGuard()
	if g.OnRefused == nil || g.OwnPR == nil {
		return
	}
	number := w.number
	if w.kind == reviewWriteReply {
		n, err := p.reviewCommentPR(req, agentName, w)
		if err != nil {
			return
		}
		number = n
	}
	if _, owned := g.OwnPR(w.repo, number); !owned {
		return
	}
	body, inReplyTo := peekReviewDraft(req)
	if w.kind == reviewWriteReply {
		inReplyTo = w.commentID
	}
	g.OnRefused(ReviewWriteRefusal{Agent: agentName, Repo: w.repo, Number: number, InReplyTo: inReplyTo, Body: body, Reason: reason, At: g.now()})
}

// peekReviewDraft reads the JSON body of a reply ({"body": ..., "in_reply_to":
// ...}) without consuming it.
func peekReviewDraft(req *http.Request) (body, inReplyTo string) {
	if req.Body == nil {
		return "", ""
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, reviewDraftBodyLimit))
	req.Body = bodyReadCloser{Reader: io.MultiReader(bytes.NewReader(raw), req.Body), Closer: req.Body}
	if err != nil {
		return "", ""
	}
	var payload struct {
		Body      string          `json:"body"`
		InReplyTo json.RawMessage `json:"in_reply_to"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return "", ""
	}
	return payload.Body, strings.Trim(string(payload.InReplyTo), `"`)
}

// ownCommentDeleteRefusal allows DELETE only of the hive's own recent comment.
func (p *GitHubProxy) ownCommentDeleteRefusal(req *http.Request, agentName string, w reviewThreadWrite, g ReviewThreadGuard) (string, bool) {
	meta, err := p.reviewCommentMeta(req, agentName, w.repo, w.commentSurface, w.commentID)
	if err != nil {
		return reviewThreadRefusalText(fmt.Sprintf("could not confirm comment %s is the hive's own (%v)", w.commentID, err)), true
	}
	logins := p.hiveLoginsFor(w.repo, g)
	own := false
	for _, l := range logins {
		if l != "" && strings.EqualFold(l, meta.User.Login) {
			own = true
			break
		}
	}
	if !own {
		return reviewThreadRefusalText(fmt.Sprintf("comment %s was written by %s, not by the hive", w.commentID, meta.User.Login)), true
	}
	age := g.now().Sub(meta.CreatedAt)
	if meta.CreatedAt.IsZero() || age < -ownCommentClockSkew {
		return reviewThreadRefusalText(fmt.Sprintf("comment %s has no usable creation time", w.commentID)), true
	}
	if age > OwnCommentDeleteWindow {
		return reviewThreadRefusalText(fmt.Sprintf("comment %s is %s old, past the %s retract window", w.commentID, age.Round(time.Second), OwnCommentDeleteWindow)), true
	}
	return "", false
}

// hiveLoginsFor is the hive's identity on repo: the guard's logins plus the
// reference repo's write Login when repo is one.
func (p *GitHubProxy) hiveLoginsFor(repo string, g ReviewThreadGuard) []string {
	var out []string
	if g.HiveLogins != nil {
		out = append(out, g.HiveLogins()...)
	}
	if pol, ok := p.ReferenceRepoPolicyFor(repo); ok && pol.Login != "" {
		out = append(out, pol.Login)
	}
	return out
}

// reviewCommentMetadata is the slice of a GitHub comment the gate reads.
type reviewCommentMetadata struct {
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt      time.Time `json:"created_at"`
	PullRequestURL string    `json:"pull_request_url"`
}

// reviewCommentMeta GETs /repos/{repo}/{issues|pulls}/comments/{id}.
func (p *GitHubProxy) reviewCommentMeta(req *http.Request, agentName, repo, surface, id string) (reviewCommentMetadata, error) {
	var meta reviewCommentMetadata
	body, err := p.referenceLookupOrDefault()(referenceAPIBase(req)+"/repos/"+repo+"/"+surface+"/comments/"+id, p.reviewLookupToken(req, agentName, repo))
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return meta, err
	}
	if meta.User.Login == "" {
		return meta, fmt.Errorf("comment has no author")
	}
	return meta, nil
}

// reviewCommentPR resolves a review comment to its PR number.
func (p *GitHubProxy) reviewCommentPR(req *http.Request, agentName string, w reviewThreadWrite) (int, error) {
	meta, err := p.reviewCommentMeta(req, agentName, w.repo, "pulls", w.commentID)
	if err != nil {
		return 0, err
	}
	i := strings.LastIndex(meta.PullRequestURL, "/pulls/")
	if i < 0 {
		return 0, fmt.Errorf("comment carries no pull_request_url")
	}
	n, err := strconv.Atoi(meta.PullRequestURL[i+len("/pulls/"):])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("unreadable pull_request_url %q", meta.PullRequestURL)
	}
	return n, nil
}

// reviewLookupToken picks the credential for the gate's own GET: a reference
// repo's write token, else the agent's hub-held token, else the credential the
// request itself carries. All three are already on this request's path; none
// is borrowed from another agent. "" sends the GET unauthenticated.
func (p *GitHubProxy) reviewLookupToken(req *http.Request, agentName, repo string) string {
	if pol, ok := p.ReferenceRepoPolicyFor(repo); ok {
		if tok := p.referenceToken(pol); tok != "" {
			return tok
		}
	}
	if p.agentTokenSource != nil {
		if tok, ok := p.agentTokenSource(agentName); ok && tok != "" {
			return tok
		}
	}
	auth := strings.TrimSpace(req.Header.Get("Authorization"))
	for _, scheme := range []string{"token ", "bearer "} {
		if len(auth) > len(scheme) && strings.EqualFold(auth[:len(scheme)], scheme) {
			return strings.TrimSpace(auth[len(scheme):])
		}
	}
	return ""
}

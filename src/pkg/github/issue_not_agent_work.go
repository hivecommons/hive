package github

// Not-agent-work triage verdict (hivecommons/hive#18675).
//
// A scanner triage that concludes an issue is not agent work — a single-hive
// configuration condition rather than a defect, or work that needs a person
// or an environment agents do not have (a real subscription login, a Mac on
// a Zoom call) — used to be only a comment. The issue kept
// stage/010-actionable, the board showed it as Actionable for hours, and the
// scanner re-triaged it on every pass with the same verdict (#18636 got
// three identical comments in an hour).
//
// The verdict is now structured: the comment relay's `--not-agent-work
// <verdict>` flag (IssueRequest.NotAgentWork), or the marker
// `<!-- hive:not-agent-work verdict=<verdict> -->` in the comment body. The
// relay posts the note once with the marker and parks the issue on a person
// with `needs-human`, which the stage engine derives to the needs-human park
// (stage/045) and which keeps the issue out of the actionable set, so the
// next scanner pass does not see it. A repeat of the same verdict with no
// human comment since the last one is dropped (already_existed) instead of
// posted again. A later comment from a person re-queues the issue: the
// un-park sweep lifts the hive's own `needs-human` (never a person's) and
// answers once.

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stageengine"
)

// Not-agent-work verdicts the relay accepts.
const (
	// NotAgentWorkVerdictNotADefect: a configuration condition or expected
	// behaviour, not a code defect; a person confirms and closes it.
	NotAgentWorkVerdictNotADefect = "not-a-defect"
	// NotAgentWorkVerdictNeedsHuman: the work needs a person's action,
	// access or judgement.
	NotAgentWorkVerdictNeedsHuman = "needs-human"
	// NotAgentWorkVerdictNeedsEnvironment: the work needs an environment
	// agents lack (real logins, hardware, a live meeting).
	NotAgentWorkVerdictNeedsEnvironment = "needs-environment"
)

var notAgentWorkVerdicts = map[string]bool{
	NotAgentWorkVerdictNotADefect:       true,
	NotAgentWorkVerdictNeedsHuman:       true,
	NotAgentWorkVerdictNeedsEnvironment: true,
}

// notAgentWorkMarkerPrefix opens the marker; distinct from every other
// hive:* marker so only this file parses it.
const notAgentWorkMarkerPrefix = "<!-- hive:not-agent-work verdict="

var notAgentWorkMarkerRE = regexp.MustCompile(`<!--\s*hive:not-agent-work\s+verdict=([A-Za-z-]+)\s*-->`)

// notAgentWorkRequeueMarker tags the hive's one reply when a person's comment
// re-queues a parked not-agent-work issue (the reply also carries the
// un-park reply marker for that comment, which is what keeps it idempotent).
const notAgentWorkRequeueMarker = "<!-- hive:not-agent-work-requeued -->"

// NormalizeNotAgentWorkVerdict lower-cases and validates a verdict.
func NormalizeNotAgentWorkVerdict(v string) (string, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	return v, notAgentWorkVerdicts[v]
}

// NotAgentWorkMarker renders the marker for a verdict.
func NotAgentWorkMarker(verdict string) string {
	return notAgentWorkMarkerPrefix + verdict + " -->"
}

// ParseNotAgentWorkMarker returns the verdict of the first valid marker in
// text.
func ParseNotAgentWorkMarker(text string) (string, bool) {
	for _, m := range notAgentWorkMarkerRE.FindAllStringSubmatch(text, -1) {
		if v, ok := NormalizeNotAgentWorkVerdict(m[1]); ok {
			return v, true
		}
	}
	return "", false
}

// notAgentWorkVerdictFor returns the verdict a comment request carries: the
// structured field first, else a marker in the body.
func notAgentWorkVerdictFor(req IssueRequest, body string) (string, bool) {
	if v, ok := NormalizeNotAgentWorkVerdict(req.NotAgentWork); ok {
		return v, true
	}
	return ParseNotAgentWorkMarker(body)
}

// notAgentWorkNoteBody makes body carry exactly the marker for verdict,
// first, so the note is recognisable on the next pass.
func notAgentWorkNoteBody(verdict, body string) string {
	body = strings.TrimSpace(notAgentWorkMarkerRE.ReplaceAllString(body, ""))
	return NotAgentWorkMarker(verdict) + "\n" + body
}

// latestNotAgentWorkTriage finds the newest not-agent-work note the hive
// itself posted: its index in comments and its verdict; -1 when none.
func (c *Client) latestNotAgentWorkTriage(comments []*gh.IssueComment) (int, string) {
	for i := len(comments) - 1; i >= 0; i-- {
		cm := comments[i]
		if cm == nil || !c.markerAuthorTrusted(cm.GetUser()) {
			continue
		}
		if v, ok := ParseNotAgentWorkMarker(cm.GetBody()); ok {
			return i, v
		}
	}
	return -1, ""
}

// firstHumanCommentAfter returns the first comment after index i written by a
// person other than this hive, or nil.
func (c *Client) firstHumanCommentAfter(comments []*gh.IssueComment, i int) *gh.IssueComment {
	for j := i + 1; j < len(comments); j++ {
		if cm := comments[j]; cm != nil && c.isHumanAuthor(cm.GetUser()) {
			return cm
		}
	}
	return nil
}

// isRepeatedNotAgentWorkTriage reports whether the hive already posted the
// same verdict and no person has commented since: nothing changed, so there
// is nothing new to say.
func (c *Client) isRepeatedNotAgentWorkTriage(comments []*gh.IssueComment, verdict string) bool {
	idx, prior := c.latestNotAgentWorkTriage(comments)
	if idx < 0 || prior != verdict {
		return false
	}
	return c.firstHumanCommentAfter(comments, idx) == nil
}

// relayNotAgentWorkNote handles a comment request carrying a not-agent-work
// verdict on an issue. handled is false for a pull request (the comment then
// takes the ordinary path). A repeat is acknowledged as already_existed;
// otherwise the note is posted with its marker. park reports that the
// caller must add `needs-human` (the relay's one label write) and then call
// parkNotAgentWorkStage.
func (c *Client) relayNotAgentWorkNote(ctx context.Context, req IssueRequest, verdict, body string, resp *IssueResponse) (handled, park bool, err error) {
	if c == nil || c.client == nil {
		return false, false, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(req.Repo)
	issue, _, err := c.client.Issues.Get(ctx, owner, name, req.Number)
	if err != nil {
		return true, false, fmt.Errorf("reading %s#%d before not-agent-work note: %w", req.Repo, req.Number, err)
	}
	if issue.IsPullRequest() {
		return false, false, nil
	}
	comments, err := c.listIssueComments(ctx, owner, name, req.Number)
	if err != nil {
		return true, false, fmt.Errorf("reading comments on %s#%d before not-agent-work note: %w", req.Repo, req.Number, err)
	}
	park = !strings.EqualFold(issue.GetState(), "closed") && !hasIssueNeedsHumanLabel(issueLabelNames(issue.Labels))
	if c.isRepeatedNotAgentWorkTriage(comments, verdict) {
		c.noteCommentSuppressed()
		if c.logger != nil {
			c.logger.Info("issue-request watcher: identical not-agent-work triage already on issue, not re-posting",
				slog.String("repo", req.Repo), slog.Int("number", req.Number), slog.String("agent", req.Agent), slog.String("verdict", verdict))
		}
		resp.AlreadyExisted = true
		return true, park, nil
	}
	if err := c.CreateIssueCommentBudgeted(ctx, req.Repo, req.Number, notAgentWorkNoteBody(verdict, body)); err != nil {
		return true, false, err
	}
	if park {
		resp.LabelsAdded = []string{issueNeedsHumanLabel}
	}
	return true, park, nil
}

// parkNotAgentWorkStage submits the `needs-human` the relay just added to
// the stage engine as a park, as the label relay does for an agent-added
// `needs-human`, then checks the park landed (a failure retries the
// request, and the retry is a suppressed repeat that only re-parks).
func (c *Client) parkNotAgentWorkStage(ctx context.Context, repo string, number int) error {
	if err := c.StageFacts().Parked(ctx, c.IssueStageRef(repo, number), issueNeedsHumanLabel); err != nil {
		return err
	}
	return c.verifyIssueParkLanded(ctx, repo, number, issueNeedsHumanLabel)
}

// tryRequeueNotAgentWork re-queues an issue the hive parked on a
// not-agent-work verdict once a person comments after the verdict: the
// hive's own `needs-human` comes off, the stage engine is told the park
// lifted, and one reply (keyed to the comment) says so. A `/hive` command
// is left to the un-park paths; a `needs-human` a person applied stays.
func (c *Client) tryRequeueNotAgentWork(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, comments []*gh.IssueComment, answered map[int64]bool, verbs []string, result *IssueUnparkSweepResult, audit func(IssueUnparkEvent)) (bool, error) {
	labels := issueLabelNames(issue.Labels)
	if !hasIssueNeedsHumanLabel(labels) {
		return false, nil
	}
	idx, verdict := c.latestNotAgentWorkTriage(comments)
	if idx < 0 {
		return false, nil
	}
	reply := c.firstHumanCommentAfter(comments, idx)
	if reply == nil || answered[reply.GetID()] {
		return false, nil
	}
	if _, isCommand := parseUnparkCommandFor(reply.GetBody(), verbs); isCommand {
		return false, nil
	}
	number := issue.GetNumber()
	if out := c.needsHumanOutcome(ctx, owner, repo, number, nil); out.applier != stageengine.NeedsHumanApplierHive {
		return false, nil
	}
	if err := c.removeIssueLabel(ctx, owner, repo, number, issueNeedsHumanLabel); err != nil {
		return false, fmt.Errorf("removing %s from %s/%s#%d: %w", issueNeedsHumanLabel, owner, repo, number, err)
	}
	actor := safeGetLogin(reply.GetUser())
	if err := c.StageFacts().Unparked(ctx, c.IssueStageRef(owner+"/"+repo, number), actor); err != nil {
		return false, err
	}
	notifyItemRefresh(owner+"/"+repo, number, false)
	body := notAgentWorkRequeueMarker + "\n" + fmt.Sprintf("Re-queued: %s commented after the hive's not-agent-work triage (%s), so `needs-human` is off and agents may pick this issue up again.", actor, verdict)
	if err := c.postUnparkReply(ctx, owner, repo, number, reply.GetID(), body); err != nil {
		return false, err
	}
	event := IssueUnparkEvent{Repo: displayRepo, Number: number, Actor: actor, Decision: "re-queued after not-agent-work triage (" + verdict + ")", CommentURL: reply.GetHTMLURL()}
	result.Unparked = append(result.Unparked, event)
	if audit != nil {
		audit(event)
	}
	if c.logger != nil {
		c.logger.Info("not-agent-work issue re-queued by a person's comment", "repo", displayRepo, "issue", number, "actor", actor, "verdict", verdict)
	}
	return true, nil
}

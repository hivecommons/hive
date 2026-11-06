package dashboard

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// A needs-decision verdict parks the issue at 045: the contributor relay
// applies the decision label with the task credential (markIssueNeedsDecision
// in bin/contributor-relay.js), and the task prompt asks the agent to leave
// the question as a comment. Nothing checked that it did.
// projectbluefin/common#1605 (2026-11-01 05:58Z) and projectbluefin/utah#1036
// (15:36Z) were parked that way with no comment at all, so the maintainer saw
// "Parked until a maintainer decides" and no question. The hub has the
// verdict's reason, so it posts the question itself — once per task, and
// only when nobody explained the park on the issue after the task was
// assigned.

// needsDecisionParkTimeout bounds the detached GitHub work: one comment
// listing and at most one comment.
const needsDecisionParkTimeout = 30 * time.Second

// needsDecisionParkMarkerPrefix tags the hub's park comment; with the task id
// it makes a re-delivered completion post nothing twice.
const needsDecisionParkMarkerPrefix = "<!-- hive:contribute-park task="

func needsDecisionParkMarker(taskID string) string {
	return needsDecisionParkMarkerPrefix + sanitizeParkTaskID(taskID) + " -->"
}

var parkTaskIDUnsafe = regexp.MustCompile(`[^A-Za-z0-9._/#:-]`)

func sanitizeParkTaskID(id string) string {
	return parkTaskIDUnsafe.ReplaceAllString(strings.TrimSpace(id), "")
}

// parkExplanationMinLen is the shortest comment that counts as the agent's
// own park explanation; a footer or a bare "/hive" line does not.
const parkExplanationMinLen = 40

// parkCommentExplains reports whether a comment posted after the task was
// assigned already says why the issue is parked: any non-trivial comment
// that is not one of the hive's own marked notices (a good-first-issue
// withdrawal, a claim, an un-park notice), and the hub's own park comment.
func parkCommentExplains(body string) bool {
	if strings.Contains(body, needsDecisionParkMarkerPrefix) {
		return true
	}
	if strings.Contains(body, "<!-- hive:") {
		return false
	}
	return len(strings.TrimSpace(body)) >= parkExplanationMinLen
}

// recommendationLabelRE is a leading "Recommended:" / "Recommendation —" label.
var recommendationLabelRE = regexp.MustCompile(`(?i)^recommend(?:ed|ation)?\s*[:—–-]\s*`)

// parkRecommendation is the clause of a reason that names a preferred option
// ("…; recommended: B, because 3.14 is everywhere", "I recommend keeping…"),
// without a leading label; "" when the reason names none. Clauses split on
// ";" and newlines only, so a version number's dot does not cut one short.
func parkRecommendation(reason string) string {
	for _, clause := range strings.FieldsFunc(reason, func(r rune) bool { return r == ';' || r == '\n' }) {
		clause = strings.TrimSpace(clause)
		if strings.Contains(strings.ToLower(clause), "recommend") {
			return strings.TrimSpace(recommendationLabelRE.ReplaceAllString(clause, ""))
		}
	}
	return ""
}

// needsDecisionParkComment is the question the hub posts for a park the agent
// left unexplained. The reason is sanitized (no mentions, no HTML comments).
// When the reason names no preferred option the text avoids the word
// "recommend": the un-park menu reads any hive comment mentioning it as a
// recommendation and would offer `/hive approve` for nothing.
func needsDecisionParkComment(taskID, reporter, reason, label string) string {
	who := "A contributor's agent"
	if r := strings.TrimSpace(reporter); r != "" {
		who = "@" + strings.TrimPrefix(r, "@") + "'s agent"
	}
	reason = sanitizeReportedReason(reason)
	var b strings.Builder
	b.WriteString(needsDecisionParkMarker(taskID))
	b.WriteString("\n")
	fmt.Fprintf(&b, "**Parked for a maintainer decision.** %s stopped on this issue with a decision it could not take, and the hosted contributor relay applied `%s`. The agent did not post its question here, so Hive posts what it reported.\n\n", who, label)
	if reason == "" {
		b.WriteString("**Question:** the agent stated none. Reply `/hive decision <your instructions>` to send the issue back with direction, or remove the label to re-offer it as it is.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "**Question:** %s\n\n", reason)
	if rec := parkRecommendation(reason); rec != "" {
		fmt.Fprintf(&b, "**Recommendation:** %s\n", rec)
	} else {
		b.WriteString("The agent named no preferred option. Reply `/hive decision <your instructions>` with the direction to take.\n")
	}
	return b.String()
}

// postNeedsDecisionPark posts the park question for a needs-decision verdict
// on a GitHub-backed issue task. Runs detached from the read loop; callers
// `go` it. Failures cost a log line, never the completion.
func (h *ContributeWSHub) postNeedsDecisionPark(task *WSTaskAssign, taskID, reporter, reason string, assignedAt time.Time) {
	if h == nil || task == nil || task.Kind != "issue" || task.Repo == "" || task.Number <= 0 || task.ExternalID != "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), needsDecisionParkTimeout)
	defer cancel()
	body := needsDecisionParkComment(taskID, reporter, reason, h.configuredNeedsDecisionLabel())
	if err := h.needsDecisionParkPost(ctx, task.Repo, task.Number, assignedAt, body); err != nil {
		h.logger.Warn("[contribute-ws] needs-decision park: could not post the question",
			"repo", task.Repo, "number", task.Number, "reporter", reporter, "error", err)
	}
}

func (h *ContributeWSHub) needsDecisionParkPost(ctx context.Context, repo string, number int, since time.Time, body string) error {
	if h.needsDecisionParkPoster != nil {
		return h.needsDecisionParkPoster(ctx, repo, number, since, body)
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return ghpkg.ErrNoGitHubClient
	}
	client := h.server.deps.GHClient
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return fmt.Errorf("repo %q is not owner/name", repo)
	}
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	if !since.IsZero() {
		at := since.Add(-time.Minute)
		opts.Since = &at
	}
	comments, _, err := client.GoGitHub().Issues.ListComments(ctx, owner, name, number, opts)
	if err != nil {
		return err
	}
	for _, c := range comments {
		if since.IsZero() || !c.GetCreatedAt().Time.Before(since) {
			if parkCommentExplains(c.GetBody()) {
				return nil
			}
		}
	}
	return client.CreateIssueCommentBudgeted(ctx, repo, number, body) // writeback:solicited the contributor's needs_decision verdict parked this issue; this is that park's question
}

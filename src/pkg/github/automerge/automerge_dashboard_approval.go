package automerge

// The requires_human release consumes the owner's dashboard approval
// (hivecommons/hive#19536).
//
// The dashboard's Approve posts `/approve` as the App on the owner's behalf
// with a head-bound marker (`<!-- hive:pr-approval head=<sha> by=<login> -->`,
// hgithub.ParsePRApprovalCommandComment) and records the decision in the
// in-process human change request store. The store write is the only thing
// the requires_human release used to read besides a GitHub APPROVED review,
// so an approval whose store write did not reach the sweeping process was
// lost: the stage engine re-derived stage/085 minutes after the owner
// approved (hivecommons/hive#19252).
//
// The marker comment is the durable record on GitHub, so the release now
// reads it as a third source, under the same bar an APPROVED review meets:
//
//   - the comment is authored by the hive's App login, so nobody else can
//     post one, and it is unedited, so a repository admin cannot rewrite an
//     old App comment into an approval;
//   - its marker names the current head exactly: a stale-head marker never
//     counts, and an unmarked legacy comment is not bound to any head here;
//   - the login it is attributed to passes isTrustedMerger, the same check
//     the gate applies to an APPROVED review's author.
//
// Submitting a real GitHub Approve review as the operator instead would
// need the operator's own user token in the hive, which the hive does not
// hold and should not: every write goes through the App. Consuming the
// App-authored marker keeps the credential model unchanged and is exactly as
// strong as the approval the dashboard already performs.

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

const (
	// dashboardApprovalCommand is the command line the dashboard's approval
	// comment carries that releases requires_human.
	dashboardApprovalCommand = "/approve"
	// dashboardApprovalRESTCaller tags the comment read.
	dashboardApprovalRESTCaller = "hive:dashboard_approval_release"
)

// dashboardApprovalForHead returns the trusted merger whose dashboard
// approval comment is bound to head (dashboardApprovalActor), newest first.
// A read failure, a missing App identity or an unconfigured merger
// authorizer releases nothing.
func (c *Engine) dashboardApprovalForHead(ctx context.Context, owner, repo string, number int, head string) (string, bool) {
	head = strings.TrimSpace(head)
	if c == nil || c.gh == nil || c.transport == nil || head == "" {
		return "", false
	}
	appLogin := strings.TrimSpace(c.transport.AppBotLogin())
	if appLogin == "" {
		return "", false
	}
	comments, err := c.listIssueComments(hgithub.WithRESTCaller(ctx, dashboardApprovalRESTCaller), owner, repo, number)
	if err != nil {
		c.warn("requires_human release: could not read PR comments for a dashboard approval", "owner", owner, "repo", repo, "pr", number, "head", head, "error", err)
		return "", false
	}
	for i := len(comments) - 1; i >= 0; i-- {
		login, ok := dashboardApprovalActor(comments[i], head, appLogin)
		if !ok {
			continue
		}
		if trusted, configured := c.isTrustedMerger(login); configured && trusted {
			c.noteStageTrustedHumanApproval(owner, repo, number, head, login)
			return login, true
		}
	}
	return "", false
}

// dashboardApprovalActor reads one comment as the dashboard's `/approve`
// bound to head and returns the login it is attributed to. It requires the
// App as author, an unedited comment and a marker whose head equals head;
// the caller still checks the login's merger role.
func dashboardApprovalActor(comment *gh.IssueComment, head, appLogin string) (string, bool) {
	if comment == nil || strings.TrimSpace(appLogin) == "" {
		return "", false
	}
	if !strings.EqualFold(strings.TrimSpace(hgithub.SafeGetLogin(comment.GetUser())), strings.TrimSpace(appLogin)) {
		return "", false
	}
	if comment.GetUpdatedAt().Time.After(comment.GetCreatedAt().Time) {
		return "", false
	}
	command, actor, markerHead, ok := hgithub.ParsePRApprovalCommandComment(comment.GetBody())
	if !ok || command != dashboardApprovalCommand {
		return "", false
	}
	markerHead, head = strings.TrimSpace(markerHead), strings.TrimSpace(head)
	if markerHead == "" || head == "" || !strings.EqualFold(markerHead, head) {
		return "", false
	}
	actor = strings.TrimSpace(actor)
	return actor, actor != ""
}

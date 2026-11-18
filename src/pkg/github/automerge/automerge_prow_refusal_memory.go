package automerge

// Repo-level memory that Prow refuses the App's /ok-to-test
// (hivecommons/hive#18608). Whether Prow trusts the App is the repository's
// Prow configuration, so one refusal on any PR answers the question for every
// later fork PR in that repository. The per-PR memory
// (workflowApprovalUntrusted) cannot: it starts empty for each new PR and for
// each restarted process, so the first /ok-to-test on every new fork PR drew
// Prow's "Cannot trigger testing" reply.
//
// The memory is a cache over the repository's own comments, so it survives a
// restart without a store: one repository-wide comment listing, bounded by
// prowRefusalLookback and prowRefusalMaxPages, looks for Prow's refusal
// addressed to the App or for the hive's own needs-human note that quotes it.
// The result, positive or negative, is kept for okToTestWorkflowCheckTTL. It
// clears when the repository carries the self-contained hive-ok-to-test
// workflow, which approves held runs whatever Prow thinks of the App.

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

const (
	// prowRefusalLookback is how far back the repository's comments are read
	// for evidence of a refusal.
	prowRefusalLookback = 30 * 24 * time.Hour
	// prowRefusalMaxPages bounds the quota one derivation may spend.
	prowRefusalMaxPages = 3
	// prowRefusalPageSize is the comments per page of that listing.
	prowRefusalPageSize = 100
)

// prowRefusalMemo is one repository's remembered answer and when it was read.
type prowRefusalMemo struct {
	at      time.Time
	refused bool
}

func prowRefusalKey(owner, repo string) string {
	return strings.ToLower(owner + "/" + repo)
}

// rememberProwRefusal records that Prow refused the App's command in this
// repository, so no later PR here is asked first.
func (c *Engine) rememberProwRefusal(owner, repo string) {
	c.storeProwRefusal(owner, repo, true)
}

func (c *Engine) storeProwRefusal(owner, repo string, refused bool) {
	c.workflowApprovalCommandsMu.Lock()
	defer c.workflowApprovalCommandsMu.Unlock()
	if c.prowRefusals == nil {
		c.prowRefusals = make(map[string]prowRefusalMemo)
	}
	c.prowRefusals[prowRefusalKey(owner, repo)] = prowRefusalMemo{at: c.clock(), refused: refused}
}

// prowRefusesApp reports whether Prow is known to refuse the App's
// /ok-to-test in this repository. A repository that carries the
// hive-ok-to-test workflow is never reported as refusing. Unknown, or a
// lookup that failed, reports false so the existing post-once behaviour holds.
func (c *Engine) prowRefusesApp(ctx context.Context, displayRepo, owner, repo string) bool {
	key := prowRefusalKey(owner, repo)
	c.workflowApprovalCommandsMu.Lock()
	memo, ok := c.prowRefusals[key]
	c.workflowApprovalCommandsMu.Unlock()
	if !ok || c.clock().Sub(memo.at) >= okToTestWorkflowCheckTTL {
		memo.refused = c.deriveProwRefusal(ctx, displayRepo, owner, repo)
		c.storeProwRefusal(owner, repo, memo.refused)
	}
	if !memo.refused {
		return false
	}
	if c.okToTestWorkflowInstalled(ctx, displayRepo, owner, repo) {
		c.storeProwRefusal(owner, repo, false)
		return false
	}
	return true
}

// deriveProwRefusal reads the repository's recent comments for a Prow refusal
// addressed to the App, or the hive's own note quoting one.
func (c *Engine) deriveProwRefusal(ctx context.Context, displayRepo, owner, repo string) bool {
	appLogin := ""
	if c.transport != nil {
		appLogin = strings.TrimSpace(c.transport.AppBotLogin())
	}
	since := c.clock().Add(-prowRefusalLookback)
	opts := &gh.IssueListCommentsOptions{
		Sort:        gh.Ptr("created"),
		Direction:   gh.Ptr("desc"),
		Since:       &since,
		ListOptions: gh.ListOptions{PerPage: prowRefusalPageSize},
	}
	for page := 0; page < prowRefusalMaxPages; page++ {
		comments, resp, err := c.gh.Issues.ListComments(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, 0, opts)
		if err != nil {
			c.warn("automerge could not list repository comments for Prow ok-to-test refusals", "repo", displayRepo, "error", err)
			return false
		}
		for _, comment := range comments {
			if comment == nil || !strings.Contains(comment.GetBody(), prowOkToTestRejection) {
				continue
			}
			body := comment.GetBody()
			login := hgithub.SafeGetLogin(comment.GetUser())
			if appLogin != "" && strings.EqualFold(login, appLogin) {
				if strings.Contains(body, workflowApprovalRejectedMarker) {
					return true
				}
				continue
			}
			if appLogin == "" || strings.Contains(strings.ToLower(body), "@"+strings.ToLower(appLogin)) {
				return true
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return false
}

// prowKnownRefusalNote is the one needs-human comment posted on a fork PR
// when the repository is already known to refuse the App's command, instead
// of posting the command to be refused. It deliberately omits Prow's refusal
// wording: only a real refusal may renew the repository's memory, so the
// memory ages out if Prow later trusts the App.
func prowKnownRefusalNote(appLogin string) string {
	if appLogin == "" {
		appLogin = "the Hive App"
	}
	var b strings.Builder
	b.WriteString(workflowApprovalRejectedMarker)
	b.WriteString("\nneeds-human: Prow has not run this fork PR's presubmits. Its author is not one of Prow's trusted users, so the trigger plugin waits for `")
	b.WriteString(workflowApprovalCommand)
	b.WriteString("` from a trusted user. Prow already refused the command from `")
	b.WriteString(appLogin)
	b.WriteString("` on this repository, so Hive did not post it here.\n\nA maintainer can comment `")
	b.WriteString(workflowApprovalCommand)
	b.WriteString("` (plus `/test all` for the current head), or make `")
	b.WriteString(appLogin)
	b.WriteString("` a Prow trusted user so Hive's command counts.")
	return b.String()
}

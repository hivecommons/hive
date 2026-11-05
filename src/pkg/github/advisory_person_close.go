package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// A closed advisory issue is reused (#4167) — but not one a person closed.
// projectbluefin/common#639: a maintainer closed the June advisory digest on
// 2026-10-27 ("no longer used"); the hive reopened it the next day, re-added
// hive/advisory and parked it at 045, with no comment. A person's close is a
// decision, so the hive leaves that issue closed and the caller files a new
// advisory issue whose body says why and names `advisory.target`, the setting
// that moves the digest off GitHub. A close by the hive or another bot, or one
// whose closer GitHub does not report, is reused as before.

// advisoryPersonClose is the person-closed advisory issue a new one replaces.
type advisoryPersonClose struct {
	Number   int
	ClosedBy string
	ClosedAt time.Time
}

// advisoryClosedByPerson reports whether issue was closed by a person: a
// closer GitHub names that is neither this hive's account nor a bot.
func (c *Client) advisoryClosedByPerson(issue *gh.Issue) (advisoryPersonClose, bool) {
	if issue == nil || issue.GetState() != "closed" {
		return advisoryPersonClose{}, false
	}
	closer := issue.GetClosedBy()
	login := strings.TrimSpace(closer.GetLogin())
	if login == "" || strings.EqualFold(closer.GetType(), "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") || c.isHiveAccount(closer) {
		return advisoryPersonClose{}, false
	}
	return advisoryPersonClose{Number: issue.GetNumber(), ClosedBy: login, ClosedAt: issue.GetClosedAt().Time}, true
}

// noteAdvisoryPersonClose remembers, per "owner/repo", the person-closed issue
// the next create replaces, so its body can say so.
func (c *Client) noteAdvisoryPersonClose(repo string, pc advisoryPersonClose) {
	c.forgeState.mu.Lock()
	defer c.forgeState.mu.Unlock()
	if c.forgeState.advisoryPersonClosed == nil {
		c.forgeState.advisoryPersonClosed = map[string]advisoryPersonClose{}
	}
	c.forgeState.advisoryPersonClosed[repo] = pc
}

// takeAdvisoryPersonClose returns and forgets the note for repo.
func (c *Client) takeAdvisoryPersonClose(repo string) (advisoryPersonClose, bool) {
	c.forgeState.mu.Lock()
	defer c.forgeState.mu.Unlock()
	pc, ok := c.forgeState.advisoryPersonClosed[repo]
	delete(c.forgeState.advisoryPersonClosed, repo)
	return pc, ok
}

// advisoryReplacesNote is the paragraph a new advisory issue opens with when
// it replaces one a person closed.
func advisoryReplacesNote(pc advisoryPersonClose) string {
	when := ""
	if !pc.ClosedAt.IsZero() {
		when = " on " + pc.ClosedAt.UTC().Format("2006-01-02")
	}
	return fmt.Sprintf("This replaces #%d, which %s closed%s. Hive does not reopen an issue a person closed, so the digest moved here. "+
		"To keep the digest off GitHub, set `advisory.target: linear` (with `advisory.linear_issue`) in hive.yaml; "+
		"to go back to #%d, reopen it and close this one.\n\n", pc.Number, pc.ClosedBy, when, pc.Number)
}

// advisoryReusable reads closed advisory issue n and reports whether it may be
// reopened: false, with the note recorded, when a person closed it.
func (c *Client) advisoryReusable(ctx context.Context, owner, repo string, n int, issue *gh.Issue) bool {
	if issue == nil {
		got, _, err := c.client.Issues.Get(ctx, owner, repo, n)
		if err != nil {
			// Unknown closer: reuse as before (#4167).
			return true
		}
		issue = got
	}
	pc, person := c.advisoryClosedByPerson(issue)
	if !person {
		return true
	}
	c.noteAdvisoryPersonClose(owner+"/"+repo, pc)
	c.logger.Info("advisory issue was closed by a person; not reopening it, a new one will say why",
		"repo", owner+"/"+repo, "number", n, "closed_by", pc.ClosedBy)
	return false
}

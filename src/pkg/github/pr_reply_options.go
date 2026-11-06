package github

import (
	"context"
	"strconv"
	"strings"
	"sync"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/writeback"
)

// The "Your move" section of a PR's hive status comment. A pull request
// that waits on a person used to say so only in prose ("review and approve
// it"), with no word of the `/hive` replies that answer it: the "What to
// reply" notice is posted on issues only (owner on Danathar/tududi#546: "I'd
// always want to see /hive options on PRs if I need to know what to do").
// The section lists the replies that answer this PR's wait, and the GitHub
// review that does the same.
//
// It lives in the status comment, edited in place, rather than in a comment
// of its own kept at the bottom as the issue notice is: the hive posts on
// its own PRs often, and every re-post would notify the owner again.
//
// The sweep that answers `/hive` commands on PRs at stage/085 and
// stage/070 writes it. A PR waits on a person when it is parked at 085, or
// when the review swarm's verdict holds it for a person and no trusted
// merger has released it (the merge gate's own reading). The section is
// emptied when the wait ends: on the next pass for a PR still at 085 or
// 070, and at the end of a full pass for a PR this process wrote it on that
// has left those stages or closed.

// prReplyOptions remembers, per "owner/repo#number", whether this process
// last wrote the section (true) or found it empty (false). A PR it has not
// seen is cleared once, so a section left behind across a restart does not
// outlive its wait.
type prReplyOptions struct {
	mu      sync.Mutex
	written map[string]bool
}

func prReplyOptionsItem(repo string, number int) string {
	return strings.ToLower(repo) + "#" + strconv.Itoa(number)
}

// state reports what this process knows of the PR's section.
func (p *prReplyOptions) state(item string) (written, known bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	written, known = p.written[item]
	return written, known
}

func (p *prReplyOptions) set(item string, written bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.written == nil {
		p.written = map[string]bool{}
	}
	p.written[item] = written
}

// writtenIn lists the PR numbers of repo whose section this process wrote.
func (p *prReplyOptions) writtenIn(repo string) []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	prefix := strings.ToLower(repo) + "#"
	var out []int
	for item, written := range p.written {
		if !written || !strings.HasPrefix(item, prefix) {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(item, prefix)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (p *prReplyOptions) forget(item string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.written, item)
}

// prWait is what a PR waits on a person for.
type prWait struct {
	// Parked: the PR is at stage/085-needs-decision.
	Parked bool
	// Held: the review swarm's verdict holds the PR for a person and no
	// trusted merger has released it; Verdict and HeadSHA describe it.
	Held    bool
	Verdict string
	HeadSHA string
	// HiveAuthored: the hive opened the PR, so a `/hive decision <text>`
	// reaches its agent as a change request.
	HiveAuthored bool
}

// renderPRReplyOptions is the section text for w; "" when the PR waits on
// nobody.
func renderPRReplyOptions(w prWait) string {
	if !w.Parked && !w.Held {
		return ""
	}
	var b strings.Builder
	b.WriteString("**Your move:** ")
	switch {
	case w.Held:
		verdict := strings.TrimSpace(w.Verdict)
		if verdict == "" {
			verdict = "requires_human"
		}
		b.WriteString("the hive's review (`" + verdict + "`")
		if head := strings.TrimSpace(w.HeadSHA); head != "" {
			b.WriteString(" on `" + shortSHA(head) + "`")
		}
		b.WriteString(") holds this pull request for a person")
		if w.Parked {
			b.WriteString(", and it is parked for your decision")
		}
		b.WriteString(".\n\nReply on this PR with one of these:\n\n")
		b.WriteString("- `" + ReviewHoldReleaseCommand + "` approves the current head and releases the hold. The other merge checks still apply.\n")
	default:
		b.WriteString("this pull request is parked for your decision (`" + PRNeedsDecisionStageLabel + "`).\n\nReply on this PR with one of these:\n\n")
	}
	if w.Parked {
		b.WriteString("- `/hive decision approve <why>` approves the current head, with your reason on the record.\n")
	}
	if w.HiveAuthored {
		b.WriteString("- `/hive decision <what to change>` sends it back: the hive's agent gets your words as a change request.\n")
	} else {
		b.WriteString("- To ask for changes, leave a GitHub **Request changes** review. The hive does not push to a PR it did not open.\n")
	}
	b.WriteString("\nA GitHub **Approve** review on the current head does the same as approving here, except on a PR you opened: GitHub refuses that. Only a trusted merger on this hive can approve. `/hive help` lists every command.")
	return b.String()
}

// prWaitFor reads what the PR waits on a person for. An error means the
// PR's reviews or comments could not be read to look for a release.
func (c *Client) prWaitFor(ctx context.Context, owner, repo string, issue *gh.Issue, labels []string) (prWait, error) {
	w := prWait{
		Parked:       labelPresent(labels, PRNeedsDecisionStageLabel),
		HiveAuthored: c.isHiveAccount(issue.GetUser()) || labelPresent(labels, authorkind.Hive.Label()),
	}
	if c.mergeReviewVerdictHold == nil {
		return w, nil
	}
	hold := c.mergeReviewVerdictHold(owner+"/"+repo, issue.GetNumber())
	// A hold with no recording time cannot be released by a reply (the
	// merge gate fails it closed), so it offers nothing to reply with.
	if !hold.Held || hold.RecordedAt.IsZero() {
		return w, nil
	}
	releaser := c.mergeReviewHoldReleaser
	if releaser == nil {
		releaser = func(string) bool { return false }
	}
	isPerson := func(u *gh.User) bool { return IsPersonLogin(u, c.AppBotLogin()) }
	_, released, err := FindReviewHoldRelease(WithRESTCaller(ctx, "hive:pr_reply_options"), c.client, owner, repo, issue.GetNumber(), hold, isPerson, releaser)
	if err != nil {
		return w, err
	}
	if !released {
		w.Held, w.Verdict, w.HeadSHA = true, hold.Verdict, hold.HeadSHA
	}
	return w, nil
}

// syncPRReplyOptions writes or clears the PR's "Your move" section. A PR
// with nothing to say is written to only when this process wrote its
// section, or has not looked at it yet.
func (c *Client) syncPRReplyOptions(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, labels []string) error {
	w, err := c.prWaitFor(ctx, owner, repo, issue, labels)
	if err != nil {
		return err
	}
	return c.writePRReplyOptions(ctx, displayRepo, issue, labels, renderPRReplyOptions(w))
}

func (c *Client) writePRReplyOptions(ctx context.Context, displayRepo string, issue *gh.Issue, labels []string, text string) error {
	number := issue.GetNumber()
	item := prReplyOptionsItem(displayRepo, number)
	if written, known := c.prReplyOptions.state(item); text == "" && known && !written {
		return nil
	}
	wb := c.WritebackItemForIssue(Issue{Number: number, Author: safeGetLogin(issue.GetUser()), Labels: labels, CreatedAt: issue.GetCreatedAt().Time})
	if d := c.WritebackAllowed(displayRepo, wb, writeback.KindNeedsDecisionComment, text); !d.Allowed {
		return nil
	}
	if _, err := c.UpsertPRStatusSection(ctx, displayRepo, number, prstatus.KeyReplyOptions, text); err != nil {
		return err
	}
	c.prReplyOptions.set(item, text != "")
	return nil
}

// clearLeftPRReplyOptions empties the section on every PR of repo this
// process wrote it on that the pass did not see at 085 or 070: it left
// those stages, or closed. Called only after a full pass over the repo's
// open items, so a PR the pass never reached is not mistaken for one that
// left.
func (c *Client) clearLeftPRReplyOptions(ctx context.Context, displayRepo string, seen map[int]bool) {
	for _, number := range c.prReplyOptions.writtenIn(displayRepo) {
		if seen[number] {
			continue
		}
		item := prReplyOptionsItem(displayRepo, number)
		if _, err := c.UpsertPRStatusSection(ctx, displayRepo, number, prstatus.KeyReplyOptions, ""); err != nil {
			if c.logger != nil {
				c.logger.Warn("unpark sweep: clearing a PR's reply options failed", "repo", displayRepo, "pr", number, "error", err)
			}
			continue
		}
		c.prReplyOptions.forget(item)
	}
}

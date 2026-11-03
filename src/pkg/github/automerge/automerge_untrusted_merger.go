package automerge

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// noteUntrustedMerger leaves one durable notice per head, including when a
// merger's authorization is revoked between sweep evaluation and lane execution.
// Failure to publish the notice never changes the merge authorization decision.
func (c *Engine) noteUntrustedMerger(ctx context.Context, owner, repo string, number int, head, queuedBy string) {
	if err := c.commentUntrustedMerger(ctx, owner, repo, number, head, queuedBy); err != nil {
		c.warn("automerge could not report untrusted merger", "repo", owner+"/"+repo, "pr", number, "error", err)
	}
}

func (c *Engine) commentUntrustedMerger(ctx context.Context, owner, repo string, number int, head, queuedBy string) error {
	marker := "<!-- hive:untrusted-merger head=" + head + " -->"
	appLogin := strings.TrimSpace(c.transport.AppBotLogin())
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return err
		}
		for _, comment := range comments {
			if appLogin != "" && strings.EqualFold(hgithub.SafeGetLogin(comment.GetUser()), appLogin) && strings.Contains(comment.GetBody(), marker) {
				return nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	body := fmt.Sprintf("%s\nHive auto-merge is blocked (untrusted-merger): @%s is not a trusted merger for head `%s`. Queue again from a signed-in owner/merger account listed in `dashboard.authorized_users`; an empty allowlist permits no queued merges.", marker, queuedBy, head)
	if c.writebackGate() != nil {
		pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
		if err != nil {
			return err
		}
		item := writeback.Item{Number: number, IsPR: true, Author: pr.GetUser().GetLogin(), CreatedAt: pr.GetCreatedAt().Time}
		item.HiveAuthored = appLogin != "" && strings.EqualFold(item.Author, appLogin)
		for _, label := range pr.Labels {
			item.Labels = append(item.Labels, label.GetName())
		}
		if d := c.writebackAllowed(owner+"/"+repo, item, writeback.KindNeedsDecisionComment, body); !d.Allowed {
			c.info("automerge: untrusted merger note withheld by write-back gate", "repo", owner+"/"+repo, "pr", number, "reason", d.Reason)
			return nil
		}
	}
	_, err := effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindIssueComment,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: map[string]string{"body": effects.StableDigest(marker)},
	}, func(ctx context.Context) (effects.Result, error) {
		comment, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}) // writeback:gated
		if err != nil {
			return effects.Result{}, effects.NotApplied(err)
		}
		c.noteOwnStageComment(owner, repo, number)
		return effects.Result{Provenance: comment.GetHTMLURL()}, nil
	})
	return err
}

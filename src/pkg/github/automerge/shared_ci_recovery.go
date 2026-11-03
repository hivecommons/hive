package automerge

import (
	"context"
	"fmt"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

const unauthorizedStepRerunSharedCI = "rerun-shared-ci"

type sharedCIRecoveryTransport interface {
	SharedCIRerunBlocked(repo string, number int, head string) (hgithub.SharedCIRerunBlock, bool)
}

func (c *Engine) sharedCIRecoveryUnauthorized(ctx context.Context, obs stageObservation) string {
	t, ok := c.transport.(sharedCIRecoveryTransport)
	if !ok {
		return ""
	}
	pr := &gh.PullRequest{Head: &gh.PullRequestBranch{SHA: gh.Ptr(obs.HeadSHA)}}
	for _, name := range obs.Labels {
		pr.Labels = append(pr.Labels, &gh.Label{Name: gh.Ptr(name)})
	}
	block, blocked := t.SharedCIRerunBlocked(obs.Owner+"/"+obs.Repo, obs.Number, obs.HeadSHA)
	if !blocked {
		c.unauthorizedMu.Lock()
		marked := c.unauthorized[c.unauthorizedKey(obs.Owner, obs.Repo, obs.Number)].Step == unauthorizedStepRerunSharedCI
		c.unauthorizedMu.Unlock()
		if marked {
			c.clearUnauthorized(ctx, obs.Display, obs.Owner, obs.Repo, pr, obs.Number, unauthorizedStepRerunSharedCI)
		}
		return ""
	}
	action := fmt.Sprintf("A repository maintainer with Actions write can re-run failed jobs at https://github.com/%s/%s/actions/runs/%d . "+
		"Alternatively, the App owner can request Actions read and write for this recovery installation and its owner must accept the update. "+
		"This is a CI recovery permission block, not a source-code failure or merge approval. Hive retries after its bounded permission cooldown; no alternate credential is selected.",
		obs.Owner, obs.Repo, block.RunID)
	c.markUnauthorized(ctx, obs.Display, obs.Owner, obs.Repo, pr, obs.Number, obs.HeadSHA,
		unauthorizedStepRerunSharedCI, unauthorizedPermissionActionsWrite, action)
	return unauthorizedStepRerunSharedCI
}

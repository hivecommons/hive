package automerge

import (
	"context"
	"errors"
	"path/filepath"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

func (c *Engine) acquireDestinationMerge(ctx context.Context, owner, repo string, number int) (context.Context, func(), error) {
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:destination_merge"), owner, repo, number)
	if err != nil {
		return ctx, nil, err
	}
	dir := ""
	if stateDir := c.preparationStore().dir; stateDir != "" {
		dir = filepath.Join(filepath.Dir(stateDir), "destination-merges")
	}
	return hgithub.AcquireDestinationMerge(ctx, dir, owner+"/"+repo, pr.GetBase().GetRef())
}

func (c *Engine) guardedPR(ctx context.Context, owner, repo string, number int) (*gh.PullRequest, context.Context, func(), error) {
	guarded, unlock, err := c.acquireDestinationMerge(ctx, owner, repo, number)
	if err != nil {
		return nil, ctx, nil, err
	}
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(guarded, "hive:automerge_sweep"), owner, repo, number)
	if err != nil {
		unlock()
		return nil, ctx, nil, err
	}
	// Reacquiring a different destination would let a retargeted PR escape
	// the guard. Verify the fresh destination against the held lease instead.
	if !hgithub.DestinationMergeHeld(guarded, destinationDirectory(c), owner+"/"+repo, pr.GetBase().GetRef()) {
		unlock()
		return nil, ctx, nil, errors.New("merge destination changed while acquiring guard")
	}
	return pr, guarded, unlock, nil
}

func destinationDirectory(c *Engine) string {
	if dir := c.preparationStore().dir; dir != "" {
		return filepath.Join(filepath.Dir(dir), "destination-merges")
	}
	return ""
}

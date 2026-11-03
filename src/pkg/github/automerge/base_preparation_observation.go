package automerge

import (
	"context"
	"net/http"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// BasePreparationObservation is scheduling context, never merge admission or a
// review verdict. HeadSHA is the current live head, not the pre-refresh head.
type BasePreparationObservation struct {
	Repo         string
	BaseRef      string
	Number       int
	HeadSHA      string
	SelectedAt   time.Time
	Waiting      int
	WaitingSince time.Time
	Reason       string
}

// ActiveBasePreparation validates a read-only owner snapshot. The caller should
// cache it once per destination per planning pass. GitHub reads occur outside
// all preparation locks, and a final snapshot rejects concurrent release.
func (c *Engine) ActiveBasePreparation(ctx context.Context, owner, repo, base string) (BasePreparationObservation, bool, error) {
	if !c.requireCurrentBaseFor(owner+"/"+repo) || c.gh == nil {
		return BasePreparationObservation{}, false, nil
	}
	key := preparationKey(owner, repo, base)
	snapshot, err := c.preparationStore().ownerSnapshot(key)
	if err != nil {
		return BasePreparationObservation{}, false, err
	}
	if snapshot.Owner.Number == 0 || c.now().Sub(snapshot.Owner.Since) >= c.preparationWindow() {
		return BasePreparationObservation{}, false, nil
	}
	ctx = hgithub.WithRESTCaller(ctx, "hive:base_preparation_observation")
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, snapshot.Owner.Number)
	if isGitHubStatus(err, http.StatusNotFound) {
		return BasePreparationObservation{}, false, nil
	}
	if err != nil {
		return BasePreparationObservation{}, false, err
	}
	repairing := snapshot.Owner.Repair && c.now().Sub(snapshot.Owner.Since) < effects.DefaultUnresolvedTTL
	if c.preparationInvalidPR(pr, base, repairing) != "" {
		return BasePreparationObservation{}, false, nil
	}
	head := pr.GetHead().GetSHA()
	if head == "" {
		return BasePreparationObservation{}, false, nil
	}
	if head != snapshot.Owner.Head {
		// UpdateBranch merges the live destination tip, which can differ
		// from the attempt's PR snapshot. Reuse the App-authored refresh
		// proof against the destination ref, not that stale base SHA.
		if snapshot.Attempt.Head == "" || snapshot.Attempt.Base == "" {
			return BasePreparationObservation{}, false, nil
		}
		if !c.baseRefresh.HeadIsBaseRefreshOf(ctx, c.gh.Repositories, owner, repo, snapshot.Owner.Number,
			base, snapshot.Attempt.Head, head, c.transport.AppBotLogin()) {
			c.info("automerge base preparation observation skipped", "repo", owner+"/"+repo,
				"base", base, "pr", snapshot.Owner.Number, "head", head, "reason", "unverified-app-base-refresh")
			return BasePreparationObservation{}, false, nil
		}
	}
	current, err := c.preparationStore().ownerSnapshot(key)
	if err != nil {
		return BasePreparationObservation{}, false, err
	}
	if current.Owner != snapshot.Owner || current.Attempt != snapshot.Attempt ||
		c.now().Sub(current.Owner.Since) >= c.preparationWindow() {
		return BasePreparationObservation{}, false, nil
	}
	return BasePreparationObservation{
		Repo: owner + "/" + repo, BaseRef: base, Number: pr.GetNumber(), HeadSHA: head,
		SelectedAt: snapshot.Owner.Since, Waiting: current.Waiting, WaitingSince: current.WaitingSince,
		Reason: basePreparationReady,
	}, true, nil
}

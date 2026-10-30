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
		// An accepted refresh changes the head, but an author's subsequent
		// content push must not inherit its priority. Prove the exact merge
		// of the attempted head and base, rather than trusting any new SHA.
		if snapshot.Attempt.Head == "" || snapshot.Attempt.Base == "" {
			return BasePreparationObservation{}, false, nil
		}
		commit, _, err := c.gh.Repositories.GetCommit(ctx, owner, repo, head, nil)
		if err != nil {
			return BasePreparationObservation{}, false, err
		}
		if commit.GetSHA() != head || len(commit.Parents) != 2 ||
			commit.Parents[0].GetSHA() != snapshot.Attempt.Head || commit.Parents[1].GetSHA() != snapshot.Attempt.Base {
			return BasePreparationObservation{}, false, nil
		}
		// An author can amend a merge's tree while preserving both parents.
		// Reuse the review planner's fail-closed same-patch proof as well.
		patches, ok := c.transport.(interface {
			ReviewHeadsSamePatch(context.Context, string, string, string, string) bool
		})
		if !ok || !patches.ReviewHeadsSamePatch(ctx, owner+"/"+repo, snapshot.Attempt.Base, snapshot.Attempt.Head, head) {
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

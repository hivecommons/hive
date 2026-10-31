package automerge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const (
	basePreparationPrefix = "base-refresh:"
	basePreparationReady  = basePreparationPrefix + "preparing"
)

var sharedBasePreparation = NewBasePreparationStore(filepath.Join(stagelabels.DefaultStateDir, "base-preparation"))

type preparationWait struct{ reason string }

func (e *preparationWait) Error() string { return e.reason }

func preparationWaitReason(err error) string {
	var wait *preparationWait
	if errors.As(err, &wait) {
		return wait.reason
	}
	return ""
}

func (c *Engine) preparationStore() *BasePreparationStore {
	if c.basePreparation != nil {
		return c.basePreparation
	}
	return sharedBasePreparation
}

func (c *Engine) preparationWindow() time.Duration {
	// Use the existing CI/review dwell, including the operator's override.
	// A disabled stall clock must not disable bounded ownership.
	d := stagelabels.Dwell(stagelabels.ReviewPending, c.stagePolicy().Dwell)
	if d <= 0 {
		d = stagelabels.ReviewPending.Dwell()
	}
	return d
}

// preparationInvalid checks live terminal gates without requiring green CI or
// a new verdict: those are expected to become pending after our refresh.
func (c *Engine) preparationInvalid(ctx context.Context, owner, repo, base string, number int, expectedHead string, repair bool) (string, error) {
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:base_preparation"), owner, repo, number)
	if isGitHubStatus(err, http.StatusNotFound) {
		return "gone", nil
	}
	if err != nil {
		return "", err
	}
	if why := c.preparationInvalidPR(pr, base, repair); why != "" {
		return why, nil
	}
	if expectedHead != "" && pr.GetHead().GetSHA() != expectedHead {
		return "head-changed-before-preparation", nil
	}
	return "", nil
}

func (c *Engine) preparationInvalidPR(pr *gh.PullRequest, base string, repair bool) string {
	switch {
	case !strings.EqualFold(pr.GetState(), "open"):
		return "closed"
	case pr.GetBase().GetRef() != base:
		return "retargeted"
	case pr.GetDraft():
		return "draft"
	case prMergeableStateConflicting(pr) && !repair:
		return "conflicting"
	case c.isHeld(labelNames(pr.Labels)):
		return "held"
	case c.transport.IsExemptLabels(labelNames(pr.Labels)):
		return "exempt-label"
	}
	if reason := c.humanMergeRequiredReason(pr.GetTitle(), labelNames(pr.Labels)); reason != "" {
		return reason
	}
	return ""
}

func preparationQueued(front int) string {
	return fmt.Sprintf("%squeued-behind#%d", basePreparationPrefix, front)
}

// admitBasePreparation is called only after admission gates, not after the
// current-base gate: a green/reviewed behind head can therefore enter. The
// durable owner is not a ready merge-train seat.
func (c *Engine) admitBasePreparation(ctx context.Context, owner, repo, base string, number int, head, lane string, repair bool) (string, error) {
	reason := ""
	now := c.now()
	key := preparationKey(owner, repo, base)
	err := c.preparationStore().change(key, func(st *basePreparation) error {
		if st.Owner.Number != 0 {
			repairing := st.Owner.Repair && now.Sub(st.Owner.Since) < effects.DefaultUnresolvedTTL
			why, err := c.preparationInvalid(ctx, owner, repo, base, st.Owner.Number, "", repairing)
			if err != nil {
				return err
			}
			if why == "" && now.Sub(st.Owner.Since) >= c.preparationWindow() {
				why = "preparation-expired"
			}
			if why != "" {
				c.info("automerge base preparation released", "repo", owner+"/"+repo, "base", base, "pr", st.Owner.Number, "reason", why)
				st.Owner = preparationCandidate{}
			}
		}
		if st.Owner.Number == number {
			return nil // sticky through expected pending checks and review
		}
		i := slices.IndexFunc(st.Waiting, func(w preparationCandidate) bool { return w.Number == number })
		if i < 0 {
			st.Waiting = append(st.Waiting, preparationCandidate{Number: number, Head: head, Lane: lane, Repair: repair, Since: now})
		} else {
			st.Waiting[i].Head = head
		}
		candidate, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
		if err != nil {
			return err
		}
		if candidate.GetHead().GetSHA() != head || c.preparationInvalidPR(candidate, base, repair) != "" {
			reason = basePreparationPrefix + "snapshot-changed"
			return nil
		}
		var yielded preparationCandidate
		if st.Owner.Number != 0 {
			front, _, err := c.gh.PullRequests.Get(ctx, owner, repo, st.Owner.Number)
			if err != nil {
				return err
			}
			if st.Owner.Repair || repair || !c.preparationCompleted(ctx, owner, repo, front, st.Attempts[st.Owner.Number]) ||
				!c.preparationMayPass(ctx, owner, repo, base, candidate, front, lane, st.Owner.Lane) {
				reason = preparationQueued(st.Owner.Number)
				return nil
			}
			yielded = st.Owner
			ticket := yielded
			ticket.Head = front.GetHead().GetSHA()
			ticket.Yielded = true
			st.Waiting = append([]preparationCandidate{ticket}, st.Waiting...)
			st.Owner = preparationCandidate{}
		}
		if st.Owner.Number == 0 {
			passed := 0
			for index := 0; index < len(st.Waiting); {
				next := st.Waiting[index]
				why, err := c.preparationInvalid(ctx, owner, repo, base, next.Number, next.Head, next.Repair)
				if err != nil {
					return err
				}
				if why == "head-changed-before-preparation" && next.Yielded {
					reason = preparationQueued(next.Number)
					break
				}
				if why != "" || (!next.Yielded && now.Sub(next.Since) >= c.preparationWindow()) {
					st.Waiting = slices.Delete(st.Waiting, index, index+1)
					continue
				}
				if next.Number != number {
					if passed >= preparationMaxPredecessors {
						reason = preparationQueued(next.Number)
						break
					}
					front, _, err := c.gh.PullRequests.Get(ctx, owner, repo, next.Number)
					if err != nil {
						return err
					}
					if !repair && !next.Repair && c.preparationMayPass(ctx, owner, repo, base, candidate, front, lane, next.Lane) {
						passed++
						index++
						continue
					}
					if yielded.Number != 0 && st.Owner.Number == number {
						c.info("automerge base preparation completed owner yielded", "repo", owner+"/"+repo, "base", base, "pr", number, "former_owner", yielded.Number, "former_since", yielded.Since)
					}
					reason = preparationQueued(next.Number)
					break
				}
				st.Waiting = slices.Delete(st.Waiting, index, index+1)
				next.Since = now
				next.Lane = lane
				next.Repair = repair
				st.Owner = next
				c.info("automerge base preparation selected", "repo", owner+"/"+repo, "base", base, "pr", next.Number, "head", next.Head, "lane", next.Lane)
				break
			}
		}
		if yielded.Number != 0 && st.Owner.Number != number {
			// A later ambiguous predecessor prevented admission; do not
			// relinquish a completed owner's durable priority for nothing.
			st.Owner = yielded
			st.Waiting = slices.DeleteFunc(st.Waiting, func(w preparationCandidate) bool { return w.Number == yielded.Number })
		}
		if st.Owner.Number != number {
			if reason == "" {
				reason = preparationQueued(st.Owner.Number)
			}
			front := st.Owner.Number
			if front == 0 && len(st.Waiting) > 0 {
				front = st.Waiting[0].Number
			}
			c.info("automerge base preparation waiting", "repo", owner+"/"+repo, "base", base, "pr", number, "front", front, "reason", reason)
		}
		return nil
	})
	return reason, err
}

// claimBasePreparationUpdate writes the attempt BEFORE issuing update-branch.
// Neither a new Engine, another lane nor a pod restart repeats that head/base
// pair while it is unresolved. After the existing mutation reconciliation TTL,
// only a still-identical authoritative head permits a head-pinned retry.
func (c *Engine) claimBasePreparationUpdate(ctx context.Context, owner, repo string, number int, head string) error {
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:base_preparation_update"), owner, repo, number)
	if err != nil {
		return err
	}
	base := pr.GetBase().GetRef()
	branch, _, err := c.gh.Repositories.GetBranch(ctx, owner, repo, base, 0)
	if err != nil {
		return err
	}
	baseSHA := branch.GetCommit().GetSHA()
	if head == "" || pr.GetHead().GetSHA() != head || base == "" || baseSHA == "" {
		return &preparationWait{basePreparationPrefix + "snapshot-changed"}
	}
	var wait string
	err = c.preparationStore().change(preparationKey(owner, repo, base), func(st *basePreparation) error {
		if st.Owner.Number != number {
			wait = preparationQueued(st.Owner.Number)
			return nil
		}
		if why := c.preparationInvalidPR(pr, base, st.Owner.Repair); why != "" {
			wait = why
			return nil
		}
		if st.Attempts == nil {
			st.Attempts = map[int]preparationAttempt{}
		}
		prior := st.Attempts[number]
		if prior.Head == head && prior.Base == baseSHA && c.now().Sub(prior.At) < effects.DefaultUnresolvedTTL {
			wait = basePreparationReady
			return nil
		}
		st.Attempts[number] = preparationAttempt{Head: head, Base: baseSHA, At: c.now(), AuthoritativeBase: true}
		c.info("automerge base preparation refresh claimed", "repo", owner+"/"+repo, "base", base, "pr", number, "head", head, "base_sha", baseSHA)
		return nil
	})
	if err != nil {
		return err
	}
	if wait != "" {
		c.info("automerge base preparation waiting", "repo", owner+"/"+repo, "base", base, "pr", number, "reason", wait)
		return &preparationWait{wait}
	}
	return nil
}

func (c *Engine) forgetRejectedPreparationUpdate(owner, repo string, number int, head string) {
	// A synchronous refusal cannot have applied the write. Forget only our
	// reservation; the mutation journal still owns ambiguous reconciliation.
	pr, _, err := c.gh.PullRequests.Get(context.Background(), owner, repo, number)
	if err != nil {
		return
	}
	err = c.preparationStore().change(preparationKey(owner, repo, pr.GetBase().GetRef()), func(st *basePreparation) error {
		if st.Attempts[number].Head == head {
			delete(st.Attempts, number)
		}
		return nil
	})
	if err != nil {
		c.warn("automerge base preparation persistence failed", "repo", owner+"/"+repo, "pr", number, "error", err)
	}
}

func preparationKeepsOwner(reason string, err error) bool {
	if err != nil || reason == "" || strings.HasPrefix(reason, basePreparationPrefix) ||
		stageengine.ClassifyCIReason(reason) == stageengine.CIReasonPending ||
		reason == autoMergeReasonReviewGatePrefix+ReviewGateOutcomeAwaiting ||
		reason == autoMergeReasonReviewGatePrefix+ReviewGateOutcomeNoBundle ||
		reason == contributorLaneReasonNoVerdict || reason == contributorLaneReasonSoaking ||
		reason == "head-changed-since-eval" || reason == autoMergeReasonBehindBudgetExhausted {
		return true
	}
	// Another lane declining authorship is not the owning lane withdrawing.
	return reason == "not-app-authored" || reason == trustedAuthorReasonAppAuthored || reason == contributorLaneReasonAppAuthored
}

func (c *Engine) observeBasePreparation(lane, owner, repo string, pr *gh.PullRequest, reason string, merged bool, evalErr error) {
	if pr == nil || !c.requireCurrentBaseFor(owner+"/"+repo) {
		return
	}
	base := pr.GetBase().GetRef()
	if base == "" {
		return
	}
	if err := c.preparationStore().change(preparationKey(owner, repo, base), func(st *basePreparation) error {
		keep := !merged && preparationKeepsOwner(reason, evalErr)
		if reason == basePreparationPrefix+"snapshot-changed" && st.Attempts[pr.GetNumber()].Head == "" {
			// A push won the race before we issued any refresh. Pending
			// work on that unreviewed head is not our preparation.
			keep = false
		}
		// Waiting is not preparation yet: a changed or now-pending waiting
		// head loses its admission ticket and must qualify again.
		for i := range st.Waiting {
			if st.Waiting[i].Yielded && st.Waiting[i].Number == pr.GetNumber() && keep {
				st.Waiting[i].Head = pr.GetHead().GetSHA()
			}
		}
		st.Waiting = slices.DeleteFunc(st.Waiting, func(w preparationCandidate) bool {
			if w.Yielded && keep && !merged {
				return false
			}
			return w.Lane == lane && w.Number == pr.GetNumber() && (w.Head != pr.GetHead().GetSHA() || !strings.HasPrefix(reason, basePreparationPrefix))
		})
		if !keep && st.Owner.Lane == lane && st.Owner.Number == pr.GetNumber() {
			if merged {
				reason = "merged"
				delete(st.Attempts, pr.GetNumber())
			}
			c.info("automerge base preparation released", "repo", owner+"/"+repo, "base", base, "pr", pr.GetNumber(), "reason", reason)
			st.Owner = preparationCandidate{}
		}
		return nil
	}); err != nil {
		c.warn("automerge base preparation persistence failed", "repo", owner+"/"+repo, "pr", pr.GetNumber(), "error", err)
	}
}

func (c *Engine) preparingBase(owner, repo, base string, number int) bool {
	if !c.requireCurrentBaseFor(owner + "/" + repo) {
		return false
	}
	preparing := false
	err := c.preparationStore().change(preparationKey(owner, repo, base), func(st *basePreparation) error {
		preparing = st.Owner.Number == number
		return nil
	})
	// A failed ownership read must not let a potentially preparing PR become
	// a misleading ready train member. The merge gate still fails closed.
	return preparing || err != nil
}

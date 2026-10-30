package automerge

import (
	"strings"
	"time"
)

// BasePreparationMember is a copied persisted ticket, not live merge readiness.
// HeadSHA is the admitted head; a refresh may have changed the live head.
type BasePreparationMember struct {
	Number    int
	HeadSHA   string
	Since     time.Time
	ExpiresAt time.Time
}

// BasePreparationSnapshot is authoritative persisted lifecycle evidence.
// Consumers must distinguish stale/mismatched tickets from live eligibility.
type BasePreparationSnapshot struct {
	Enabled    bool
	Present    bool
	Repo       string
	BaseRef    string
	Owner      *BasePreparationMember
	Waiting    []BasePreparationMember
	ObservedAt time.Time
}

// BasePreparationSnapshot never mutates/adopts/expires state or calls GitHub.
// An error is unavailable state, not evidence that the queue is empty.
func (c *Engine) BasePreparationSnapshot(repo, base string) (BasePreparationSnapshot, error) {
	out := BasePreparationSnapshot{Repo: repo, BaseRef: base, ObservedAt: c.now()}
	if !c.requireCurrentBaseFor(repo) {
		return out, nil
	}
	out.Enabled = true
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return out, nil
	}
	snapshot, err := c.preparationStore().ownerSnapshot(preparationKey(owner, name, base))
	if err != nil {
		return out, err
	}
	out.Present = snapshot.Present
	member := func(candidate preparationCandidate) BasePreparationMember {
		return BasePreparationMember{Number: candidate.Number, HeadSHA: candidate.Head,
			Since: candidate.Since, ExpiresAt: candidate.Since.Add(c.preparationWindow())}
	}
	if snapshot.Owner.Number != 0 {
		current := member(snapshot.Owner)
		out.Owner = &current
	}
	for _, waiting := range snapshot.Waiters {
		out.Waiting = append(out.Waiting, member(waiting))
	}
	return out, nil
}

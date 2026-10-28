package review

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Review heads (hivecommons/hive#13542): with split perspectives, a base
// reviewer's agents.<name>.review_heads each review every perspective, so two
// or more models judge the same perspective independently. This file holds
// the planner side: choosing the head reviewers, working out which
// (perspective, head) pairs a PR head still needs, and planning one
// record-only kick per pair.

// dispatchReviewers splits the review-capable agents into the heads this
// cycle dispatches to and the plain reviewers that follow the ordinary path.
// Agents derived from review_heads only review with split perspectives
// (combined_perspectives off); there the bases that declare them, and those
// bases' plain replicas, are left for the decision step and every other
// reviewer stays on the ordinary path, so a lane without heads is never
// silently dropped (config.validateReviewHeads rejects such a mix, since
// head dispatch leaves it nothing to review). Under combined perspectives
// heads are dropped and every other reviewer works exactly as before.
func dispatchReviewers(reviewers []AgentCapability, combined bool) (heads, others []AgentCapability) {
	declaring := map[string]bool{}
	for _, r := range reviewers {
		if r.HasReviewHeads {
			declaring[r.Name] = true
		}
		if r.ReviewHead != "" && !combined {
			heads = append(heads, r)
		}
	}
	for _, r := range reviewers {
		switch {
		case r.ReviewHead != "":
			continue
		case len(heads) > 0 && (r.HasReviewHeads || declaring[baseOf(r)]):
			// A head-declaring base and its plain replicas review only
			// through their heads.
			continue
		}
		others = append(others, r)
	}
	return heads, others
}

// headGroups groups head reviewers by head key, returning the keys in order.
func headGroups(reviewers []AgentCapability) ([]string, map[string][]AgentCapability) {
	groups := map[string][]AgentCapability{}
	for _, r := range reviewers {
		if r.ReviewHead == "" {
			continue
		}
		groups[r.ReviewHead] = append(groups[r.ReviewHead], r)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, groups
}

// headPerspective is one (perspective, head) review.
type headPerspective struct {
	Perspective Perspective
	Head        string
}

// pendingMissingHeadPerspectives lists the (perspective, head) pairs this PR
// head still needs: not pending for that head and not already reported by it.
// A pending entry or reported perspective that carries no head (asked before
// review_heads were configured) covers the perspective for every head, so
// turning heads on never re-reviews a perspective already asked.
func pendingMissingHeadPerspectives(state DispatchState, pr PullRequest, perspectives []Perspective, heads []string, delivered map[Perspective]Verdict, deliveredHeads map[Perspective][]string) []headPerspective {
	covered := map[headPerspective]bool{}
	coveredAll := map[Perspective]bool{}
	// A head that ran out of retries while another head answered is settled
	// for the decision step and is not re-sent.
	for _, m := range state.MissingHeads {
		if samePRHead(m.Repo, m.Number, m.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			covered[headPerspective{m.Perspective, m.Head}] = true
		}
	}
	for _, p := range state.Pending {
		if !samePRHead(p.Repo, p.Number, p.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			continue
		}
		if p.Head == "" {
			coveredAll[p.Perspective] = true
			continue
		}
		covered[headPerspective{p.Perspective, p.Head}] = true
	}
	for p := range delivered {
		hs := deliveredHeads[p]
		if len(hs) == 0 {
			coveredAll[p] = true
			continue
		}
		for _, h := range hs {
			covered[headPerspective{p, h}] = true
		}
	}
	if len(perspectives) == 0 {
		perspectives = DefaultPerspectives
	}
	var missing []headPerspective
	for _, p := range perspectives {
		if coveredAll[p] {
			continue
		}
		for _, h := range heads {
			if !covered[headPerspective{p, h}] {
				missing = append(missing, headPerspective{p, h})
			}
		}
	}
	return missing
}

// dispatchHeads plans one record-only kick per missing (perspective, head)
// for this PR, each to a replica of that head with no review kick in flight
// (freeReviewer; a busy head's perspectives wait for a later cycle), and returns the
// parallel slots left. Each kick costs one slot. max_perspectives_per_pr still
// bounds how many perspectives a PR head receives; an admitted perspective is
// asked of every head so it is judged by all of them.
//
// Heads run with their own backend and model, so no review_models pool or
// perspective_models override applies: the head IS the model.
func (p *DispatchPlan) dispatchHeads(pr PullRequest, reviewers []AgentCapability, opts DispatchOptions, delivered map[Perspective]Verdict, deliveredHeads map[Perspective][]string, revisiting bool, delta *DeltaContext, availableSlots int, now time.Time) int {
	keys, groups := headGroups(reviewers)
	if len(keys) == 0 {
		return availableSlots
	}
	missing := pendingMissingHeadPerspectives(p.State, pr, opts.Perspectives.List(), keys, delivered, deliveredHeads)
	if perPR := opts.effectiveMaxPerspectivesPerPR(); perPR > 0 {
		admitted := map[Perspective]bool{}
		for _, e := range p.State.Pending {
			if samePRHead(e.Repo, e.Number, e.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
				admitted[e.Perspective] = true
			}
		}
		for persp := range delivered {
			admitted[persp] = true
		}
		var kept []headPerspective
		for _, m := range missing {
			if !admitted[m.Perspective] {
				if len(admitted) >= perPR {
					continue
				}
				admitted[m.Perspective] = true
			}
			kept = append(kept, m)
		}
		missing = kept
	}
	for _, m := range missing {
		if availableSlots <= 0 {
			break
		}
		reviewer, free := freeReviewer(groups[m.Head], p.inFlight(opts, now))
		if !free {
			// Every replica of this head is mid-review; a later cycle asks
			// the first one to free up. Other heads may still have room.
			continue
		}
		msg := BuildPerspectivePromptWith(m.Perspective, pr, PromptOptions{
			HeadRecordOnly:       m.Head,
			Revise:               revisiting,
			Perspectives:         opts.Perspectives,
			ProposeFixesOnly:     !fixPushAllowed(pr, opts),
			WritingGuideSection:  opts.WritingGuideSection,
			ReviewBotLogins:      opts.ReviewBotLogins,
			ReviewBotMinPriority: opts.ReviewBotMinPriority,
			ReviewBackend:        reviewer.Backend,
			ReviewModel:          reviewer.Model,
			Delta:                delta,
		})
		p.ReviewKicks = append(p.ReviewKicks, DispatchKick{Agent: reviewer.Name, Message: msg, PRRef: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), Kind: "review", Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, BodyDigest: pr.BodyDigest, Perspective: m.Perspective, Head: m.Head, AuthorAgent: pr.AuthorAgent, AuthorModel: pr.AuthorModel, ReviewBackend: reviewer.Backend, ReviewModel: reviewer.Model, DeltaPreviousHead: deltaPreviousHead(delta)})
		p.State.Pending = append(p.State.Pending, PendingReview{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, BodyDigest: pr.BodyDigest, Perspective: m.Perspective, Agent: reviewer.Name, Head: m.Head, AuthorAgent: pr.AuthorAgent, AuthorModel: pr.AuthorModel, ReviewModel: strings.TrimSpace(reviewer.Model), DeltaPreviousHead: deltaPreviousHead(delta), Dispatched: now})
		availableSlots--
	}
	return availableSlots
}

// removePendingForPerspectivesAndHeads is removePendingForPerspectives for a
// head-aware aggregate: a head's pending entry is dropped only once that head
// has reported the perspective, so the other heads' entries keep their TTL.
// Entries without a head, or an aggregate without head data, behave exactly
// as removePendingForPerspectives.
func removePendingForPerspectivesAndHeads(pending []PendingReview, pr PullRequest, reported map[Perspective]Verdict, heads map[Perspective][]string) []PendingReview {
	if len(heads) == 0 {
		return removePendingForPerspectives(pending, pr, reported)
	}
	out := pending[:0]
	for _, p := range pending {
		_, done := reported[p.Perspective]
		if _, hasHeads := heads[p.Perspective]; hasHeads {
			done = true
		}
		if done && samePRHead(p.Repo, p.Number, p.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			// The decision dispatch stays pending until its decided report
			// settles the PR head: the heads' reports for a perspective do not
			// answer it.
			if p.Decision {
				out = append(out, p)
				continue
			}
			if p.Head == "" || containsHead(heads[p.Perspective], p.Head) {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

func containsHead(items []string, want string) bool {
	for _, s := range items {
		if s == want {
			return true
		}
	}
	return false
}

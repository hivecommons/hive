package review

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Planner side of the review-heads decision step (hivecommons/hive#13542,
// part 2): per-head retries, the decision kick, and the skip rule's
// acknowledgement.

// HeadRetryCount counts how many times one head's review of one perspective
// of a PR head expired without a verdict.
type HeadRetryCount struct {
	Repo        string      `json:"repo"`
	Number      int         `json:"number"`
	HeadSHA     string      `json:"head_sha,omitempty"`
	Perspective Perspective `json:"perspective"`
	Head        string      `json:"head"`
	Count       int         `json:"count"`
	LastExpired time.Time   `json:"last_expired_at"`
}

// MissingHead is a head that never answered a perspective after
// review.head_max_retries while another head did. The decision step
// proceeds without it and names it.
type MissingHead struct {
	Repo        string      `json:"repo"`
	Number      int         `json:"number"`
	HeadSHA     string      `json:"head_sha,omitempty"`
	Perspective Perspective `json:"perspective"`
	Head        string      `json:"head"`
	Attempts    int         `json:"attempts"`
	At          time.Time   `json:"at"`
}

// AcknowledgedHead records that a PR head's skip-rule acknowledgement was
// planned, so it is posted once.
type AcknowledgedHead struct {
	Repo    string    `json:"repo"`
	Number  int       `json:"number"`
	HeadSHA string    `json:"head_sha,omitempty"`
	At      time.Time `json:"at"`
}

// ReviewAcknowledgement is one no-findings comment for the caller to post.
type ReviewAcknowledgement struct {
	Repo    string
	Number  int
	HeadSHA string
	Body    string
}

// MissingHeadsFromState turns the dispatch state's settled-missing heads into
// AggregateOptions.MissingHeads.
func MissingHeadsFromState(state DispatchState) map[string][]string {
	if len(state.MissingHeads) == 0 {
		return nil
	}
	out := map[string][]string{}
	for _, m := range state.MissingHeads {
		key := reviewKey(m.Repo, m.Number, m.HeadSHA, "")
		out[key] = appendUnique(out[key], missingHeadPair(m.Perspective, m.Head))
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func pruneReviewHeadState(state DispatchState, openHeads map[string]bool) DispatchState {
	var retries []HeadRetryCount
	for _, r := range state.HeadRetries {
		if openHeads[reviewKey(r.Repo, r.Number, r.HeadSHA, "")] {
			retries = append(retries, r)
		}
	}
	state.HeadRetries = retries
	var missing []MissingHead
	for _, m := range state.MissingHeads {
		if openHeads[reviewKey(m.Repo, m.Number, m.HeadSHA, "")] {
			missing = append(missing, m)
		}
	}
	state.MissingHeads = missing
	var acked []AcknowledgedHead
	for _, a := range state.Acknowledged {
		if openHeads[reviewKey(a.Repo, a.Number, a.HeadSHA, "")] {
			acked = append(acked, a)
		}
	}
	state.Acknowledged = acked
	return state
}

func (o DispatchOptions) effectiveHeadMaxRetries() int {
	if o.HeadMaxRetries > 0 {
		return o.HeadMaxRetries
	}
	return o.effectiveMaxRedispatch()
}

// expireHeadPending applies review.head_max_retries to this PR head's head
// dispatches that outlived PendingTTL. An expired head review is dropped and
// re-sent while its head is within the limit. Past the limit:
//
//   - if another head already answered the perspective, the head is recorded
//     as settled-missing and the decision step proceeds without it;
//   - if no head answered it, the entry is left for expirePending, so the PR
//     head's max_redispatch budget and human hold apply exactly as before.
//
// Decision dispatches and entries without a head are left to expirePending.
func (p *DispatchPlan) expireHeadPending(pr PullRequest, opts DispatchOptions, deliveredHeads map[Perspective][]string, now time.Time) {
	ttl := opts.effectivePendingTTL()
	if ttl <= 0 || p.redispatchExhausted(pr) {
		return
	}
	limit := opts.effectiveHeadMaxRetries()
	kept := make([]PendingReview, 0, len(p.State.Pending))
	for _, e := range p.State.Pending {
		if !samePRHead(e.Repo, e.Number, e.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) || strings.TrimSpace(e.Head) == "" || e.Decision || e.Dispatched.IsZero() {
			kept = append(kept, e)
			continue
		}
		age := now.Sub(e.Dispatched)
		if age < ttl {
			kept = append(kept, e)
			continue
		}
		count := p.bumpHeadRetry(pr, e.Perspective, e.Head, now)
		p.Expired = append(p.Expired, ExpiredPending{Repo: e.Repo, Number: e.Number, HeadSHA: e.HeadSHA, Perspective: e.Perspective, Agent: e.Agent, Dispatched: e.Dispatched, Age: age, Count: count})
		if count <= limit {
			continue
		}
		if answeredByOtherHead(deliveredHeads[e.Perspective], e.Head) {
			p.State.MissingHeads = upsertMissingHead(p.State.MissingHeads, MissingHead{Repo: e.Repo, Number: e.Number, HeadSHA: e.HeadSHA, Perspective: e.Perspective, Head: strings.TrimSpace(e.Head), Attempts: count, At: now})
			continue
		}
		kept = append(kept, e)
	}
	p.State.Pending = kept
}

func (p *DispatchPlan) bumpHeadRetry(pr PullRequest, perspective Perspective, head string, now time.Time) int {
	head = strings.TrimSpace(head)
	for i := range p.State.HeadRetries {
		r := &p.State.HeadRetries[i]
		if samePRHead(r.Repo, r.Number, r.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) && r.Perspective == perspective && r.Head == head {
			r.Count++
			r.LastExpired = now
			return r.Count
		}
	}
	p.State.HeadRetries = append(p.State.HeadRetries, HeadRetryCount{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: perspective, Head: head, Count: 1, LastExpired: now})
	return 1
}

func answeredByOtherHead(answered []string, head string) bool {
	for _, h := range answered {
		if strings.TrimSpace(h) != strings.TrimSpace(head) {
			return true
		}
	}
	return false
}

func upsertMissingHead(items []MissingHead, item MissingHead) []MissingHead {
	for i := range items {
		m := items[i]
		if samePRHead(m.Repo, m.Number, m.HeadSHA, item.Repo, item.Number, item.HeadSHA) && m.Perspective == item.Perspective && m.Head == item.Head {
			items[i] = item
			return items
		}
	}
	return append(items, item)
}

// decisionAgents are the bases that declare review_heads: the agents that
// run the decision step.
func decisionAgents(capable []AgentCapability) []AgentCapability {
	var out []AgentCapability
	for _, a := range capable {
		if a.HasReviewHeads && a.ReviewHead == "" {
			out = append(out, a)
		}
	}
	return out
}

// dispatchDecision plans the one decision kick for a PR head whose heads have
// all settled (agg.AwaitingDecision), unless one is already pending. It goes
// to the least-loaded base reviewer and covers every perspective the heads
// judged; one pending entry per perspective binds the decided reports the
// relay records. It costs one parallel slot.
func (p *DispatchPlan) dispatchDecision(pr PullRequest, agg Aggregate, bases []AgentCapability, opts DispatchOptions, revisiting bool, availableSlots int, now time.Time) int {
	if availableSlots <= 0 || len(bases) == 0 || !agg.AwaitingDecision {
		return availableSlots
	}
	for _, e := range p.State.Pending {
		if e.Decision && samePRHead(e.Repo, e.Number, e.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			return availableSlots
		}
	}
	perspectives := decisionPerspectives(agg, opts.Perspectives)
	if len(perspectives) == 0 {
		return availableSlots
	}
	base, free := freeReviewer(bases, p.inFlight(opts, now))
	if !free {
		return availableSlots
	}
	msg := BuildDecisionPrompt(pr, perspectives, agg, PromptOptions{
		PostComments:          opts.PostComments,
		AcknowledgeNoFindings: opts.AcknowledgeNoFindings,
		Revise:                revisiting,
		Perspectives:          opts.Perspectives,
		ProposeFixesOnly:      !fixPushAllowed(pr, opts),
		WritingGuideSection:   opts.WritingGuideSection,
	})
	p.ReviewKicks = append(p.ReviewKicks, DispatchKick{Agent: base.Name, Message: msg, PRRef: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), Kind: "review", Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, BodyDigest: pr.BodyDigest, Perspective: perspectives[0], Perspectives: perspectives, Decision: true, AuthorAgent: pr.AuthorAgent, HiveAuthored: hiveAuthored(pr, opts), AuthorModel: pr.AuthorModel, ReviewBackend: base.Backend, ReviewModel: base.Model, Priority: pr.Priority})
	for _, persp := range perspectives {
		p.State.Pending = append(p.State.Pending, PendingReview{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, BodyDigest: pr.BodyDigest, Perspective: persp, Agent: base.Name, Decision: true, AuthorAgent: pr.AuthorAgent, HiveAuthored: hiveAuthored(pr, opts), AuthorModel: pr.AuthorModel, ReviewModel: strings.TrimSpace(base.Model), Dispatched: now})
	}
	return availableSlots - 1
}

// decisionPerspectives are the perspectives the heads judged, in the hive's
// configured order.
func decisionPerspectives(agg Aggregate, set PerspectiveSet) []Perspective {
	judged := map[Perspective]bool{}
	for _, r := range agg.HeadReports {
		judged[r.Perspective] = true
	}
	var out []Perspective
	for _, p := range set.List() {
		if judged[p] {
			out = append(out, p)
			delete(judged, p)
		}
	}
	return out
}

// acknowledgeSkippedDecision plans the one-line acknowledgement for a PR head
// the skip rule settled: every head approved every perspective with no
// findings, so no decision session ran and no comment was posted. It follows
// review.acknowledge_no_findings, and is planned once per PR head.
func (p *DispatchPlan) acknowledgeSkippedDecision(pr PullRequest, agg Aggregate, opts DispatchOptions, now time.Time) {
	if !agg.DecisionSkipped || agg.Verdict != VerdictApprove || !opts.AcknowledgeNoFindings || !opts.PostComments {
		return
	}
	for _, a := range p.State.Acknowledged {
		if samePRHead(a.Repo, a.Number, a.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			return
		}
	}
	p.State.Acknowledged = append(p.State.Acknowledged, AcknowledgedHead{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, At: now})
	p.Acknowledgements = append(p.Acknowledgements, ReviewAcknowledgement{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Body: SkippedDecisionAcknowledgement(agg)})
}

// SkippedDecisionAcknowledgement is the comment for a PR head every review
// head approved with no findings: the same one line a clean combined review
// posts, naming the heads that agreed.
func SkippedDecisionAcknowledgement(agg Aggregate) string {
	perspectives := make([]string, 0, len(agg.Perspectives))
	for p := range agg.Perspectives {
		perspectives = append(perspectives, string(p))
	}
	sort.Strings(perspectives)
	heads := map[string]bool{}
	for _, r := range agg.HeadReports {
		heads[r.Head] = true
	}
	names := make([]string, 0, len(heads))
	for h := range heads {
		names = append(names, h)
	}
	sort.Strings(names)
	line := fmt.Sprintf("**Reviewed** — no findings from %s", strings.Join(perspectives, ", "))
	if len(names) > 0 {
		line += fmt.Sprintf(" (all %d review heads agreed: %s)", len(names), strings.Join(names, ", "))
	}
	return line + "."
}

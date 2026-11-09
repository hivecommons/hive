package review

import (
	"fmt"
	"strings"
	"time"
)

// AgentGitHubRefusal answers whether the hive's GitHub proxy refused one of
// agent's requests at or after since, because the agent reached
// agents.github_api_hourly_cap or the shared bucket fell below
// github.agent_reserve_floor, and when the last refusal said the agent could
// read again (its Retry-After). crux/proxy's GitHubProxy.AgentGitHubRefusal
// is the live answer; nil means the planner has no proxy record.
type AgentGitHubRefusal func(agent string, since time.Time) (clears time.Time, refused bool)

const (
	// DefaultMaxCapRedispatch is how many expiries the agent GitHub API cap
	// may cause on one head before the planner stops asking and holds the PR
	// for a human with HoldReasonDispatchExhausted. Each one lasts at least
	// review.pending_ttl and waits for the cap to clear, so eight is about a
	// working day of a reviewer that cannot read: generous for a busy hour
	// or two, still finite for a reviewer whose cap is set too low to ever
	// finish a review.
	DefaultMaxCapRedispatch = 8
	// agentCapWaitLimit bounds how long past pending_ttl an expiry the cap
	// caused waits for the refusal to clear. It is the proxy's cap window:
	// the sliding hour frees a slot within it, and the shared bucket resets
	// within it, so a refusal that says later than that is not waited for.
	agentCapWaitLimit = time.Hour
)

func (o DispatchOptions) effectiveMaxCapRedispatch() int {
	if o.MaxCapRedispatch <= 0 {
		return DefaultMaxCapRedispatch
	}
	return o.MaxCapRedispatch
}

// capCausedExpiry reports whether the agent GitHub API cap caused this
// expiry, and if so whether to wait before asking again.
//
// The cap caused it when the proxy refused the agent of every expired row at
// or after that row was sent: the session that should have answered could
// not read (projectbluefin/utah-packages#959). The relay sets aside the
// requires_human such a session writes (capStoppedReview in pkg/github), so
// the row expires instead; a session the cap stopped before it posted at all
// expires the same way. One expired row whose agent was not refused makes it
// an ordinary expiry, which spends max_redispatch as before.
//
// It waits while the refusal has not cleared (the sliding hour has no free
// slot yet, or the bucket has not reset), up to agentCapWaitLimit past the
// TTL: asking again sooner would only be refused again. The rows stay in
// place meanwhile, so the perspective is not asked twice.
func capCausedExpiry(expired []ExpiredPending, opts DispatchOptions, ttl time.Duration, now time.Time) (capCaused, wait bool) {
	refusal := opts.AgentGitHubRefusal
	if refusal == nil || len(expired) == 0 {
		return false, false
	}
	var clears, earliest time.Time
	for _, e := range expired {
		agent := strings.TrimSpace(e.Agent)
		if agent == "" {
			return false, false
		}
		at, refused := refusal(agent, e.Dispatched)
		if !refused {
			return false, false
		}
		if at.After(clears) {
			clears = at
		}
		if earliest.IsZero() || e.Dispatched.Before(earliest) {
			earliest = e.Dispatched
		}
	}
	waitUntil := clears
	if limit := earliest.Add(ttl + agentCapWaitLimit); waitUntil.After(limit) {
		waitUntil = limit
	}
	return true, now.Before(waitUntil)
}

// expireCapCaused is expirePending for an expiry the agent cap caused: the
// rows are dropped and asked again like any expiry, but the count goes to
// CapRedispatches, not Redispatches. Before, each such expiry spent one of
// review.max_redispatch, so a reviewer capped for about 2h15m on one head
// (three 45m TTLs) ended in a review-dispatch-exhausted hold and a person was
// asked about a PR nobody had read. Once the cap count reaches
// MaxCapRedispatch the head is held the same way, so a reviewer that can
// never read still reaches a person instead of looping.
func (p *DispatchPlan) expireCapCaused(pr PullRequest, expired []ExpiredPending, opts DispatchOptions, ttl time.Duration, now time.Time) bool {
	idx := redispatchIndex(p.State.CapRedispatches, pr)
	if idx < 0 {
		p.State.CapRedispatches = append(p.State.CapRedispatches, RedispatchCount{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA})
		idx = len(p.State.CapRedispatches) - 1
	}
	p.State.CapRedispatches[idx].Count++
	p.State.CapRedispatches[idx].LastExpired = now
	count := p.State.CapRedispatches[idx].Count
	for i := range expired {
		expired[i].Count = count
		expired[i].CapCaused = true
	}
	p.Expired = append(p.Expired, expired...)
	if count >= opts.effectiveMaxCapRedispatch() {
		hold := HumanReviewHold{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA,
			Reason:    fmt.Sprintf("%s: %d review dispatches for this head ended at the agent GitHub API cap without a verdict", HoldReasonDispatchExhausted, count),
			UpdatedAt: now}
		p.addHuman(pr, hold.Reason, now)
		p.Exhausted = append(p.Exhausted, hold)
		return false
	}
	p.dropExpiredPending(pr, ttl, now)
	return true
}

// CapRedispatchCountFor is the number of this head's expiries the agent
// GitHub API cap caused.
func (s DispatchState) CapRedispatchCountFor(repo string, number int, headSHA string) int {
	for _, r := range s.CapRedispatches {
		if samePRHead(r.Repo, r.Number, r.HeadSHA, repo, number, headSHA) {
			return r.Count
		}
	}
	return 0
}

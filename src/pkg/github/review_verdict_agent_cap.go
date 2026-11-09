package github

import (
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

// agentCapSessionLookback bounds how far back a refusal counts against a
// review when no dispatch row says when the session was asked. It is the
// proxy's cap window: a refusal older than that came from a window the
// session cannot have shared.
const agentCapSessionLookback = time.Hour

var (
	agentGitHubRefusalMu sync.RWMutex
	agentGitHubRefusalFn review.AgentGitHubRefusal
)

// SetAgentGitHubRefusalFunc installs (or, with nil, removes) the
// process-wide answer to "did the hive's GitHub proxy refuse this agent a
// request at or after since, and when did it say the agent could read
// again?", for refusals because the agent reached
// agents.github_api_hourly_cap or the shared bucket fell below
// github.agent_reserve_floor. The proxy is one per process, built after the
// GitHub client, so main.go installs its AgentGitHubRefusal once the proxy
// exists. Unset, the review relay sets nothing aside on that ground and the
// planner counts every expiry against review.max_redispatch.
func SetAgentGitHubRefusalFunc(fn review.AgentGitHubRefusal) {
	agentGitHubRefusalMu.Lock()
	agentGitHubRefusalFn = fn
	agentGitHubRefusalMu.Unlock()
}

// AgentGitHubRefusalFunc is the installed proxy record, or nil. The review
// planner passes it as review.DispatchOptions.AgentGitHubRefusal, so an
// expiry the cap caused does not spend review.max_redispatch.
func AgentGitHubRefusalFunc() review.AgentGitHubRefusal {
	agentGitHubRefusalMu.RLock()
	defer agentGitHubRefusalMu.RUnlock()
	return agentGitHubRefusalFn
}

// capStoppedReview reports whether report is a requires_human from a review
// session the proxy cut short: the posting agent was refused a GitHub read
// after this perspective was dispatched (review.CapStoppedReviewReason has
// which reports qualify). It returns the reviewer's summary as the reason.
//
// The test is the proxy's own record of the refusal, not the reviewer's
// wording: projectbluefin/utah-packages#959, utah#1161, finpilot#659 and
// fsdk-containers#491 all parked at 085 on 2026-11-09 with summaries
// review.UnreadableReviewReason did not recognise.
//
// It errs toward asking again. A reviewer lane runs one session at a time,
// so a refusal between the dispatch and the verdict may have hit the
// session before this one; the cost of that is one more review, never a
// verdict nobody read for.
func (c *Client) capStoppedReview(req ReviewRequest, report review.PerspectiveReport, now time.Time) (string, bool) {
	refusal := AgentGitHubRefusalFunc()
	if refusal == nil {
		return "", false
	}
	agent := strings.TrimSpace(req.Agent)
	if agent == "" {
		return "", false
	}
	reason, ok := review.CapStoppedReviewReason(report)
	if !ok {
		return "", false
	}
	since := reviewPerspectiveDispatchedAt(report)
	if since.IsZero() || since.After(now) {
		since = now.Add(-agentCapSessionLookback)
	}
	if _, refused := refusal(agent, since); !refused {
		return "", false
	}
	return reason, true
}

// reviewPerspectiveDispatchedAt is when the open dispatch row for report's
// PR and perspective was sent: the start of the session that answered it.
// With several rows (an older head still open) the latest wins. It is zero
// when the state cannot be read or holds no such row.
func reviewPerspectiveDispatchedAt(report review.PerspectiveReport) time.Time {
	state, err := review.LoadDispatchState("")
	if err != nil {
		return time.Time{}
	}
	var latest time.Time
	for _, p := range state.Pending {
		if p.Number != report.Number || p.Perspective != report.Perspective ||
			!strings.EqualFold(strings.TrimSpace(p.Repo), strings.TrimSpace(report.Repo)) {
			continue
		}
		if p.Dispatched.After(latest) {
			latest = p.Dispatched
		}
	}
	return latest
}

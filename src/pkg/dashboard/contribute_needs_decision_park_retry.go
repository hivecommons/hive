package dashboard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// The park question is posted once, seconds after the relay parks the issue.
// When GitHub refused that one attempt for quota, the hub logged a warning
// and the question was gone: the issue sat at 045 with the relay's label and
// nothing a maintainer could answer. A rate limit lifts by itself, at a time
// GitHub names, so the hub now tries again after it.
//
// The retry is bounded: at most needsDecisionParkMaxRetries more attempts
// per question, each after the wait GitHub named (never more than an hour,
// ghpkg.RateLimitRetryDelay), and at most needsDecisionParkMaxPending
// questions waiting at once. When the attempts run out the hub logs one
// warning saying the question was not posted, which is where every
// rate-limited question ended before. An error that is not a rate limit is
// not retried, as before.
//
// The wait is in memory only. A hub that restarts while a question waits
// does not post it.

const (
	// needsDecisionParkMaxRetries is how many more attempts a rate-limited
	// park question gets after the first.
	needsDecisionParkMaxRetries = 3
	// needsDecisionParkRetryFallback is the wait when GitHub refused for
	// quota without naming a time, or the hive's own pacer shed the request:
	// GitHub's rule for that case is to wait at least a minute.
	needsDecisionParkRetryFallback = time.Minute
	// needsDecisionParkRetryMargin is added to the wait GitHub named, so the
	// retry runs after the limit's reset rather than on it.
	needsDecisionParkRetryMargin = 5 * time.Second
	// needsDecisionParkMaxPending bounds the park questions waiting for a
	// retry at one time; one more is not kept and is reported not posted.
	needsDecisionParkMaxPending = 64
)

// needsDecisionParkQuestion is one park question and where it goes.
type needsDecisionParkQuestion struct {
	repo     string
	number   int
	reporter string
	since    time.Time
	body     string
}

// needsDecisionParkRetryState is the retry's test seams and its count of
// waiting questions. The zero value is ready to use.
type needsDecisionParkRetryState struct {
	// after replaces time.AfterFunc and now replaces time.Now; test hooks.
	after func(time.Duration, func())
	now   func() time.Time
	// stillParked replaces the GitHub read a retry makes before it posts;
	// a test hook.
	stillParked func(ctx context.Context, repo string, number int) (bool, error)
	// pending counts the questions waiting for a retry.
	pending atomic.Int32
}

func (st *needsDecisionParkRetryState) clock() time.Time {
	if st.now != nil {
		return st.now()
	}
	return time.Now()
}

func (st *needsDecisionParkRetryState) schedule(d time.Duration, f func()) {
	if st.after != nil {
		st.after(d, f)
		return
	}
	time.AfterFunc(d, f)
}

// needsDecisionParkRetryDelay reports whether err is a refusal that lifts by
// itself, and how long to wait before trying again: a GitHub primary or
// secondary rate limit or a bare 429 (until the reset or Retry-After GitHub
// named), or a request the hive's own pacer shed, which it does only while a
// rate-limit caution window is engaged (ghpkg.ErrPacingShed). The delegated
// decision preview groups the same errors (delegatedPlanRateLimited).
func needsDecisionParkRetryDelay(err error, now time.Time) (time.Duration, bool) {
	delay, limited := ghpkg.RateLimitRetryDelay(err, now)
	if !limited && !errors.Is(err, ghpkg.ErrPacingShed) {
		return 0, false
	}
	if delay <= 0 {
		delay = needsDecisionParkRetryFallback
	}
	return delay + needsDecisionParkRetryMargin, true
}

// needsDecisionParkNotPosted handles a failed attempt to post q, the
// retries-th retry (0 for the first attempt). A rate limit schedules the
// next attempt while any are left; everything else, and the last attempt,
// costs one warning and stops.
func (h *ContributeWSHub) needsDecisionParkNotPosted(q needsDecisionParkQuestion, err error, retries int) {
	st := &h.needsDecisionParkRetry
	delay, limited := needsDecisionParkRetryDelay(err, st.clock())
	if !limited {
		h.logger.Warn("[contribute-ws] needs-decision park: could not post the question",
			"repo", q.repo, "number", q.number, "reporter", q.reporter, "error", err)
		return
	}
	if retries >= needsDecisionParkMaxRetries {
		h.logger.Warn("[contribute-ws] needs-decision park: the question was not posted; GitHub rate limits outlasted every retry",
			"repo", q.repo, "number", q.number, "reporter", q.reporter, "attempts", retries+1, "error", err)
		return
	}
	if st.pending.Add(1) > needsDecisionParkMaxPending {
		st.pending.Add(-1)
		h.logger.Warn("[contribute-ws] needs-decision park: the question was not posted; too many park questions are already waiting on a rate limit",
			"repo", q.repo, "number", q.number, "reporter", q.reporter, "waiting", needsDecisionParkMaxPending, "error", err)
		return
	}
	h.logger.Info("[contribute-ws] needs-decision park: GitHub rate limit; the question will be tried again",
		"repo", q.repo, "number", q.number, "reporter", q.reporter,
		"retry_in", delay.String(), "retry", retries+1, "of", needsDecisionParkMaxRetries, "error", err)
	st.schedule(delay, func() {
		st.pending.Add(-1)
		h.retryNeedsDecisionPark(q, retries+1)
	})
}

// retryNeedsDecisionPark is one later attempt at q. The first attempt ran
// seconds after the park; this one runs minutes to hours after it, so it
// first checks that the issue is still open and parked: a question saying
// "parked for a maintainer decision" on an issue a maintainer has since
// un-parked or closed would be wrong. The post itself is the same idempotent
// one: it posts nothing when someone has explained the park meanwhile.
func (h *ContributeWSHub) retryNeedsDecisionPark(q needsDecisionParkQuestion, retries int) {
	if h.stopCh != nil {
		select {
		case <-h.stopCh:
			return // the hub closed while the question waited
		default:
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), needsDecisionParkTimeout)
	defer cancel()
	parked, err := h.needsDecisionParkStillParked(ctx, q.repo, q.number)
	if err == nil && !parked {
		h.logger.Info("[contribute-ws] needs-decision park: question not posted on retry; the issue is no longer open with the park label",
			"repo", q.repo, "number", q.number, "reporter", q.reporter, "retry", retries)
		return
	}
	if err == nil {
		err = h.needsDecisionParkPost(ctx, q.repo, q.number, q.since, q.body)
	}
	if err != nil {
		h.needsDecisionParkNotPosted(q, err, retries)
		return
	}
	h.logger.Info("[contribute-ws] needs-decision park: question settled on retry (posted, or the park was explained meanwhile)",
		"repo", q.repo, "number", q.number, "reporter", q.reporter, "retry", retries)
}

// needsDecisionParkStillParked reports whether repo#number is open and still
// parked for a decision: it carries the label the relay applies
// (hub.contribute_needs_decision_label) or stage/045-needs-decision
// (ghpkg.NeedsDecisionStageLabel), which the stage reconciler puts in place
// of a legacy spelling, so a retry that runs after that migration still
// finds the park. Names are compared ignoring case. On a hive configured to
// apply no label (the setting is empty) an open issue is all that can be
// checked.
func (h *ContributeWSHub) needsDecisionParkStillParked(ctx context.Context, repo string, number int) (bool, error) {
	if hook := h.needsDecisionParkRetry.stillParked; hook != nil {
		return hook(ctx, repo, number)
	}
	if h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return false, ghpkg.ErrNoGitHubClient
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return false, fmt.Errorf("repo %q is not owner/name", repo)
	}
	issue, _, err := h.server.deps.GHClient.GoGitHub().Issues.Get(ctx, owner, name, number)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(issue.GetState(), "open") {
		return false, nil
	}
	label := h.configuredNeedsDecisionLabel()
	if label == "" {
		return true, nil
	}
	for _, l := range issue.Labels {
		name := strings.TrimSpace(l.GetName())
		if strings.EqualFold(name, label) || strings.EqualFold(name, ghpkg.NeedsDecisionStageLabel) {
			return true, nil
		}
	}
	return false, nil
}

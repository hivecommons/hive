package automerge

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// Permanent merge refusals. GitHub answers some merge calls with a 405 that
// the same call can never get past: the base only accepts merges through
// the merge queue, branch protection wants an approving review the App
// cannot give, or the repository has the chosen merge method switched off.
// Each sweep used to return merge-failed and call merge again on every pass
// (projectbluefin fsdk-containers#463, chairlift#627 and utah#892,
// 2026-10-25). They are now classified, remembered, and reported as skip
// reasons: merge-queue-required stays at 100-merge-queue, the other two park
// at 085-needs-decision.

const (
	autoMergeReasonMergeRefusedReview = prstage.AutoMergeReasonMergeRefusedReview
	autoMergeReasonMergeRefusedMethod = prstage.AutoMergeReasonMergeRefusedMethod
)

// mergeRefusalRecheck is how long a remembered review or merge-method
// refusal skips the merge call before the sweep tries again, so a new
// ruleset, repository setting or review is picked up without a restart.
const mergeRefusalRecheck = prstage.MergeRefusalRecheck

// mergeRefusalPruneAt is the remembered-refusal count at which expired
// entries are dropped on the next insert.
const mergeRefusalPruneAt = 1024

// "At least 1 approving review is required by reviewers with write access.",
// "At least 2 approving reviews are required ...", and the ruleset form
// "1 review requesting changes by reviewers with write access.".
var mergeRefusedReviewRE = regexp.MustCompile(`(?i)\d+ approving reviews? (?:is|are) required|\d+ reviews? requesting changes`)

// "Pull Request has merge conflicts": the base and head conflict at the
// time of the call, whatever the PR read before it said.
var mergeConflictRE = regexp.MustCompile(`(?i)pull request has merge conflicts`)

// mergeConflictMergeError reports GitHub's 405 for a merge call on a PR
// that has merge conflicts.
func mergeConflictMergeError(err error) bool {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil || ghErr.Response.StatusCode != http.StatusMethodNotAllowed {
		return false
	}
	return mergeConflictRE.MatchString(ghErr.Message) || mergeConflictRE.MatchString(ghErr.Error())
}

// "Merge commits are not allowed on this repository." and the squash and
// rebase forms.
var mergeRefusedMethodRE = regexp.MustCompile(`(?i)(?:merge commits|squash merges|rebase merges) are not allowed on this repository`)

// mergeRefusalReason classifies a merge error as a review-required or
// method-not-allowed refusal, or returns "". Only a 405 counts; anything
// else stays merge-failed.
func mergeRefusalReason(err error) string {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil || ghErr.Response.StatusCode != http.StatusMethodNotAllowed {
		return ""
	}
	msg := ghErr.Message + "\n" + ghErr.Error()
	switch {
	case mergeRefusedReviewRE.MatchString(msg):
		return autoMergeReasonMergeRefusedReview
	case mergeRefusedMethodRE.MatchString(msg):
		return autoMergeReasonMergeRefusedMethod
	}
	return ""
}

// unresolvedConversationsMergeError reports GitHub's 405 for a merge call on
// a base that requires conversation resolution while a review thread is
// unresolved ("Repository rule violations found ... All comments must be
// resolved.").
func unresolvedConversationsMergeError(err error) bool {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil || ghErr.Response.StatusCode != http.StatusMethodNotAllowed {
		return false
	}
	return hgithub.IsUnresolvedConversationsError(err)
}

// isPermanentMergeRefusal reports whether reason is one of the classified
// 405 refusals above.
func isPermanentMergeRefusal(reason string) bool {
	switch reason {
	case autoMergeReasonMergeQueueRequired, autoMergeReasonMergeRefusedReview, autoMergeReasonMergeRefusedMethod,
		autoMergeReasonUnresolvedReviewThreads:
		return true
	}
	return false
}

// mergeAttempt is what the refusal memory needs to know about one merge
// call: the base it targets, the head it pins, GitHub's mergeable_state for
// the PR when it was read, and the merge method.
type mergeAttempt struct {
	owner, repo, base string
	number            int
	head, state       string
	method            string
	// review is GitHub's reviewDecision for the PR, which only the
	// native-queue path reads (nativeQueueMergeAttempt); a direct merge
	// leaves it empty.
	review string
}

type mergeRefusal struct {
	head  string
	state string
	at    time.Time
	// behind marks a native-queue rejection whose head the lane found
	// behind its base (nativeQueueRejectedHeadBehind); it re-arms on a new
	// head only.
	behind bool
	// base is the base branch tip a native-queue rejection of a head that
	// is not behind was judged against: the tip the lane's compare read on
	// its first pass after the rejection (noteNativeQueueRejectionBase).
	base string
	// repeated marks a failed_checks rejection of a head on a base tip
	// whose one 30-minute retry was already spent; it re-arms at once on a
	// new head or base tip, and otherwise only on the slow backoff
	// (mergeRefusal.recheck). priorBase is the base tip of that spent
	// retry, until the lane's compare reads base; retries counts the slow
	// retries already spent on the head and tip.
	repeated  bool
	priorBase string
	retries   int
	// review is GitHub's reviewDecision when a native-queue enqueue was
	// refused or failed; the mark re-arms when it changes (rearmedBy). A
	// direct merge's refusal leaves it empty.
	review string
	// detail is the error text of a failed native-queue enqueue the lane
	// is waiting out (noteNativeQueueEnqueueFailure). On that mark retries
	// counts the failed enqueues in a row on the head, and refused says
	// GitHub definitely turned the last one down, which lets the wait grow
	// further (nativeQueueEnqueueBackoff).
	detail  string
	refused bool
}

// rearmedBy reports whether a is no longer the attempt r was remembered
// for: the head moved, GitHub's mergeable_state for the PR changed, or its
// reviewDecision did.
func (r mergeRefusal) rearmedBy(a mergeAttempt) bool {
	state := comparableMergeableState(a.state)
	return !strings.EqualFold(r.head, strings.TrimSpace(a.head)) || (state != "" && state != r.state) ||
		!strings.EqualFold(r.review, strings.TrimSpace(a.review))
}

// mergeRefusalPrunable reports whether a remembered refusal stored under
// key may be dropped at now. A native-queue mark that waits for a new head
// or base tip instead of the clock (behind, repeated, the spent retry) is
// kept for sweepMemoryIdle, so a busy memory does not hand the same head
// another merge-group run. So is a failed enqueue's backoff, whose wait
// grows past the recheck window.
func mergeRefusalPrunable(key string, r mergeRefusal, now time.Time) bool {
	idle := mergeRefusalRecheck
	if r.behind || r.repeated || strings.HasPrefix(key, nativeQueueRetryKeyPrefix) || strings.HasPrefix(key, nativeQueueEnqueueKeyPrefix) {
		idle = sweepMemoryIdle
	}
	return now.Sub(r.at) >= idle
}

func mergeRefusalReviewKey(a mergeAttempt) string {
	return "review|" + strings.ToLower(a.owner+"/"+a.repo) + "#" + strconv.Itoa(a.number)
}

// mergeRefusalChecksKey remembers a "required status checks are expected"
// 405 per PR: the head's required checks have not reported yet.
func mergeRefusalChecksKey(a mergeAttempt) string {
	return "checks|" + strings.ToLower(a.owner+"/"+a.repo) + "#" + strconv.Itoa(a.number)
}

// autoMergeReasonChecksExpected is the skip reason while GitHub has told the
// sweep the head's required checks are still expected.
const autoMergeReasonChecksExpected = "check-pending"

// requiredChecksExpected reports whether err is GitHub's 405 "N of M
// required status checks are expected" and, when it is, remembers the head
// and the mergeable_state it was refused in so knownMergeRefusal skips the
// merge call on the following passes while GitHub still reports that state
// (hivecommons/docs#512: the sweep put /merge on every pass, each refusal
// lighting "Merge failed" on the tracker for a PR whose checks had simply
// not started). A check run reporting moves mergeable_state (blocked →
// clean, or clean → unstable/blocked when GitHub had called the head clean
// before its required checks were even created, hivecommons/hive#15419),
// which re-arms the call, as does a new head or the recheck window.
func (c *Engine) requiredChecksExpected(a mergeAttempt, err error) bool {
	if !requiredChecksExpectedMergeError(err) {
		return false
	}
	if c == nil {
		return true
	}
	c.rememberMergeRefusal(mergeRefusalChecksKey(a), mergeRefusal{head: strings.TrimSpace(a.head), state: comparableMergeableState(a.state)})
	return true
}

// checksExpectedKnown reports whether the head of a is remembered as
// waiting on required checks and GitHub still reports the mergeable_state
// it was refused in. It does not require that state to be "blocked": the
// queued lane only reaches the merge call for a head GitHub already calls
// mergeable, so a "blocked"-only memory was never consulted there and the
// merge was re-PUT every pass (hivecommons/hive#15419).
func (c *Engine) checksExpectedKnown(a mergeAttempt, now time.Time) bool {
	m := c.memory()
	r, ok := m.mergeRefusals[mergeRefusalChecksKey(a)]
	if !ok {
		return false
	}
	if now.Sub(r.at) >= mergeRefusalRecheck || !strings.EqualFold(r.head, strings.TrimSpace(a.head)) || comparableMergeableState(a.state) != r.state {
		delete(m.mergeRefusals, mergeRefusalChecksKey(a))
		return false
	}
	return true
}

func mergeRefusalMethodKey(a mergeAttempt) string {
	return "method|" + strings.ToLower(a.owner+"/"+a.repo) + "|" + strings.ToLower(strings.TrimSpace(a.method))
}

// comparableMergeableState drops "unknown": GitHub reports it while it is
// still computing, so it says nothing about whether a review arrived.
func comparableMergeableState(state string) string {
	state = strings.ToLower(strings.TrimSpace(state))
	if state == "unknown" {
		return ""
	}
	return state
}

// knownMergeRefusal returns the reason this merge is already known to be
// refused, or "". The merge-queue base is remembered per base branch
// (mergeQueueBaseRecheck), a disabled merge method per repository and
// method, and a missing approving review per PR head. A review refusal
// re-arms as soon as the head moves or GitHub's mergeable_state for the PR
// changes (an approving review turns `blocked` into `clean`); every refusal
// re-arms once mergeRefusalRecheck has passed.
func (c *Engine) knownMergeRefusal(a mergeAttempt) string {
	if c == nil {
		return ""
	}
	if c.mergeQueueBaseKnown(a.owner, a.repo, a.base) {
		return autoMergeReasonMergeQueueRequired
	}
	if reason := c.knownUnresolvedThreads(a.owner+"/"+a.repo, a.number, a.head); reason != "" {
		return reason
	}
	now := c.clock()
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	if c.checksExpectedKnown(a, now) {
		return autoMergeReasonChecksExpected
	}
	if key := mergeRefusalMethodKey(a); strings.TrimSpace(a.method) != "" {
		if r, ok := m.mergeRefusals[key]; ok {
			if now.Sub(r.at) < mergeRefusalRecheck {
				return autoMergeReasonMergeRefusedMethod
			}
			delete(m.mergeRefusals, key)
		}
	}
	if c.reviewRefusalKnown(a, now) {
		return autoMergeReasonMergeRefusedReview
	}
	return ""
}

// reviewRefusalKnown reports whether the head of a is remembered as refused
// for a missing approving review, dropping a mark that has re-armed: the
// recheck window passed (mergeRefusalRecheck), the head moved, or GitHub's
// mergeable_state (or, on the native-queue path, its reviewDecision) for
// the PR changed. The direct merge and the native-queue enqueue share the
// mark and its window. The caller holds mergeRefusalsMu.
func (c *Engine) reviewRefusalKnown(a mergeAttempt, now time.Time) bool {
	m := c.memory()
	key := mergeRefusalReviewKey(a)
	r, ok := m.mergeRefusals[key]
	if !ok {
		return false
	}
	if now.Sub(r.at) >= mergeRefusalRecheck || r.rearmedBy(a) {
		delete(m.mergeRefusals, key)
		return false
	}
	return true
}

// rememberReviewRefusal remembers that GitHub refused the head of a for a
// missing approving review, for reviewRefusalKnown.
func (c *Engine) rememberReviewRefusal(a mergeAttempt) {
	c.rememberMergeRefusal(mergeRefusalReviewKey(a), mergeRefusal{head: strings.TrimSpace(a.head), state: comparableMergeableState(a.state), review: strings.TrimSpace(a.review)})
}

// refusedMerge classifies a failed merge call as a permanent refusal,
// remembers it for knownMergeRefusal, and returns the skip reason; "" leaves
// the error to the caller's merge-failed path unchanged. An
// unresolved-conversations refusal first gets the hive's own threads
// resolved when they are the only ones open (unresolvedThreadsRefusal).
func (c *Engine) refusedMerge(ctx context.Context, a mergeAttempt, err error) string {
	if mergeQueueRequiredMergeError(err) {
		c.noteMergeQueueBase(a.owner, a.repo, a.base)
		return autoMergeReasonMergeQueueRequired
	}
	if unresolvedConversationsMergeError(err) {
		return c.unresolvedThreadsRefusal(ctx, a.owner+"/"+a.repo, a.number, a.head)
	}
	if mergeConflictMergeError(err) {
		// Not remembered: GitHub's mergeable_state says dirty on the next
		// read, and the lanes skip on that. Reported as the conflict it is,
		// not merge-failed, which parked the PR at 080 for a pass
		// (projectbluefin/utah-packages#633).
		return "conflicting"
	}
	reason := mergeRefusalReason(err)
	if reason == "" || c == nil {
		return reason
	}
	switch reason {
	case autoMergeReasonMergeRefusedReview:
		c.rememberReviewRefusal(a)
	case autoMergeReasonMergeRefusedMethod:
		if strings.TrimSpace(a.method) == "" {
			return reason
		}
		c.rememberMergeRefusal(mergeRefusalMethodKey(a), mergeRefusal{})
	}
	return reason
}

// rememberMergeRefusal stores r under key, stamped now, pruning expired
// entries once the map is large.
func (c *Engine) rememberMergeRefusal(key string, r mergeRefusal) {
	r.at = c.clock()
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	if m.mergeRefusals == nil {
		m.mergeRefusals = make(map[string]mergeRefusal)
	}
	if len(m.mergeRefusals) >= mergeRefusalPruneAt {
		// Entries for PRs that merged or closed are never read again.
		for k, old := range m.mergeRefusals {
			if mergeRefusalPrunable(k, old, r.at) {
				delete(m.mergeRefusals, k)
			}
		}
	}
	m.mergeRefusals[key] = r
}

func nativeQueueMethodRefusalKey(claim effects.Claim) string {
	return "queue-method|" + strings.ToLower(claim.Repo) + "#" + claim.Target
}

// knownNativeQueueMethodRefusal reports whether this PR's head was already
// refused by a queue whose merge method cannot keep mixed authorship, so the
// sweep does not re-read the queue's method every pass. Like a remembered
// merge-method 405 it re-arms after mergeRefusalRecheck, and on a new head.
func (c *Engine) knownNativeQueueMethodRefusal(claim effects.Claim, head string) bool {
	if c == nil {
		return false
	}
	key := nativeQueueMethodRefusalKey(claim)
	now := c.clock()
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	r, ok := m.mergeRefusals[key]
	if !ok {
		return false
	}
	if now.Sub(r.at) >= mergeRefusalRecheck || !strings.EqualFold(r.head, strings.TrimSpace(head)) {
		delete(m.mergeRefusals, key)
		return false
	}
	return true
}

// noteNativeQueueMethodRefusal remembers a queue-method refusal for
// knownNativeQueueMethodRefusal.
func (c *Engine) noteNativeQueueMethodRefusal(claim effects.Claim, head string) {
	if c == nil {
		return
	}
	r := mergeRefusal{head: strings.TrimSpace(head), at: c.clock()}
	m := c.memory()
	m.mergeRefusalsMu.Lock()
	defer m.mergeRefusalsMu.Unlock()
	if m.mergeRefusals == nil {
		m.mergeRefusals = make(map[string]mergeRefusal)
	}
	if len(m.mergeRefusals) >= mergeRefusalPruneAt {
		for k, old := range m.mergeRefusals {
			if mergeRefusalPrunable(k, old, r.at) {
				delete(m.mergeRefusals, k)
			}
		}
	}
	m.mergeRefusals[nativeQueueMethodRefusalKey(claim)] = r
}

package automerge

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

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
const mergeRefusalRecheck = 30 * time.Minute

// mergeRefusalPruneAt is the remembered-refusal count at which expired
// entries are dropped on the next insert.
const mergeRefusalPruneAt = 1024

// "At least 1 approving review is required by reviewers with write access.",
// "At least 2 approving reviews are required ...", and the ruleset form
// "1 review requesting changes by reviewers with write access.".
var mergeRefusedReviewRE = regexp.MustCompile(`(?i)\d+ approving reviews? (?:is|are) required|\d+ reviews? requesting changes`)

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

// isPermanentMergeRefusal reports whether reason is one of the classified
// 405 refusals above.
func isPermanentMergeRefusal(reason string) bool {
	switch reason {
	case autoMergeReasonMergeQueueRequired, autoMergeReasonMergeRefusedReview, autoMergeReasonMergeRefusedMethod:
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
}

type mergeRefusal struct {
	head  string
	state string
	at    time.Time
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
	r, ok := c.mergeRefusals[mergeRefusalChecksKey(a)]
	if !ok {
		return false
	}
	if now.Sub(r.at) >= mergeRefusalRecheck || !strings.EqualFold(r.head, strings.TrimSpace(a.head)) || comparableMergeableState(a.state) != r.state {
		delete(c.mergeRefusals, mergeRefusalChecksKey(a))
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
	now := c.clock()
	c.mergeRefusalsMu.Lock()
	defer c.mergeRefusalsMu.Unlock()
	if c.checksExpectedKnown(a, now) {
		return autoMergeReasonChecksExpected
	}
	if key := mergeRefusalMethodKey(a); strings.TrimSpace(a.method) != "" {
		if r, ok := c.mergeRefusals[key]; ok {
			if now.Sub(r.at) < mergeRefusalRecheck {
				return autoMergeReasonMergeRefusedMethod
			}
			delete(c.mergeRefusals, key)
		}
	}
	key := mergeRefusalReviewKey(a)
	r, ok := c.mergeRefusals[key]
	if !ok {
		return ""
	}
	state := comparableMergeableState(a.state)
	if now.Sub(r.at) >= mergeRefusalRecheck || !strings.EqualFold(r.head, strings.TrimSpace(a.head)) || (state != "" && state != r.state) {
		delete(c.mergeRefusals, key)
		return ""
	}
	return autoMergeReasonMergeRefusedReview
}

// refusedMerge classifies a failed merge call as a permanent refusal,
// remembers it for knownMergeRefusal, and returns the skip reason; "" leaves
// the error to the caller's merge-failed path unchanged.
func (c *Engine) refusedMerge(a mergeAttempt, err error) string {
	if mergeQueueRequiredMergeError(err) {
		c.noteMergeQueueBase(a.owner, a.repo, a.base)
		return autoMergeReasonMergeQueueRequired
	}
	reason := mergeRefusalReason(err)
	if reason == "" || c == nil {
		return reason
	}
	var key string
	var r mergeRefusal
	switch reason {
	case autoMergeReasonMergeRefusedReview:
		key, r.head, r.state = mergeRefusalReviewKey(a), strings.TrimSpace(a.head), comparableMergeableState(a.state)
	case autoMergeReasonMergeRefusedMethod:
		if strings.TrimSpace(a.method) == "" {
			return reason
		}
		key = mergeRefusalMethodKey(a)
	}
	c.rememberMergeRefusal(key, r)
	return reason
}

// rememberMergeRefusal stores r under key, stamped now, pruning expired
// entries once the map is large.
func (c *Engine) rememberMergeRefusal(key string, r mergeRefusal) {
	r.at = c.clock()
	c.mergeRefusalsMu.Lock()
	defer c.mergeRefusalsMu.Unlock()
	if c.mergeRefusals == nil {
		c.mergeRefusals = make(map[string]mergeRefusal)
	}
	if len(c.mergeRefusals) >= mergeRefusalPruneAt {
		// Entries for PRs that merged or closed are never read again.
		for k, old := range c.mergeRefusals {
			if r.at.Sub(old.at) >= mergeRefusalRecheck {
				delete(c.mergeRefusals, k)
			}
		}
	}
	c.mergeRefusals[key] = r
}

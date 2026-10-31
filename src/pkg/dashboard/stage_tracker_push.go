package dashboard

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Push progress on a CI / Review row (hivecommons/hive#15784). A PR whose
// author must push read "waiting on @zoe to push · acknowledged" and
// "Needs @zoe" until the next sweep re-staged it — nothing said the person
// had pushed, or how long the hive had been waiting. The row's `push`
// field answers from the stage record alone: when the waiting-on-push
// state began (Record.WaitingPushSince) and the last head change seen
// while the row sat in the column (Record.LastPushAt / LastPushSha, booked
// by the webhook re-read of a synchronize delivery or by the enumerate
// that followed). The chevron keeps the stage; the gray pill carries the
// who and the push; the hive caption says what is queued.

// Push states on the wire.
const (
	// pushStateWaiting: the hive waits for the author's push; `since` is
	// when the wait began.
	pushStateWaiting = "waiting"
	// pushStatePushed: a head landed; CI and the re-review for it are
	// still running. `since` is when it landed.
	pushStatePushed = "pushed"
	// No new head: the canonical observation sees checks pending again.
	pushStateChecking = "checking"

	// pushNextCI / pushNextReReview name what the push queued.
	pushNextCI       = "ci"
	pushNextReReview = "re-review"
)

// StageTrackerPush is the `push` field of a PR row in the CI / Review column.
type StageTrackerPush struct {
	// State is waiting, pushed, or checking (same-head CI pending again).
	State string `json:"state"`
	// Login is the PR author: whose push the hive waits on, or who pushed.
	Login string `json:"login,omitempty"`
	// Since is when the state began: the wait's start, or the push.
	Since *time.Time `json:"since,omitempty"`
	// Sha is the observed head; checking does not imply a new push.
	// URL links to the pushed commit on GitHub.
	Sha string `json:"sha,omitempty"`
	URL string `json:"url,omitempty"`
	// Next is the outstanding CI / re-review work, not readiness proof.
	Next string `json:"next,omitempty"`
	// Stale is set on a wait older than dashboard.stage_tracker.waiting_push_stale_h.
	Stale bool `json:"stale,omitempty"`
}

// stageTrackerPushFor derives the `push` field for a PR row at stage. rec
// is the row's stage record; staleAfter is the configured wait threshold
// (0: never stale).
func stageTrackerPushFor(it StageTrackerItem, stage stagelabels.Stage, rec stagelabels.Record, hasRec bool, staleAfter time.Duration, now time.Time) *StageTrackerPush {
	if it.Kind != "pr" || !hasRec || !stage.TracksPush() {
		return nil
	}
	login := strings.TrimSpace(it.Author)
	if rec.PushedSinceWaiting() && stageTrackerPushOnHead(it, rec) && stageTrackerPushSettling(stage, rec) {
		at := rec.LastPushAt
		if at.After(now) {
			at = now
		}
		return &StageTrackerPush{
			State: pushStatePushed,
			Login: login,
			Since: &at,
			Sha:   rec.LastPushSha,
			URL:   stageTrackerCommitURL(it, rec.LastPushSha),
			Next:  stageTrackerPushNext(stage, rec.Reason),
		}
	}
	if stageTrackerCIRetryObserved(it, stage, rec) {
		return &StageTrackerPush{State: pushStateChecking, Sha: it.Head, Next: pushNextCI}
	}
	if stageTrackerAuthorPushSatisfied(it, stage, rec) {
		// The nudge names the old head even when the webhook push ledger
		// has not caught up. A PR update time is not a known push time.
		return &StageTrackerPush{
			State: pushStatePushed,
			Login: login,
			Sha:   it.Head,
			URL:   stageTrackerCommitURL(it, it.Head),
			Next:  stageTrackerPushNext(stage, rec.Reason),
		}
	}
	if !stage.WaitsOnPush() || rec.WaitingPushSince.IsZero() {
		return nil
	}
	since := rec.WaitingPushSince
	if since.After(now) {
		since = now
	}
	return &StageTrackerPush{
		State: pushStateWaiting,
		Login: login,
		Since: &since,
		Stale: staleAfter > 0 && now.Sub(since) >= staleAfter,
	}
}

func stageTrackerCIRetryObserved(it StageTrackerItem, stage stagelabels.Stage, rec stagelabels.Record) bool {
	if !stage.WaitsOnPush() || !it.ciChecking || it.ciRed || it.Head == "" || !strings.EqualFold(rec.ActivityHead, it.Head) ||
		(stageengine.ClassifyCIReason(rec.Reason) != stageengine.CIReasonRed && rec.Reason != "ci-red") {
		return false
	}
	for _, m := range it.Modifiers {
		switch m {
		case modNeedsRebase, modNeedsHuman, modNeedsDecision, modUnauthorized, modHold, modBlocked, modDraft:
			return false
		}
	}
	return true
}

// stageTrackerPushSettling reports whether CI or the re-review for the
// pushed head is still ahead: the sweep has not re-staged the row off its
// waiting stage yet, has not seen the head at all, the item waits on the
// hive (checks running, verdict pending), or the hive itself is working
// the head (070 / 074). Once the head is seen, settled and nothing waits
// on the hive, the push is old news and the pill goes.
func stageTrackerPushSettling(stage stagelabels.Stage, rec stagelabels.Record) bool {
	switch {
	case stage.WaitsOnPush(), stage == stagelabels.Fixing, stage == stagelabels.AdoptedByHive:
		return true
	case rec.WaitingOnHive:
		return true
	case !strings.EqualFold(rec.ActivityHead, rec.LastPushSha):
		return true
	}
	return false
}

// stageTrackerPushNext names what the push queued: a re-review when the
// row was returned for review findings, CI otherwise.
func stageTrackerPushNext(stage stagelabels.Stage, reason string) string {
	lower := strings.ToLower(reason)
	if strings.Contains(lower, "changes") || strings.Contains(lower, "review") {
		return pushNextReReview
	}
	if stage == stagelabels.ReviewPending {
		return pushNextReReview
	}
	return pushNextCI
}

// stageTrackerCommitURL is the GitHub page of sha on the row's repository,
// derived from the PR URL ("https://github.com/o/r/pull/N"); "" when the
// URL is not shaped like that.
func stageTrackerCommitURL(it StageTrackerItem, sha string) string {
	if sha == "" {
		return ""
	}
	i := strings.Index(it.URL, "/pull/")
	if i < 0 {
		return ""
	}
	return it.URL[:i] + "/commit/" + sha
}

// stageTrackerPushCaption is the hive line for a pushed row: "push seen 2m
// ago → CI/re-review queued". "" when the row carries no push.
func stageTrackerPushCaption(p *StageTrackerPush, now time.Time) string {
	if p != nil && p.State == pushStateChecking {
		return "checks pending again on the current head — no new push requested"
	}
	if p == nil || p.State != pushStatePushed {
		return ""
	}
	if p.Since == nil {
		if p.Next == pushNextReReview {
			return "new head observed → re-review pending"
		}
		return "new head observed → CI pending"
	}
	ago := shortDuration(now.Sub(*p.Since))
	if ago == "0s" {
		return "push seen just now → CI/re-review queued"
	}
	return "push seen " + ago + " ago → CI/re-review queued"
}

// stageTrackerPushWhy is the next-move line for a pushed row: who pushed
// and what the hive runs on the new head.
func stageTrackerPushWhy(p *StageTrackerPush) string {
	if p.State == pushStateChecking {
		return "checks pending again on " + shortSHA(p.Sha) + " — no new push requested"
	}
	who := "the author"
	if p.Login != "" {
		who = "@" + p.Login
	}
	what := "CI runs on the new head"
	if p.Next == pushNextReReview {
		what = "the re-review is queued for the new head"
	}
	return who + " pushed " + shortSHA(p.Sha) + " — " + what
}

// stageTrackerWaitingPushStale is dashboard.stage_tracker.waiting_push_stale_h
// as the server reads it (the config default when no config is wired).
func (s *Server) stageTrackerWaitingPushStale() time.Duration {
	if s != nil && s.deps != nil && s.deps.Config != nil {
		return s.deps.Config.StageTrackerWaitingPushStale()
	}
	return (*config.Config)(nil).StageTrackerWaitingPushStale()
}

// stageTrackerPushOnHead reports whether the row's head is still the pushed
// one; once the head moves on, the push is old news (hivecommons/hive#15797).
// A row that carries no head cannot say, so the push stands; so does one
// whose listed head is the one the enumerate last booked — the webhook
// re-read books the push ahead of the list, which still shows the head
// the author pushed over.
func stageTrackerPushOnHead(it StageTrackerItem, rec stagelabels.Record) bool {
	return it.Head == "" || strings.EqualFold(it.Head, rec.LastPushSha) || strings.EqualFold(it.Head, rec.ActivityHead)
}

// A nudge's old-head request is satisfied before a push webhook or a stage
// relabel necessarily lands. A newly evaluated author hold starts its own wait;
// that current-head request must not be released by an older nudge.
func stageTrackerAuthorPushSatisfied(it StageTrackerItem, stage stagelabels.Stage, rec stagelabels.Record) bool {
	if it.Kind != "pr" || !stage.WaitsOnPush() || it.Head == "" || rec.NudgeHead == "" || strings.EqualFold(it.Head, rec.NudgeHead) {
		return false
	}
	if strings.EqualFold(rec.ActivityHead, it.Head) && !rec.WaitingOnHive && rec.WaitingPushSince.After(rec.NudgeStart()) {
		return false
	}
	return true
}

// stageTrackerPushPending reports a head awaiting CI/re-review, or observed
// same-head checks running again without requiring a new push.
func stageTrackerPushPending(it StageTrackerItem) bool {
	return it.Push != nil && (it.Push.State == pushStatePushed || it.Push.State == pushStateChecking)
}

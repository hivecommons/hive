package stagelabels

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultStateDir matches the review swarm's durable data dir (the PVC on a
// hosted spoke) so stage timing survives a pod restart.
const DefaultStateDir = "/data"

// StateFile is the persisted stage-timing file name.
const StateFile = "stage-labels.json"

// StatePath is where the shared Store persists. A var so tests and the
// operator can redirect it.
var StatePath = filepath.Join(DefaultStateDir, StateFile)

// Record is what the hive remembers about one item's stage: when it entered
// the stage and when it last saw activity (for stalled detection, kept
// locally so no GitHub timeline GET is needed), its recent transitions and
// any unresolved regression (narrated in a comment, no longer a label).
type Record struct {
	Stage     Stage     `json:"stage"`
	EnteredAt time.Time `json:"entered_at"`
	// RegressedFrom is the stage the item fell back from; None when the item
	// has not regressed or has since recovered.
	RegressedFrom Stage     `json:"regressed_from,omitempty"`
	RegressedAt   time.Time `json:"regressed_at,omitempty"`
	// RegressionCommented records that the one-line regression comment for
	// the current regression was posted, so it is never posted twice.
	RegressionCommented bool `json:"regression_commented,omitempty"`
	// RegressionCommentHead is the head SHA the last regression comment was
	// posted for. Unlike RegressionCommented it survives a recover→regress
	// flap, so a gate flapping on one head posts at most one regression
	// comment for it (hivecommons/hive#12557). Empty for items without a head.
	RegressionCommentHead string `json:"regression_comment_head,omitempty"`
	// StallCommented records that the stall note for the current stage
	// visit was posted (stages with StallNote only).
	StallCommented bool `json:"stall_commented,omitempty"`
	// Reason is the lane/CI/verdict reason behind the current stage.
	Reason string `json:"reason,omitempty"`
	// ReasonChangedAt is when Reason last changed meaning: stage entry, or
	// a pass that observed a different reason class (ReasonClass) at the
	// same stage. The dashboard ages the row's hive note from it rather
	// than from LastSeen, so a pass that re-reads the same reason does not
	// read as a fresh note (hivecommons/hive#15729). Zero on records
	// written before it existed; NoteChangedAt falls back to EnteredAt.
	ReasonChangedAt time.Time `json:"reason_changed_at,omitempty"`
	// UnblockedAt is when the blocked sweep last lifted the item's block
	// (hivecommons/hive#15779). A blocked issue has no stage, so the lift
	// leaves a stage-less record carrying only this time until the next
	// stage-label pass stages the issue; the dashboard ages the row from
	// StageSinceAt so "since" restarts at the lift, not at issue creation.
	UnblockedAt time.Time `json:"unblocked_at,omitempty"`
	// Author is the item's author login, lower-cased, as the lane saw it
	// (hivecommons/hive#12502). It lets the dashboard answer "which of this
	// contributor's PRs are returned to them" from the store alone, with no
	// GitHub call. Empty for records written before the field existed.
	Author string `json:"author,omitempty"`
	// NudgeHead, NudgeCount and LastNudgeAt track the stage-072 contributor
	// nudge: the head the nudges were for, how many were posted for it
	// (cap 2) and when the last one went out.
	NudgeHead   string    `json:"nudge_head,omitempty"`
	NudgeCount  int       `json:"nudge_count,omitempty"`
	LastNudgeAt time.Time `json:"last_nudge_at,omitempty"`
	// FirstNudgeAt is when the first nudge for NudgeHead went out: the
	// adoption grace (online 2h / offline 15m) counts from here.
	FirstNudgeAt time.Time `json:"first_nudge_at,omitempty"`
	// NudgeOnline records whether the author's relay was online at the first
	// nudge for NudgeHead (selects the online vs offline grace).
	NudgeOnline bool `json:"nudge_online,omitempty"`
	// AdoptedHead / AdoptedBy / AdoptedAt record the stage-074 adoption: the
	// contributor head the hive adopted, the superseding App PR's number
	// (0 while the attempt is recorded but no PR exists) and when. One
	// attempt per head.
	AdoptedHead string    `json:"adopted_head,omitempty"`
	AdoptedBy   int       `json:"adopted_by,omitempty"`
	AdoptedAt   time.Time `json:"adopted_at,omitempty"`
	// AdoptCheckedAt is the last time the superseding PR's state was polled.
	AdoptCheckedAt time.Time `json:"adopt_checked_at,omitempty"`
	// SupersededBy / SupersededAt record that the superseding App PR merged
	// and the close note was posted on the original (hivecommons/hive#14471).
	// Set once per adoption: the settle pass never re-posts, and the
	// dashboard stage tracker drops the row even when the close itself was
	// refused (a fork PR the App may not close). SupersededOpen is true when
	// the original could not be closed and is left open with the note.
	SupersededBy   int       `json:"superseded_by,omitempty"`
	SupersededAt   time.Time `json:"superseded_at,omitempty"`
	SupersededOpen bool      `json:"superseded_open,omitempty"`
	// SupersededClosedFirst is true while the original was closed at
	// adoption time, before its copy merged (close-first supersession,
	// hivecommons/hive#15135). The reconciler keeps polling the copy
	// (CloseFirstPollDue): merged clears the flag and the record reads as
	// an ordinary supersession; closed unmerged, red across two heads, or a
	// branch that cannot be pushed reopens the original
	// (MarkAdoptionReopened). AdoptCopyRedHeads are the copy heads seen red
	// so far; AdoptReopenedAt / AdoptReopenedFrom record the last reopen
	// and the copy that failed, so one original is reopened at most once
	// per AdoptionReopenWindow.
	SupersededClosedFirst bool      `json:"superseded_closed_first,omitempty"`
	AdoptCopyRedHeads     []string  `json:"adopt_copy_red_heads,omitempty"`
	AdoptReopenedAt       time.Time `json:"adopt_reopened_at,omitempty"`
	AdoptReopenedFrom     int       `json:"adopt_reopened_from,omitempty"`
	// AdoptCopyConflicting is set when the hive's copy (AdoptedBy) opened
	// conflicting with its base because the contributor head could not be
	// replayed onto it and the fixer's rebase-resolve episode owns the
	// conflict (hivecommons/hive#15307); cleared when the copy reads
	// mergeable again. The tracker row reads "Adopting · fixer resolving
	// conflicts" while it is set.
	AdoptCopyConflicting bool `json:"adopt_copy_conflicting,omitempty"`
	// SupersederMergedNotedHead is the original's head for which the
	// "superseder merged but this PR has moved on" note was posted, so a
	// diverged original is told once per head rather than every poll.
	SupersederMergedNotedHead string `json:"superseder_merged_noted_head,omitempty"`
	// DCOHead, DCOTotal and DCOUnsigned cache the commit check for one head
	// (hivecommons/hive#12542): how many non-merge commits the PR carried
	// and how many lacked a Signed-off-by trailer. DCOUnsignedHive /
	// DCOUnsignedOther split the unsigned ones into those GitHub links to
	// the App bot (the hive's own to rewrite with sign-off) and the rest
	// (re-home); records cached before the split existed have neither (see
	// Record.DCOSplitKnown). One commits GET per head, never per cycle.
	DCOHead              string `json:"dco_head,omitempty"`
	DCOTotal             int    `json:"dco_total,omitempty"`
	DCOUnsigned          int    `json:"dco_unsigned,omitempty"`
	DCOUnsignedHive      int    `json:"dco_unsigned_hive,omitempty"`
	DCOUnsignedOther     int    `json:"dco_unsigned_other,omitempty"`
	DCOSquashUnsafeHive  int    `json:"dco_squash_unsafe_hive,omitempty"`
	DCOSquashUnsafeKnown bool   `json:"dco_squash_unsafe_known,omitempty"`
	// EntryCommented records that the stage's entry comment (085 "needs a
	// decision", 090 "ready for a human merge") was posted for the current
	// stage visit; reset on every stage change so each transition explains
	// itself exactly once (hivecommons/hive#12528).
	EntryCommented bool `json:"entry_commented,omitempty"`
	// EntryCommentedHead / EntryCommentedStage are the head and stage of the
	// last entry comment posted. Unlike EntryCommented they survive stage
	// changes, so a same-head flap back into the same stage does not repost a
	// byte-identical comment (hivecommons/hive#12878).
	EntryCommentedHead  string `json:"entry_commented_head,omitempty"`
	EntryCommentedStage Stage  `json:"entry_commented_stage,omitempty"`
	// EntryCommentedCauses are the cause digests (EntryCause) of every
	// entry comment posted for EntryCommentedStage/EntryCommentedHead. A
	// parked PR whose decision cause changes on the same head — sentinel
	// alert cleared, workflow-path fence now the blocker — gets each distinct
	// cause explained once, not a byte-identical repost and not silence.
	EntryCommentedCauses []string `json:"entry_commented_causes,omitempty"`
	// EntryCommentedDecisionKey is the writeback.DecisionKey of the last 085
	// entry comment: the superseding PR the reviewer named, or the
	// normalised question. It survives heads and stage flaps, so the same
	// question re-asked on a merge-from-base head is not re-posted
	// (hivecommons/hive#15499).
	EntryCommentedDecisionKey string `json:"entry_commented_decision_key,omitempty"`
	// History is the item's recent stage transitions, oldest first, capped
	// at HistoryLimit: each entry is the stage entered, when, and the reason
	// at entry. The dashboard's Hive activity monitor renders it as the
	// item's tracker trail; nothing in the reconciler reads it back.
	History []Hop `json:"history,omitempty"`
	// AdoptMode is how the hive adopted the PR: AdoptModeSupersede (an App
	// PR from hive/adopt-<n> carries the head) or AdoptModeInPlace (the
	// hive's fix lane pushes to the PR's own branch, maintainer_can_modify).
	// Empty on records written before in-place adoption existed, which were
	// all supersessions.
	AdoptMode string `json:"adopt_mode,omitempty"`
	// AdoptReason is why the hive adopted ("contributor-offline",
	// "contributor-unregistered", "grace-elapsed").
	AdoptReason string `json:"adopt_reason,omitempty"`
	// AdoptDiffFence is the contributor lane's diff-fence reason
	// ("contributor-lane:tests-removed", "contributor-lane:workflow-path")
	// the adopted head tripped when the hive took it over. Adoption is not
	// a merge, so the fence does not refuse it; it is carried on the record
	// (and into the superseding PR's body) so the merge stays a person's
	// whichever lane evaluates the head. Empty when the diff read clean.
	AdoptDiffFence string `json:"adopt_diff_fence,omitempty"`
	// YieldedHead is the contributor head the hive yielded an in-place
	// adoption on (the author pushed again); the yield note is posted once
	// per head.
	YieldedHead string `json:"yielded_head,omitempty"`
	// HivePushedHeads are heads the hive recorded pushing to this adopted PR.
	// They are trusted local evidence for in-place adoption attribution when
	// the later GitHub commit object cannot link the commit to the App.
	HivePushedHeads []string `json:"hive_pushed_heads,omitempty"`
	// CoveringPR is the store key ("owner/repo#n") of the open pull request
	// that put an issue at 040-covered-by-pr, as the last derivation knew
	// it; empty at any other stage or when the covering PR was not named.
	// An out-of-cycle fact (a claim, its release, a start mark) consults it
	// so a PR that still covers the issue is not forgotten between sweeps.
	CoveringPR string `json:"covering_pr,omitempty"`
	// AdoptedHeadAt is when AdoptedHead last moved under a live in-place
	// adoption: the adoption itself, then each hive push. Zero only on
	// records written before it existed (hivecommons/hive#13040); the
	// reconciler stamps those on first sight (StampAdoptedHeadAt) rather
	// than reading the dwell from AdoptedAt, which may be a head or more
	// behind. AdoptedHeadSince resolves it.
	AdoptedHeadAt time.Time `json:"adopted_head_at,omitempty"`
	// AdoptEscalatedHead / AdoptEscalatedReason record that the adoption
	// sits at this head past the 074 dwell with nothing the hive can push
	// and is a person's (stage 085) for that lane reason. The escalation
	// holds while the reason holds: ClearAdoptionEscalation releases it
	// when the blocker clears or the head moves.
	AdoptEscalatedHead   string `json:"adopt_escalated_head,omitempty"`
	AdoptEscalatedReason string `json:"adopt_escalated_reason,omitempty"`
	// Hold is who set the `hold` label the item wears and why, read by the
	// stage-label pass from the label event and the hive's notice comment
	// (hivecommons/hive#15020); nil when the item is not held or nothing
	// about the hold could be read.
	Hold *HoldProvenance `json:"hold,omitempty"`
	// AdoptEscalationNotedHead is the head the escalation note was posted
	// for; it survives ClearAdoptionEscalation so a blocker that flaps on
	// one head is noted once.
	AdoptEscalationNotedHead string `json:"adopt_escalation_noted_head,omitempty"`
	// SoakHead / SoakStartedAt / SoakWindowSeconds record the soak the lane
	// is holding this PR for: the head that must sit unchanged, when it was
	// first seen green, and the window it must sit for. Written by the pass
	// whether the lane admitted the head (095) or trust still blocks it
	// (060, RouteHuman's "soak Nm remaining"), so the dashboard counts the
	// same clock down in both places and notices a head that moved on.
	// Cleared when the head is not soaking.
	SoakHead          string    `json:"soak_head,omitempty"`
	SoakStartedAt     time.Time `json:"soak_started_at,omitempty"`
	SoakWindowSeconds int64     `json:"soak_window_s,omitempty"`
	// AdoptBlockedHead / AdoptBlockedBy / AdoptBlockedAt record that
	// adoption came due at this head and the engine held it for a reason a
	// person must settle (AdoptBlockedAuthorMaintainer: the author
	// maintains the repository under maintainer_policy: ask —
	// hivecommons/hive#14524). The activity monitor reads it to say why a
	// nudged PR is not adopted and to offer Adopt; adoption or a new nudge
	// head clears it.
	AdoptBlockedHead string    `json:"adopt_blocked_head,omitempty"`
	AdoptBlockedBy   string    `json:"adopt_blocked_by,omitempty"`
	AdoptBlockedAt   time.Time `json:"adopt_blocked_at,omitempty"`
	// AdmittedHead / AdmittedBy / AdmittedAt record a person's trust
	// override for one head (hivecommons/hive#15136): an owner or trusted
	// merger approved the PR from the dashboard (or `/hive decision
	// approve`), so for THAT head the contributor lane treats the author
	// as trusted and the adoption engine does not take the PR over.
	// AdmittedBy is "operator-approval:<login>". A new head drops it:
	// the person approved what they read, not what came after.
	AdmittedHead string    `json:"admitted_head,omitempty"`
	AdmittedBy   string    `json:"admitted_by,omitempty"`
	AdmittedAt   time.Time `json:"admitted_at,omitempty"`
	// AdoptForcedBy / AdoptForcedAt record an operator's "Adopt now": the
	// login that forced adoption and when. The next sweep adopts by
	// supersession at once, regardless of the author's presence and of
	// maintainer_policy; the attempt clears it (MarkAdopted).
	AdoptForcedBy string    `json:"adopt_forced_by,omitempty"`
	AdoptForcedAt time.Time `json:"adopt_forced_at,omitempty"`
	// RerouteTo / RerouteBy / RerouteAt / RerouteReason record an
	// operator's "Reroute to another clanker": the PR's follow-up work was
	// offered to RerouteTo's queue instead of waiting on the author.
	// RerouteHead is the head the reroute was issued for; a new head (the
	// target or the author pushed) ends the reroute. RerouteFallbackAt is
	// when the pickup window lapsed and the hive fell back to Adopt now.
	RerouteTo         string    `json:"reroute_to,omitempty"`
	RerouteBy         string    `json:"reroute_by,omitempty"`
	RerouteAt         time.Time `json:"reroute_at,omitempty"`
	RerouteReason     string    `json:"reroute_reason,omitempty"`
	RerouteHead       string    `json:"reroute_head,omitempty"`
	RerouteFallbackAt time.Time `json:"reroute_fallback_at,omitempty"`
	// DCORepairHead is the in-place-adopted head whose unsigned hive
	// commits the hive last rewrote with its sign-off on the contributor
	// branch (hivecommons/hive#14666); one attempt per head.
	DCORepairHead string `json:"dco_repair_head,omitempty"`
	// AdoptRecoveryAttempt is the "<head>@<base tip>" pair the hive last
	// tried to recover a stale-red in-place adoption on (update-branch or a
	// re-run of the failed jobs, hivecommons/hive#14500); one attempt per
	// pair. AdoptRecoveryCount counts attempts across heads for the
	// adoption so a base that keeps moving under a red head cannot churn
	// update-branch commits forever; both reset with the adoption.
	AdoptRecoveryAttempt string `json:"adopt_recovery_attempt,omitempty"`
	AdoptRecoveryCount   int    `json:"adopt_recovery_count,omitempty"`
	// AdoptOrphanedBy / AdoptOrphanedAt / AdoptOrphanCount record that a
	// supersede-mode adoption's hive copy was closed without merging
	// (hivecommons/hive#15052: the duplicate-fix sweep closed #14977 under
	// #14947). The reconciler re-adopts from the same head; the count and
	// time let it stop when the fresh copy is closed again within
	// AdoptionOrphanLoopWindow (reason LaneReasonAdoptionChildClosedTwice).
	// All three reset with the adoption.
	AdoptOrphanedBy  int       `json:"adopt_orphaned_by,omitempty"`
	AdoptOrphanedAt  time.Time `json:"adopt_orphaned_at,omitempty"`
	AdoptOrphanCount int       `json:"adopt_orphan_count,omitempty"`
	// AdoptMarkerScanHead is the head at which the reconciler last read the
	// PR's comments for an adoption marker the store no longer carries (a
	// record that forgot its adoption, or a store rebuilt from scratch);
	// one read per head.
	AdoptMarkerScanHead string `json:"adopt_marker_scan_head,omitempty"`
	// MergeCommitNotedHead is the head the `merge-method/merge` note was
	// posted for, so the lane explains the merge method once per head.
	MergeCommitNotedHead string `json:"merge_commit_noted_head,omitempty"`
	// TrustAnnouncedAt is set on a TrustPromotionKey record when the
	// contributor lane posted its one "now trusted for contributor-lane
	// merging in this hive" comment for the author
	// (hivecommons/hive#15443). Zero while the author is promoted but no
	// comment has landed yet.
	TrustAnnouncedAt time.Time `json:"trust_announced_at,omitempty"`
	// LastSeen is the last cycle the item was observed; stale records are
	// pruned by Prune so a closed item does not live in the file forever.
	LastSeen time.Time `json:"last_seen"`
	// LastActivityAt is when somebody other than the hive last touched the
	// item as far as the reconciler can tell (hivecommons/hive#12534): a new
	// head, a comment, a review/verdict, a CI state change or a non-hive
	// label change. Stalled measures quiet time from here, never from the
	// PR's updated_at (which the hive's own writes bump).
	LastActivityAt time.Time `json:"last_activity_at,omitempty"`
	// ActivityHead, ActivityComments, ActivityReviews and ActivityDigest are
	// the last observed activity inputs; a change in any of them is
	// activity. ActivityComments counts the hive's own comments too, so the
	// reconciler adds the ones it posts (CountOwnComments) and they never
	// read as someone else acting. ActivityReviews counts review
	// submissions by people (not the hive app), with or without inline
	// comments: a body-only Approve / Request-changes is activity too.
	ActivityHead     string `json:"activity_head,omitempty"`
	ActivityComments int    `json:"activity_comments,omitempty"`
	ActivityReviews  int    `json:"activity_reviews,omitempty"`
	ActivityDigest   string `json:"activity_digest,omitempty"`
	// WaitingOnHive records that, at the last observation, the item was
	// waiting on the hive (required checks running, reviewer verdict
	// pending for the head): it cannot stall, whatever the quiet time.
	WaitingOnHive bool `json:"waiting_on_hive,omitempty"`
	// HiveHeldWarned is set once the hive-side alert for an item held by
	// the hive past its dwell has been logged; activity resets it.
	HiveHeldWarned bool `json:"hive_held_warned,omitempty"`
	// WaitingPushSince is when the current waiting-on-push state began
	// (hivecommons/hive#15784): entry to 071 / 072, or a head the sweep
	// evaluated and still routed back to the author. The dashboard ages
	// the "waiting on @author to push" pill from it. Zero outside those
	// stages.
	WaitingPushSince time.Time `json:"waiting_push_since,omitempty"`
	// LastPushAt / LastPushSha record the last head change seen while the
	// item sat in the CI / Review column: from the webhook re-read of a
	// `pull_request` synchronize delivery (NotePush) or, as the fallback,
	// from the head change the next enumerate observed (NoteActivity). A
	// new waiting-on-push state clears them; leaving the column clears
	// them too.
	LastPushAt  time.Time `json:"last_push_at,omitempty"`
	LastPushSha string    `json:"last_push_sha,omitempty"`
	// LabelsSuppressed is set when the write-back gate kept the stage
	// labels off the item on GitHub (a contributor PR without the opt-in
	// label): the stage lives in this record only, so a label-less item
	// is not evidence that a park lifted (hivecommons/hive#15541 rows read
	// "PR in progress" while the record said 085).
	LabelsSuppressed bool `json:"labels_suppressed,omitempty"`
	// TrainEjectedWhy / TrainEjectedHead / TrainEjectedAt record the merge
	// train's last ejection of this PR (hivecommons/hive#15825): the head
	// it was ejected at, why (head-moved, ci-pending, ci-red,
	// verdict-owed, conflicting, …) and when. The tracker's gray pill
	// carries the lineage ("left merge train · pushed 3m ago") while the
	// row sits back in the CI / Review column; re-admission clears it.
	TrainEjectedWhy  string    `json:"train_ejected_why,omitempty"`
	TrainEjectedHead string    `json:"train_ejected_head,omitempty"`
	TrainEjectedAt   time.Time `json:"train_ejected_at,omitempty"`
}

// Activity is what the reconciler observed this cycle that a person (or
// CI) could have changed; see Store.NoteActivity.
type Activity struct {
	// Head is the current head SHA ("" for an issue).
	Head string
	// Comments is the issue + review comment count, or -1 when unknown
	// (the lane did not fetch the PR this cycle).
	Comments int
	// OwnComments is how many comments the hive posted on the item after
	// Comments was fetched this cycle. They are added to the stored count
	// but never compared, so the next fetch (which includes them) does not
	// read them as a person acting.
	OwnComments int
	// Reviews is the number of review submissions by people (the hive
	// app's own reviews excluded), or -1 when unknown (the lane did not
	// list reviews this cycle). It catches a body-only Approve or
	// Request-changes, which Comments (inline comments only) misses.
	Reviews int
	// Digest summarises the verdict, CI state and non-hive labels; any
	// change is activity.
	Digest string
	// At, when non-zero, is the real timestamp of the activity when the
	// caller has one; zero means "now" (the cycle that first observed it,
	// which is what the PR reconciler passes: the list payload carries no
	// commit or comment timestamps).
	At time.Time
	// WaitingOnHive says the item is waiting on the hive (checks running,
	// verdict pending): it must not stall.
	WaitingOnHive bool
}

// QuietSince is when the item's current quiet period began: the later of
// entering the stage and the last activity.
func (r Record) QuietSince() time.Time {
	if r.LastActivityAt.After(r.EnteredAt) {
		return r.LastActivityAt
	}
	return r.EnteredAt
}

// QuietFor is how long the item has been quiet at now (0 if unknown).
func (r Record) QuietFor(now time.Time) time.Duration {
	since := r.QuietSince()
	if since.IsZero() || !now.After(since) {
		return 0
	}
	return now.Sub(since)
}

// Hop is one stage entry in a Record's History: the stage entered, where
// it came from (None on the first observation), when and why. It is also
// the record of backward moves now that the stage/regressed label is
// retired.
type Hop struct {
	From   Stage     `json:"from,omitempty"`
	Stage  Stage     `json:"stage"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
	// Regressed marks a backward hop (IsRegression); it replaces the
	// retired stage/regressed label as the record of the move.
	Regressed bool `json:"regressed,omitempty"`
}

// HistoryLimit bounds Record.History so the store file stays small for an
// item that flaps between stages.
const HistoryLimit = 8

// appendHop appends h to history, dropping the oldest entries past
// HistoryLimit. The result never aliases history's backing array when it
// grows, so a Record copy held by a caller is not mutated.
func appendHop(history []Hop, h Hop) []Hop {
	out := make([]Hop, 0, len(history)+1)
	out = append(out, history...)
	out = append(out, h)
	if len(out) > HistoryLimit {
		out = out[len(out)-HistoryLimit:]
	}
	return out
}

// DCOFor returns the cached commit check for head: total non-merge commits,
// how many are unsigned, and whether the cache is for this head at all.
func (r Record) DCOFor(head string) (total, unsigned int, ok bool) {
	if head == "" || r.DCOHead != head {
		return 0, 0, false
	}
	return r.DCOTotal, r.DCOUnsigned, true
}

// DCOSplitKnown reports whether the cached commit check for head carries
// the hive/other ownership split of its unsigned commits (false for a
// signed head, a different head, or a record cached before the split).
func (r Record) DCOSplitKnown(head string) bool {
	_, unsigned, ok := r.DCOFor(head)
	return ok && unsigned > 0 && r.DCOUnsignedHive+r.DCOUnsignedOther == unsigned
}

// DCO is the cached verdict for head (DCOUnknown when not cached).
func (r Record) DCO(head string) DCOState {
	total, unsigned, ok := r.DCOFor(head)
	switch {
	case !ok || total == 0:
		return DCOUnknown
	case unsigned > 0:
		return DCONo
	}
	return DCOYes
}

// Regressed reports whether the item carries an unresolved regression.
func (r Record) Regressed() bool { return r.RegressedFrom != None }

// RegressionHop is the backward hop that opened the current regression:
// the latest History entry that fell from RegressedFrom. ok is false when
// the item is not regressed or the hop has aged out of History (the caller
// falls back to RegressedAt and the current stage).
func (r Record) RegressionHop() (hop Hop, ok bool) {
	if !r.Regressed() {
		return Hop{}, false
	}
	for i := len(r.History) - 1; i >= 0; i-- {
		if h := r.History[i]; h.Regressed && h.From == r.RegressedFrom {
			return h, true
		}
	}
	return Hop{}, false
}

// RegressionCommentDue reports whether the regression comment for the
// current regression still needs posting on head: not when it was already
// posted for this regression, nor when one was already posted for the same
// non-empty head SHA in an earlier recover→regress cycle.
func (r Record) RegressionCommentDue(head string) bool {
	if r.RegressionCommented {
		return false
	}
	head = strings.TrimSpace(head)
	return head == "" || !strings.EqualFold(r.RegressionCommentHead, head)
}

// Transition is what Observe reports about one stage update.
type Transition struct {
	Prev, Next Stage
	// Changed is true when the stage differs from the previous record.
	Changed bool
	// Regressed is true when this observation moved the stage backwards
	// (a new regression, which deserves a comment).
	Regressed bool
	// Recovered is true when a prior regression cleared because the stage
	// reached or passed the point it fell from.
	Recovered bool
}

// Store is the persisted per-item stage memory. One Store is shared
// process-wide (Shared) because the automerge sweep builds a fresh engine
// per tick.
type Store struct {
	mu    sync.Mutex
	path  string
	now   func() time.Time
	items map[string]Record
	dirty bool
	// seenSaved is each record's LastSeen as last written to (or read
	// from) the file; ObserveProgress dirties the store for a LastSeen-only
	// move once the saved value is LastSeenPersistEvery old.
	seenSaved map[string]time.Time
}

// NewStore returns an empty in-memory store that persists to path ("" =
// never persists; tests use this).
func NewStore(path string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{path: path, now: now, items: map[string]Record{}, seenSaved: map[string]time.Time{}}
}

// markSeenSaved notes the LastSeen the file now holds for every record.
// Caller holds s.mu.
func (s *Store) markSeenSaved() {
	s.seenSaved = make(map[string]time.Time, len(s.items))
	for k, r := range s.items {
		s.seenSaved[k] = r.LastSeen
	}
}

var (
	sharedOnce  sync.Once
	sharedStore *Store
)

// Shared returns the process-wide store backed by StatePath, loading it on
// first use. A missing or unreadable file starts empty: the only effect is
// that stalled timers restart from this cycle.
func Shared() *Store {
	sharedOnce.Do(func() {
		sharedStore = NewStore(StatePath, nil)
		_ = sharedStore.Load()
	})
	return sharedStore
}

// Key is the store key for repo#number. repo is owner/repo.
func Key(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + fmt.Sprint(number)
}

// Load replaces the in-memory items with the persisted file. A missing
// file is not an error.
func (s *Store) Load() error {
	if s == nil || s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var items map[string]Record
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("decoding %s: %w", s.path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if items == nil {
		items = map[string]Record{}
	}
	s.items = items
	s.markSeenSaved()
	s.dirty = false
	return nil
}

// Save writes the store if anything changed since the last Save/Load.
func (s *Store) Save() error {
	if s == nil || s.path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	data, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.markSeenSaved()
	s.dirty = false
	return nil
}

// Dirty reports whether anything changed since the last Save or Load.
func (s *Store) Dirty() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dirty
}

// Get returns the record for key.
func (s *Store) Get(key string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	return r, ok
}

// Observe records that key is now in stage for reason and reports the
// transition. A None stage is terminal: the record is dropped and the
// transition carries Prev so the caller can clear labels.
func (s *Store) Observe(key string, stage Stage, reason string) Transition {
	return s.ObserveProgress(key, stage, reason, false)
}

// ObserveProgress is Observe with the caller's knowledge of WHY the stage
// moved. progress reports that whatever took the item to stage was forward
// motion even if the stage number fell — an issue leaving 040 for 010
// because the PR covering it merged and addressed part of it. A fall that
// is progress books no regression and resolves one already open: the work
// landed, so nothing was lost.
func (s *Store) ObserveProgress(key string, stage Stage, reason string, progress bool) Transition {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	prev, had := s.items[key]
	tr := Transition{Prev: prev.Stage, Next: stage}
	if stage == None {
		if had {
			s.forgetStageLocked(key, prev)
			tr.Changed = prev.Stage != None
		}
		return tr
	}
	next := prev
	next.Stage = stage
	next.Reason = reason
	next.LastSeen = now
	if !had || prev.Stage != stage {
		next.EnteredAt = now
		next.ReasonChangedAt = now
		next.StallCommented = false
		next.EntryCommented = false
		tr.Changed = true
		next.notePushStageChange(now)
	} else if ReasonClass(prev.Reason) != ReasonClass(reason) {
		next.ReasonChangedAt = now
	}
	switch {
	case had && prev.Stage != None && !progress && IsRegression(prev.Stage, stage):
		// Backwards. A second fall while still regressed keeps the ORIGINAL
		// high-water mark so the narration names where the item first fell
		// from.
		if !prev.Regressed() || prev.RegressedFrom.Before(prev.Stage) {
			next.RegressedFrom = prev.Stage
		}
		next.RegressedAt = now
		next.RegressionCommented = false
		tr.Regressed = true
	case prev.Regressed() && (progress || prev.Stage.Before(stage) || !IsRegression(prev.RegressedFrom, stage)):
		// The regression is resolved the moment the item moves ahead: any
		// forward hop, a fall that is progress (the covering PR merged), or
		// a mark that a fall from is no longer counted (records written
		// before the rule changed heal here). A non-regression backward hop
		// (020 → 010 after a 040 → 020 fall, 072 → 070) neither books nor
		// clears. The owner's reading (hivecommons/hive#14570):
		// "regressed" on something that is moving ahead asks a person to
		// act on nothing, so the badge comes off with the first forward
		// step rather than waiting for the climb back to the high-water
		// mark.
		next.RegressedFrom = None
		next.RegressedAt = time.Time{}
		next.RegressionCommented = false
		tr.Recovered = true
	}
	if tr.Changed {
		next.History = appendHop(prev.History, Hop{From: prev.Stage, Stage: stage, At: now, Reason: reason, Regressed: tr.Regressed})
	}
	if !had || tr.Changed || !recordEqual(next, prev) || now.Sub(s.seenSaved[key]) >= LastSeenPersistEvery {
		s.dirty = true
	}
	s.items[key] = next
	return tr
}

// LastSeenPersistEvery bounds how stale a record's persisted LastSeen may
// be when a pass changes nothing else about it. A pass that re-observes
// the same stage and reason moves LastSeen in memory (the dashboard reads
// the live store, so "last checked" stays exact) but does not by itself
// dirty the store (hivecommons/hive#15729: a soaking PR rewrote the file
// every sweep); once the saved LastSeen is this old the pass writes it so
// Prune stays roughly right across a restart.
const LastSeenPersistEvery = 10 * time.Minute

// recordEqual compares two records field by field, ignoring History (a
// slice that makes Record incomparable with ==, and only changes when the
// stage does) and LastSeen (which moves on every pass and is persisted on
// its own schedule, LastSeenPersistEvery).
func recordEqual(a, b Record) bool {
	a.History, b.History = nil, nil
	a.LastSeen, b.LastSeen = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

// ReasonClass is the part of a reason that names what it means: the token
// up to any parenthesised detail, so `contributor-lane:soaking(remaining=12m0s)`
// (the pre-record soak reason) and `contributor-lane:soaking` are one
// class. Two passes whose reasons share a class did not change the note.
func ReasonClass(reason string) string {
	reason = strings.TrimSpace(reason)
	if i := strings.IndexByte(reason, '('); i >= 0 {
		reason = reason[:i]
	}
	return reason
}

// NoteChangedAt is when the record's note — its stage and reason class —
// last changed: ReasonChangedAt when recorded, else EnteredAt.
func (r Record) NoteChangedAt() time.Time {
	if !r.ReasonChangedAt.IsZero() {
		return r.ReasonChangedAt
	}
	return r.EnteredAt
}

// StageSinceAt is when the row's since-time starts: stage entry, or the
// later block lift when the sweep unblocked the item after it entered the
// stage (or before any stage was recorded).
func (r Record) StageSinceAt() time.Time {
	if r.UnblockedAt.After(r.EnteredAt) {
		return r.UnblockedAt
	}
	return r.EnteredAt
}

// NoteUnblocked records that the blocked sweep lifted key's block at now.
// A blocked issue carries no stage record (its stage is None), so a
// stage-less record is created to hold the time; the next ObserveProgress
// keeps UnblockedAt while it stages the issue, and a None observation
// drops the record as usual.
func (s *Store) NoteUnblocked(key string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.items[key]
	r.UnblockedAt = now
	if r.LastSeen.IsZero() {
		r.LastSeen = now
	}
	s.items[key] = r
	s.dirty = true
}

// NoteActivity folds this cycle's activity inputs into key's record and
// reports whether anything a person could have done changed since the last
// observation. The first observation of a record seeds LastActivityAt
// (with a.At when known, else now) so the quiet clock starts at upgrade
// rather than reading a long-idle item as stalled on the first pass.
func (s *Store) NoteActivity(key string, a Activity) (changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return false
	}
	now := s.now()
	at := a.At
	if at.IsZero() || at.After(now) {
		at = now
	}
	switch {
	case r.LastActivityAt.IsZero():
		changed = true
	case a.Head != "" && a.Head != r.ActivityHead:
		changed = true
	case a.Comments >= 0 && a.Comments > r.ActivityComments:
		changed = true
	case a.Reviews >= 0 && a.Reviews > r.ActivityReviews:
		changed = true
	case a.Digest != r.ActivityDigest:
		changed = true
	}
	if changed && at.After(r.LastActivityAt) {
		r.LastActivityAt = at
	}
	if a.Head != "" && r.ActivityHead != "" && a.Head != r.ActivityHead {
		// The enumerate fallback for a push no webhook announced
		// (hivecommons/hive#15784). A head the sweep evaluated and still
		// routed back to the author starts a fresh waiting-on-push state.
		switch {
		case r.Stage.WaitsOnPush():
			r.WaitingPushSince = at
			r.LastPushAt, r.LastPushSha = time.Time{}, ""
		case a.Head != r.LastPushSha:
			r.LastPushAt, r.LastPushSha = at, a.Head
		}
	}
	if changed {
		r.HiveHeldWarned = false
	}
	if a.Head != "" {
		r.ActivityHead = a.Head
	}
	if a.Comments >= 0 {
		r.ActivityComments = a.Comments + a.OwnComments
	}
	if a.Reviews >= 0 {
		r.ActivityReviews = a.Reviews
	}
	r.ActivityDigest = a.Digest
	r.WaitingOnHive = a.WaitingOnHive
	if !recordEqual(r, s.items[key]) {
		s.dirty = true
	}
	s.items[key] = r
	return changed
}

// notePushStageChange keeps the push-progress fields in step with a stage
// change (hivecommons/hive#15784): entering 071 / 072 starts a fresh
// waiting-on-push state (a push before it is history), leaving them keeps
// the push on record so the row can say "pushed abc1234" while CI and the
// re-review run, and leaving the CI / Review column drops it all.
func (r *Record) notePushStageChange(now time.Time) {
	switch {
	case r.Stage.WaitsOnPush():
		r.WaitingPushSince = now
		r.LastPushAt, r.LastPushSha = time.Time{}, ""
	case r.Stage.TracksPush():
		r.WaitingPushSince = time.Time{}
	default:
		r.WaitingPushSince = time.Time{}
		r.LastPushAt, r.LastPushSha = time.Time{}, ""
	}
}

// NotePush records that key's head moved to sha at the given instant, as a
// webhook re-read of a `pull_request` synchronize delivery saw it
// (hivecommons/hive#15784). It reports whether a push was booked: a head the
// store already knows (the enumerate's ActivityHead, or the last push) is
// not one, and nothing is booked for a record whose head was never seen
// — a first read cannot tell a push from the initial head. A zero or
// future instant reads as now.
func (s *Store) NotePush(key, sha string, at time.Time) bool {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || !r.Stage.TracksPush() {
		return false
	}
	known := r.ActivityHead
	if r.LastPushSha != "" {
		known = r.LastPushSha
	}
	if known == "" || strings.EqualFold(sha, known) {
		return false
	}
	now := s.now()
	if at.IsZero() || at.After(now) {
		at = now
	}
	r.LastPushAt, r.LastPushSha = at, sha
	s.items[key] = r
	s.dirty = true
	return true
}

// NoteTrainEjected records that the merge train ejected an existing
// record's PR at head for why (hivecommons/hive#15825). A zero or future
// instant reads as now.
func (s *Store) NoteTrainEjected(key, head, why string, now time.Time) {
	why = strings.TrimSpace(why)
	if why == "" {
		s.ClearTrainEjected(key)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	at := now
	if clock := s.now(); at.IsZero() || at.After(clock) {
		at = clock
	}
	if r.TrainEjectedWhy == why && r.TrainEjectedHead == head {
		return
	}
	r.TrainEjectedWhy, r.TrainEjectedHead, r.TrainEjectedAt = why, strings.TrimSpace(head), at
	s.items[key] = r
	s.dirty = true
}

// ClearTrainEjected forgets a record's merge-train ejection: the PR rides
// a train again, or merged.
func (s *Store) ClearTrainEjected(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || (r.TrainEjectedWhy == "" && r.TrainEjectedHead == "" && r.TrainEjectedAt.IsZero()) {
		return
	}
	r.TrainEjectedWhy, r.TrainEjectedHead, r.TrainEjectedAt = "", "", time.Time{}
	s.items[key] = r
	s.dirty = true
}

// TrainEjected reports the recorded merge-train ejection.
func (r Record) TrainEjected() (why, head string, at time.Time, ok bool) {
	if r.TrainEjectedWhy == "" {
		return "", "", time.Time{}, false
	}
	return r.TrainEjectedWhy, r.TrainEjectedHead, r.TrainEjectedAt, true
}

// PushedSinceWaiting reports whether a push landed after the current
// waiting-on-push state began: the ball is back with the hive even if the
// sweep has not re-staged the item yet.
func (r Record) PushedSinceWaiting() bool {
	return !r.LastPushAt.IsZero() && !r.LastPushAt.Before(r.WaitingPushSince)
}

// CountOwnComments records that the hive itself posted n comments on key,
// so the next observed comment count does not read as outside activity.
// The credit sits on top of the last count NoteActivity stored, so it must
// be applied after this cycle's NoteActivity (or in a cycle that observes
// no count at all): a NoteActivity that follows it overwrites the credit
// with the pre-comment count and the next fetch reads the hive's own
// comment as a person acting.
func (s *Store) CountOwnComments(key string, n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.ActivityComments += n
	s.items[key] = r
	s.dirty = true
}

// IsRegression reports whether moving from → to is a backward transition
// worth flagging. A lower number is backward, except leaving a claim (020 is
// a reservation with nothing built behind it), leaving a repair stage
// (fixing, needs-author, returned-to-contributor, adopted-by-hive) for review of the new
// head, which is the repair route itself, not a second regression, and
// leaving the human stages: 045, 085 and 090 are parking stages ("the hive
// cannot act"), not progress, so moving out of them means the picture
// changed, whatever the destination — 090 → 072 is the hive learning the
// head is red, not the author losing ground (hivecommons/hive#12403 carried
// stage/regressed through 090 → 050 → 072 churn that was never a step
// backwards for the author).
func IsRegression(from, to Stage) bool {
	if !to.Before(from) {
		return false
	}
	switch from {
	case Claimed:
		// 020 is a reservation, not progress: a claim or start mark with
		// no PR behind it. Releasing it, letting it lapse or the prior-art
		// gate parking the issue (→ 010 / 015 / 017) puts the issue back in
		// the pool exactly as it was before the claim; nothing anyone built
		// was lost, so there is nothing to have regressed from. The
		// scanner's split-out issues wore a red "regressed" badge minutes
		// after being filed because of this edge alone.
		return false
	case PRInProgress:
		// 030 → 020 is the same claim, still held, with the PR-under-way
		// evidence behind it gone quiet for a tick: hivecommons/hive#14525
		// and rationguard#396 flapped 030 ↔ 020 every renewal and wore a
		// red "regressed" badge for it. Nothing enumerated existed at 030
		// (an open PR would be 040), so the holder lost no ground. Losing
		// the claim itself (→ 010 / 015) stays a fall: an agent announced a
		// PR and vanished.
		return to != Claimed
	case CoveredByPR:
		// 040 → 030 is the agent going back to work on the PR that covers
		// the issue (a live claim with the PR under way): the PR is still
		// open, so the issue has not lost ground.
		return to != PRInProgress
	case AdoptedByHive:
		// 074 → 072 is an adoption that was declined (superseder closed
		// unmerged): the PR is handed back, not knocked back.
		if to == ReturnedToContributor || to == NeedsAuthor {
			return false
		}
		return to != ReviewPending && to != Reviewed
	case Fixing, NeedsAuthor, ReturnedToContributor:
		// 070/071/072 are one "someone must fix this head" state split by
		// who owes the fix. Moving between them re-attributes the work
		// (072 → 070 when the hive's own follow-up commit is what broke
		// the squash sign-off gate, 072 → 071 when the author kind is
		// learned late); nothing moved backwards for the author.
		if to == Fixing || to == NeedsAuthor || to == ReturnedToContributor {
			return false
		}
		return to != ReviewPending && to != Reviewed
	case ReadyForHumanMerge, NeedsDecision, IssueNeedsDecision, PossiblyExisting:
		// Leaving a parking bay (017 included: the author confirmed the
		// gap) is never a step back.
		return false
	case TriageNeeded:
		// 015 → 010 is a maintainer triaging the issue (triage/accepted,
		// hive/verified-open, approved-direction added or needs-triage
		// removed): the only lower stage, and it is progress, not a fall.
		return false
	case MergeQueue:
		// 100 → 095 is a reclassification, not a knock-back: the PR was
		// soaking all along and records written before 095 existed called
		// that merge-queue. Nothing moved backwards for the author.
		return to != PreMergeSoak
	}
	return true
}

// MarkRegressionCommented records that the regression comment was posted.
func (s *Store) MarkRegressionCommented(key string) {
	s.MarkRegressionCommentedForHead(key, "")
}

// MarkRegressionCommentedForHead records that the regression comment was
// posted for head, so later regressions on the same head stay silent (see
// RegressionCommentDue). An empty head records the comment only for the
// current regression.
func (s *Store) MarkRegressionCommentedForHead(key, head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	head = strings.TrimSpace(head)
	if r.RegressionCommented && (head == "" || r.RegressionCommentHead == head) {
		return
	}
	r.RegressionCommented = true
	if head != "" {
		r.RegressionCommentHead = head
	}
	s.items[key] = r
	s.dirty = true
}

// MarkEntryCommented records that the entry comment for the current stage
// visit was posted for the given head.
func (s *Store) MarkEntryCommented(key, head string) {
	s.MarkEntryCommentedCause(key, head, "")
}

// MarkEntryCommentedCause is MarkEntryCommented that also remembers which
// cause (EntryCause of the comment's reasons) the comment explained. A new
// head or stage starts the cause list over.
func (s *Store) MarkEntryCommentedCause(key, head, cause string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	sameVisit := r.EntryCommented && r.EntryCommentedHead == head && r.EntryCommentedStage == r.Stage
	if sameVisit && (cause == "" || slices.Contains(r.EntryCommentedCauses, cause)) {
		return
	}
	if r.EntryCommentedHead != head || r.EntryCommentedStage != r.Stage {
		r.EntryCommentedCauses = nil
	}
	r.EntryCommented = true
	r.EntryCommentedHead = head
	r.EntryCommentedStage = r.Stage
	if cause != "" && !slices.Contains(r.EntryCommentedCauses, cause) {
		r.EntryCommentedCauses = append(r.EntryCommentedCauses, cause)
	}
	s.items[key] = r
	s.dirty = true
}

// EntryCause digests an entry comment's reasons (the bullet list, not the
// evidence summary whose soak clock ticks) so two comments that would tell
// the maintainer the same thing compare equal.
func EntryCause(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(reasons, "\n")))
	return hex.EncodeToString(sum[:8])
}

// MarkEntryCommentedDecisionKey records the decision identity of the 085
// entry comment just posted (or found already explained).
func (s *Store) MarkEntryCommentedDecisionKey(key, decisionKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.EntryCommentedDecisionKey == decisionKey {
		return
	}
	r.EntryCommentedDecisionKey = decisionKey
	s.items[key] = r
	s.dirty = true
}

// EntryCommentRepeatsDecision reports whether an 085 entry comment with
// this decision identity was already posted on this PR — on any head: a
// reviewer re-asking the same question on a new head (one that only
// merged the base) is not a new decision (hivecommons/hive#15499).
func (r Record) EntryCommentRepeatsDecision(decisionKey string) bool {
	return decisionKey != "" && r.EntryCommentedStage == NeedsDecision && r.EntryCommentedDecisionKey == decisionKey
}

// EntryCommentExplains reports whether an entry comment already posted for
// this stage on head covered the given cause (EntryCommented itself may be
// reset by a flap through another stage). Records written before
// causes were tracked (empty list) are taken as explained: the cause they
// posted is unknown, and a repost on every parked PR after an upgrade is
// worse than one missed refresh.
func (r Record) EntryCommentExplains(head, cause string) bool {
	if r.EntryCommentedHead != head || r.EntryCommentedStage != r.Stage {
		return false
	}
	return len(r.EntryCommentedCauses) == 0 || cause == "" || slices.Contains(r.EntryCommentedCauses, cause)
}

// MarkStallCommented records that the stall note for this stage visit was posted.
func (s *Store) MarkStallCommented(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.StallCommented {
		return
	}
	r.StallCommented = true
	s.items[key] = r
	s.dirty = true
}

// MarkNudged records that nudge number n for head was posted at now. A new
// head restarts the count and the adoption grace; online says whether the
// author's relay was reachable (first nudge decides the grace kind).
func (s *Store) MarkNudged(key, head string, n int, online bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	if r.NudgeHead != head {
		r.NudgeCount = 0
		r.FirstNudgeAt = now
		r.NudgeOnline = online
	}
	if r.FirstNudgeAt.IsZero() {
		r.FirstNudgeAt = now
		r.NudgeOnline = online
	}
	r.NudgeHead = head
	if n > r.NudgeCount {
		r.NudgeCount = n
	} else {
		r.NudgeCount++
	}
	r.LastNudgeAt = now
	s.items[key] = r
	s.dirty = true
}

// Adoption modes (Record.AdoptMode).
const (
	AdoptModeSupersede = "supersede"
	AdoptModeInPlace   = "in-place"
)

// NudgeStart is when the adoption grace for NudgeHead starts counting:
// FirstNudgeAt, or LastNudgeAt for records written before FirstNudgeAt
// existed (a single nudge recorded only its time). Zero when never nudged.
func (r Record) NudgeStart() time.Time {
	if !r.FirstNudgeAt.IsZero() {
		return r.FirstNudgeAt
	}
	if r.NudgeCount > 0 {
		return r.LastNudgeAt
	}
	return time.Time{}
}

// Adopted reports whether head is the head the hive adopted (stage 074).
func (r Record) Adopted(head string) bool {
	return head != "" && r.AdoptedHead == head
}

// AdoptedInPlace reports whether the hive adopted this PR in place (its fix
// lane owns the PR's own branch) and the adoption is still live. The head is
// not compared: the hive's own pushes move it (see Store.MarkAdoptedHead).
func (r Record) AdoptedInPlace() bool {
	return r.AdoptedHead != "" && r.AdoptMode == AdoptModeInPlace
}

// HivePushedHead reports whether the hive recorded that it pushed head.
func (r Record) HivePushedHead(head string) bool {
	head = strings.TrimSpace(head)
	if head == "" {
		return false
	}
	for _, h := range r.HivePushedHeads {
		if h == head {
			return true
		}
	}
	return false
}

// HasHivePushEvidence reports whether any item in repo has recorded a
// hive-pushed head. It lets the adoption reconciler keep old/first-deploy
// in-place adoptions fail-open until the new attribution evidence exists.
func (s *Store) HasHivePushEvidence(repo string) bool {
	if s == nil {
		return false
	}
	prefix := Key(repo, 0)
	prefix = strings.TrimSuffix(prefix, "#0") + "#"
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, r := range s.items {
		if strings.HasPrefix(key, prefix) && len(r.HivePushedHeads) > 0 {
			return true
		}
	}
	return false
}

// AdoptionLive reports whether the hive currently owns this PR (stage 074):
// an in-place adoption that was not yielded, or a superseding adoption of
// exactly this head. Adoption is an ownership state, not a CI state — it
// holds across CI going green or a verdict arriving, until the adoption is
// released (ClearAdoption / MarkYielded) or the PR reaches the merge gate
// (Stage.YieldsToAdoption; hivecommons/hive#12736).
func (r Record) AdoptionLive(head string) bool {
	return r.AdoptedInPlace() || r.Adopted(head)
}

// AdoptedHeadSince is when the adopted head last moved: the hive's last
// recorded push under an in-place adoption, else the adoption itself. Zero
// when the record carries no adoption.
func (r Record) AdoptedHeadSince() time.Time {
	if !r.AdoptedHeadAt.IsZero() {
		return r.AdoptedHeadAt
	}
	return r.AdoptedAt
}

// AdoptionEscalated reports whether the adoption at head is currently a
// person's (stage 085) for sitting past the 074 dwell.
func (r Record) AdoptionEscalated(head string) bool {
	return head != "" && r.AdoptEscalatedHead == head
}

// AdoptionEscalationNoted reports whether the escalation note was already
// posted for head, whether or not the escalation still holds.
func (r Record) AdoptionEscalationNoted(head string) bool {
	return head != "" && r.AdoptEscalationNotedHead == head
}

// DCORepairAttempted reports whether the in-place sign-off rewrite already
// ran for head.
func (r Record) DCORepairAttempted(head string) bool {
	return head != "" && r.DCORepairHead == head
}

// AdoptionRecoveryKey is the "<head>@<base>" pair AdoptRecoveryAttempt stores.
func AdoptionRecoveryKey(head, base string) string { return head + "@" + base }

// AdoptionRecoveryAttempted reports whether the stale-red recovery already
// ran for head against this base tip.
func (r Record) AdoptionRecoveryAttempted(head, base string) bool {
	return head != "" && base != "" && r.AdoptRecoveryAttempt == AdoptionRecoveryKey(head, base)
}

// MergeCommitNoted reports whether the merge-method note was posted for head.
func (r Record) MergeCommitNoted(head string) bool {
	return head != "" && r.MergeCommitNotedHead == head
}

// AdoptedHeadClockUnset reports a live in-place adoption whose head clock
// was never stamped: a record written before AdoptedHeadAt existed.
func (r Record) AdoptedHeadClockUnset() bool {
	return r.AdoptedInPlace() && r.AdoptedHeadAt.IsZero()
}

// AdoptionAttemptedNothingOpened reports a supersession that was recorded
// for head but opened no App PR (the push or create failed): the hive owns
// the PR on paper and nothing is working it.
func (r Record) AdoptionAttemptedNothingOpened(head string) bool {
	return r.Adopted(head) && r.AdoptMode == AdoptModeSupersede && r.AdoptedBy == 0
}

// MarkAdoptedInPlace records an in-place adoption of head for reason.
func (s *Store) MarkAdoptedInPlace(key, head, reason string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.AdoptedHead, r.AdoptedBy, r.AdoptedAt, r.AdoptCheckedAt = head, 0, now, now
	r.AdoptMode, r.AdoptReason = AdoptModeInPlace, reason
	r.AdoptBlockedHead, r.AdoptBlockedBy, r.AdoptBlockedAt = "", "", time.Time{}
	r.AdoptedHeadAt = now
	s.items[key] = r
	s.dirty = true
}

// StampAdoptedHeadAt starts the adopted-head clock at now on a live in-place
// adoption of head that has none (hivecommons/hive#13040: a record written
// before AdoptedHeadAt existed, whose AdoptedAt may predate the head by
// days). Reports whether it stamped.
func (s *Store) StampAdoptedHeadAt(key, head string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || head == "" || r.AdoptedHead != head || !r.AdoptedHeadClockUnset() {
		return false
	}
	r.AdoptedHeadAt = s.now()
	s.items[key] = r
	s.dirty = true
	return true
}

// MarkAdoptedHead moves a live adoption to a new head the hive itself
// pushed, so the record keeps reading as adopted.
func (s *Store) MarkAdoptedHead(key, head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.AdoptedHead == "" || head == "" {
		return
	}
	if r.AdoptedHead != head {
		r.AdoptedHeadAt = s.now()
	}
	r.AdoptedHead = head
	s.items[key] = r
	s.dirty = true
}

// MarkHivePushedHead records a head that the hive itself pushed to an
// in-place-adopted PR. It is a no-op unless the record is a live in-place
// adoption, so stray push-site calls cannot create ownership.
func (s *Store) MarkHivePushedHead(key, head string) {
	head = strings.TrimSpace(head)
	if head == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || !r.AdoptedInPlace() {
		return
	}
	for _, h := range r.HivePushedHeads {
		if h == head {
			return
		}
	}
	r.HivePushedHeads = append(r.HivePushedHeads, head)
	s.items[key] = r
	s.dirty = true
}

// MarkYielded forgets an in-place adoption because the contributor pushed
// head themselves, remembering the head so the yield note posts once.
func (s *Store) MarkYielded(key, head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.AdoptedHead, r.AdoptedBy, r.AdoptedAt, r.AdoptCheckedAt = "", 0, time.Time{}, time.Time{}
	r.AdoptMode, r.AdoptReason, r.AdoptDiffFence = "", "", ""
	r.AdoptedHeadAt, r.AdoptEscalatedHead, r.AdoptEscalatedReason, r.AdoptEscalationNotedHead = time.Time{}, "", "", ""
	r.AdoptRecoveryAttempt, r.AdoptRecoveryCount = "", 0
	r.YieldedHead = head
	s.items[key] = r
	s.dirty = true
}

// MarkAdopted records the stage-074 adoption of head by superseding PR `by`
// (0 = attempted, nothing opened).
func (s *Store) MarkAdopted(key, head string, by int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.AdoptedHead, r.AdoptedBy, r.AdoptedAt, r.AdoptCheckedAt = head, by, now, now
	r.AdoptMode = AdoptModeSupersede
	r.AdoptCopyConflicting = false
	r.AdoptBlockedHead, r.AdoptBlockedBy, r.AdoptBlockedAt = "", "", time.Time{}
	r.AdoptForcedBy, r.AdoptForcedAt = "", time.Time{}
	r.RerouteTo, r.RerouteBy, r.RerouteAt, r.RerouteReason, r.RerouteHead = "", "", time.Time{}, "", ""
	s.items[key] = r
	s.dirty = true
}

// AdoptBlockedAuthorMaintainer is the AdoptBlockedBy reason for a due
// adoption held under governor.labels.stage.adoption.maintainer_policy: ask.
const AdoptBlockedAuthorMaintainer = "author-is-maintainer"

// AdoptBlockedSelfAuthored is the AdoptBlockedBy reason for a delegated
// supersede of the hive App's own PR: there is no contributor branch to
// re-home, so the request is consumed with one note instead of looping.
const AdoptBlockedSelfAuthored = "self-authored"

// AdoptBlockedSupersedeConflict is the AdoptBlockedBy reason (and the 07x
// lane reason) for a supersession whose head could not be replayed onto a
// fresh base: the hive copy would open conflicting, so the attempt is
// recorded, the original keeps its lane and one note names the failure.
const AdoptBlockedSupersedeConflict = "supersede-conflict"

// AdoptionRequestConsumed reports whether a delegated adoption request at
// head was settled by a terminal refusal that recorded no adoption: the
// request label clears so the engine does not retry every sweep.
func (r Record) AdoptionRequestConsumed(head string) bool {
	switch r.AdoptionBlocked(head) {
	case AdoptBlockedSelfAuthored, AdoptBlockedSupersedeConflict:
		return true
	}
	return false
}

// NoteAdoptionBlocked records that adoption is due at head but held for
// reason; the first sighting's time is kept while head and reason hold.
func (s *Store) NoteAdoptionBlocked(key, head, reason string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || head == "" || reason == "" {
		return
	}
	if r.AdoptBlockedHead == head && r.AdoptBlockedBy == reason {
		return
	}
	r.AdoptBlockedHead, r.AdoptBlockedBy, r.AdoptBlockedAt = head, reason, now
	s.items[key] = r
	s.dirty = true
}

// AdoptionOrphanLoopWindow is how soon after one orphan recovery a second
// closed copy counts as a loop: the PR then parks at 085 with
// LaneReasonAdoptionChildClosedTwice instead of a third copy being opened.
const AdoptionOrphanLoopWindow = time.Hour

// LaneReasonAdoptionChildClosedTwice is the 085 reason for an adopted PR
// whose hive copy was closed unmerged twice within AdoptionOrphanLoopWindow:
// something keeps closing the copy and a person must say what.
const LaneReasonAdoptionChildClosedTwice = "adoption-child-closed-twice"

// NoteAdoptionOrphaned records that superseding PR `by` was closed without
// merging while the adoption stood (hivecommons/hive#15052).
func (s *Store) NoteAdoptionOrphaned(key string, by int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || by <= 0 {
		return
	}
	r.AdoptOrphanedBy, r.AdoptOrphanedAt = by, now
	r.AdoptOrphanCount++
	s.items[key] = r
	s.dirty = true
}

// AdoptionOrphanedWithin reports whether an orphan was recorded less than
// window before now: a second closed copy inside it is a loop.
func (r Record) AdoptionOrphanedWithin(now time.Time, window time.Duration) bool {
	return r.AdoptOrphanCount > 0 && !r.AdoptOrphanedAt.IsZero() && now.Sub(r.AdoptOrphanedAt) < window
}

// AdoptionChildClosedTwice reports the loop-guard park: the adoption stands
// on record, escalated at its head for LaneReasonAdoptionChildClosedTwice.
func (r Record) AdoptionChildClosedTwice() bool {
	return r.AdoptEscalatedReason == LaneReasonAdoptionChildClosedTwice && r.AdoptEscalatedHead != "" && r.AdoptEscalatedHead == r.AdoptedHead
}

// MarkAdoptMarkerScanned records that the PR's comments were read for an
// adoption marker at head.
func (s *Store) MarkAdoptMarkerScanned(key, head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || head == "" || r.AdoptMarkerScanHead == head {
		return
	}
	r.AdoptMarkerScanHead = head
	s.items[key] = r
	s.dirty = true
}

// ClearAdoptionBlocked forgets a held adoption (it went ahead, or the
// policy no longer holds it).
func (s *Store) ClearAdoptionBlocked(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.AdoptBlockedHead == "" && r.AdoptBlockedBy == "" {
		return
	}
	r.AdoptBlockedHead, r.AdoptBlockedBy, r.AdoptBlockedAt = "", "", time.Time{}
	s.items[key] = r
	s.dirty = true
}

// AdoptionBlocked reports the reason a due adoption at head is held, or "".
func (r Record) AdoptionBlocked(head string) string {
	if head == "" || r.AdoptBlockedHead != head || r.Adopted(head) {
		return ""
	}
	return r.AdoptBlockedBy
}

// AdoptBlockedOperatorAdmission is the AdoptBlockedBy reason for a due
// adoption the engine refused because a person admitted the head
// (Record.Admission): their approval said "merge this as-is", so the hive
// does not take the PR over (hivecommons/hive#15136).
const AdoptBlockedOperatorAdmission = "operator-admission"

// AdoptBlockedAuthorPerson is the AdoptBlockedBy reason for an adoption the
// engine refused because a person pushed the PR themselves (author/human,
// or one of this hive's owners): there is no contributor hive to be
// offline, so the hive asks the author instead (hivecommons/hive#15202).
const AdoptBlockedAuthorPerson = "author-is-person"

// AdmittedByOperatorApprovalPrefix prefixes AdmittedBy when the admission
// came from a person's Approve: "operator-approval:octocat".
const AdmittedByOperatorApprovalPrefix = "operator-approval:"

// OperatorApprovalAdmission is the AdmittedBy value for login's Approve.
func OperatorApprovalAdmission(login string) string {
	login = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(login), "@"))
	if login == "" {
		return ""
	}
	return AdmittedByOperatorApprovalPrefix + login
}

// AdmissionLogin returns the person behind an AdmittedBy value ("" when it
// is not an operator approval).
func AdmissionLogin(admittedBy string) string {
	if !strings.HasPrefix(admittedBy, AdmittedByOperatorApprovalPrefix) {
		return ""
	}
	return strings.TrimPrefix(admittedBy, AdmittedByOperatorApprovalPrefix)
}

// NoteAdmission records that `by` admitted head on an existing record
// (hivecommons/hive#15136). It returns true when the record changed.
func (s *Store) NoteAdmission(key, head, by string, now time.Time) bool {
	head, by = strings.TrimSpace(head), strings.TrimSpace(by)
	if head == "" || by == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || (r.AdmittedHead == head && r.AdmittedBy == by) {
		return false
	}
	r.AdmittedHead, r.AdmittedBy, r.AdmittedAt = head, by, now
	s.items[key] = r
	s.dirty = true
	return true
}

// Admission reports who admitted head ("" when nobody did, or the admission
// is for another head).
func (r Record) Admission(head string) string {
	head = strings.TrimSpace(head)
	if head == "" || r.AdmittedHead != head {
		return ""
	}
	return r.AdmittedBy
}

// NoteAdoptionForced records an operator's "Adopt now" by login at now. The
// pending reroute (if any) is dropped: a forced adoption supersedes it. So
// is a recorded adoption blocker (a person's admission, maintainer policy):
// the forced adoption outranks it, and the tracker must stop saying "not
// adopted" the moment the operator asked.
func (s *Store) NoteAdoptionForced(key, login string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || login == "" {
		return
	}
	r.AdoptForcedBy, r.AdoptForcedAt = login, now
	r.RerouteTo, r.RerouteBy, r.RerouteAt, r.RerouteReason, r.RerouteHead = "", "", time.Time{}, "", ""
	r.AdoptBlockedHead, r.AdoptBlockedBy, r.AdoptBlockedAt = "", "", time.Time{}
	s.items[key] = r
	s.dirty = true
}

// ClearAdoptionForced forgets a forced adoption (it was attempted).
func (s *Store) ClearAdoptionForced(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.AdoptForcedBy == "" {
		return
	}
	r.AdoptForcedBy, r.AdoptForcedAt = "", time.Time{}
	s.items[key] = r
	s.dirty = true
}

// AdoptionForced reports the operator who forced adoption of this PR and
// whether that force is still pending (no attempt recorded yet).
func (r Record) AdoptionForced() (string, bool) {
	return r.AdoptForcedBy, r.AdoptForcedBy != ""
}

// NoteReroute records that by rerouted the PR's follow-up work at head to
// login for reason.
func (s *Store) NoteReroute(key, head, login, by, reason string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || login == "" {
		return
	}
	r.RerouteTo, r.RerouteBy, r.RerouteAt, r.RerouteReason, r.RerouteHead = login, by, now, reason, head
	r.RerouteFallbackAt = time.Time{}
	s.items[key] = r
	s.dirty = true
}

// ClearReroute ends a reroute (picked up, head moved, adopted, or fallen
// back). fallback stamps RerouteFallbackAt so the row can say the pickup
// window lapsed.
func (s *Store) ClearReroute(key string, fallback bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.RerouteTo == "" {
		return
	}
	r.RerouteTo, r.RerouteBy, r.RerouteAt, r.RerouteReason, r.RerouteHead = "", "", time.Time{}, "", ""
	if fallback {
		r.RerouteFallbackAt = now
	}
	s.items[key] = r
	s.dirty = true
}

// Rerouted reports the clanker login the PR's work at head is currently
// rerouted to, or "" (no reroute, or the head has moved since).
func (r Record) Rerouted(head string) string {
	if r.RerouteTo == "" || (head != "" && r.RerouteHead != "" && r.RerouteHead != head) {
		return ""
	}
	return r.RerouteTo
}

// NoteAdoptReason records why the current adoption happened (the marker's
// reason=<r>) on an existing record.
func (s *Store) NoteAdoptReason(key, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.AdoptReason == reason {
		return
	}
	r.AdoptReason = reason
	s.items[key] = r
	s.dirty = true
}

// MarkAdoptChecked records that the superseding PR was polled at now.
func (s *Store) MarkAdoptChecked(key string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.AdoptCheckedAt = now
	s.items[key] = r
	s.dirty = true
}

// MarkAdoptionEscalated records that the adoption at head is a person's
// for reason (stage 085) and that head has been noted; AdoptionEscalated
// (head) then holds until ClearAdoptionEscalation or the head changes.
func (s *Store) MarkAdoptionEscalated(key, head, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || head == "" || (r.AdoptEscalatedHead == head && r.AdoptEscalatedReason == reason && r.AdoptEscalationNotedHead == head) {
		return
	}
	r.AdoptEscalatedHead, r.AdoptEscalatedReason, r.AdoptEscalationNotedHead = head, reason, head
	s.items[key] = r
	s.dirty = true
}

// MarkDCORepair records one in-place sign-off rewrite attempt for head.
// Recorded before the API call, as every adoption path does, so a refused
// rewrite is not retried each sweep.
func (s *Store) MarkDCORepair(key, head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || head == "" || r.DCORepairHead == head {
		return
	}
	r.DCORepairHead = head
	s.items[key] = r
	s.dirty = true
}

// MarkAdoptionRecovery records one stale-red recovery attempt for head
// against base (hivecommons/hive#14500). Recorded before the API call, as
// every adoption path does, so a refused call is not retried each sweep.
func (s *Store) MarkAdoptionRecovery(key, head, base string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || head == "" || base == "" {
		return
	}
	pair := AdoptionRecoveryKey(head, base)
	if r.AdoptRecoveryAttempt == pair {
		return
	}
	r.AdoptRecoveryAttempt = pair
	r.AdoptRecoveryCount++
	s.items[key] = r
	s.dirty = true
}

// MarkMergeCommitNoted records that the merge-method note was posted for head.
func (s *Store) MarkMergeCommitNoted(key, head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || head == "" || r.MergeCommitNotedHead == head {
		return
	}
	r.MergeCommitNotedHead = head
	s.items[key] = r
	s.dirty = true
}

// ClearAdoptionEscalation releases a stage-085 escalation whose reason no
// longer holds (the blocker cleared), returning the adoption to 074 flow.
// The noted head is kept so the same head is not noted twice.
func (s *Store) ClearAdoptionEscalation(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || (r.AdoptEscalatedHead == "" && r.AdoptEscalatedReason == "") {
		return
	}
	r.AdoptEscalatedHead, r.AdoptEscalatedReason = "", ""
	s.items[key] = r
	s.dirty = true
}

// ClearAdoption forgets the adoption (the superseding PR closed unmerged).
func (s *Store) ClearAdoption(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.AdoptedHead, r.AdoptedBy, r.AdoptedAt, r.AdoptCheckedAt = "", 0, time.Time{}, time.Time{}
	r.AdoptMode, r.AdoptReason, r.AdoptDiffFence = "", "", ""
	r.AdoptedHeadAt, r.AdoptEscalatedHead, r.AdoptEscalatedReason, r.AdoptEscalationNotedHead = time.Time{}, "", "", ""
	r.AdoptRecoveryAttempt, r.AdoptRecoveryCount = "", 0
	r.AdoptOrphanedBy, r.AdoptOrphanedAt, r.AdoptOrphanCount = 0, time.Time{}, 0
	r.SupersededBy, r.SupersededAt, r.SupersededOpen, r.SupersederMergedNotedHead = 0, time.Time{}, false, ""
	r.SupersededClosedFirst, r.AdoptCopyRedHeads, r.AdoptReopenedFrom = false, nil, 0
	r.AdoptCopyConflicting = false
	s.items[key] = r
	s.dirty = true
}

// NoteAdoptCopyConflicting records whether the hive's copy of this
// adoption is conflicting with its base and in the fixer's hands
// (hivecommons/hive#15307). No-op when nothing changes.
func (s *Store) NoteAdoptCopyConflicting(key string, conflicting bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.AdoptCopyConflicting == conflicting {
		return
	}
	r.AdoptCopyConflicting = conflicting
	s.items[key] = r
	s.dirty = true
}

// MarkSuperseded records that superseding PR `by` merged and the original
// was told (hivecommons/hive#14471). open is true when the original could
// not be closed and stays open with the note; the tracker drops it either
// way. Idempotent: a record already superseded by the same PR is unchanged.
func (s *Store) MarkSuperseded(key string, by int, open bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || by <= 0 {
		return
	}
	if r.SupersededBy == by && r.SupersededOpen == open {
		return
	}
	if r.SupersededBy != by {
		r.SupersededAt = now
	}
	r.SupersededBy, r.SupersededOpen = by, open
	s.items[key] = r
	s.dirty = true
}

// MarkSupersederMergedNoted records that the diverged-head note was posted
// for head, so the next poll of the same head stays silent.
func (s *Store) MarkSupersederMergedNoted(key, head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || head == "" || r.SupersederMergedNotedHead == head {
		return
	}
	r.SupersederMergedNotedHead = head
	s.items[key] = r
	s.dirty = true
}

// Superseded reports whether the adoption's superseding PR merged and the
// original was told — or, under close-first supersession, the original was
// closed in favour of a copy still in flight: the row is finished whatever
// its labels say.
func (r Record) Superseded() bool {
	return r.SupersededBy > 0
}

// AdoptionReopenWindow is how long after a reopen-on-failure the same
// original is not reopened again (hivecommons/hive#15135): a copy that
// keeps failing must not flip the original closed/open every sweep.
const AdoptionReopenWindow = 24 * time.Hour

// LaneReasonAdoptionCopyFailed is the 085 reason for an original the hive
// reopened because its close-first copy was closed unmerged, stayed red
// across two heads, or could not be pushed: a person must decide.
const LaneReasonAdoptionCopyFailed = "adoption-copy-failed"

// MarkSupersededClosedFirst records that the original was closed as soon
// as superseding PR `by` existed (close-first supersession). The copy is
// still polled; see CloseFirstPollDue. open is true when GitHub refused
// the close and the original stays open with the note.
func (s *Store) MarkSupersededClosedFirst(key string, by int, open bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || by <= 0 {
		return
	}
	if r.SupersededBy != by {
		r.SupersededAt = now
	}
	r.SupersededBy, r.SupersededOpen, r.SupersededClosedFirst = by, open, true
	r.AdoptCopyRedHeads = nil
	s.items[key] = r
	s.dirty = true
}

// MarkCloseFirstSettled ends the polling of a close-first supersession:
// the copy merged (the record reads as an ordinary finished supersession)
// or the reopen guard left the original closed for a person.
func (s *Store) MarkCloseFirstSettled(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || !r.SupersededClosedFirst {
		return
	}
	r.SupersededClosedFirst, r.AdoptCopyRedHeads = false, nil
	s.items[key] = r
	s.dirty = true
}

// NoteAdoptCopyRedHead records that the copy's required checks were red at
// head and returns how many distinct red heads the copy has shown.
func (s *Store) NoteAdoptCopyRedHead(key, head string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return 0
	}
	head = strings.TrimSpace(head)
	if head == "" {
		return len(r.AdoptCopyRedHeads)
	}
	for _, h := range r.AdoptCopyRedHeads {
		if h == head {
			return len(r.AdoptCopyRedHeads)
		}
	}
	r.AdoptCopyRedHeads = append(r.AdoptCopyRedHeads, head)
	s.items[key] = r
	s.dirty = true
	return len(r.AdoptCopyRedHeads)
}

// MarkAdoptionReopened records that the original was reopened because copy
// `from` failed: the supersession is forgotten, the adoption record stays
// (one attempt per head) and the reopen time starts AdoptionReopenWindow.
// The caller parks the head with MarkAdoptionEscalated.
func (s *Store) MarkAdoptionReopened(key string, from int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.SupersededBy, r.SupersededAt, r.SupersededOpen, r.SupersededClosedFirst = 0, time.Time{}, false, false
	r.AdoptCopyRedHeads = nil
	r.AdoptReopenedAt, r.AdoptReopenedFrom = now, from
	r.AdoptCheckedAt = now
	s.items[key] = r
	s.dirty = true
}

// AdoptionCopyFailed reports whether the record's current copy is the one
// whose failure reopened the original (hivecommons/hive#15135): the copy is
// not polled again and the row reads "copy failed" until a person acts.
func (r Record) AdoptionCopyFailed() bool {
	return r.AdoptedBy > 0 && r.AdoptReopenedFrom == r.AdoptedBy && r.AdoptEscalatedReason == LaneReasonAdoptionCopyFailed
}

// AdoptionReopenedWithin reports whether the original was reopened for a
// failed copy less than window before now.
func (r Record) AdoptionReopenedWithin(now time.Time, window time.Duration) bool {
	return !r.AdoptReopenedAt.IsZero() && now.Sub(r.AdoptReopenedAt) < window
}

// CloseFirstPollDue reports whether a close-first supersession's copy
// should be polled: the original is closed in its favour, the copy has not
// merged, and the last poll is older than interval.
func (r Record) CloseFirstPollDue(now time.Time, interval time.Duration) bool {
	if !r.SupersededClosedFirst || r.AdoptedBy <= 0 || r.SupersededBy != r.AdoptedBy {
		return false
	}
	return r.AdoptCheckedAt.IsZero() || now.Sub(r.AdoptCheckedAt) >= interval
}

// SupersederPollDue reports whether a supersede-mode adoption with a
// superseding PR on record should be polled: not yet superseded, and the
// last poll older than interval.
func (r Record) SupersederPollDue(now time.Time, interval time.Duration) bool {
	if r.AdoptedBy <= 0 || r.AdoptMode != AdoptModeSupersede || r.Superseded() {
		return false
	}
	return r.AdoptCheckedAt.IsZero() || now.Sub(r.AdoptCheckedAt) >= interval
}

// NoteAdoptDiffFence records the diff-fence reason the adopted head tripped
// ("" when the diff read clean) on an existing record.
func (s *Store) NoteAdoptDiffFence(key, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.AdoptDiffFence == reason {
		return
	}
	r.AdoptDiffFence = reason
	s.items[key] = r
	s.dirty = true
}

// SetCoveringPR records (or, with an empty prKey, clears) the covering PR
// of an issue the store knows. A no-op for an unknown key.
func (s *Store) SetCoveringPR(key, prKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.CoveringPR == prKey {
		return
	}
	r.CoveringPR = prKey
	s.items[key] = r
	s.dirty = true
}

// Forget drops key (closed/merged item), lift record included.
func (s *Store) Forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[key]; ok {
		delete(s.items, key)
		s.dirty = true
	}
}

// ForgetStage drops key's stage for an item that is still open but has no
// stage (a blocked issue, or one outside the hive's buckets). Unlike
// Forget it keeps the blocked sweep's lift, so the stage-label pass that
// runs right after an unblock does not reset the row's since-time back to
// creation (hivecommons/hive#15779).
func (s *Store) ForgetStage(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.items[key]; ok {
		s.forgetStageLocked(key, r)
	}
}

// forgetStageLocked is ForgetStage with s.mu held and the record in hand.
func (s *Store) forgetStageLocked(key string, r Record) {
	if r.UnblockedAt.IsZero() {
		delete(s.items, key)
	} else {
		s.items[key] = Record{UnblockedAt: r.UnblockedAt, LastSeen: s.now()}
	}
	s.dirty = true
}

// Prune drops records not seen for maxAge (closed items the sweep stopped
// listing). Returns how many were dropped. Author-wide trust-promotion
// records (TrustPromotionKey) are per contributor, not per item, and
// dropping one re-announces a promotion, so they outlive maxAge: they are
// dropped only once unseen for trustPromotionRetention (or maxAge if longer).
func (s *Store) Prune(maxAge time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	cutoff := now.Add(-maxAge)
	trustCutoff := now.Add(-max(maxAge, trustPromotionRetention))
	n := 0
	for k, r := range s.items {
		c := cutoff
		if strings.HasPrefix(k, trustPromotionKeyPrefix) {
			c = trustCutoff
		}
		if r.LastSeen.Before(c) {
			delete(s.items, k)
			n++
		}
	}
	if n > 0 {
		s.dirty = true
	}
	return n
}

// trustPromotionKeyPrefix prefixes the per-author records that remember
// whether the contributor lane announced an author's auto-promotion.
const trustPromotionKeyPrefix = "trust-promoted:"

// trustPromotionRetention bounds how long an unseen trust-promotion record
// is kept before Prune drops it.
const trustPromotionRetention = 180 * 24 * time.Hour

// TrustPromotionKey is the store key for an author's auto-promotion
// record. Promotion is a property of the author in the hive, not of a PR
// (hivecommons/hive#15443), so the key carries the login only.
func TrustPromotionKey(login string) string {
	return trustPromotionKeyPrefix + strings.ToLower(strings.TrimSpace(login))
}

// ClaimTrustPromotionAnnouncement records that login is currently
// auto-promoted and reports whether the caller should post the one
// author-wide announcement: true exactly once per promotion, for the first
// caller after the author gained (or regained) it; false when the comment
// was already posted or another PR of the author claimed it this cycle. The
// claim is taken before the comment is posted so concurrent PRs by the
// author cannot both announce; ReleaseTrustPromotionAnnouncement undoes it
// when the post fails.
func (s *Store) ClaimTrustPromotionAnnouncement(login string) bool {
	if s == nil || strings.TrimSpace(login) == "" {
		return false
	}
	key := TrustPromotionKey(login)
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.items[key]
	claimed := r.TrustAnnouncedAt.IsZero()
	now := s.now()
	if claimed {
		r.TrustAnnouncedAt = now
	}
	r.LastSeen = now
	s.items[key] = r
	s.dirty = true
	return claimed
}

// ReleaseTrustPromotionAnnouncement forgets a claim whose comment did not
// land, so the next PR by the author announces instead.
func (s *Store) ReleaseTrustPromotionAnnouncement(login string) {
	if s == nil {
		return
	}
	key := TrustPromotionKey(login)
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.TrustAnnouncedAt.IsZero() {
		return
	}
	r.TrustAnnouncedAt = time.Time{}
	s.items[key] = r
	s.dirty = true
}

// ClearTrustPromotion forgets the author's promotion (revert or sentinel
// evidence demoted them) so regaining it is announced once more.
func (s *Store) ClearTrustPromotion(login string) {
	if s == nil {
		return
	}
	s.Forget(TrustPromotionKey(login))
}

// TrustPromotionAnnounced reports whether the author's promotion comment
// was posted (dashboard and tests).
func (s *Store) TrustPromotionAnnounced(login string) bool {
	if s == nil {
		return false
	}
	r, ok := s.Get(TrustPromotionKey(login))
	return ok && !r.TrustAnnouncedAt.IsZero()
}

// Keys lists the stored keys, sorted (for tests and the dashboard).
func (s *Store) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.items))
	for k := range s.items {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NoteAuthor records login as the author of key (hivecommons/hive#12502).
// A no-op for an unknown key or an empty login; the login is lower-cased
// so lookups by GitHub username are case-insensitive like GitHub itself.
func (s *Store) NoteAuthor(key, login string) {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.Author == login {
		return
	}
	r.Author = login
	s.items[key] = r
	s.dirty = true
}

// NoteSoak records the soak the lane holds an existing record's head for:
// head must sit unchanged from startedAt for window. A head of "" or a
// window of 0 clears the soak (the PR is not soaking).
func (s *Store) NoteSoak(key, head string, startedAt time.Time, window time.Duration) {
	head = strings.TrimSpace(head)
	if head == "" || window <= 0 {
		s.ClearSoak(key)
		return
	}
	startedAt = startedAt.Truncate(time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || (r.SoakHead == head && r.SoakStartedAt.Equal(startedAt) && r.SoakWindowSeconds == int64(window/time.Second)) {
		return
	}
	r.SoakHead, r.SoakStartedAt, r.SoakWindowSeconds = head, startedAt, int64(window/time.Second)
	s.items[key] = r
	s.dirty = true
}

// ClearSoak forgets a record's soak.
func (s *Store) ClearSoak(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || (r.SoakHead == "" && r.SoakStartedAt.IsZero() && r.SoakWindowSeconds == 0) {
		return
	}
	r.SoakHead, r.SoakStartedAt, r.SoakWindowSeconds = "", time.Time{}, 0
	s.items[key] = r
	s.dirty = true
}

// Soak reports the recorded soak: the head, when it started, its window.
// ok is false when none is recorded.
func (r Record) Soak() (head string, startedAt time.Time, window time.Duration, ok bool) {
	if r.SoakHead == "" || r.SoakStartedAt.IsZero() || r.SoakWindowSeconds <= 0 {
		return "", time.Time{}, 0, false
	}
	return r.SoakHead, r.SoakStartedAt, time.Duration(r.SoakWindowSeconds) * time.Second, true
}

// NoteHold records the hold provenance on an existing record; a nil
// provenance clears it (the item is no longer held, or the hold is now
// somebody else's).
func (s *Store) NoteHold(key string, hold *HoldProvenance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.Hold.Equal(hold) {
		return
	}
	if hold == nil {
		r.Hold = nil
	} else {
		cp := *hold
		r.Hold = &cp
	}
	s.items[key] = r
	s.dirty = true
}

// ClearHold drops the hold provenance from an existing record.
func (s *Store) ClearHold(key string) { s.NoteHold(key, nil) }

// NoteDCO caches the commit check for head on an existing record:
// unsignedHive is the App-linked share of the unsigned commits, the rest
// is recorded as other.
func (s *Store) NoteDCO(key, head string, total, unsigned, unsignedHive, squashUnsafeHive int) {
	if head == "" {
		return
	}
	unsignedHive = max(0, min(unsignedHive, unsigned))
	squashUnsafeHive = max(0, squashUnsafeHive)
	other := unsigned - unsignedHive
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || (r.DCOHead == head && r.DCOTotal == total && r.DCOUnsigned == unsigned && r.DCOUnsignedHive == unsignedHive && r.DCOUnsignedOther == other && r.DCOSquashUnsafeHive == squashUnsafeHive && r.DCOSquashUnsafeKnown) {
		return
	}
	r.DCOHead, r.DCOTotal, r.DCOUnsigned, r.DCOUnsignedHive, r.DCOUnsignedOther, r.DCOSquashUnsafeHive = head, total, unsigned, unsignedHive, other, squashUnsafeHive
	r.DCOSquashUnsafeKnown = true
	s.items[key] = r
	s.dirty = true
}

// AuthorItem is one stored record with its key, as ByAuthor returns it.
type AuthorItem struct {
	Key    string
	Record Record
}

// ByAuthor returns every record whose Author is login (case-insensitive),
// optionally restricted to stage (None = any stage), sorted by key so the
// dashboard view is stable across polls.
func (s *Store) ByAuthor(login string, stage Stage) []AuthorItem {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AuthorItem
	for k, r := range s.items {
		if r.Author != login || (stage != None && r.Stage != stage) {
			continue
		}
		out = append(out, AuthorItem{Key: k, Record: r})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Dwell resolves the stalled threshold for stage: the override for its
// slug when set, else the table default. 0 means never stalled.
func Dwell(stage Stage, overrides map[string]time.Duration) time.Duration {
	if d, ok := overrides[stage.Slug()]; ok {
		return d
	}
	return stage.Dwell()
}

// Stalled reports whether nobody has worked r for its stage's dwell: the
// quiet time (since the later of entering the stage and the last observed
// activity) reached the threshold and the item is not waiting on the hive.
// requireQuiet=false (governor.labels.stage.stalled.require_quiet) measures
// from stage entry alone, the pre-#12534 rule; waiting on the hive still
// never stalls.
func Stalled(r Record, overrides map[string]time.Duration, now time.Time) bool {
	return StalledWith(r, overrides, now, true)
}

// StalledWith is Stalled with the require_quiet policy explicit.
func StalledWith(r Record, overrides map[string]time.Duration, now time.Time, requireQuiet bool) bool {
	if r.Stage == None || r.EnteredAt.IsZero() || r.WaitingOnHive {
		return false
	}
	d := Dwell(r.Stage, overrides)
	if d <= 0 {
		return false
	}
	since := r.EnteredAt
	if requireQuiet {
		since = r.QuietSince()
	}
	return now.Sub(since) >= d
}

// HiveHeld reports whether the item has been quiet past its stage's dwell
// while waiting on the hive. The author is not the holder, so this is not
// a stage/stalled case: it is the hive's own lane (reviewer, CI) that
// needs attention, and the engine surfaces it as a log warning and a
// dashboard count rather than a label on the PR (hivecommons/hive#12534).
func HiveHeld(r Record, overrides map[string]time.Duration, now time.Time) bool {
	if r.Stage == None || r.EnteredAt.IsZero() || !r.WaitingOnHive {
		return false
	}
	d := Dwell(r.Stage, overrides)
	return d > 0 && r.QuietFor(now) >= d
}

// NoteLabelsSuppressed records whether the last stage-label write for key
// was kept off GitHub by the write-back gate. A missing record is left
// missing.
func (s *Store) NoteLabelsSuppressed(key string, suppressed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.LabelsSuppressed == suppressed {
		return
	}
	r.LabelsSuppressed = suppressed
	s.items[key] = r
	s.dirty = true
}

// MarkHiveHeldWarned records that the hive-held alert was logged for key.
func (s *Store) MarkHiveHeldWarned(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.HiveHeldWarned {
		return
	}
	r.HiveHeldWarned = true
	s.items[key] = r
	s.dirty = true
}

// RegressionComment is the one-line, idempotent comment posted on a
// backward transition: `stage 100-merge-queue → 070-fixing: <reason>`.
func RegressionComment(from, to Stage, reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "state changed"
	}
	return RegressionCommentMarker + "\nstage " + from.String() + " → " + to.String() + ": " + reason
}

// StallComment is the one-line, idempotent note posted when an item stalls
// in a StallNote stage: `stage 080-awaiting-merge-admission: no activity
// for 30m0s: <lane's last skip reason>`.
func StallComment(stage Stage, dwell time.Duration, reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "lane has not evaluated this head"
	}
	body := StallCommentMarker + "\nstage " + stage.String() + ": no activity for " + dwell.String() + ": " + reason
	if stage == PreMergeSoak {
		body += ". " + PreMergeSoakStallNote
	}
	return body
}

// PreMergeSoakStallNote is appended to the stall comment at 095: the dwell
// there is the soak window plus a grace, so overrunning it means the soak
// has elapsed but the lane has not promoted the head to 100 / merged it —
// a lane problem to look into, not something the author can act on.
const PreMergeSoakStallNote = "The soak window should have elapsed but the automerge lane has not promoted this PR to the merge queue; this is likely a lane bug, not something the author can act on"

// StallCommentMarker tags the stall note.
const StallCommentMarker = "<!-- hive:stage-stalled -->"

// RegressionCommentMarker tags the regression comment so a reader (or a
// later dedupe pass) can find it.
const RegressionCommentMarker = "<!-- hive:stage-regressed -->"

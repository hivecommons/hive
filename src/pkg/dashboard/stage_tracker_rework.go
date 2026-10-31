package dashboard

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Rework pickup on a waiting-on row (hivecommons/hive#15280): when a PR is
// returned to its author (071 Needs author for a human, 072 Returned to
// contributor for a clanker PR), the waiting-on pill said who the hive
// waits on and whether their clanker was online — nothing about whether
// anyone had picked the request up. The operator's question: "how can I
// know when the clanker or the author has started the rework?" The row's
// `rework` field answers from data the tracker already holds, never a
// fresh GitHub call: the stage record (when the request went out and for
// which head), the hub's fix queue (the clanker's relay was handed the fix
// task, or it waits in their queue), the PR list payload (updated_at,
// head) — in that order of confidence.

// Rework states and evidence on the wire.
const (
	reworkStateRequested    = "requested"
	reworkStateAcknowledged = "acknowledged"
	reworkStateInProgress   = "in_progress"
	reworkStatePushed       = "pushed"

	// reworkEvidenceQueued: the fix waits in the clanker's hub queue behind
	// other fixes (still "requested": nobody has it in hand yet).
	reworkEvidenceQueued = "queued"
	// reworkEvidenceClaim: the clanker's relay took the fix-pr task.
	reworkEvidenceClaim = "claim"
	// reworkEvidenceActivity: the PR's updated_at moved after the request
	// and after the hive's own last word — a comment or an edit by someone
	// (the list payload cannot say who).
	reworkEvidenceActivity = "activity"
	// reworkEvidenceCommit: the PR head is no longer the one the request
	// named.
	reworkEvidenceCommit = "commit"

	// reworkActivitySlack is how far past the request (or the hive's own
	// nudge comment) updated_at must move before it counts as somebody
	// else's activity: the hive's label writes and nudge comment bump
	// updated_at themselves, within seconds of the record's timestamps.
	reworkActivitySlack = 2 * time.Minute
)

// StageTrackerRework is the `rework` field of a row returned to its author.
type StageTrackerRework struct {
	// State is requested (sent, nobody has acted), acknowledged (activity on
	// the PR since), in_progress (the clanker's relay holds the fix task)
	// or pushed (a new head is up; re-review pending).
	State string `json:"state"`
	// Actor is who the signal is about: the author, or the clanker an
	// operator rerouted the fix to.
	Actor string `json:"actor"`
	// RequestedAt is when the request went out: the first nudge / 071 note
	// when one was posted, else when the row entered the stage.
	RequestedAt *time.Time `json:"requested_at,omitempty"`
	// Since is when the current state began (absent when unknown).
	Since *time.Time `json:"since,omitempty"`
	// Evidence names what the state rests on: queued, claim, activity, commit.
	Evidence string `json:"evidence,omitempty"`
	// Mode is the fix dispatch mode on a claim (spawn / preempt / boundary).
	Mode string `json:"mode,omitempty"`
	// Ahead is how many fixes wait before this one in the clanker's queue.
	Ahead int `json:"ahead,omitempty"`
	// Head is the new head on a pushed state.
	Head string `json:"head,omitempty"`
}

// StageTrackerFixWork is the hub's fix-queue standing for one PR and one
// contributor (ContributeWSHub.FixWorkFor).
type StageTrackerFixWork struct {
	Queued       bool
	QueuedAt     time.Time
	Ahead        int
	InFlight     bool
	DispatchedAt time.Time
	Mode         string
}

// stageTrackerFixWorkLookup is the live fix-queue lookup the tracker uses:
// the contribute hub when the server has one. Tests inject fixWorkLookup.
func (s *Server) stageTrackerFixWorkLookup() func(login, repo string, number int) (StageTrackerFixWork, bool) {
	if s == nil {
		return nil
	}
	if s.fixWorkLookup != nil {
		return s.fixWorkLookup
	}
	if s.contributeHub == nil {
		return nil
	}
	hub := s.contributeHub
	return hub.FixWorkFor
}

// stageTrackerReworkFor derives the `rework` field for a PR row waiting on
// login at stage (071 / 072 only). rec is the row's stage record; fixWork
// is the hub's fix-queue lookup (nil when the server has none).
func stageTrackerReworkFor(it StageTrackerItem, login string, stage stagelabels.Stage, rec stagelabels.Record, hasRec bool, fixWork func(login, repo string, number int) (StageTrackerFixWork, bool), now time.Time) *StageTrackerRework {
	if login == "" || it.Kind != "pr" || !hasRec {
		return nil
	}
	if stage != stagelabels.NeedsAuthor && stage != stagelabels.ReturnedToContributor {
		return nil
	}
	requested := rec.NudgeStart()
	if requested.IsZero() {
		requested = rec.EnteredAt
	}
	// A re-ask on the head the author pushed restarts the nudge clock and
	// the stage entry; the ask the operator cares about is the original
	// one (hivecommons/hive#16021).
	pushedHead, pushedAt, pushed := stageTrackerReworkPushed(it, rec)
	if pushed && !rec.ReworkAskedAt.IsZero() && rec.ReworkAskedAt.Before(requested) {
		requested = rec.ReworkAskedAt
	}
	if requested.IsZero() {
		return nil
	}
	out := &StageTrackerRework{State: reworkStateRequested, Actor: login}
	t := requested
	out.RequestedAt = &t
	// A rerouted fix is another clanker's to pick up while the reroute
	// stands on this head.
	actors := []string{login}
	if to := strings.TrimSpace(rec.RerouteTo); to != "" && !strings.EqualFold(to, login) && (rec.RerouteHead == "" || rec.RerouteHead == it.Head) {
		actors = append(actors, to)
	}
	// Pushed: the head the request named is gone. The stage engine routes
	// the row back to 050 on its next sweep; until then the row says so.
	// Or the engine already did, found the pushed head red again and asked
	// once more for it: the author answered the ask all the same.
	if pushed {
		out.State, out.Evidence, out.Head = reworkStatePushed, reworkEvidenceCommit, pushedHead
		if !pushedAt.IsZero() {
			if pushedAt.After(now) {
				pushedAt = now
			}
			out.Since = &pushedAt
		} else if it.UpdatedAt != nil && it.UpdatedAt.After(requested) {
			u := *it.UpdatedAt
			out.Since = &u
		}
		return out
	}
	if fixWork != nil {
		var queued *StageTrackerFixWork
		var queuedActor string
		for _, actor := range actors {
			w, ok := fixWork(actor, it.Repo, it.Number)
			if !ok {
				continue
			}
			if w.InFlight {
				out.State, out.Evidence, out.Actor, out.Mode = reworkStateInProgress, reworkEvidenceClaim, actor, w.Mode
				if !w.DispatchedAt.IsZero() {
					d := w.DispatchedAt
					out.Since = &d
				}
				return out
			}
			if w.Queued && queued == nil {
				wc := w
				queued, queuedActor = &wc, actor
			}
		}
		if queued != nil {
			out.Evidence, out.Actor, out.Ahead = reworkEvidenceQueued, queuedActor, queued.Ahead
			if !queued.QueuedAt.IsZero() {
				q := queued.QueuedAt
				out.Since = &q
			}
		}
	}
	// Acknowledged: the PR moved after the request and after the hive's own
	// last comment on it — somebody replied or edited.
	if it.UpdatedAt != nil {
		floor := requested
		if rec.LastNudgeAt.After(floor) {
			floor = rec.LastNudgeAt
		}
		if rec.EnteredAt.After(floor) {
			floor = rec.EnteredAt
		}
		if u := *it.UpdatedAt; u.After(floor.Add(reworkActivitySlack)) && !u.After(now.Add(reworkActivitySlack)) {
			out.State, out.Evidence, out.Mode, out.Ahead = reworkStateAcknowledged, reworkEvidenceActivity, "", 0
			out.Since = &u
			return out
		}
	}
	if out.Since == nil {
		out.Since = out.RequestedAt
	}
	return out
}

// stageTrackerReworkPushed reports the author's head pushed after the ask,
// and when it landed when the record knows: the row's head is no longer
// the one the nudge named (the sweep has not re-staged the row yet; the
// push instant is the booked one when the webhook saw it), or the record
// remembers the push across the re-ask on that head
// (hivecommons/hive#16021) — unless the re-ask is a review verdict on it,
// which means the re-review the push queued has happened and the new ask
// stands on its own.
func stageTrackerReworkPushed(it StageTrackerItem, rec stagelabels.Record) (head string, at time.Time, ok bool) {
	if rec.NudgeHead != "" && it.Head != "" && it.Head != rec.NudgeHead {
		if strings.EqualFold(rec.LastPushSha, it.Head) {
			at = rec.LastPushAt
		}
		return it.Head, at, true
	}
	head, at, ok = rec.ReworkPushed()
	if !ok || (it.Head != "" && !strings.EqualFold(it.Head, head)) {
		return "", time.Time{}, false
	}
	if stageTrackerReworkAnswered(rec.Reason) {
		return "", time.Time{}, false
	}
	return head, at, true
}

// stageTrackerReworkAnswered reports whether the standing ask's reason is
// a review verdict — a reviewer or the swarm looked at the pushed head and
// asked for more — rather than a gate the hive reads off the head (CI,
// conflict, sign-off).
func stageTrackerReworkAnswered(reason string) bool {
	return stageTrackerPushNext(stagelabels.NeedsAuthor, reason) == pushNextReReview
}

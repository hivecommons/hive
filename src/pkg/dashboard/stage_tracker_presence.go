package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Presence on a waiting-on row (hivecommons/hive#15051): when a PR row
// waits on a named person (071 Needs author, 072 Returned to contributor,
// 017 Possibly existing on a PR), the operator wants to know whether that
// person's clanker — their connected contributor agent — is there to
// answer, because that decides the hive's next move: a connected clanker
// means the hive waits; a disconnected one means adoption is the likely
// next move and the row can say when; no registered clanker means a human
// has to reply. The lookup is the one the adoption engine consults
// (automerge.ContributorPresenceSource via stageTrackerPresence) and the
// adopt-at clock follows automerge.adoptionDue's rules, so the indicator
// never contradicts the adoption text on the same row.

// Presence states and kinds on the wire.
const (
	presenceStateOnline  = "online"
	presenceStateOffline = "offline"
	presenceStateUnknown = "unknown"

	presenceKindClanker = "clanker"
	presenceKindHuman   = "human"

	presenceNextWait  = "wait"
	presenceNextAdopt = "adopt"
)

// StageTrackerPresence is the `presence` field of a row that waits on a
// person.
type StageTrackerPresence struct {
	// Login is the GitHub login the row waits on.
	Login string `json:"login"`
	// State is online / offline (a registered clanker, by its heartbeat)
	// or unknown (no clanker registered here for this login).
	State string `json:"state"`
	// Kind is clanker when a contributor clanker is registered for the
	// login or the PR was authored by one, else human.
	Kind string `json:"kind"`
	// Since is when the wait on this person began: the first nudge when
	// one went out, else when the row entered its stage.
	Since *time.Time `json:"since,omitempty"`
	// LastSeen is the clanker's freshest heartbeat (absent when never seen).
	LastSeen *time.Time `json:"last_seen,omitempty"`
	// Next is the hive's likely next move: wait (for the person) or adopt
	// (the adoption clock is running on a disconnected or unregistered
	// clanker and nothing holds it).
	Next string `json:"next"`
	// AdoptAt is when the adoption engine would judge adoption due at the
	// current head, when its clock is running (072, nudged, not held).
	AdoptAt *time.Time `json:"adopt_at,omitempty"`
}

// stageTrackerAdoptionClock is the slice of governor.labels.stage the
// adopt-at projection and the takeover offer need: the same
// automerge.AdoptionClock cmd/hive hands the adoption engine, so the
// tracker's "adopt?" and the sweep's adoption are one verdict
// (automerge.AutoAdoption).
type stageTrackerAdoptionClock struct {
	Enabled bool
	automerge.AdoptionClock
}

func (s *Server) stageTrackerAdoptionClock() stageTrackerAdoptionClock {
	var stage config.StageLabelsConfig
	if s != nil && s.deps != nil && s.deps.Config != nil {
		stage = s.deps.Config.Governor.Labels.Stage
	}
	return stageTrackerAdoptionClock{
		Enabled: s.stageAdoptionEnabled(),
		AdoptionClock: automerge.AdoptionClock{
			GraceOnline:            stage.EffectiveContributorGraceOnline(),
			GraceOffline:           stage.EffectiveContributorGraceOffline(),
			AdoptAfterOffline:      stage.Adoption.EffectiveAfterOffline(),
			AdoptAfterUnregistered: stage.Adoption.EffectiveAfterUnregistered(),
			AdoptOfflineGrace:      stage.Adoption.EffectiveOfflineGrace(),
		},
	}
}

// stageTrackerAutoAdoption is the engine's verdict for a row: whether the
// adoption clock runs on its head and when (or that) it fires. It is the
// one predicate the sweep acts on, so a row that says "adopting" is one
// the hive adopts on its next sweep without a click.
func stageTrackerAutoAdoption(it StageTrackerItem, rec stagelabels.Record, hasRec bool, presence func(string) (automerge.ContributorPresence, bool), clock stageTrackerAdoptionClock, now time.Time) automerge.AdoptionVerdict {
	if !hasRec || !clock.Enabled || it.Kind != "pr" || it.AuthorKind == authorKindHuman {
		return automerge.AdoptionVerdict{}
	}
	head := it.Head
	if head == "" {
		head = rec.NudgeHead
	}
	var p automerge.ContributorPresence
	hasPresence := false
	if presence != nil && it.Author != "" {
		p, hasPresence = presence(it.Author)
	}
	return automerge.AutoAdoption(rec, head, clock.AdoptionClock, now, p, hasPresence)
}

// stageTrackerWaitingOn names the person a PR row waits on: its author at
// the stages whose caption says "@author must act", and a clanker's PR the
// engine has not classified yet (the row reads "PR in progress ·
// @author's clanker": the clanker is the one on it, so whether it is
// still there is the row's live fact). "" when the row waits on the hive,
// a gate or an unnamed maintainer.
func stageTrackerWaitingOn(it StageTrackerItem, stage stagelabels.Stage) string {
	if it.Kind != "pr" || strings.TrimSpace(it.Author) == "" {
		return ""
	}
	switch stage {
	case stagelabels.NeedsAuthor, stagelabels.ReturnedToContributor, stagelabels.PossiblyExisting:
		return strings.TrimSpace(it.Author)
	case stagelabels.None:
		if it.AuthorKind == authorKindClanker {
			return strings.TrimSpace(it.Author)
		}
	}
	return ""
}

// stageTrackerOfflineAuthorCaption appends, to the caption of a clanker's
// PR whose clanker is offline, what that means for the row: on a PR the
// engine has not classified, nothing — a green PR is reviewed and merged
// without its author; adoption starts only once the PR is returned to the
// contributor (red CI, conflicts, review asks) and the clanker stays away
// for the offline grace. 072 rows carry the adoption line instead
// (stageTrackerAdoptionCaption) and are left alone here.
func stageTrackerOfflineAuthorCaption(caption string, p *StageTrackerPresence, stage int, clock stageTrackerAdoptionClock, now time.Time) string {
	if p == nil || p.Kind != presenceKindClanker || p.State != presenceStateOffline || stage != stagelabels.None.Number() {
		return caption
	}
	gone := stageTrackerAdoptionOfflineFor(StageTrackerAdoption{Since: p.Since, AuthorLastSeen: p.LastSeen}, now)
	clause := "@" + p.Login + "'s clanker is offline" + gone + "; a green PR is reviewed and merged without its author"
	if clock.Enabled {
		grace := orDefault(clock.AdoptAfterOffline, config.DefaultAdoptAfterOffline)
		clause += " — hive adopts it only once it is returned (red CI, conflicts or review asks) and nothing is pushed for " + stageTrackerShortDuration(grace)
	}
	return joinClause(caption, clause)
}

// stageTrackerShortDuration renders a grace window as "30m" / "2h" /
// "1h30m".
func stageTrackerShortDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// stageTrackerPresenceFor builds the `presence` field for a row waiting on
// login. presence is the live lookup (nil when the server has none: the
// state is then unknown); rec is the row's stage record for the nudge
// clock; clock is the adoption timing policy.
func stageTrackerPresenceFor(it StageTrackerItem, login string, stage stagelabels.Stage, rec stagelabels.Record, hasRec bool, presence func(string) (automerge.ContributorPresence, bool), clock stageTrackerAdoptionClock, now time.Time) *StageTrackerPresence {
	if login == "" {
		return nil
	}
	out := &StageTrackerPresence{Login: login, State: presenceStateUnknown, Kind: presenceKindHuman, Next: presenceNextWait}
	var p automerge.ContributorPresence
	hasPresence := false
	if presence != nil {
		p, hasPresence = presence(login)
	}
	switch {
	case p.Registered && p.Online:
		out.State, out.Kind = presenceStateOnline, presenceKindClanker
	case p.Registered:
		out.State, out.Kind = presenceStateOffline, presenceKindClanker
	case it.AuthorKind != "" && it.AuthorKind != authorKindHuman:
		// A clanker's PR whose author has no profile here: another hive
		// posted it. Adoption treats it as unregistered.
		out.Kind = presenceKindClanker
	}
	if !p.LastSeen.IsZero() {
		t := p.LastSeen
		out.LastSeen = &t
	}
	start := time.Time{}
	if hasRec && rec.NudgeCount > 0 && !rec.NudgeStart().IsZero() {
		start = rec.NudgeStart()
	} else if it.StageSince != nil {
		start = *it.StageSince
	}
	if !start.IsZero() {
		t := start
		out.Since = &t
	}
	// The adoption clock runs only where adoption itself does: a nudged
	// clanker PR at 072 whose head has not moved since the nudge, not yet
	// adopted, with nothing holding it — automerge.AutoAdoption, the
	// sweep's own predicate.
	if stage != stagelabels.ReturnedToContributor || !clock.Enabled || it.AuthorKind == authorKindHuman || out.Kind != presenceKindClanker {
		return out
	}
	head := it.Head
	if head == "" {
		head = rec.NudgeHead
	}
	if hasRec && rec.AdoptionBlocked(head) != "" {
		// Held (maintainer_policy ask / never): the adoption text says so;
		// the engine will not adopt on its own.
		return out
	}
	v := automerge.AutoAdoption(rec, head, clock.AdoptionClock, now, p, hasPresence)
	if !v.Running {
		return out
	}
	if v.Reason != automerge.AdoptReasonGrace || out.State != presenceStateOnline {
		out.Next = presenceNextAdopt
	}
	at := v.At
	out.AdoptAt = &at
	return out
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

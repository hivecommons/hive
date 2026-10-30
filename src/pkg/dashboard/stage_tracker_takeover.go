package dashboard

import (
	"slices"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The operator's takeover actions on a PR waiting on a contributor's
// clanker (hivecommons/hive#15038): a 072 / 071 clanker PR carries
// `takeover` — whether Adopt now and Reroute are offered (and why not), the
// online clankers a reroute can target, a pending reroute ("rerouted →
// @X") and a pending forced adoption. The SPA builds the ⋯ menu items, the
// consequence lines and the pills from it; the needs-human popover lists
// the same two options when the blocker is waiting on the contributor.

// StageTrackerTakeover is the `takeover` field of a 072 / 071 clanker PR row.
type StageTrackerTakeover struct {
	Author       string `json:"author,omitempty"`
	AuthorOnline bool   `json:"author_online"`
	// AdoptNow is true when "Adopt now" is offered; AdoptNowReason says why
	// not when it is not (adoption disabled, hold label).
	AdoptNow       bool   `json:"adopt_now"`
	AdoptNowReason string `json:"adopt_now_reason,omitempty"`
	// Reroute is true when at least one other clanker is online and can
	// take fix-pr work; RerouteReason is the disabled reason otherwise.
	Reroute       bool               `json:"reroute"`
	RerouteReason string             `json:"reroute_reason,omitempty"`
	Clankers      []RerouteCandidate `json:"clankers,omitempty"`
	// Rerouted is the live reroute, when one is pending.
	Rerouted *StageTrackerRerouted `json:"rerouted,omitempty"`
	// ForcedBy / ForcedAt is a pending Adopt now the sweep has not run yet.
	ForcedBy string     `json:"forced_by,omitempty"`
	ForcedAt *time.Time `json:"forced_at,omitempty"`
	// FallbackAt is when a reroute's pickup window lapsed and the hive fell
	// back to Adopt now.
	FallbackAt *time.Time `json:"fallback_at,omitempty"`
	// GraceElapsed is true once the engine's own predicate
	// (automerge.AutoAdoption) says adoption is due at this head: the hive
	// adopts on its next sweep without a click, and the delegated "Let hive
	// decide" picks Adopt now. It is also true when adoption is due but
	// held (maintainer_policy ask / never, a person's admission).
	GraceElapsed bool `json:"grace_elapsed,omitempty"`
	// AutoAdoptAt is when the same predicate fires, while its clock runs.
	AutoAdoptAt *time.Time `json:"auto_adopt_at,omitempty"`
}

// StageTrackerRerouted is a pending reroute on the row.
type StageTrackerRerouted struct {
	To           string     `json:"to"`
	By           string     `json:"by,omitempty"`
	Since        *time.Time `json:"since,omitempty"`
	Reason       string     `json:"reason,omitempty"`
	TargetOnline bool       `json:"target_online"`
	// Text is the pill: "rerouted → @X".
	Text string `json:"text"`
}

// Row action ids for the takeover actions, in the context menu's
// "decide:<verb>" vocabulary.
const (
	decisionActionAdoptNow = "decide:adopt-now"
	decisionActionReroute  = "decide:reroute"
)

// takeoverAdoptNowDisabledHold is the Adopt now disabled reason under a hold label.
const takeoverAdoptNowDisabledHold = "release the hold first"

// takeoverAdoptNowDisabledOff is the reason when adoption is off on this hive.
const takeoverAdoptNowDisabledOff = "adoption is disabled (governor.labels.stage.adoption.enabled)"

// takeoverAdoptNowQueued is the reason once Adopt now was pressed and the
// sweep has not landed yet: the action is spent, not offered again.
const takeoverAdoptNowQueued = "adoption already queued"

// stageTrackerRerouteCandidates returns the lookup for the online clankers a
// reroute may target, excluding the PR's author. Tests inject
// rerouteCandidates.
func (s *Server) stageTrackerRerouteCandidates() func(author string) []RerouteCandidate {
	if s == nil {
		return nil
	}
	if s.rerouteCandidates != nil {
		return s.rerouteCandidates
	}
	if s.contributeHub == nil {
		return nil
	}
	hub := s.contributeHub
	return func(author string) []RerouteCandidate { return hub.RerouteCandidates(time.Now(), author) }
}

// stageTrackerTakeover builds the `takeover` field for a clanker PR row at
// 072 / 071; nil for other rows and for an adopted head.
func stageTrackerTakeover(it StageTrackerItem, rec stagelabels.Record, hasRec bool, presence func(string) (automerge.ContributorPresence, bool), candidates func(string) []RerouteCandidate, clock stageTrackerAdoptionClock, now time.Time) *StageTrackerTakeover {
	adoptionEnabled := clock.Enabled
	if it.Kind != "pr" || it.AuthorKind == authorKindHuman || (it.Stage != trackerNumberReturned && it.Stage != trackerNumberNeedsAuthor) {
		return nil
	}
	if hasRec && rec.AdoptedHead != "" && (it.Head == "" || rec.Adopted(it.Head) || rec.AdoptedInPlace()) {
		return nil
	}
	out := &StageTrackerTakeover{Author: it.Author}
	if presence != nil && it.Author != "" {
		if p, ok := presence(it.Author); ok {
			out.AuthorOnline = p.Online
		}
	}
	held := slices.Contains(it.Modifiers, modHold)
	_, forcedPending := rec.AdoptionForced()
	forcedPending = forcedPending && hasRec
	switch {
	case forcedPending:
		out.AdoptNowReason = takeoverAdoptNowQueued
	case !adoptionEnabled:
		out.AdoptNowReason = takeoverAdoptNowDisabledOff
	case held:
		out.AdoptNowReason = takeoverAdoptNowDisabledHold
	default:
		out.AdoptNow = true
	}
	var cands []RerouteCandidate
	if candidates != nil {
		cands = candidates(it.Author)
	}
	out.Clankers = cands
	switch {
	case len(cands) == 0:
		out.RerouteReason = RerouteNoneOnlineReason
	case held:
		out.RerouteReason = takeoverAdoptNowDisabledHold
	default:
		out.Reroute = true
	}
	if hasRec {
		if to := rec.Rerouted(it.Head); to != "" {
			text := "Rerouted to @" + to
			if rec.RerouteBy != "" {
				text += " by @" + rec.RerouteBy
			}
			if rec.RerouteReason != "" {
				text += " — " + rec.RerouteReason
			}
			r := &StageTrackerRerouted{To: to, By: rec.RerouteBy, Reason: rec.RerouteReason, Text: text}
			if !rec.RerouteAt.IsZero() {
				t := rec.RerouteAt
				r.Since = &t
			}
			if presence != nil {
				if p, ok := presence(to); ok {
					r.TargetOnline = p.Online
				}
			}
			// A live connection on the contribute hub is presence too.
			for _, c := range cands {
				if strings.EqualFold(c.Login, to) {
					r.TargetOnline = true
				}
			}
			out.Rerouted = r
		}
		if by, ok := rec.AdoptionForced(); ok {
			out.ForcedBy = by
			if !rec.AdoptForcedAt.IsZero() {
				t := rec.AdoptForcedAt
				out.ForcedAt = &t
			}
		}
		if !rec.RerouteFallbackAt.IsZero() && out.Rerouted == nil {
			t := rec.RerouteFallbackAt
			out.FallbackAt = &t
		}
		v := stageTrackerAutoAdoption(it, rec, hasRec, presence, clock, now)
		if v.Running {
			at := v.At
			out.AutoAdoptAt = &at
		}
		out.GraceElapsed = rec.AdoptionBlocked(it.Head) != "" || (it.Adoption != nil && it.Adoption.Due) || v.Due
	}
	return out
}

// takeoverDecisionOptions are the popover options the takeover adds to a
// waiting-on-contributor decision: Adopt now (lead) and Reroute.
func takeoverDecisionOptions(t *StageTrackerTakeover, held bool) []StageTrackerDecisionOption {
	if t == nil {
		return nil
	}
	adoptWhy := t.AdoptNowReason
	if held && adoptWhy == "" {
		adoptWhy = "Release the hold first"
	}
	rerouteWhy := t.RerouteReason
	if held && rerouteWhy == "" {
		rerouteWhy = "Release the hold first"
	}
	out := []StageTrackerDecisionOption{}
	if t.ForcedBy == "" {
		// Spent once pressed: the row says "Adopting… (queued)" instead.
		out = append(out, decisionOption(decisionActionAdoptNow, "Adopt now — hive takes it over", t.AdoptNow && !held, adoptWhy))
	}
	return append(out, decisionOption(decisionActionReroute, "Reroute to another clanker", t.Reroute && !held, rerouteWhy))
}

// stageTrackerTakeoverCaption appends the reroute pill text to the caption.
func stageTrackerTakeoverCaption(caption string, t *StageTrackerTakeover) string {
	if t == nil || t.Rerouted == nil {
		return caption
	}
	if strings.Contains(caption, t.Rerouted.Text) {
		return caption
	}
	return caption + " — " + t.Rerouted.Text
}

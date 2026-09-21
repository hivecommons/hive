package standby

import "time"

// Reason is the machine-readable cause of a matching decision, in the style of
// the taskUnavailable* constants in pkg/dashboard. It is always set, including
// on success, so every tile cell, every log line and every standby_ack entry
// has one cause with one spelling.
//
// A rejection states its reason CLASS and never the delta. ReasonBelowFloor
// carries no floor value and no "you would qualify at T3". An accepted
// candidate is told the floor it is held to by the caller, because a
// contributor who is in needs to know the bar; a rejected one is told only
// that they are under it. Honest refusal, no lobbying material.
type Reason string

const (
	// ReasonQualified: the candidate clears the lane's floor, is approved and
	// unsuspended, and has cap left.
	ReasonQualified Reason = "qualified"
	// ReasonLaneUnknown: the named lane does not exist on this hive. Decided
	// by the caller before it can build a LanePolicy; named here so the
	// vocabulary has one home.
	ReasonLaneUnknown Reason = "lane_unknown"
	// ReasonStandbyDisabled: the lane exists but its standby block is off.
	// Decided by the caller, for the same reason as ReasonLaneUnknown.
	ReasonStandbyDisabled Reason = "standby_disabled"
	// ReasonNotApproved: the contributor is not in hub.standby_contributors.
	// Volunteering is not approval.
	ReasonNotApproved Reason = "not_approved"
	// ReasonSuspended: this configuration is suspended on this hive until the
	// owner clears it (the suspend rule lands in S6).
	ReasonSuspended Reason = "suspended"
	// ReasonConfigurationUnknown: the reported configuration has no entry in
	// hub.standby_model_tiers, or names no backend or model. Nobody assessed
	// it, so it clears nothing. This is the out-of-the-box answer on a hive
	// that has not written the mapping.
	ReasonConfigurationUnknown Reason = "configuration_unknown"
	// ReasonBelowFloor: the configuration's tier is weaker than the lane's
	// capability floor. It carries no floor value on purpose.
	ReasonBelowFloor Reason = "below_floor"
	// ReasonItemTierUnknown: item-tier matching is in force on this hive and
	// this item is on no entry of the owner's item-tier list. Nobody said this
	// class of work is donatable, so it is not standby-eligible at all — and
	// a tier the classifier proposed for it changes nothing (S7).
	ReasonItemTierUnknown Reason = "item_tier_unknown"
	// ReasonBelowItemTier: the configuration's tier is weaker than the ITEM's
	// tier. The lane floor and the item tier are both required: a T1
	// configuration may take a T3 item, a T3 configuration may not take a T1
	// item. Like ReasonBelowFloor it carries no value.
	ReasonBelowItemTier Reason = "below_item_tier"
	// ReasonCapExhausted: no daily cap left in the trailing window — including
	// the default cap of zero, which dispatches nothing.
	ReasonCapExhausted Reason = "cap_exhausted"
	// ReasonFloorUnknown: the lane policy carries no legal floor. Config
	// validation rejects such a floor at load, so this should be unreachable
	// from a parsed hive.yaml; it exists so that a LanePolicy assembled some
	// other way fails CLOSED rather than admitting everything. A floor of
	// "unknown" is the absence of a floor, and a lane with no floor is a lane
	// anything clears — the exact fail-open this design is built to avoid.
	ReasonFloorUnknown Reason = "floor_unknown"
)

// String renders the reason for logs and wire fields.
func (r Reason) String() string { return string(r) }

// Candidate is one approved-or-not contributor standing by, with the
// configuration they are actually running and their dispatch history on the
// lane being matched.
type Candidate struct {
	// Contributor is the GitHub login. It is carried for the caller's logging
	// and counting; matching never reads it, because the floor is a property
	// of the configuration, not the person.
	Contributor string
	// Config is the configuration the relay reported on its most recent
	// standby_declare — the one actually running, not the one it registered
	// with.
	Config Configuration
	// Approved is membership of hub.standby_contributors, resolved by the
	// caller.
	Approved bool
	// Suspended is the suspend rule's verdict for this configuration on this
	// hive (S6). False until that lands.
	Suspended bool
	// Dispatches are this contributor's donated-task dispatches on THIS lane.
	// Only those inside DispatchWindow count; the caller may pass a longer
	// history.
	Dispatches []time.Time
}

// LanePolicy is the standby half of a lane's configuration: the floor a
// donated configuration must clear, and how many donated tasks one contributor
// may be dispatched per rolling day.
type LanePolicy struct {
	// Floor is the lane's minimum model capability from the standby block of
	// hive.yaml (the wire key is owned by pkg/config, which is the only
	// package allowed to name it). It defaults to T1 — the strongest — at
	// parse, and it is writable only by editing hive.yaml.
	Floor Tier
	// DailyCap is standby.daily_cap_per_contributor. Zero, the default, means
	// nothing is dispatched.
	DailyCap int
}

// Qualifies is the whole matching decision for one candidate on one lane, for
// one item.
//
// The checks run cheapest-and-most-structural first, and each has its own
// Reason so the cause is never inferred from a coincidence. In particular the
// unknown-configuration guard stands on its own line: without it, an unmapped
// configuration would still be refused by the strength comparison (unknown
// scores zero and every legal floor scores at least one) — but it would be
// refused as "below_floor", which is a lie. It is not below the floor; nobody
// assessed it. Deleting the guard changes the reported reason, and the test
// for it asserts the reason, so the test fails rather than passing on the
// encoding's coincidence. The item's unknown guard (S7) is there for the same
// reason and is asserted the same way.
//
// `item` is the owner's list's answer about the work, from ItemTiers.Match.
// The ZERO value means item-tier matching is not in force — the shipped state,
// because the owner's item-tier list ships empty — and then this function
// decides exactly what it decided in S4.
//
// Returning false is a normal outcome, not an error. A lane on which no
// candidate qualifies stays paused, reports zero, and offers no next step.
func Qualifies(c Candidate, p LanePolicy, item ItemMatch, tiers TierMap, now time.Time) (bool, Reason) {
	// The floor itself must be a real tier. Fail closed if it is not.
	floor := NormalizeTier(string(p.Floor))
	if !floor.Known() {
		return false, ReasonFloorUnknown
	}
	if !c.Approved {
		return false, ReasonNotApproved
	}
	if c.Suspended {
		return false, ReasonSuspended
	}
	// Unknown never qualifies. Explicitly, before any comparison.
	tier := tiers.Tier(c.Config)
	if !tier.Known() {
		return false, ReasonConfigurationUnknown
	}
	if tier.strength() < floor.strength() {
		return false, ReasonBelowFloor
	}
	// The item's own bar, when the owner has set one. Unknown never qualifies
	// here either, and explicitly: an item nobody listed is not weak work, it
	// is work nobody said was donatable.
	if item.Enforced() {
		if !item.Tier().Known() {
			return false, ReasonItemTierUnknown
		}
		if tier.strength() < item.Tier().strength() {
			return false, ReasonBelowItemTier
		}
	}
	if CapRemaining(c, p, now) <= 0 {
		return false, ReasonCapExhausted
	}
	return true, ReasonQualified
}

// QualifiedCount counts the contributors standing by on this lane who clear
// its floor, clear the item's tier, and have cap left. Pass the zero ItemMatch
// for the lane-only question.
//
// It is a plain count over Qualifies with no side effect. A count of zero is a
// terminal, acceptable state: the lane stays paused and nothing is proposed.
func QualifiedCount(candidates []Candidate, p LanePolicy, item ItemMatch, tiers TierMap, now time.Time) int {
	n := 0
	for _, c := range candidates {
		if ok, _ := Qualifies(c, p, item, tiers, now); ok {
			n++
		}
	}
	return n
}

// QualifiedCountForQueue is the M in the tile's "N waiting, M qualify" once
// items carry a tier: how many contributors standing by on this lane could be
// offered AT LEAST ONE of the items queued on it.
//
// Counting per contributor rather than per (contributor, item) pair is what
// keeps M comparable with the N beside it — N is a queue depth, M is a head
// count, and the cell has always read that way.
//
// Build the queue with ItemTiers.LaneQueue, which returns a single not-in-force
// match when the owner's list is empty. That is the shipped state, and it
// makes this exactly S4's count: the queue is not consulted and every
// contributor is judged against the lane floor alone.
func QualifiedCountForQueue(candidates []Candidate, p LanePolicy, queue []ItemMatch, tiers TierMap, now time.Time) int {
	n := 0
	for _, c := range candidates {
		for _, item := range queue {
			if ok, _ := Qualifies(c, p, item, tiers, now); ok {
				n++
				break
			}
		}
	}
	return n
}

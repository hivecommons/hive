package standby

import (
	"strings"
	"testing"
	"time"
)

// now is the injected clock for every test in this package. The package takes
// no ambient clock, so these tests never sleep and never flake.
var now = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

// noItem is the item half of a decision when item-tier matching is NOT IN
// FORCE — the shipped state, because the owner's item-tier list is empty until
// they write it.
//
// Every S4-era test in this package passes it, which is what makes them the
// regression test for S7's acceptance rule: with the owner's item list empty,
// the S4 answer must be reproduced exactly. item_test.go covers the same
// decisions with the list in force.
var noItem = ItemMatch{}

// tiers is the owner-authored mapping the tests match against: one
// configuration at each legal tier, all on distinct models.
func tiers(t *testing.T) TierMap {
	t.Helper()
	m, err := NewTierMap([]TierEntry{
		{Config: Configuration{Backend: "claude", Model: "claude-opus-5", ReasoningEffort: "high"}, Tier: T1},
		{Config: Configuration{Backend: "codex", Model: "gpt-5.6-terra", ReasoningEffort: "high"}, Tier: T2},
		{Config: Configuration{Backend: "claude", Model: "claude-haiku-4-5", ReasoningEffort: "low"}, Tier: T3},
	})
	if err != nil {
		t.Fatalf("NewTierMap: %v", err)
	}
	return m
}

func configFor(tier Tier) Configuration {
	switch tier {
	case T1:
		return Configuration{Backend: "claude", Model: "claude-opus-5", ReasoningEffort: "high"}
	case T2:
		return Configuration{Backend: "codex", Model: "gpt-5.6-terra", ReasoningEffort: "high"}
	case T3:
		return Configuration{Backend: "claude", Model: "claude-haiku-4-5", ReasoningEffort: "low"}
	default:
		// Deliberately unmapped: a real-looking configuration nobody assessed.
		return Configuration{Backend: "claude", Model: "claude-sonnet-5", ReasoningEffort: "medium"}
	}
}

// approved builds a candidate who is past every gate except the one under
// test: approved, unsuspended, no dispatches yet.
func approved(tier Tier) Candidate {
	return Candidate{Contributor: "alice", Config: configFor(tier), Approved: true}
}

// openLane is a lane with cap to spare, so a rejection is always about the
// floor rather than the cap.
func openLane(floor Tier) LanePolicy {
	return LanePolicy{Floor: floor, DailyCap: 2}
}

// TestFloorAdmitsItsOwnTierAndEverythingStronger walks the whole floor ×
// configuration grid. T1 > T2 > T3, so a floor of T2 admits T1 and T2 and
// refuses T3.
func TestFloorAdmitsItsOwnTierAndEverythingStronger(t *testing.T) {
	tm := tiers(t)
	legal := []Tier{T1, T2, T3}
	for _, floor := range legal {
		for _, cfg := range legal {
			want := cfg.strength() >= floor.strength()
			got, reason := Qualifies(approved(cfg), openLane(floor), noItem, tm, now)
			if got != want {
				t.Errorf("floor %s, configuration %s: Qualifies = %v (%s), want %v", floor, cfg, got, reason, want)
			}
			wantReason := ReasonQualified
			if !want {
				wantReason = ReasonBelowFloor
			}
			if reason != wantReason {
				t.Errorf("floor %s, configuration %s: reason = %q, want %q", floor, cfg, reason, wantReason)
			}
		}
	}
}

// TestUnknownConfigurationNeverQualifiesAndSaysWhy is the guard test the
// design asks for. It asserts the REASON, not just the refusal: unknown loses
// the strength comparison anyway, so a test that only checked `false` would
// keep passing if the explicit guard in Qualifies were deleted. Asserting
// configuration_unknown makes deleting the guard a test failure, because the
// candidate would then fall through and be reported as below_floor — which it
// is not. Nobody assessed it.
func TestUnknownConfigurationNeverQualifiesAndSaysWhy(t *testing.T) {
	tm := tiers(t)
	for _, floor := range []Tier{T1, T2, T3} {
		// Unmapped but well-formed.
		ok, reason := Qualifies(approved(TierUnknown), openLane(floor), noItem, tm, now)
		if ok {
			t.Errorf("floor %s: an unmapped configuration qualified", floor)
		}
		if reason != ReasonConfigurationUnknown {
			t.Errorf("floor %s: reason = %q, want %q — the explicit unknown guard is what states this invariant",
				floor, reason, ReasonConfigurationUnknown)
		}
	}
	// And against the weakest floor there is, with an empty configuration.
	ok, reason := Qualifies(Candidate{Contributor: "alice", Approved: true}, openLane(T3), noItem, tm, now)
	if ok || reason != ReasonConfigurationUnknown {
		t.Errorf("empty configuration against the weakest floor: (%v, %q), want (false, %q)",
			ok, reason, ReasonConfigurationUnknown)
	}
	// An empty tier map is the shipped state: nobody qualifies anywhere.
	var empty TierMap
	if ok, reason := Qualifies(approved(T1), openLane(T3), noItem, empty, now); ok || reason != ReasonConfigurationUnknown {
		t.Errorf("shipped (empty) tier map: (%v, %q), want (false, %q)", ok, reason, ReasonConfigurationUnknown)
	}
}

// TestWholeConfigurationIsCompared: the tier is a property of the whole tuple,
// so changing any component changes the key, and a key nobody mapped is
// unknown. This is what makes "you cannot offer Opus and run something else"
// structural rather than a promise.
func TestWholeConfigurationIsCompared(t *testing.T) {
	tm := tiers(t)
	base := configFor(T1)
	if got := tm.Tier(base); got != T1 {
		t.Fatalf("precondition: Tier(base) = %q, want T1", got)
	}
	for _, tc := range []struct {
		name   string
		mutate func(Configuration) Configuration
	}{
		{"different reasoning effort", func(c Configuration) Configuration { c.ReasoningEffort = "low"; return c }},
		{"no reasoning effort", func(c Configuration) Configuration { c.ReasoningEffort = ""; return c }},
		{"different backend", func(c Configuration) Configuration { c.Backend = "codex"; return c }},
		{"different model", func(c Configuration) Configuration { c.Model = "claude-opus-4"; return c }},
		{"an advisor the mapping did not include", func(c Configuration) Configuration {
			c.AdvisorModel = "claude-haiku-4-5"
			return c
		}},
		{"an advisor effort the mapping did not include", func(c Configuration) Configuration {
			c.AdvisorEffort = "low"
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := approved(T1)
			c.Config = tc.mutate(base)
			ok, reason := Qualifies(c, openLane(T1), noItem, tm, now)
			if ok {
				t.Fatalf("a configuration differing by %s matched the mapped one", tc.name)
			}
			if reason != ReasonConfigurationUnknown {
				t.Errorf("reason = %q, want %q", reason, ReasonConfigurationUnknown)
			}
		})
	}
}

// TestAdvisorFieldsAreMatchedNotIgnored: an entry written with advisor fields
// matches only a configuration that reports them, and vice versa. Empty is a
// value of the tuple, never a wildcard.
func TestAdvisorFieldsAreMatchedNotIgnored(t *testing.T) {
	withAdvisor := Configuration{
		Backend: "claude", Model: "claude-opus-5", ReasoningEffort: "high",
		AdvisorModel: "claude-haiku-4-5", AdvisorEffort: "low",
	}
	tm, err := NewTierMap([]TierEntry{{Config: withAdvisor, Tier: T1}})
	if err != nil {
		t.Fatalf("NewTierMap: %v", err)
	}
	if got := tm.Tier(withAdvisor); got != T1 {
		t.Errorf("Tier(withAdvisor) = %q, want T1", got)
	}
	bare := Configuration{Backend: "claude", Model: "claude-opus-5", ReasoningEffort: "high"}
	if got := tm.Tier(bare); got != TierUnknown {
		t.Errorf("Tier(no advisor) = %q against an entry that names one, want unknown", got)
	}
}

func TestApprovalAndSuspensionGateBeforeTheFloor(t *testing.T) {
	tm := tiers(t)

	// Volunteering is not approval: the strongest configuration there is still
	// gets nothing without a login in hub.standby_contributors.
	c := approved(T1)
	c.Approved = false
	if ok, reason := Qualifies(c, openLane(T3), noItem, tm, now); ok || reason != ReasonNotApproved {
		t.Errorf("unapproved T1: (%v, %q), want (false, %q)", ok, reason, ReasonNotApproved)
	}

	// A suspended configuration is out regardless of how strong it is.
	c = approved(T1)
	c.Suspended = true
	if ok, reason := Qualifies(c, openLane(T3), noItem, tm, now); ok || reason != ReasonSuspended {
		t.Errorf("suspended T1: (%v, %q), want (false, %q)", ok, reason, ReasonSuspended)
	}
}

// TestFloorThatIsNotATierFailsClosed: config validation rejects such a floor
// at load, so this is the second line. It matters because the failure mode it
// prevents — a lane whose floor is the absence of a floor — is exactly the
// fail-open the design exists to avoid.
func TestFloorThatIsNotATierFailsClosed(t *testing.T) {
	tm := tiers(t)
	for _, floor := range []Tier{TierUnknown, Tier("unknown"), Tier("T0"), Tier("any"), Tier("  ")} {
		ok, reason := Qualifies(approved(T1), LanePolicy{Floor: floor, DailyCap: 5}, noItem, tm, now)
		if ok {
			t.Errorf("floor %q admitted a candidate; a lane with no floor must admit nobody", string(floor))
		}
		if reason != ReasonFloorUnknown {
			t.Errorf("floor %q: reason = %q, want %q", string(floor), reason, ReasonFloorUnknown)
		}
	}
	// A legal floor in unusual casing still works — spelling is not substance.
	if ok, _ := Qualifies(approved(T1), LanePolicy{Floor: Tier("t1"), DailyCap: 5}, noItem, tm, now); !ok {
		t.Error(`floor "t1" rejected a T1 configuration`)
	}
}

// TestNobodyQualifiesIsANormalOutcome: the runbook's step 2. A T2 contributor
// against the default T1 floor produces a count of zero, and that is the end
// of it — the lane stays paused and nothing else happens.
func TestNobodyQualifiesIsANormalOutcome(t *testing.T) {
	tm := tiers(t)
	pool := []Candidate{
		approved(T2),
		func() Candidate { c := approved(T3); c.Contributor = "bob"; return c }(),
		func() Candidate { c := approved(TierUnknown); c.Contributor = "carol"; return c }(),
	}
	lane := openLane(T1)

	if got := QualifiedCount(pool, lane, noItem, tm, now); got != 0 {
		t.Fatalf("QualifiedCount = %d, want 0", got)
	}

	// Counting is pure: it changed no candidate's state, spent no cap, and
	// reported nothing beyond the count.
	for i, c := range pool {
		if len(c.Dispatches) != 0 {
			t.Errorf("candidate %d gained %d dispatches from being counted", i, len(c.Dispatches))
		}
		if c.Suspended {
			t.Errorf("candidate %d was suspended by being counted", i)
		}
	}

	// The runbook's second half: the owner lowers the floor to T2 by editing
	// hive.yaml, and exactly the T2 contributor qualifies.
	if got := QualifiedCount(pool, openLane(T2), noItem, tm, now); got != 1 {
		t.Errorf("after lowering the floor to T2, QualifiedCount = %d, want 1", got)
	}
}

func TestQualifiedCountOverAMixedPool(t *testing.T) {
	tm := tiers(t)
	spent := func(c Candidate) Candidate {
		c.Dispatches = []time.Time{now.Add(-time.Hour), now.Add(-2 * time.Hour)}
		return c
	}
	pool := []Candidate{
		approved(T1),          // qualifies
		approved(T2),          // qualifies at a T2 floor
		approved(T3),          // below the floor
		approved(TierUnknown), // nobody assessed it
		spent(approved(T1)),   // cap spent
		func() Candidate { c := approved(T1); c.Suspended = true; return c }(),
		func() Candidate { c := approved(T1); c.Approved = false; return c }(),
	}
	if got := QualifiedCount(pool, LanePolicy{Floor: T2, DailyCap: 2}, noItem, tm, now); got != 2 {
		t.Errorf("QualifiedCount = %d, want 2", got)
	}
	if got := QualifiedCount(nil, LanePolicy{Floor: T2, DailyCap: 2}, noItem, tm, now); got != 0 {
		t.Errorf("QualifiedCount(nil) = %d, want 0", got)
	}
}

// TestRejectionCarriesNoDelta is the anti-nudge rule as an assertion. No
// rejection may hand a caller the material for "you would qualify at T3" —
// not in the reason vocabulary, and not through an exported ordering.
func TestRejectionCarriesNoDelta(t *testing.T) {
	all := []Reason{
		ReasonQualified, ReasonLaneUnknown, ReasonStandbyDisabled, ReasonNotApproved,
		ReasonSuspended, ReasonConfigurationUnknown, ReasonBelowFloor, ReasonCapExhausted,
		ReasonFloorUnknown, ReasonItemTierUnknown, ReasonBelowItemTier,
	}
	for _, r := range all {
		s := strings.ToUpper(r.String())
		for _, tier := range []string{"T1", "T2", "T3"} {
			if strings.Contains(s, tier) {
				t.Errorf("reason %q names a tier; a rejection states its class, never the delta", r)
			}
		}
	}
	// The concrete case the design calls out: a T3 configuration against a T1
	// floor is told below_floor and nothing else.
	tm := tiers(t)
	_, reason := Qualifies(approved(T3), openLane(T1), noItem, tm, now)
	if reason != ReasonBelowFloor {
		t.Fatalf("reason = %q, want %q", reason, ReasonBelowFloor)
	}
	if strings.Contains(reason.String(), "T1") {
		t.Errorf("reason %q leaks the floor", reason)
	}
}

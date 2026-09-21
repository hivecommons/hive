package standby

import (
	"strings"
	"testing"
)

// itemList is the owner-authored list the tests match against: one class of
// item at each legal tier, each on a distinct label. The T3 entry names its
// signal, because a T3 entry that does not is a load error.
func itemList(t *testing.T) ItemTiers {
	t.Helper()
	m, err := NewItemTiers([]ItemTierEntry{
		{Label: "kind/security", Tier: T1},
		{Label: "refactor", Tier: T2},
		{Label: "dependencies", Tier: T3, Signal: "the lockfile diff plus green CI"},
	})
	if err != nil {
		t.Fatalf("NewItemTiers: %v", err)
	}
	return m
}

func itemFor(tier Tier) Item {
	switch tier {
	case T1:
		return Item{Repo: "hivecommons/hive", Labels: []string{"kind/security"}}
	case T2:
		return Item{Repo: "hivecommons/hive", Labels: []string{"refactor"}}
	case T3:
		return Item{Repo: "hivecommons/hive", Labels: []string{"dependencies"}}
	default:
		// Deliberately unlisted: an ordinary issue nobody said was donatable.
		return Item{Repo: "hivecommons/hive", Labels: []string{"kind/bug", "area/api"}}
	}
}

// TestBothBarsAreRequiredAcrossTheGrid walks the FULL lane floor × item tier ×
// configuration tier grid. A configuration must clear BOTH bars: a T1
// configuration may take a T3 item, a T3 configuration may not take a T1 item.
func TestBothBarsAreRequiredAcrossTheGrid(t *testing.T) {
	tm, items := tiers(t), itemList(t)
	legal := []Tier{T1, T2, T3}
	for _, floor := range legal {
		for _, itemTier := range legal {
			for _, cfg := range legal {
				match := items.Match(itemFor(itemTier), TierUnknown)
				if got := match.Tier(); got != itemTier {
					t.Fatalf("precondition: item for %s resolved to %s", itemTier, got)
				}
				clearsFloor := cfg.strength() >= floor.strength()
				clearsItem := cfg.strength() >= itemTier.strength()
				ok, reason := Qualifies(approved(cfg), openLane(floor), match, tm, now)
				if want := clearsFloor && clearsItem; ok != want {
					t.Errorf("floor %s, item %s, configuration %s: Qualifies = %v (%s), want %v",
						floor, itemTier, cfg, ok, reason, want)
				}
				wantReason := ReasonQualified
				switch {
				case !clearsFloor:
					// The floor is the lane's own bar and is checked first.
					wantReason = ReasonBelowFloor
				case !clearsItem:
					wantReason = ReasonBelowItemTier
				}
				if reason != wantReason {
					t.Errorf("floor %s, item %s, configuration %s: reason = %q, want %q",
						floor, itemTier, cfg, reason, wantReason)
				}
			}
		}
	}
}

// TestUnknownItemNeverQualifiesAndSaysWhy is the item half of the guard test
// the design asks for, and it asserts the REASON for the same reason its
// configuration-side twin does: unknown scores zero, so it would lose the
// strength comparison anyway and a test checking only `false` would keep
// passing with the explicit guard deleted. Asserting item_tier_unknown makes
// removing the guard a failure, because the candidate would then be reported
// as below_item_tier — which it is not. Nobody listed the item.
func TestUnknownItemNeverQualifiesAndSaysWhy(t *testing.T) {
	tm, items := tiers(t), itemList(t)
	unlisted := items.Match(itemFor(TierUnknown), TierUnknown)
	if got := unlisted.Tier(); got != TierUnknown {
		t.Fatalf("precondition: an unlisted item resolved to %s", got)
	}
	// Even the strongest configuration, against the weakest floor, with cap to
	// spare. An unlisted item is not weak work; it is work nobody said was
	// donatable.
	for _, floor := range []Tier{T1, T2, T3} {
		ok, reason := Qualifies(approved(T1), openLane(floor), unlisted, tm, now)
		if ok {
			t.Errorf("floor %s: an unlisted item was offered to a T1 configuration", floor)
		}
		if reason != ReasonItemTierUnknown {
			t.Errorf("floor %s: reason = %q, want %q — the explicit unknown-item guard states this invariant",
				floor, reason, ReasonItemTierUnknown)
		}
	}
	// An item with no labels at all is the unclassified case the design names.
	bare := items.Match(Item{Repo: "hivecommons/hive"}, TierUnknown)
	if ok, reason := Qualifies(approved(T1), openLane(T3), bare, tm, now); ok || reason != ReasonItemTierUnknown {
		t.Errorf("unclassified item: (%v, %q), want (false, %q)", ok, reason, ReasonItemTierUnknown)
	}
}

// TestEmptyItemListIsNotInForce is S7's acceptance rule: with the owner's list
// empty — the shipped state — nothing changes, and every decision is the S4
// decision. This is asserted on the SAME items that would be refused under a
// non-empty list.
func TestEmptyItemListIsNotInForce(t *testing.T) {
	tm := tiers(t)
	var shipped ItemTiers
	if shipped.InForce() || shipped.Len() != 0 {
		t.Fatalf("the shipped item list is in force (len %d); S7 must change nothing until an owner opts in", shipped.Len())
	}
	for _, item := range []Item{itemFor(T1), itemFor(TierUnknown), {}} {
		match := shipped.Match(item, T3)
		if match.Enforced() {
			t.Errorf("item %v: match is enforced against an empty list", item.Labels)
		}
		for _, floor := range []Tier{T1, T2, T3} {
			for _, cfg := range []Tier{T1, T2, T3} {
				withItem, reasonWith := Qualifies(approved(cfg), openLane(floor), match, tm, now)
				s4, reasonS4 := Qualifies(approved(cfg), openLane(floor), noItem, tm, now)
				if withItem != s4 || reasonWith != reasonS4 {
					t.Errorf("floor %s, configuration %s, item %v: (%v, %q) with an empty list, want the S4 answer (%v, %q)",
						floor, cfg, item.Labels, withItem, reasonWith, s4, reasonS4)
				}
			}
		}
	}
	// And the count the tile renders is the S4 count, item for item.
	pool := []Candidate{approved(T1), approved(T2), approved(T3)}
	queue := shipped.LaneQueue([]Item{itemFor(T1), itemFor(TierUnknown)}, nil)
	got := QualifiedCountForQueue(pool, openLane(T2), queue, tm, now)
	want := QualifiedCount(pool, openLane(T2), noItem, tm, now)
	if got != want {
		t.Errorf("QualifiedCountForQueue with an empty list = %d, want the S4 count %d", got, want)
	}
	if want != 2 {
		t.Fatalf("precondition: the S4 count is %d, want 2 — the assertion above would be vacuous", want)
	}
}

// TestOwnerListBeatsEveryProposal is the authority rule, across the whole
// grid of (what the owner listed) × (what the classifier proposed). The list
// wins in every disagreement, and a proposal never widens it.
func TestOwnerListBeatsEveryProposal(t *testing.T) {
	items := itemList(t)
	listed := map[Tier]Item{T1: itemFor(T1), T2: itemFor(T2), T3: itemFor(T3), TierUnknown: itemFor(TierUnknown)}
	for authoritative, item := range listed {
		for _, proposed := range []Tier{T1, T2, T3, TierUnknown, Tier("T0")} {
			match := items.Match(item, proposed)
			if got := match.Tier(); got != authoritative {
				t.Errorf("item listed as %s with a %s proposal resolved to %s — the owner's list is authoritative",
					authoritative, proposed, got)
			}
		}
	}

	// The case the design names: an item ABSENT from the list, proposed as T3,
	// does not qualify as T3. A T3 configuration is refused, and the reason is
	// that nobody listed the item — not that the configuration is too weak.
	tm := tiers(t)
	match := items.Match(itemFor(TierUnknown), T3)
	if got := match.Proposed(); got != T3 {
		t.Fatalf("precondition: the proposal was not carried (got %s)", got)
	}
	ok, reason := Qualifies(approved(T3), openLane(T3), match, tm, now)
	if ok || reason != ReasonItemTierUnknown {
		t.Errorf("an unlisted item proposed T3: (%v, %q), want (false, %q)", ok, reason, ReasonItemTierUnknown)
	}
	// And a listed item is not widened by a weaker proposal: the T1 class stays
	// T1 however cheap the classifier thought it looked.
	if ok, reason := Qualifies(approved(T3), openLane(T3), items.Match(itemFor(T1), T3), tm, now); ok || reason != ReasonBelowItemTier {
		t.Errorf("a T1-listed item proposed T3: (%v, %q), want (false, %q)", ok, reason, ReasonBelowItemTier)
	}
}

// TestOverlappingEntriesNarrowNeverWiden: when more than one entry matches an
// item — a repo-scoped entry and an unscoped one, or two labels the item
// carries — the strongest wins.
func TestOverlappingEntriesNarrowNeverWiden(t *testing.T) {
	m, err := NewItemTiers([]ItemTierEntry{
		{Label: "dependencies", Tier: T3, Signal: "the lockfile diff plus green CI"},
		{Repo: "hivecommons/hive", Label: "dependencies", Tier: T1},
		{Label: "kind/security", Tier: T1},
	})
	if err != nil {
		t.Fatalf("NewItemTiers: %v", err)
	}
	cases := []struct {
		name string
		item Item
		want Tier
	}{
		{"unscoped entry applies to any repository", Item{Repo: "someone/else", Labels: []string{"dependencies"}}, T3},
		{"the repo-scoped entry narrows it there", Item{Repo: "hivecommons/hive", Labels: []string{"dependencies"}}, T1},
		{"two labels: the stronger wins", Item{Repo: "other/repo", Labels: []string{"dependencies", "kind/security"}}, T1},
		{"repository spelling is folded", Item{Repo: "HiveCommons/Hive", Labels: []string{"Dependencies"}}, T1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.Tier(tc.item); got != tc.want {
				t.Errorf("Tier(%v in %s) = %s, want %s", tc.item.Labels, tc.item.Repo, got, tc.want)
			}
		})
	}
}

// TestItemLabelMatchingIsWholeLabelsAndSegments: the entry's label must be the
// whole label or a whole "/"-delimited segment of it. A substring is not a
// match — the #5856 rule, restated here because an item-tier list is the one
// place where a loose match would hand work to a weaker configuration.
func TestItemLabelMatchingIsWholeLabelsAndSegments(t *testing.T) {
	m, err := NewItemTiers([]ItemTierEntry{{Label: "dependencies", Tier: T2}})
	if err != nil {
		t.Fatalf("NewItemTiers: %v", err)
	}
	for _, tc := range []struct {
		label string
		want  Tier
	}{
		{"dependencies", T2},
		{"kind/dependencies", T2},
		{"DEPENDENCIES", T2},
		{"dependencies-bot", TierUnknown},
		{"external-dependencies", TierUnknown},
		{"dependenc", TierUnknown},
	} {
		if got := m.Tier(Item{Labels: []string{tc.label}}); got != tc.want {
			t.Errorf("label %q resolved to %s, want %s", tc.label, got, tc.want)
		}
	}
}

// TestNewItemTiersRejectsWhatWouldBeSilent mirrors the config loader's checks,
// so a list assembled in code fails the same way rather than failing open.
func TestNewItemTiersRejectsWhatWouldBeSilent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []ItemTierEntry
		wantErr string
	}{
		{"a tier that is not T1/T2/T3", []ItemTierEntry{{Label: "dependencies", Tier: Tier("T4")}}, "must be one of"},
		{"unknown as a tier", []ItemTierEntry{{Label: "dependencies", Tier: TierUnknown}}, "must be one of"},
		{"no label", []ItemTierEntry{{Tier: T2}}, "label is required"},
		{"a T3 entry naming no signal", []ItemTierEntry{{Label: "dependencies", Tier: T3}}, "must name the automated signal"},
		{"two entries for one class", []ItemTierEntry{
			{Label: "dependencies", Tier: T2},
			{Label: "Dependencies", Tier: T1},
		}, "duplicate item class"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewItemTiers(tc.entries)
			if err == nil {
				t.Fatalf("NewItemTiers accepted %v", tc.entries)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
			if m.InForce() {
				t.Errorf("a rejected list is in force; it must fail closed")
			}
		})
	}
}

// TestLaneQueueCountsAContributorOnce: M is a head count, comparable with the
// N beside it, so a contributor who could take several items still counts one.
func TestLaneQueueCountsAContributorOnce(t *testing.T) {
	tm, items := tiers(t), itemList(t)
	queue := items.LaneQueue([]Item{itemFor(T3), itemFor(T3), itemFor(TierUnknown)}, nil)
	if len(queue) != 3 {
		t.Fatalf("LaneQueue returned %d matches, want 3", len(queue))
	}
	pool := []Candidate{approved(T3), approved(T1)}
	if got := QualifiedCountForQueue(pool, openLane(T3), queue, tm, now); got != 2 {
		t.Errorf("QualifiedCountForQueue = %d, want 2 (each contributor counted once)", got)
	}
	// A queue of nothing but unlisted items qualifies nobody, and that is a
	// normal outcome: the lane stays paused.
	unlisted := items.LaneQueue([]Item{itemFor(TierUnknown), itemFor(TierUnknown)}, nil)
	if got := QualifiedCountForQueue(pool, openLane(T3), unlisted, tm, now); got != 0 {
		t.Errorf("QualifiedCountForQueue over unlisted items = %d, want 0", got)
	}
	// An empty queue is nothing to donate.
	if got := QualifiedCountForQueue(pool, openLane(T3), items.LaneQueue(nil, nil), tm, now); got != 0 {
		t.Errorf("QualifiedCountForQueue over an empty queue = %d, want 0", got)
	}
}

// TestItemRejectionCarriesNoDelta: the anti-nudge rule covers the item bar too.
// No refusal may hand a caller the material for "this item would be donatable
// at T3", and the item's tier is reachable only through the owner's own list.
func TestItemRejectionCarriesNoDelta(t *testing.T) {
	for _, r := range []Reason{ReasonItemTierUnknown, ReasonBelowItemTier} {
		s := strings.ToUpper(r.String())
		for _, tier := range []string{"T1", "T2", "T3"} {
			if strings.Contains(s, tier) {
				t.Errorf("reason %q names a tier; a refusal states its class, never the delta", r)
			}
		}
	}
}

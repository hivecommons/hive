package standby

import (
	"fmt"
	"strings"
)

// Item-tier matching, step S7 of the design's phase map
// (`src/docs/design/standby-contributors.md`).
//
// S4 matched a contributor's configuration against one bar: the lane's floor.
// S7 adds a second, carried by the WORK rather than by the lane — an item's
// own tier — and requires BOTH. A T1 configuration may take a T3 item; a T3
// configuration may not take a T1 item.
//
// Three rules here are load-bearing, and each has a test that fails when the
// code stops stating it:
//
//  1. The owner's list is AUTHORITATIVE. Hive's classifier can propose a tier
//     for an item (`classify.ProposeStandbyItemTier`), and the proposal is
//     carried through this package as data and consulted for nothing. An item
//     the owner never listed is `unknown` however confidently it was proposed,
//     and `unknown` is not standby-eligible at all.
//  2. The list ships EMPTY, and an empty list is NOT IN FORCE. With no entries
//     there is no item bar, the lane floor decides alone, and the S4 answer is
//     reproduced exactly — which is what makes S7 a no-op until an owner opts
//     in. The zero ItemMatch is that state, so a caller that has not resolved
//     an item gets S4's behaviour and never a silent item bar of `unknown`.
//  3. When several entries match one item, the STRONGEST wins. Overlap can
//     only ever narrow eligibility, so an owner who scopes a class to one
//     repository, or lists a label twice at different scopes, cannot widen
//     what qualifies by accident.

// Item is the work item being matched: the repository it belongs to and the
// labels it carries. Nothing else about an item is matched — not its title,
// not its body, not its author — because the owner's list is written in terms
// of labels, and a label is the one part of an item the owner also controls.
type Item struct {
	// Repo is the item's repository, "org/name" or a bare name. It is folded
	// for comparison; an entry with no repo applies to every repository.
	Repo string
	// Labels are the item's labels as GitHub reports them.
	Labels []string
}

// ItemTierEntry is one owner-authored row of the hive's item-tier list
// (HubConfig.StandbyItemTiers — pkg/config owns the key and its spelling).
type ItemTierEntry struct {
	// Repo scopes the entry to one repository. Empty applies it everywhere.
	Repo string
	// Label is the item label this entry is about.
	Label string
	// Tier is the capability tier a configuration must have to be offered an
	// item of this class.
	Tier Tier
	// Signal names the automated evidence that makes this class of item cheap
	// to review. It is REQUIRED on a T3 entry and carried, never evaluated:
	// Hive does not read the signal, the reviewer does.
	Signal string
}

// itemKey is the (repo, label) tuple an entry is stored under, case-folded.
type itemKey struct {
	repo  string
	label string
}

// ItemTiers is the owner's authoritative item-tier list. The zero value is
// usable and is the shipped state: empty, not in force.
type ItemTiers struct {
	entries map[itemKey]Tier
}

// NewItemTiers builds the list from owner-authored entries, reporting the
// misconfigurations that would otherwise be silent. It mirrors the checks
// pkg/config runs at load — a tier that is not T1/T2/T3, an entry with no
// label, a T3 entry that names no signal, and two entries disagreeing about
// one class of item — because a list assembled some other way must fail the
// same way rather than fail open.
func NewItemTiers(entries []ItemTierEntry) (ItemTiers, error) {
	m := ItemTiers{entries: make(map[itemKey]Tier, len(entries))}
	for i, e := range entries {
		tier, ok := ParseTier(string(e.Tier))
		if !ok {
			return ItemTiers{}, fmt.Errorf("standby item-tier entry [%d]: tier %q must be one of T1, T2, T3", i, e.Tier)
		}
		label := foldItemToken(e.Label)
		if label == "" {
			return ItemTiers{}, fmt.Errorf("standby item-tier entry [%d]: label is required", i)
		}
		// The verifiability rule from the design's answer to the RFC's fourth
		// open question: an item is T3-eligible only if its correctness is
		// established by an automated signal a reviewer can read without
		// reconstructing the change. An owner who cannot name that signal has
		// not met the test.
		if tier == T3 && strings.TrimSpace(e.Signal) == "" {
			return ItemTiers{}, fmt.Errorf("standby item-tier entry [%d] (%s): a T3 entry must name the automated signal that establishes its correctness", i, label)
		}
		key := itemKey{repo: foldItemToken(e.Repo), label: label}
		if prev, dup := m.entries[key]; dup {
			return ItemTiers{}, fmt.Errorf("standby item-tier entry [%d]: duplicate item class %s already mapped to %s", i, label, prev)
		}
		m.entries[key] = tier
	}
	return m, nil
}

// InForce reports whether item-tier matching applies on this hive at all.
//
// An EMPTY list is not in force, and that is the shipped state: no owner has
// said which items are donatable, so the item bar does not exist and the lane
// floor decides alone, exactly as in S4. It is deliberately not "an empty list
// admits nothing": S7 is required to change nothing until an owner opts in.
func (m ItemTiers) InForce() bool { return len(m.entries) > 0 }

// Len reports how many classes of item the owner has listed.
func (m ItemTiers) Len() int { return len(m.entries) }

// Tier resolves an item against the list alone. An item matching no entry is
// TierUnknown. When several entries match — an unscoped one and a
// repository-scoped one, or two labels the item carries — the STRONGEST wins,
// so overlap narrows eligibility and never widens it.
func (m ItemTiers) Tier(item Item) Tier {
	if len(m.entries) == 0 {
		return TierUnknown
	}
	repo := foldItemToken(item.Repo)
	best := TierUnknown
	for key, tier := range m.entries {
		if key.repo != "" && key.repo != repo {
			continue
		}
		if !itemCarriesLabel(item.Labels, key.label) {
			continue
		}
		if tier.strength() > best.strength() {
			best = tier
		}
	}
	return best
}

// Match resolves an item into the value Qualifies takes.
//
// It takes the classifier's proposal so that the one place a proposal meets
// the owner's list is this function — and it is where the proposal STOPS. The
// returned match's tier is the owner's list's answer and nothing else: not the
// proposal when the list is silent, not the proposal when the two disagree,
// not a blend. Proposed is carried for logging and attribution only. A caller
// that wanted to honour a proposal has nowhere to do it, which is the point.
func (m ItemTiers) Match(item Item, proposed Tier) ItemMatch {
	if !m.InForce() {
		// Not in force: no item bar at all. The proposal is still carried, so
		// a log line can say what was proposed on a hive that never opted in.
		return ItemMatch{proposed: NormalizeTier(string(proposed))}
	}
	return ItemMatch{
		tier:     m.Tier(item),
		proposed: NormalizeTier(string(proposed)),
		enforced: true,
	}
}

// LaneQueue resolves a lane's queued items into the queue QualifiedCountForQueue
// takes. propose may be nil.
//
// When the list is NOT in force it returns exactly one not-in-force match and
// does not look at the items at all: the count is then S4's, item for item.
func (m ItemTiers) LaneQueue(items []Item, propose func(Item) Tier) []ItemMatch {
	if !m.InForce() {
		return []ItemMatch{{}}
	}
	out := make([]ItemMatch, 0, len(items))
	for _, item := range items {
		proposed := TierUnknown
		if propose != nil {
			proposed = propose(item)
		}
		out = append(out, m.Match(item, proposed))
	}
	return out
}

// ItemMatch is the item half of a matching decision: what the owner's list
// says about one item, and whether item-tier matching is in force at all.
//
// Its fields are unexported and ItemTiers.Match is the only thing that fills
// them, so no caller can hand Qualifies an item tier the owner's list did not
// produce. The ZERO VALUE is "not in force" — the shipped state — which is why
// an S4-era caller that passes ItemMatch{} keeps S4's behaviour exactly.
type ItemMatch struct {
	tier     Tier
	proposed Tier
	enforced bool
}

// Tier is the item's tier as the owner's list resolved it. TierUnknown when
// the item is on no list, and when item-tier matching is not in force.
func (m ItemMatch) Tier() Tier { return m.tier }

// Enforced reports whether this match carries an item bar at all.
func (m ItemMatch) Enforced() bool { return m.enforced }

// Proposed is the classifier's candidate tier for the item, carried for logs
// and outcome rows. It decided nothing: Tier above is the owner's list.
func (m ItemMatch) Proposed() Tier { return m.proposed }

// foldItemToken folds case and surrounding whitespace on a repo or label so an
// owner who wrote "Dependencies" matches a label GitHub reports as
// "dependencies". It folds spelling only, never substance.
func foldItemToken(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// LabelMatches reports whether one label matches a token: the token must be
// the WHOLE label, or a whole "/"-delimited segment of it. Substring matching
// inside a label is deliberately not enough.
//
// This is the same rule lane routing uses (labelMatchesRoutingToken,
// `src/pkg/classify/classifier.go`), restated here rather than imported so
// this package stays dependency-free and pure —
// TestStandbyLabelMatchingAgreesWithLaneRouting in pkg/classify asserts the
// two cannot drift. The rule matters for the same reason it did there
// (kubestellar/hive#5856): "/" is a namespace separator, so a `dependencies`
// entry is meant to match `kind/dependencies`, while `ai-fix-requested` is one
// word and is not a match for `fix`.
func LabelMatches(label, token string) bool {
	label, token = foldItemToken(label), foldItemToken(token)
	if label == "" || token == "" {
		return false
	}
	if label == token {
		return true
	}
	for _, seg := range strings.Split(label, "/") {
		if seg == token {
			return true
		}
	}
	return false
}

func itemCarriesLabel(labels []string, token string) bool {
	for _, l := range labels {
		if LabelMatches(l, token) {
			return true
		}
	}
	return false
}

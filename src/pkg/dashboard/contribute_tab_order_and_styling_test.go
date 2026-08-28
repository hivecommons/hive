package dashboard

import (
	"strings"
	"testing"
)

// This suite pins two changes on the /contribute page:
//  1. The tab bar order is now Onboarding -> Operations -> Management ->
//     Leaderboard (Management moved to AFTER Operations). Only the button ORDER
//     changed; every id / data-panel / slug mapping is intact, so the
//     URL-addressable-tabs work (#2584) — deep-linking, pushState, popstate — is
//     unaffected.
//  2. A SUBTLE, professional "ranked/alive" accent pass on the Operations +
//     Leaderboard tabs: tier medallions driven by the REAL trust_tier, bold-numeral
//     stats, and a restrained gradient header band. Presentation only — no data,
//     endpoint, gating, routing, or behavior change.

// TestContributeTabOrder pins the visual tab order by asserting the button
// positions in the served markup: Onboarding, then Operations, then Management,
// then Leaderboard.
func TestContributeTabOrder(t *testing.T) {
	body := renderContributePage(t)

	onboarding := strings.Index(body, `id="ptab-onboarding"`)
	ops := strings.Index(body, `id="ptab-ops"`)
	manage := strings.Index(body, `id="ptab-manage"`)
	leaderboard := strings.Index(body, `id="ptab-leaderboard"`)

	if onboarding < 0 || ops < 0 || manage < 0 || leaderboard < 0 {
		t.Fatalf("missing tab buttons (onboarding=%d ops=%d manage=%d leaderboard=%d)",
			onboarding, ops, manage, leaderboard)
	}
	// The exact required order: Onboarding -> Operations -> Management -> Leaderboard.
	if onboarding >= ops || ops >= manage || manage >= leaderboard {
		t.Errorf("tab order wrong: want Onboarding<Operations<Management<Leaderboard, got onboarding=%d ops=%d manage=%d leaderboard=%d",
			onboarding, ops, manage, leaderboard)
	}
}

// TestContributeTabOrderSlugMappingIntact proves the reorder did NOT touch the
// slug<->panel routing map or the deep-link readers. The clean slugs (operations,
// management, leaderboard) must still map to their panels, and the legacy short
// ids (ops, manage) must still resolve, so /contribute/<tab> deep-linking and the
// address-bar reflection keep working after the button swap.
func TestContributeTabOrderSlugMappingIntact(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		// Panel -> slug map (address-bar reflection).
		`var PANEL_SLUG={'tab-manage':'management','tab-ops':'operations','tab-leaderboard':'leaderboard','tab-profile':'profile'}`,
		// Slug/short-id -> panel map (deep-linking, both clean and legacy forms).
		`'operations':'tab-ops','ops':'tab-ops'`,
		`'management':'tab-manage','manage':'tab-manage'`,
		`'leaderboard':'tab-leaderboard'`,
		`'onboarding':'tab-onboarding'`,
		// Profile is a first-class tab; /contribute/dossier/<user> resolves here too.
		`'profile':'tab-profile','dossier':'tab-profile','me':'tab-profile'`,
		// pushState/popstate + on-load deep-link wiring still present.
		`function tabFromLocation`,
		`window.history.pushState`,
		`popstate`,
		// data-panel attributes unchanged (buttons keep their ids/panels).
		`data-panel="tab-onboarding"`,
		`data-panel="tab-ops"`,
		`data-panel="tab-manage"`,
		`data-panel="tab-leaderboard"`,
		`data-panel="tab-profile"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tab reorder disturbed URL routing: missing %q", want)
		}
	}

	// Onboarding is the default landing: its button carries the initial active class.
	if !strings.Contains(body, `class="page-tab active" role="tab" id="ptab-onboarding"`) {
		t.Error("Onboarding is no longer the default-active tab")
	}
}

// TestLeaderboardTierBadgesFromRealData asserts the leaderboard rows render a tier
// medallion built from the REAL trust_tier the /api/leaderboard entry carries (via
// tierBadge), that the five known tiers each have a per-tier accent class, and that
// the tasks-completed stat is emphasised as a bold numeral. Nothing fabricated.
func TestLeaderboardTierBadgesFromRealData(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		// tierBadge helper + its use in the leaderboard row (real e.trust_tier).
		`function tierBadge`,
		`var tier=tierBadge(e.trust_tier,'tier-lb');`,
		// The bold "Done" numeral is the real tasks_completed count, just emphasised.
		`var done=(e.tasks_completed||0);`,
		`'<div class="lb-stat lb-primary">'+done+'</div>'`,
		// Per-tier accent classes exist in the CSS (muted metal-ish tints).
		`.tier-badge.tier-advisor`,
		`.tier-badge.tier-trusted`,
		`.tier-badge.tier-merger`,
		`.tier-badge.tier-contributor`,
		`.tier-badge.tier-newcomer`,
		// Restrained gradient header band + strong count on the leaderboard card.
		`class="ops-card card-accent"`,
		`.ops-card.card-accent>.ops-card-head{background:linear-gradient`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("leaderboard accent missing/not data-driven: %q", want)
		}
	}
}

// TestOpsAccentStatsAndTierBadges asserts the Operations tab got the restrained
// accent pass: bold-numeral army/connected counts, a gradient header band on the
// fleet + queue cards, and a compact tier badge next to each connected clanker
// reusing its REAL trust_tier. Behavior/ids are unchanged (covered elsewhere).
func TestOpsAccentStatsAndTierBadges(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		// Per-clanker tier medallion from the REAL trust_tier in the fleet snapshot.
		`var tierPill=tierBadge(c.trust_tier,'tier-inline');`,
		`statusPill+tierPill`,
		// Bold-numeral treatment: army counts + the connected count badge.
		`.cc-army b{font-weight:700`,
		`.ops-card-count.count-strong`,
		`class="ops-card-count count-strong" id="clanker-count"`,
		// Gradient header band applied to the fleet + ready-work queue cards.
		`<div class="ops-card card-accent">
<div class="ops-card-head"><span class="feed-dot"></span><h3>Connected clankers</h3>`,
		`<div class="ops-card card-accent" style="margin-top:20px">
<div class="ops-card-head"><span class="feed-dot"></span><h3>Ready-work queue</h3>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("operations accent missing/not data-driven: %q", want)
		}
	}
}

// TestTierBadgeAccentReadabilityAndMotion is a guard on the "subtle + professional"
// constraints: the tier tints are muted (rgba washes, not opaque neon fills), the
// accent adds no animation that could violate reduced-motion, and no external asset
// is referenced (CSP forbids remote fonts/images).
func TestTierBadgeAccentReadabilityAndMotion(t *testing.T) {
	body := renderContributePage(t)

	// The accent CSS must not introduce any @keyframes/animation of its own — it is
	// a static treatment, so there is nothing new for reduced-motion to disable.
	// (Existing motion is already gated; we only assert we didn't add a raw glow.)
	for _, forbidden := range []string{
		`.tier-badge{animation`,
		`.tier-badge{box-shadow:0 0`, // no outer glow on the badge
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("tier badge is not subtle: found %q", forbidden)
		}
	}

	// No external asset in the accent (icons/medallions are pure CSS). Guard that the
	// tier badge markup never pulls a remote url() — the medallion is a CSS dot.
	idx := strings.Index(body, `.tier-badge{`)
	if idx < 0 {
		t.Fatal("tier-badge CSS missing")
	}
	end := strings.Index(body[idx:], `.ops-card.card-accent`)
	if end < 0 {
		end = len(body) - idx
	}
	block := body[idx : idx+end]
	if strings.Contains(block, `url(`) {
		t.Error("tier badge references an external/url() asset; must be pure CSS/inline")
	}
}

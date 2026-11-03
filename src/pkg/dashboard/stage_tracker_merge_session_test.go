package dashboard

import (
	"strings"
	"testing"
)

// Exercise the real reconcile entry point with a shared sessionStorage but
// reset SPA memory, as a reload would. Delivery enters the detached glyph
// pipeline, which counts and plays the merge sound only after landing.
func TestStageTrackerMergeSessionNode(t *testing.T) {
	html := indexHTML(t)
	script := stageTrackerCelebrateJS(t, html) + stageTrackerCelebrateFixture + `
function reload() {
  _stageTrackerMergeSessionLoaded = false;
  _stageTrackerMergeState = null;
  _stageTrackerZapped = {};
}
stageTrackerReconcileMerges(items(42));
// Webhook eviction can precede the audit fact, including across a reload.
stageTrackerReconcileMerges({ items: [] });
assert.strictEqual(banners.children.length, 0, 'absence alone is not a merge');
reload();
const merged = { items: [], recently_merged: [{ repo: 'r', number: 42 }] };
stageTrackerReconcileMerges(merged);
tick(0);
assert.strictEqual(banners.children.length, 1, 'last row absent, short audit repo still resolves after reload');
assert.strictEqual(_stageTrackerMergeState['o/r/pr#42'], true);
assert.strictEqual(_stageTrackerZapped['o/r/pr#42'], true);
assert.strictEqual(flights().length, 1, 'exactly one delivery to the detached celebration pipeline');
assert.strictEqual(zaps.length, 0, 'sound waits for the bee to land');
tick(STAGE_TRACKER_CELEBRATE_FLY_MS * 1.5 + 10);
assert.strictEqual(flights().length, 0, 'bee landed');
assert.strictEqual(zaps.length, 1, 'exactly one delivery to the celebration/count pipeline');
stageTrackerReconcileMerges({ items: [], merged: { recent: [{ repo: 'o/r', number: 42 }] } });
reload();
stageTrackerReconcileMerges(merged);
tick(0);
assert.strictEqual(banners.children.length, 1, 'reload after delivery does not repeat the banner');
assert.strictEqual(flights().length, 0, 'reload after delivery does not repeat the bee');
tick(STAGE_TRACKER_CELEBRATE_FLY_MS * 1.5 + 10);
assert.strictEqual(zaps.length, 1, 'ledger + audit + reload do not repeat the counter tick');
// Delivered identity outlives the server's recent-history window, even
// if a stale open snapshot resurfaces after another reload.
stageTrackerReconcileMerges({ items: [] });
reload();
stageTrackerReconcileMerges(items(42));
stageTrackerReconcileMerges(payloadMerged(42));
tick(0);
assert.strictEqual(banners.children.length, 1, 'expired recent history cannot erase delivered identity');
// A tab's first snapshot is a baseline, not a replay of old merges.
delete session[STAGE_TRACKER_MERGE_SESSION_KEY];
reload();
stageTrackerReconcileMerges(payloadMerged(99));
tick(0);
assert.strictEqual(banners.children.length, 1);
// A corrupt/unavailable browser store must not break normal live delivery.
session[STAGE_TRACKER_MERGE_SESSION_KEY] = '{broken';
reload();
stageTrackerReconcileMerges(items(43));
stageTrackerReconcileMerges(payloadMerged(43));
tick(0);
assert.strictEqual(banners.children.length, 2);
`
	runStageTrackerRowActionsScript(t, script)
}

func TestStageTrackerMergeBannerCountdownNode(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerCelebrateJS(t, html)+stageTrackerCelebrateFixture+`
const ttl = 2468;
stageTrackerMergeBannerMs = () => ttl;
const b = stageTrackerMergeBanner({ key: 'o/r/pr#7', repo: 'o/r', number: 7, at: clock });
assert.ok(b.querySelector('.toast-bar'), 'merge banner draws the shared countdown');
assert.strictEqual(b.style.props['--toast-ttl'], ttl + 'ms');
tick(ttl - 1);
assert.ok(b.parentNode, 'banner remains for its configured lifetime');
tick(1);
assert.ok(b.classList.contains('merge-banner-out'), 'shared timer dismisses at the bar deadline');
// No animationend is emitted by the fake DOM: allow the shared
// stageTrackerAfter fallback (1.5 times the 180ms exit animation) to fire.
tick(270);
assert.strictEqual(b.parentNode, null);
const manual = stageTrackerMergeBanner({ key: 'o/r/pr#8', repo: 'o/r', number: 8 });
assert.ok(manual._countdown.timer);
stageTrackerMergeBannerDismiss(manual);
assert.strictEqual(manual._countdown, null, 'manual dismissal cancels countdown');
`)
	if !strings.Contains(cssRule(t, html, ".merge-banner"), "position: relative;") {
		t.Fatal("merge countdown must be positioned within its own banner")
	}
}

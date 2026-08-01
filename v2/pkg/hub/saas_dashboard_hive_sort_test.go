package hub

import (
	"strings"
	"testing"
)

// The hive-table sort and the scroll/sort persistence live inside the
// dashboardHTML raw string, so there is no Go function to call. These tests
// guard the wiring the same way the neighbouring dashboard tests do: every
// handler the markup references must exist, and the invariants that are silent
// when broken must stay asserted somewhere CI runs.
//
// "Silent when broken" is the whole point here. If the default sort key stops
// being 'name', or a persisted choice starts being overwritten by the default,
// or the scroll restore moves back ahead of the render that gives the document
// its height, nothing throws — the table just quietly ignores the operator.

// TestHiveSortDefaultsToAlphabetical pins the first-visit ordering to the Hive
// column, ascending. Before this the table rendered in registry order, which is
// arrival order and reads as arbitrary.
func TestHiveSortDefaultsToAlphabetical(t *testing.T) {
	html := dashScript(t)
	for _, snippet := range []string{
		"var HIVE_SORT_DEFAULT_KEY = 'name';",
		"var HIVE_SORT_DEFAULT_ASC = true;",
		// The state seed must match the default so the very first paint —
		// paintCachedHives, which runs before any network call — is already A-Z.
		"var _dashSortKey = 'name', _dashSortAsc = true;",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("dashboardHTML is missing %q — the default hive sort is no longer A-Z", snippet)
		}
	}
}

// TestHiveSortPersistenceWiring asserts the persisted choice survives a reload
// and, critically, that the A-Z default does NOT clobber it.
func TestHiveSortPersistenceWiring(t *testing.T) {
	html := dashScript(t)
	for _, snippet := range []string{
		"var LS_HIVE_SORT = 'hive-sort';",
		"function persistHiveSort()",
		"function loadHiveSortPrefs()",
		// A header click must record the choice, otherwise nothing is ever
		// persisted and the reload silently falls back to the default.
		"persistHiveSort();",
		// The stored value is validated before use, so a stale key degrades to
		// the default instead of sorting on a property no row has.
		"function isHiveSortKey(key)",
		"isHiveSortKey(stored.key)",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("dashboardHTML is missing %q — hive sort persistence is not wired", snippet)
		}
	}

	// loadHiveSortPrefs must run at init, AFTER loadDashViewPrefs: applying a
	// default saved view sets a sort of its own, and the operator's explicit
	// stored sort is the more specific signal, so it has to have the last word.
	viewIdx := strings.Index(html, "loadDashViewPrefs();")
	sortIdx := strings.Index(html, "loadHiveSortPrefs();")
	if viewIdx < 0 || sortIdx < 0 {
		t.Fatal("init no longer calls loadDashViewPrefs()/loadHiveSortPrefs()")
	}
	if sortIdx < viewIdx {
		t.Error("loadHiveSortPrefs() runs before loadDashViewPrefs() — a default saved view would override the operator's explicit sort")
	}
}

// TestHiveScrollRestoreRunsAfterRender is the ordering guard. The rows paint
// asynchronously, and the document is only as tall as what has been painted, so
// scrolling before the table exists clamps to the current maximum and silently
// lands at the top — the exact bug the feature is meant to fix.
func TestHiveScrollRestoreRunsAfterRender(t *testing.T) {
	html := dashScript(t)
	for _, snippet := range []string{
		"var LS_HIVE_SCROLL = 'hive-scroll';",
		"function persistHiveScroll()",
		"function readHiveScroll()",
		"function restoreHiveScroll()",
		// Throttled through rAF: scroll fires at input rate and setItem is
		// synchronous, so an unthrottled write janks the scroll it records.
		"window.requestAnimationFrame(",
		"window.addEventListener('scroll', persistHiveScroll, {passive: true});",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("dashboardHTML is missing %q — scroll persistence is not wired", snippet)
		}
	}

	loadIdx := strings.Index(html, "await loadHives();")
	restoreIdx := strings.Index(html, "restoreHiveScroll();")
	if loadIdx < 0 || restoreIdx < 0 {
		t.Fatal("init no longer calls loadHives()/restoreHiveScroll()")
	}
	if restoreIdx < loadIdx {
		t.Error("restoreHiveScroll() runs before await loadHives() — the rows have not rendered yet, so the offset clamps to a short page and the restore silently does nothing")
	}
}

// TestHiveProvisionSortUsesRegisteredAt pins the timestamp field.
//
// registeredAt, NOT the per-hive meta.json created_at: created_at is written on
// exactly one provisioning path, so hives created any other way — or before the
// field existed — carry the empty string. On the live fleet 26 of 50 meta.json
// files have "created_at": "" while registeredAt was populated on 50 of 50
// registry entries. registeredAt is also carried forward across heartbeats
// (see the RegisteredAt copy in handleHeartbeat) rather than restamped, so it
// stays first-seen time instead of drifting to "now" on every beat.
func TestHiveProvisionSortUsesRegisteredAt(t *testing.T) {
	html := dashScript(t)
	for _, snippet := range []string{
		"function hiveProvisionTime(h)",
		"var raw = h && h.registeredAt;",
		"sortDashHives(\\'registeredAt\\')",
		// The provision-date sort survives as a thin "Prov ⇅" header (the date
		// itself moved into the status hover); the sort trigger must remain.
		"Prov ⇅",
		// registeredAt must be a recognised sort key, otherwise a persisted
		// choice of it would be rejected on the next load.
		"'registeredAt'",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("dashboardHTML is missing %q — provision-date sort is not wired to registeredAt", snippet)
		}
	}

	// created_at must NOT be what the hive table sorts on. It is still legitimately
	// used by the admin USERS table (fmtUserTS / sortUsers), so assert on the
	// hive-side accessor specifically rather than banning the string outright.
	if strings.Contains(html, "h.created_at") {
		t.Error("the hive table reads h.created_at — it is empty on over half the live fleet; use registeredAt")
	}
}

// TestHiveProvisionTimeGuardedByUsability guards the render path: the provisioned
// time (now shown in the status hover panel, not a dedicated column) must only be
// emitted when hiveProvisionTime says it is usable, so a hive with an empty
// registeredAt never renders the "Invalid Date" that new Date(”) stringifies to.
func TestHiveProvisionTimeGuardedByUsability(t *testing.T) {
	html := dashScript(t)
	// The hover line and the sort comparator must agree on "is this usable", so
	// the line is gated on hiveProvisionTime rather than testing the raw field.
	if !strings.Contains(html, "hiveProvisionTime(h) !== null") {
		t.Error("the provisioned hover line is not gated on hiveProvisionTime — a hive with an empty registeredAt could render 'Invalid Date'")
	}
	// And it must have left the table body: no standalone Provisioned <td>/<th>.
	if strings.Contains(html, ">Provisioned ⇅</th>") {
		t.Error("the Provisioned column header is still present — it was meant to move into the status hover panel")
	}
}

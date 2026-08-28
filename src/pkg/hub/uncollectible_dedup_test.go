package hub

import (
	"testing"
)

// The uncollectible-upgrade de-duplication memory used to be a package-level
// global. These tests pin the two properties that fixed: it is per-server, and
// its entries have a lifetime bounded by hive removal.

func countUncollectibleTimeline(s *HubServer, hiveID string) int {
	n := 0
	for _, e := range s.timeline.recent(hiveID, 100) {
		if e.Kind == TimelineUpgradeStale {
			n++
		}
	}
	return n
}

// TestUncollectibleDedupIsPerServer is THE regression. With a shared global, the
// first server's note suppressed the second server's — the two hubs having the
// same hive ID is the normal case, not an exotic one. An operator watching the
// second hub would see no refusal at all, which is the precise silence this
// timeline entry exists to break.
func TestUncollectibleDedupIsPerServer(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s1 := pullOnlyTestServer(t)
	s2 := pullOnlyTestServer(t)

	s1.noteUncollectibleUpgrade("shared-id", "7cd059b", "never heartbeated")
	s2.noteUncollectibleUpgrade("shared-id", "7cd059b", "never heartbeated")

	if got := countUncollectibleTimeline(s1, "shared-id"); got != 1 {
		t.Errorf("server 1 timeline entries = %d, want 1", got)
	}
	if got := countUncollectibleTimeline(s2, "shared-id"); got != 1 {
		t.Errorf("server 2 timeline entries = %d, want 1 — the second hub's refusal was "+
			"suppressed by the first hub's de-dup state, so its operator sees silence", got)
	}
}

// TestUncollectibleDedupStillSuppressesRepeatsOnSameServer is the positive
// control: making the memory per-server must not disable de-duplication itself.
// The poller ticks every 2 minutes, so a lost de-dup means ~720 identical
// timeline entries per hive per day.
func TestUncollectibleDedupStillSuppressesRepeatsOnSameServer(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	for i := 0; i < 10; i++ {
		s.noteUncollectibleUpgrade("dedup-1", "7cd059b", "never heartbeated")
	}

	if got := countUncollectibleTimeline(s, "dedup-1"); got != 1 {
		t.Errorf("timeline entries = %d, want 1 — de-duplication must still hold, or the "+
			"fix trades an unbounded re-arm loop for an unbounded timeline", got)
	}
}

// TestUncollectibleDedupReportsNewTarget pins the keying: the memory is keyed on
// the TARGET, so a genuinely new upgrade opportunity (the branch advanced) is
// reported again rather than suppressed by a stale entry.
func TestUncollectibleDedupReportsNewTarget(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.noteUncollectibleUpgrade("target-1", "7cd059b", "never heartbeated")
	s.noteUncollectibleUpgrade("target-1", "7cd059b", "never heartbeated")
	s.noteUncollectibleUpgrade("target-1", "aaaa111", "never heartbeated")

	if got := countUncollectibleTimeline(s, "target-1"); got != 2 {
		t.Errorf("timeline entries = %d, want 2 — a new target is a new refusal and must "+
			"be reported afresh", got)
	}
}

// TestUncollectibleDedupDroppedOnHiveRemoval covers the leak. An uncollectible
// hive is BY DEFINITION never armed, so the arming path could never retire its
// entry; removal is the only opportunity. Without this the map grew without
// bound across hive churn — contradicting the bounded-growth claim in the
// comment that justified it — and precisely for the unassigned-placeholder
// population this code exists to handle.
func TestUncollectibleDedupDroppedOnHiveRemoval(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.registry.Hives = []RegistryEntry{{ID: "doomed", GitBranch: "v4"}}
	s.noteUncollectibleUpgrade("doomed", "7cd059b", "never heartbeated")

	s.uncollectibleUpgradeMu.Lock()
	_, present := s.uncollectibleUpgradeNoted["doomed"]
	s.uncollectibleUpgradeMu.Unlock()
	if !present {
		t.Fatal("precondition: the note should be remembered before removal")
	}

	s.removeRegistryEntry("doomed", "alice")

	s.uncollectibleUpgradeMu.Lock()
	_, stillThere := s.uncollectibleUpgradeNoted["doomed"]
	s.uncollectibleUpgradeMu.Unlock()
	if stillThere {
		t.Error("de-dup entry survived hive removal — entries leak for every deleted hive, " +
			"and an uncollectible hive is never armed so nothing else can ever drop it")
	}
}

// TestUncollectibleDedupRearmedAfterHiveReturns is the behavioural consequence
// of the removal hook: a recycled ID reports its refusal afresh rather than
// inheriting a dead predecessor's suppression.
func TestUncollectibleDedupRearmedAfterHiveReturns(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.registry.Hives = []RegistryEntry{{ID: "recycled", GitBranch: "v4"}}
	s.noteUncollectibleUpgrade("recycled", "7cd059b", "never heartbeated")
	s.removeRegistryEntry("recycled", "alice")
	s.noteUncollectibleUpgrade("recycled", "7cd059b", "never heartbeated")

	if got := countUncollectibleTimeline(s, "recycled"); got != 2 {
		t.Errorf("timeline entries = %d, want 2 — after removal the same refusal must be "+
			"reported again for a hive that comes back", got)
	}
}

// TestNoteUncollectibleUpgradeOnBareServer guards the nil-map case: several
// tests and NewHubServer paths build a HubServer without pre-allocating this
// map, so noteUncollectibleUpgrade must allocate lazily rather than panic.
func TestNoteUncollectibleUpgradeOnBareServer(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{timeline: newTimelineStore()}
	s.noteUncollectibleUpgrade("bare", "7cd059b", "never heartbeated")

	if got := countUncollectibleTimeline(s, "bare"); got != 1 {
		t.Errorf("timeline entries = %d, want 1", got)
	}
}

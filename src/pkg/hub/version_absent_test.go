package hub

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// The production signature this exists for: hosted-llm-d-llm-d-worklo-wva1
// beat every 120s for hours while its own stats collection timed out, so every
// beat carried last-good cached stats with an EMPTY git_hash. The hub had
// nothing to compare against the branch target and sent it no upgrade
// instruction at all.
//
// N-1 version-less beats must stay silent (a spoke legitimately omits the
// version during startup); the Nth fires and stays fired while the run
// continues.
func TestNoteVersionAbsentFiresOnlyAfterThreshold(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	for i := 1; i < versionAbsentBeatsToConfirm; i++ {
		if s.noteVersionAbsent("h1", "") {
			t.Fatalf("fired after only %d version-less beats (threshold is %d)", i, versionAbsentBeatsToConfirm)
		}
	}
	if !s.noteVersionAbsent("h1", "") {
		t.Fatalf("%d consecutive version-less beats did not fire", versionAbsentBeatsToConfirm)
	}
	// And it stays fired while the hive keeps beating without a version.
	for i := 0; i < 5; i++ {
		if !s.noteVersionAbsent("h1", "") {
			t.Fatal("an ongoing version-less run lost the flag")
		}
	}
}

// A beat WITH a version resets the run outright: the hub can compare again, so
// the streak of blindness is over. A later run must start from scratch rather
// than resuming a saturated counter.
func TestNoteVersionAbsentResetsOnReportedVersion(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	for i := 0; i < versionAbsentBeatsToConfirm+2; i++ {
		s.noteVersionAbsent("h2", "")
	}
	if s.noteVersionAbsent("h2", "ef5a603a") {
		t.Fatal("a beat carrying a version did not clear the flag")
	}
	// The next run must climb from zero, not resume where it left off.
	for i := 1; i < versionAbsentBeatsToConfirm; i++ {
		if s.noteVersionAbsent("h2", "") {
			t.Fatalf("counter resumed instead of restarting: fired after %d beats", i)
		}
	}
	if !s.noteVersionAbsent("h2", "") {
		t.Fatal("a fresh full run after a reset did not fire")
	}
}

// A healthy hive that always reports its version never fires, however long it
// beats.
func TestNoteVersionAbsentSilentOnHealthyHive(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	for i := 0; i < 20; i++ {
		if s.noteVersionAbsent("h3", "14c75aa0") {
			t.Fatal("a hive reporting its version every beat was flagged")
		}
	}
}

// Runs are tracked per hive: one mute hive must not drag a healthy neighbour
// into the signal.
func TestNoteVersionAbsentIsPerHive(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	for i := 0; i < versionAbsentBeatsToConfirm; i++ {
		s.noteVersionAbsent("mute", "")
		if s.noteVersionAbsent("healthy", "ef5a603a") {
			t.Fatal("a healthy hive was flagged alongside a mute one")
		}
	}
	if !s.noteVersionAbsent("mute", "") {
		t.Fatal("the mute hive lost its flag to a neighbour's beats")
	}
}

// The drift signal must fire on an online claimed hive, use the stable kind,
// be critical, and say plainly that the hive is not receiving upgrades.
func TestVersionAbsentDriftSignal(t *testing.T) {
	h := MyHiveEntry{RegistryEntry: RegistryEntry{
		ID:            "h1",
		Online:        true,
		VersionAbsent: true,
	}}
	sig, ok := hasSignal(computeDrift(h, fleetNorm{}, nil, time.Now()), DriftKindVersionAbsent)
	if !ok {
		t.Fatal("a version-absent hive raised no version-absent signal")
	}
	if sig.Severity != DriftCritical {
		t.Errorf("severity = %q, want %q", sig.Severity, DriftCritical)
	}
	if !strings.Contains(sig.Reason, "upgrade instruction") {
		t.Errorf("reason does not say the hive is not being upgraded: %q", sig.Reason)
	}
	if !strings.Contains(sig.Reason, "cannot be trusted") {
		t.Errorf("reason does not say the row cannot be trusted: %q", sig.Reason)
	}
}

// A hive whose beats carry a version never raises the signal, whatever else is
// true of it.
func TestVersionAbsentDriftSilentWhenVersionReported(t *testing.T) {
	h := MyHiveEntry{RegistryEntry: RegistryEntry{ID: "h1", Online: true, GitHash: "ef5a603a"}}
	if _, ok := hasSignal(computeDrift(h, fleetNorm{}, nil, time.Now()), DriftKindVersionAbsent); ok {
		t.Fatal("a hive reporting its version raised version-absent")
	}
}

// Offline hives and pool placeholders never fire, for the same reason the
// fleet-relative signals skip them: an offline hive is already flagged as not
// reporting, and a placeholder's version is not being upgraded toward anything.
func TestVersionAbsentDriftSkipsOfflineAndPlaceholders(t *testing.T) {
	offline := MyHiveEntry{RegistryEntry: RegistryEntry{
		ID:            "h-offline",
		Online:        false,
		VersionAbsent: true,
	}}
	if _, ok := hasSignal(computeDrift(offline, fleetNorm{}, nil, time.Now()), DriftKindVersionAbsent); ok {
		t.Error("an offline hive raised version-absent")
	}

	// ProvStatus is the authoritative placeholder marker. It is set on the
	// MyHiveEntry itself, which shadows the embedded RegistryEntry field -
	// isPlaceholderEntry reads the outer one.
	byStatus := MyHiveEntry{
		RegistryEntry: RegistryEntry{ID: "h-pool", Online: true, VersionAbsent: true},
		ProvStatus:    statusAvailable,
	}
	if _, ok := hasSignal(computeDrift(byStatus, fleetNorm{}, nil, time.Now()), DriftKindVersionAbsent); ok {
		t.Error("a placeholder (provStatus available) raised version-absent")
	}

	// The org-prefix fallback for placeholders that have not reported
	// provStatus yet.
	byPrefix := MyHiveEntry{RegistryEntry: RegistryEntry{
		ID:            "h-pool-2",
		Online:        true,
		VersionAbsent: true,
		Org:           placeholderOrgPrefix + "vllmd-01",
	}}
	if _, ok := hasSignal(computeDrift(byPrefix, fleetNorm{}, nil, time.Now()), DriftKindVersionAbsent); ok {
		t.Error("a placeholder (available- org prefix) raised version-absent")
	}
}

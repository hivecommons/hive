package hub

import (
	"log/slog"
	"testing"
	"time"
)

// The production signature this exists for: WVA reported ok at :22 and
// key-invalid at :58, every cycle, for an hour ("" is how a healthy state
// arrives — it must count in the alternation, not be skipped).
func TestNoteStatusFlipFlagsOscillation(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	beats := []string{"", "key-invalid", "", "key-invalid", "", "key-invalid", ""}
	flagged := false
	for _, st := range beats {
		flagged = s.noteStatusFlip("h1", st)
	}
	if !flagged {
		t.Fatal("a sustained ok ↔ key-invalid oscillation was not flagged")
	}
	// And it stays flagged while the oscillation continues.
	if !s.noteStatusFlip("h1", "key-invalid") {
		t.Error("ongoing oscillation lost the flag")
	}
}

// Genuine transitions must stay silent: a hive that degrades, recovers once,
// or walks through DIFFERENT states is having a normal life, not oscillating.
func TestNoteStatusFlipSilentOnRealTransitions(t *testing.T) {
	s := &HubServer{logger: slog.Default()}

	// Degrade and stay degraded.
	for _, st := range []string{"", "key-invalid", "key-invalid", "key-invalid"} {
		if s.noteStatusFlip("h2", st) {
			t.Fatal("a plain degradation was flagged as flipping")
		}
	}

	// One bounce: degraded → recovered → degraded is below the threshold.
	s2 := &HubServer{logger: slog.Default()}
	for _, st := range []string{"", "not-installed", "", "not-installed"} {
		if s2.noteStatusFlip("h3", st) {
			t.Fatal("a single bounce was flagged as flipping")
		}
	}

	// Walking through distinct states never flags: each is a fresh
	// transition, not a return.
	s3 := &HubServer{logger: slog.Default()}
	for _, st := range []string{"", "key-missing", "not-installed", "wrong-installation", "key-invalid"} {
		if s3.noteStatusFlip("h4", st) {
			t.Fatal("a walk through distinct states was flagged as flipping")
		}
	}
}

// The drift signal must fire on a flipping hive, use the stable kind, and be
// suppressed when duplicate-spoke already names the culprits — one
// oscillation must not count a hive twice in the summary strip.
func TestStatusFlippingDriftSignal(t *testing.T) {
	h := MyHiveEntry{RegistryEntry: RegistryEntry{ID: "h1", StatusFlipping: true}}
	if _, ok := hasSignal(computeDrift(h, fleetNorm{}, nil, time.Now()), DriftKindStatusFlipping); !ok {
		t.Fatal("flipping hive raised no status-flipping signal")
	}

	h.ConflictingReporters = "pod-a ↔ pod-b"
	rep := computeDrift(h, fleetNorm{}, nil, time.Now())
	if _, ok := hasSignal(rep, DriftKindStatusFlipping); ok {
		t.Error("status-flipping not suppressed when duplicate-spoke names the culprits")
	}
	if _, ok := hasSignal(rep, DriftKindDuplicateSpoke); !ok {
		t.Error("duplicate-spoke signal missing")
	}
}

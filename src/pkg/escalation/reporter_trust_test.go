package escalation

import (
	"path/filepath"
	"testing"
)

func TestReporterTrustHoldSurvivesGreenAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	store := Load(path)
	store.SetReporterTrustReason("o/r", 42, "reporter-trust hold — issue #581 filed by @stranger")
	store = Load(path)
	obs := []Observation{{Repo: "o/r", Number: 42, HeadSHA: "green", Labeled: true}}
	result := store.Sweep(obs, 3)[Key("o/r", 42)]
	if !result.Escalated || result.Unparked {
		t.Fatalf("green policy hold = %+v", result)
	}
	if store.SetReporterTrustReason("o/r", 42, "") {
		t.Fatal("policy gate fabricated an independent CI escalation")
	}
	result = store.Sweep(obs, 3)[Key("o/r", 42)]
	if !result.Unparked {
		t.Fatalf("released hold = %+v", result)
	}
}

func TestReporterTrustReleasePreservesIndependentEscalation(t *testing.T) {
	store := Load(filepath.Join(t.TempDir(), "ledger.json"))
	store.SetReporterTrustReason("o/r", 42, "reporter-trust hold")
	store.MarkEscalated("o/r", 42)
	if !store.SetReporterTrustReason("o/r", 42, "") {
		t.Fatal("release erased independent escalation")
	}
}

package crux

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestKickDispatchPending(t *testing.T) {
	if !(KickDispatch{Phase: KickPhasePending}).Pending() {
		t.Fatal("pending dispatch must report Pending")
	}
	for _, phase := range []string{KickPhaseDelivered, KickPhaseFailed, ""} {
		if (KickDispatch{Phase: phase}).Pending() {
			t.Fatalf("phase %q must not report Pending", phase)
		}
	}
}

func TestKickOutcomeSettled(t *testing.T) {
	kickAt := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	o := KickOutcome{Kind: KickOutcomeEnded, KickAt: kickAt}
	if !o.Settled(&kickAt) {
		t.Fatal("outcome for this kick must be settled")
	}
	if o.Settled(nil) {
		t.Fatal("no kick means nothing to settle")
	}
	later := kickAt.Add(time.Minute)
	if o.Settled(&later) {
		t.Fatal("outcome for an earlier kick must not settle a newer one")
	}
	if (KickOutcome{KickAt: kickAt}).Settled(&kickAt) {
		t.Fatal("an outcome with no kind is not classified")
	}
}

func TestKickDispatchJSONOmitsZeroSettledAt(t *testing.T) {
	// Pending dispatch with zero SettledAt should not include the field
	pending := KickDispatch{
		Agent:    "a",
		Phase:    KickPhasePending,
		QueuedAt: time.Now(),
	}
	data, err := json.Marshal(pending)
	if err != nil {
		t.Fatalf("marshal pending dispatch failed: %v", err)
	}
	if strings.Contains(string(data), "settledAt") {
		t.Errorf("pending dispatch JSON must not contain settledAt, got: %s", string(data))
	}

	// Dispatch with non-zero SettledAt should include the field
	settled := KickDispatch{
		Agent:     "a",
		Phase:     KickPhaseDelivered,
		QueuedAt:  time.Now(),
		SettledAt: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC),
	}
	data, err = json.Marshal(settled)
	if err != nil {
		t.Fatalf("marshal settled dispatch failed: %v", err)
	}
	if !strings.Contains(string(data), "settledAt") {
		t.Errorf("settled dispatch JSON must contain settledAt, got: %s", string(data))
	}
}

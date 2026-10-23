package types

import (
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

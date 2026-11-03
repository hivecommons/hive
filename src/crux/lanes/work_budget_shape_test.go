package lanes

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestMaximumCompletionShapeFitsReservedStateAndCASHeader(t *testing.T) {
	state, worstLease, err := maximumGroupShape()
	if err != nil {
		t.Fatal(err)
	}
	base, delivery, err := completionBudget()
	if err != nil {
		t.Fatal(err)
	}
	count := (maxStateBytes - base) / delivery
	state.Slots = count
	for n := 0; n < count; n++ {
		state.Leases[^uint64(0)-uint64(n)] = worstLease
	}
	for _, revision := range []uint64{0, ^uint64(0)} {
		data, err := encodeGroup(state, revision)
		if err != nil || len(data) > maxStateBytes {
			t.Fatal("full timestamps, IDs, attempts, history and gaps exceed reservation", err)
		}
		reserved, err := groupReservation(count)
		if err != nil || len(data)+revisionHeaderGrowth(revision) != reserved {
			t.Fatal("CAS header width is not normalized in physical reservation", err)
		}
	}
	state.Leases[^uint64(0)-uint64(count)] = worstLease
	if _, err := encodeGroup(state, 0); !errors.Is(err, ErrCapacity) {
		t.Fatal("one more worst-shape delivery was admitted", err)
	}
}

func TestGroupBudgetRejectsShapesOutsideItsValidatedContract(t *testing.T) {
	valid, l, err := maximumGroupShape()
	if err != nil {
		t.Fatal(err)
	}
	valid.Slots = minGroupSlots
	for _, corrupt := range []func(*lease){
		func(l *lease) { l.Project = string(make([]byte, MaxIDLength+1)) },
		func(l *lease) { l.Receipt = "short" },
		func(l *lease) { l.Attempt = 0 },
		func(l *lease) { l.Expires = l.Expires.In(time.FixedZone("non-utc", int(time.Hour/time.Second))) },
	} {
		candidate := l
		corrupt(&candidate)
		valid.Leases = map[uint64]lease{1: candidate}
		if _, err := encodeGroup(valid, 0); !errors.Is(err, ErrConflict) {
			t.Fatal("unbounded or invalid delivery shape accepted", err)
		}
	}

}

func TestEncodeGroupValidatesBeforeTimestampSerialization(t *testing.T) {
	state, l, err := maximumGroupShape()
	if err != nil {
		t.Fatal(err)
	}
	l.Expires = l.Expires.In(time.FixedZone("non-utc", int(time.Hour/time.Second)))
	state.Slots = minGroupSlots
	state.Leases[1] = l
	var marshaler *json.MarshalerError
	if _, err := json.Marshal(state); !errors.As(err, &marshaler) {
		t.Fatal("fixture must fail timestamp serialization before validation", err)
	}
	if _, err := encodeGroup(state, 0); !errors.Is(err, ErrConflict) {
		t.Fatal("invalid persistent shape escaped the typed state contract", err)
	}
}

func TestSlotAllocationRejectsInvalidPersistentShape(t *testing.T) {
	state, _, err := maximumGroupShape()
	if err != nil {
		t.Fatal(err)
	}
	for _, slots := range []int{-minGroupSlots, 0, MaxLedgerEntries + minGroupSlots} {
		state.Slots = slots
		if err := validateGroup(state); err == nil {
			t.Fatal("invalid persisted slot allocation accepted", slots)
		}
	}
	state.Slots = -minGroupSlots
	if _, err := encodeGroup(state, 0); !errors.Is(err, ErrConflict) {
		t.Fatal("negative allocation silently normalized", err)
	}
}

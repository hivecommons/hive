package lanes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/nats-io/nats.go/jetstream"
)

var errFixtureBroker = errors.New("injected broker response failure")

// Delegate reads and successful operations to the embedded broker; intercept
// only the response whose failure contract the caller must preserve.
type faultResponseKV struct {
	jetstream.KeyValue
	getErr, createErr, updateErr error
	updates                      int
}

func (k *faultResponseKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if k.getErr != nil {
		return nil, k.getErr
	}
	return k.KeyValue.Get(ctx, key)
}

func (k *faultResponseKV) Create(ctx context.Context, key string, data []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if k.createErr != nil {
		return 0, k.createErr
	}
	return k.KeyValue.Create(ctx, key, data, opts...)
}

func (k *faultResponseKV) Update(ctx context.Context, key string, data []byte, revision uint64) (uint64, error) {
	k.updates++
	if k.updateErr != nil {
		return 0, k.updateErr
	}
	return k.KeyValue.Update(ctx, key, data, revision)
}

func TestFailedGroupCASCannotClaimOrAcknowledgeOwnership(t *testing.T) {
	b := testBroker(t, nil)
	ctx := context.Background()
	publish(t, b, "cas-work", api.LaneWork)
	actual := b.state
	fault := &faultResponseKV{KeyValue: actual, createErr: jetstream.ErrKeyRevisionMismatch}
	b.state = fault
	if out, err := b.Claim(ctx, "atlas", "shared", "workers"); !errors.Is(err, ErrConflict) || out.Sequence != 0 {
		t.Fatal("repeated admission CAS conflict produced ownership", out, err)
	}
	if _, err := actual.Get(ctx, groupKey("shared", "workers")); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Fatal("failed admission persisted a group", err)
	}
	fault.createErr = errFixtureBroker
	if _, err := b.Claim(ctx, "atlas", "shared", "workers"); !errors.Is(err, ErrUnavailable) || !errors.Is(err, errFixtureBroker) {
		t.Fatal("failed admission lost broker failure", err)
	}
	b.state = actual
	c := claim(t, b, "atlas", "workers")
	fault.createErr, fault.updateErr = nil, jetstream.ErrKeyRevisionMismatch
	b.state = fault
	if err := b.Ack(ctx, "atlas", "shared", "workers", receipt(c)); !errors.Is(err, ErrConflict) || fault.updates != maxCASAttempts {
		t.Fatal("repeated receipt CAS conflict falsely acknowledged", err, fault.updates)
	}
	fault.updates = 0
	if _, err := b.Claim(ctx, "atlas", "shared", "workers"); !errors.Is(err, ErrConflict) || fault.updates != maxCASAttempts {
		t.Fatal("empty-cursor CAS retries were not bounded", err, fault.updates)
	}
	fault.updateErr = errFixtureBroker
	if err := b.Ack(ctx, "atlas", "shared", "workers", receipt(c)); !errors.Is(err, ErrUnavailable) {
		t.Fatal("failed receipt CAS falsely acknowledged", err)
	}
	if _, err := b.Claim(ctx, "atlas", "shared", "workers"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("empty-cursor checkpoint hid broker failure", err)
	}
	b.state = actual
	state, _, err := b.readGroup(ctx, groupKey("shared", "workers"))
	if err != nil || state.Leases[c.Sequence].Done {
		t.Fatal("failed CAS changed durable ownership", err)
	}
	if err := b.Ack(ctx, "atlas", "shared", "workers", receipt(c)); err != nil {
		t.Fatal("accepted receipt could not complete after broker recovery", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := b.Claim(canceled, "atlas", "shared", "workers"); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled claim lost cancellation", err)
	}
	if _, err := b.Renew(canceled, "atlas", "shared", "workers", receipt(c)); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled receipt lost cancellation", err)
	}
}

func TestStoredGroupByteAndReservationBoundsFailClosed(t *testing.T) {
	b := testBroker(t, func(c *Config) {
		c.MaxLedger, c.MaxPending = minGroupSlots, minGroupSlots
	})
	ctx := context.Background()
	key := groupKey("shared", "workers")
	if _, err := b.state.Put(ctx, key, bytes.Repeat([]byte("x"), maxStateBytes+1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.readGroup(ctx, key); !errors.Is(err, ErrCapacity) {
		t.Fatal("oversized stored value escaped the read bound", err)
	}
	state := groupState{Slots: minGroupSlots, Leases: map[uint64]lease{}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.state.Put(ctx, key, data); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.readGroup(ctx, key); !errors.Is(err, ErrConflict) {
		t.Fatal("unreserved stored state accepted", err)
	}
	state.Slots = minGroupSlots + 1
	if err := b.saveGroup(ctx, key, state, 0); !errors.Is(err, ErrCapacity) {
		t.Fatal("configured ledger bound ignored at write", err)
	}
}

func TestRecoveryInvalidInputsHistoryAndReadFailuresAreExplicit(t *testing.T) {
	b := testBroker(t, nil)
	ctx := context.Background()
	publish(t, b, "recover-work", api.LaneWork)
	c := claim(t, b, "atlas", "workers")
	key := groupKey("shared", "workers")
	state, revision, err := b.readGroup(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		in   api.LaneRecover
		want error
	}{
		{"invalid request", api.LaneRecover{ID: "bad.*", Revision: revision}, ErrInvalid},
		{"missing revision", api.LaneRecover{ID: "missing-revision"}, ErrInvalid},
		{"stale revision", api.LaneRecover{ID: "stale-revision", Revision: revision + 1}, ErrConflict},
		{"invalid receipt", api.LaneRecover{ID: "bad-receipt", Revision: revision, Expired: []api.LaneReceipt{{Sequence: c.Sequence, Receipt: "invalid"}}}, ErrInvalid},
		{"zero missing sequence", api.LaneRecover{ID: "zero", Revision: revision, Missing: []uint64{0}}, ErrInvalid},
		{"no accepted loss", api.LaneRecover{ID: "empty", Revision: revision, CursorThrough: state.Cursor}, ErrInvalid},
		{"retained cursor", api.LaneRecover{ID: "cursor", Revision: revision, CursorThrough: state.Cursor + 1}, ErrCursor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.in.CursorThrough = max(tt.in.CursorThrough, state.Cursor)
			if _, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", tt.in); !errors.Is(err, tt.want) {
				t.Fatal("recovery input lost typed rejection", err)
			}
		})
	}
	in := api.LaneRecover{ID: "expired-history", Revision: revision, CursorThrough: state.Cursor}
	request, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	state.Recoveries = map[string]recoveryRecord{in.ID: {
		RequestHash: digest(request),
		Result: api.LaneRecovered{ID: in.ID, Project: "atlas", Cursor: state.Cursor,
			RecoveredAt: time.Now().UTC().Add(-b.cfg.AckRetention)},
	}}
	if err := b.saveGroup(ctx, key, state, revision); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", in); !errors.Is(err, ErrStale) {
		t.Fatal("expired idempotent recovery response reported success", err)
	}
	actual := b.state
	b.state = &faultResponseKV{KeyValue: actual, getErr: errFixtureBroker}
	if _, err := b.InspectGroup(ctx, "atlas", "shared", "workers"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("inspection hid durable-state failure", err)
	}
	if _, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", in); !errors.Is(err, ErrUnavailable) {
		t.Fatal("recovery hid durable-state failure", err)
	}
	b.state = actual
	corrupt := strings.Repeat("a", receiptBytes*2)
	if err := b.Ack(ctx, "beacon", "shared", "workers", api.LaneReceipt{Sequence: c.Sequence, Receipt: corrupt}); !errors.Is(err, ErrStale) {
		t.Fatal("foreign receipt became an acknowledgement", err)
	}
}

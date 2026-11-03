package lanes

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestClosedBrokerAndCanceledOpenFail(t *testing.T) {
	b := testBroker(t, nil)
	b.Close()
	ctx := context.Background()
	in := api.LanePublish{ID: "x", Kind: api.LaneEvent, Schema: "v1", Payload: []byte("value")}
	if _, err := b.Publish(ctx, "atlas", "shared", in); err == nil {
		t.Fatal("closed broker publish")
	}
	if _, err := b.Replay(ctx, "atlas", "shared", 0, 1); err == nil {
		t.Fatal("closed broker replay")
	}
	if _, err := b.Claim(ctx, "atlas", "shared", "workers"); err == nil {
		t.Fatal("closed broker claim")
	}
	if err := b.Ack(ctx, "atlas", "shared", "workers", api.LaneReceipt{Sequence: 1, Receipt: strings.Repeat("a", receiptBytes*2)}); err == nil {
		t.Fatal("closed broker ack")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Open(canceled, b.Config()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled open: %v", err)
	}
	if backend(nil) != nil || !errors.Is(backend(context.DeadlineExceeded), context.DeadlineExceeded) {
		t.Fatal("backend context errors lost")
	}
}

func TestMalformedDurableStateFailsInsteadOfDefaulting(t *testing.T) {
	b := testBroker(t, nil)
	ctx := context.Background()
	key := groupKey("shared", "workers")
	for _, data := range []string{"invalid-json", `{}`, `{"cursor":0,"leases":{}}`} {
		if _, err := b.state.Put(ctx, key, []byte(data)); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Claim(ctx, "atlas", "shared", "workers"); err == nil {
			t.Fatal("malformed group state defaulted")
		}
	}
	if _, err := b.dedup.Put(ctx, "shared.atlas.bad", []byte("invalid-json")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(ctx, "atlas", "shared", api.LanePublish{ID: "bad", Kind: api.LaneEvent, Schema: "v1", Payload: []byte("x")}); err == nil {
		t.Fatal("malformed dedup state defaulted")
	}
	if _, err := b.js.Publish(ctx, b.subject("shared")+"bad", []byte("invalid-json")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Replay(ctx, "atlas", "shared", 0, 1); err == nil {
		t.Fatal("malformed envelope ignored")
	}
}

func TestReceiptTerminalAndInvalidStates(t *testing.T) {
	b := testBroker(t, nil)
	ctx := context.Background()
	publish(t, b, "job", api.LaneWork)
	c := claim(t, b, "atlas", "workers")
	if _, err := b.receipt(ctx, "atlas", "shared", "workers", "invalid", receipt(c)); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid receipt operation", err)
	}
	if err := b.Nack(ctx, "atlas", "shared", "workers", receipt(c)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Renew(ctx, "atlas", "shared", "workers", receipt(c)); !errors.Is(err, ErrStale) {
		t.Fatal("released lease renewed", err)
	}
	if err := b.Ack(ctx, "atlas", "shared", "workers", receipt(c)); !errors.Is(err, ErrStale) {
		t.Fatal("released lease acked", err)
	}
	makeRetriesDue(t, b)
	c = claim(t, b, "atlas", "workers")
	if err := b.Ack(ctx, "atlas", "shared", "workers", receipt(c)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Renew(ctx, "atlas", "shared", "workers", receipt(c)); !errors.Is(err, ErrStale) {
		t.Fatal("completed lease renewed", err)
	}
	if err := b.Nack(ctx, "atlas", "shared", "workers", receipt(c)); !errors.Is(err, ErrStale) {
		t.Fatal("completed lease nacked", err)
	}
	for _, r := range []api.LaneReceipt{{}, {Sequence: 1, Receipt: "invalid"}, {Sequence: c.Sequence + 1, Receipt: c.Receipt}} {
		if err := b.Ack(ctx, "atlas", "shared", "workers", r); err == nil {
			t.Fatal("invalid/unknown receipt accepted")
		}
	}
	data, delivery, err := maximumGroupShape()
	if err != nil {
		t.Fatal(err)
	}
	base, perDelivery, err := completionBudget()
	if err != nil {
		t.Fatal(err)
	}
	data.Slots = (maxStateBytes-base)/perDelivery + minGroupSlots
	for seq := uint64(1); len(data.Leases) < data.Slots; seq++ {
		data.Leases[seq] = delivery
	}
	if err := validateGroup(data); err != nil {
		t.Fatal("capacity fixture must satisfy the durable state contract", err)
	}
	if reserved, err := groupReservation(data.Slots); err != nil || reserved <= maxStateBytes {
		t.Fatal("capacity fixture must exceed the byte reservation cap", reserved, err)
	}
	if err := b.saveGroup(ctx, "oversized", data, 0); !errors.Is(err, ErrCapacity) {
		t.Fatal("oversized durable ledger accepted", err)
	}
}

func TestMalformedDurableLedgerFailsConflictBeforeCapacity(t *testing.T) {
	data := groupState{Leases: map[uint64]lease{1: {Receipt: strings.Repeat("a", maxStateBytes)}}}
	if _, err := encodeGroup(data, 0); !errors.Is(err, ErrConflict) {
		t.Fatal("malformed oversized state must fail its typed contract first", err)
	}
}

func TestPublishCommittedBeforeResponseRecovery(t *testing.T) {
	b := testBroker(t, func(c *Config) { c.Dedup = MinDedup })
	ctx := context.Background()
	in := api.LanePublish{ID: "lost-response", Kind: api.LaneWork, Schema: "v1", Payload: []byte("work")}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	res := publishReservation{Hash: digest(body), Created: time.Now().UTC(), Generation: strings.Repeat("a", receiptBytes*2)}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	key := "shared.atlas.lost-response"
	if _, err := b.dedup.Create(ctx, key, data); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(api.LaneEnvelope{LanePublish: in, Lane: "shared", Project: "atlas", CreatedAt: res.Created})
	if err != nil {
		t.Fatal(err)
	}
	ack, err := b.js.PublishMsg(ctx, &nats.Msg{Subject: b.subject("shared") + digest([]byte(key)), Data: data,
		Header: nats.Header{reservationHeader: []string{res.Generation}}},
		jetstream.WithMsgID(key+"."+res.Generation))
	if err != nil {
		t.Fatal(err)
	}
	cfg := b.Config()
	b.Close()
	timer := time.NewTimer(MinDedup)
	<-timer.C
	b, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	out, err := b.Publish(ctx, "atlas", "shared", in)
	if err != nil || !out.Duplicate || out.Sequence != ack.Sequence {
		t.Fatalf("committed response recovery: %+v %v", out, err)
	}
}

func TestConfigurationSnapshotsCannotMutateAuthorization(t *testing.T) {
	b := testBroker(t, nil)
	cfg := b.Config()
	cfg.Lanes[0].Publishers[0] = "outsider"
	cfg.Lanes[0].Groups["workers"][0] = "outsider"
	registry := b.Registry("atlas")
	registry[0].Publishers[0] = "outsider"
	registry[0].Groups["workers"][0] = "outsider"
	if _, err := b.authorize("atlas", "shared", "publish", ""); err != nil {
		t.Fatal("configuration mutated", err)
	}
	if _, err := b.authorize("outsider", "shared", "work", "workers"); !errors.Is(err, ErrForbidden) {
		t.Fatal("registry granted a capability", err)
	}
	cfg = b.Config()
	cfg.MaxKVBytes++
	if modified, err := Open(context.Background(), cfg); !errors.Is(err, ErrConflict) {
		if modified != nil {
			modified.Close()
		}
		t.Fatal("incompatible KV bounds accepted", err)
	}
}

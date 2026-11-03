package lanes

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

const (
	byteCapacityLedgerLimit = 2000
	byteCapacityRetention   = time.Minute
	byteCapacityGroupCount  = 16
)

func TestByteBoundAdmissionPreservesCompletionAndRecoveryHeadroom(t *testing.T) {
	b := testBroker(t, func(c *Config) {
		c.MaxLedger, c.MaxPending = byteCapacityLedgerLimit, byteCapacityLedgerLimit
		c.Retention, c.AckRetention, c.Lease = byteCapacityRetention, byteCapacityRetention, MinLease
		c.Dedup = MinDedup
		c.RetryDelay, c.MaxRetryDelay = MinRetryDelay, MinRetryDelay
	})
	ctx := context.Background()
	clock := time.Now().UTC().Truncate(time.Second).Add(time.Second - time.Nanosecond)
	b.now = func() time.Time { return clock }
	base, perLease, err := completionBudget()
	if err != nil || base >= maxStateBytes || perLease <= 0 {
		t.Fatal("invalid completion budget", base, perLease, err)
	}
	accepted := (maxStateBytes - base) / perLease
	if accepted >= b.cfg.MaxLedger || accepted < MaxRecoveryBatch {
		t.Fatal("regression must hit the byte limit, not the count limit")
	}
	// Bulk seed valid deliveries through the same durable admission encoder
	// used by Claim, then exercise the actual final Claim at the byte boundary.
	state := groupState{Leases: map[uint64]lease{}}
	receipts := []api.LaneReceipt{}
	for n := 0; n < accepted-1; n++ {
		published := publish(t, b, fmt.Sprintf("seed-%d", n), api.LaneWork)
		token, err := newReceipt()
		if err != nil {
			t.Fatal(err)
		}
		state.Cursor = published.Sequence
		state.Leases[published.Sequence] = lease{Receipt: token, Project: "atlas",
			Expires: clock.Add(b.cfg.Lease), Attempt: 1}
		receipts = append(receipts, api.LaneReceipt{Sequence: published.Sequence, Receipt: token})
	}
	key := groupKey("shared", "workers")
	if err := b.saveGroup(ctx, key, state, 0); err != nil {
		t.Fatal(err)
	}
	publish(t, b, "last-admitted", api.LaneWork)
	last := claim(t, b, "atlas", "workers")
	entry, err := b.state.Get(ctx, key)
	if err != nil || maxStateBytes-len(entry.Value()) >= perLease {
		t.Fatal("accepted state is not within one delivery of the byte cap", err)
	}
	reservedBytes := len(entry.Value())
	publish(t, b, "not-admitted", api.LaneWork)
	if _, err := b.Claim(ctx, "atlas", "shared", "workers"); !errors.Is(err, ErrCapacity) {
		t.Fatal("claim did not reserve worst-case completion bytes", err)
	}
	if _, err := b.Renew(ctx, "atlas", "shared", "workers", receipt(last)); err != nil {
		t.Fatal("accepted near-cap lease cannot renew", err)
	}
	if err := b.Nack(ctx, "atlas", "shared", "workers", receipt(last)); err != nil {
		t.Fatal("accepted near-cap lease cannot NACK", err)
	}
	clock = clock.Add(b.cfg.RetryDelay + time.Nanosecond)
	retried := claim(t, b, "atlas", "workers")
	if retried.Sequence != last.Sequence {
		t.Fatal("accepted near-cap NACK cannot be redelivered")
	}
	if err := b.Ack(ctx, "atlas", "shared", "workers", receipt(retried)); err != nil {
		t.Fatal("actual near-cap Claim cannot ACK", err)
	}
	if err := b.Nack(ctx, "atlas", "shared", "workers", receipts[0]); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(b.cfg.RetryDelay + time.Nanosecond)
	// The fresh turn must recognize the byte admission limit and still
	// redeliver accepted work instead of repeatedly failing fresh admission.
	retried = claim(t, b, "atlas", "workers")
	if retried.Sequence != receipts[0].Sequence {
		t.Fatal("byte-full fresh turn starved an accepted retry")
	}
	if err := b.Nack(ctx, "atlas", "shared", "workers", receipt(retried)); err != nil {
		t.Fatal(err)
	}
	// Full-ledger ACKs under the race detector can outlive retention.
	// Finish retained-work retries first; expiry and recovery follow below.
	for _, in := range receipts[2:] {
		if err := b.Ack(ctx, "atlas", "shared", "workers", in); err != nil {
			t.Fatal("accepted near-cap lease cannot ACK", err)
		}
	}
	entry, err = b.state.Get(ctx, key)
	if err != nil || len(entry.Value()) > reservedBytes {
		t.Fatal("completion consumed previously unreserved bucket bytes", err)
	}
	awaitConditionWithin(t, b.cfg.Retention+testWait, func() (bool, error) {
		info, err := b.streams["shared"].Info(ctx)
		if err != nil {
			return false, err
		}
		return info.State.FirstSeq > last.Sequence+1, nil
	})
	clock = clock.Add(b.cfg.Lease + time.Nanosecond)
	fresh := publish(t, b, "after-expiry", api.LaneWork)
	in := recoveryRequest(t, b, "atlas", "r")
	if len(in.Expired) != 2 {
		t.Fatalf("pending near-cap deliveries: %+v", in)
	}
	out, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", in)
	if err != nil || len(out.Expirations) != 2 {
		t.Fatal("near-cap missing leases cannot recover", err)
	}
	resumed := claim(t, b, "atlas", "workers")
	if resumed.Sequence != fresh.Sequence {
		t.Fatal("valid later work did not resume")
	}
	if err := b.Ack(ctx, "atlas", "shared", "workers", receipt(resumed)); err != nil {
		t.Fatal(err)
	}
	if err := b.Ack(ctx, "atlas", "shared", "workers", receipts[2]); err != nil {
		t.Fatal("recovery silently discarded unexpired ACK history", err)
	}
}

func TestNamespaceBucketAdmissionReservesExistingGroupsCompletionBytes(t *testing.T) {
	b := testBroker(t, func(c *Config) {
		c.MaxKVBytes = 2 * maxStateBytes
		c.RetryDelay, c.MaxRetryDelay = MinRetryDelay, MinRetryDelay
		for n := 0; n < byteCapacityGroupCount; n++ {
			c.Lanes[0].Groups[fmt.Sprintf("bounded-%d", n)] = []string{"atlas"}
		}
	})
	ctx := context.Background()
	publish(t, b, "shared-work", api.LaneWork)
	claims := map[string]api.LaneClaim{}
	full := false
	for n := 0; n < byteCapacityGroupCount; n++ {
		group := fmt.Sprintf("bounded-%d", n)
		c, err := b.Claim(ctx, "atlas", "shared", group)
		if err != nil {
			if !errors.Is(err, ErrUnavailable) {
				t.Fatal("unexpected bucket admission error", err)
			}
			full = true
			break
		}
		claims[group] = c
	}
	if !full || len(claims) < 2 {
		t.Fatal("regression did not fill bounded namespace storage")
	}
	stream, err := b.js.Stream(ctx, "KV_"+b.cfg.Namespace+"_state")
	if err != nil {
		t.Fatal(err)
	}
	before, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pendingGroup := ""
	var pendingClaim api.LaneClaim
	for group, c := range claims {
		if _, err := b.Renew(ctx, "atlas", "shared", group, receipt(c)); err != nil {
			t.Fatal("another group's admission consumed renewal headroom", err)
		}
		if pendingGroup == "" {
			pendingGroup, pendingClaim = group, c
			continue
		}
		if err := b.Ack(ctx, "atlas", "shared", group, receipt(c)); err != nil {
			t.Fatal("another group's admission consumed completion headroom", err)
		}
	}
	after, err := stream.Info(ctx)
	if err != nil || after.State.Bytes > before.State.Bytes {
		t.Fatal("completion grew total bucket bytes despite reservation", err)
	}
	clock := time.Now().UTC()
	b.now = func() time.Time { return clock }
	if err := b.Nack(ctx, "atlas", "shared", pendingGroup, receipt(pendingClaim)); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(b.cfg.RetryDelay + time.Nanosecond)
	retried := claim(t, b, "atlas", pendingGroup)
	if err := b.Nack(ctx, "atlas", "shared", pendingGroup, receipt(retried)); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(b.cfg.RetryDelay + time.Nanosecond)
	publish(t, b, "fresh-while-bucket-full", api.LaneWork)
	if _, err := b.Claim(ctx, "atlas", "shared", pendingGroup); !errors.Is(err, ErrUnavailable) {
		t.Fatal("regression requires failed fresh value-growth admission", err)
	}
	retried = claim(t, b, "atlas", pendingGroup)
	if retried.Sequence != pendingClaim.Sequence {
		t.Fatal("failed fresh admission permanently blocked accepted retry")
	}
	if err := b.Ack(ctx, "atlas", "shared", pendingGroup, receipt(retried)); err != nil {
		t.Fatal("accepted retry could not complete in full bucket", err)
	}
}

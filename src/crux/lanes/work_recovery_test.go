package lanes

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/nats-io/nats.go/jetstream"
)

func shortRetention(c *Config) {
	c.Retention, c.Lease, c.AckRetention = time.Second, time.Second, time.Second
	c.Dedup = MinDedup
}

func waitExpiredPrefix(t *testing.T, b *Broker, through uint64) {
	t.Helper()
	awaitCondition(t, func() (bool, error) {
		info, err := b.streams["shared"].Info(context.Background())
		if err != nil {
			return false, err
		}
		return info.State.FirstSeq > through, nil
	})
}

func recoveryRequest(t *testing.T, b *Broker, project, id string) api.LaneRecover {
	t.Helper()
	s, err := b.InspectGroup(context.Background(), project, "shared", "workers")
	if err != nil {
		t.Fatal(err)
	}
	through := s.Cursor
	if s.Cursor != 0 && s.FirstRetained > s.Cursor+1 {
		through = s.FirstRetained - 1
	}
	in := api.LaneRecover{ID: id, Revision: s.Revision, CursorThrough: through, Expired: s.Recoverable}
	for _, missing := range s.Missing {
		if missing.Project != project {
			in.Missing = append(in.Missing, missing.Sequence)
		}
	}
	return in
}

func TestExplicitGroupGapRecoverySurvivesResponseLossAndRestart(t *testing.T) {
	ctx := context.Background()
	s, opts := testServer(t)
	cfg := testConfig(s.ClientURL())
	shortRetention(&cfg)
	cfg.Retention, cfg.Lease, cfg.AckRetention = 2*time.Second, 2*time.Second, 2*time.Second
	b, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	publish(t, b, "expired-work", api.LaneWork)
	old := claim(t, b, "atlas", "workers")
	gap := publish(t, b, "expired-unread-event", api.LaneEvent)
	waitExpiredPrefix(t, b, gap.Sequence)
	fresh := publish(t, b, "retained-work", api.LaneWork)
	if _, err := b.Claim(ctx, "atlas", "shared", "workers"); !errors.Is(err, ErrCursor) {
		t.Fatal("gap was silently skipped", err)
	}
	in := recoveryRequest(t, b, "atlas", "recover-gap")
	if len(in.Expired) != 1 {
		t.Fatalf("missing own expired delivery: %+v", in)
	}
	out, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", in)
	if err != nil || out.GapFrom != old.Sequence+1 || out.GapThrough != gap.Sequence || len(out.Retired) != 1 {
		t.Fatalf("recovery: %+v %v", out, err)
	}
	cfg = b.Config()
	b.Close()
	s.Shutdown()
	s.WaitForShutdown()
	s = startServer(t, opts)
	defer func() { s.Shutdown(); s.WaitForShutdown() }()
	b, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	duplicate, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", in)
	if err != nil || !reflect.DeepEqual(out, duplicate) {
		t.Fatalf("lost recovery response: %+v %v", duplicate, err)
	}
	in.CursorThrough++
	if _, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", in); !errors.Is(err, ErrConflict) {
		t.Fatal("recovery key payload mismatch", err)
	}
	if err := b.Ack(ctx, "atlas", "shared", "workers", receipt(old)); !errors.Is(err, ErrStale) {
		t.Fatal("gap recovery falsely acknowledged expired work", err)
	}
	resumed := claim(t, b, "beacon", "workers")
	if resumed.Sequence != fresh.Sequence {
		t.Fatal("retained work remained stranded")
	}
	status, err := b.InspectGroup(ctx, "atlas", "shared", "workers")
	if err != nil || len(status.Recoveries) != 1 || status.Recoveries[0].GapThrough != gap.Sequence {
		t.Fatalf("durable gap record: %+v %v", status, err)
	}
}

func TestRecoveryCannotRetireLiveRetainedOrOtherProjectDeliveries(t *testing.T) {
	b := testBroker(t, nil)
	ctx := context.Background()
	publish(t, b, "live", api.LaneWork)
	live := claim(t, b, "atlas", "workers")
	in := recoveryRequest(t, b, "atlas", "retire-live")
	in.Expired = []api.LaneReceipt{receipt(live)}
	if _, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", in); !errors.Is(err, ErrStale) {
		t.Fatal("live lease retired", err)
	}

	if _, err := b.RecoverGroup(ctx, "beacon", "shared", "workers", in); !errors.Is(err, ErrStale) {
		t.Fatal("another project lease retired", err)
	}
	if _, err := b.RecoverGroup(ctx, "atlas", "shared", "other", in); !errors.Is(err, ErrForbidden) {
		t.Fatal("another group recovered", err)
	}
	future := live.Expires.Add(time.Nanosecond)
	b.now = func() time.Time { return future }
	if _, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", in); !errors.Is(err, ErrCursor) {
		t.Fatal("expired lease with retained work retired", err)
	}
	in.ID, in.Expired, in.CursorThrough = "skip-retained", nil, live.Sequence+1
	if _, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", in); !errors.Is(err, ErrCursor) {
		t.Fatal("retained cursor prefix skipped", err)
	}
	if _, err := b.InspectGroup(ctx, "outsider", "shared", "workers"); !errors.Is(err, ErrForbidden) {
		t.Fatal("outsider inspected group", err)
	}
}

func TestRecoveryAuditsForeignExpirationWithoutAcknowledgement(t *testing.T) {
	b := testBroker(t, shortRetention)
	ctx := context.Background()
	publish(t, b, "atlas-old", api.LaneWork)
	atlas := claim(t, b, "atlas", "workers")
	publish(t, b, "beacon-old", api.LaneWork)
	beacon := claim(t, b, "beacon", "workers")
	gap := publish(t, b, "unread-gap", api.LaneEvent)
	waitExpiredPrefix(t, b, gap.Sequence)
	fresh := publish(t, b, "fresh", api.LaneWork)
	in := recoveryRequest(t, b, "atlas", "atlas-recovery")
	if len(in.Expired) != 1 || in.Expired[0].Sequence != atlas.Sequence {
		t.Fatal("recovery exposed foreign receipts")
	}
	out, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", in)
	if err != nil || out.MissingThrough != gap.Sequence {
		t.Fatal("missing prefix was not durably accepted", err)
	}
	after, _, err := b.readGroup(ctx, groupKey("shared", "workers"))
	if _, exists := after.Leases[beacon.Sequence]; err != nil || exists {
		t.Fatal("expired foreign delivery still consumes ledger capacity", err)
	}
	if len(out.Expirations) != 2 || out.Expirations[1].Project != "beacon" {
		t.Fatal("foreign expiration audit missing")
	}
	resumed := claim(t, b, "atlas", "workers")
	if resumed.Sequence != fresh.Sequence {
		t.Fatal("foreign missing delivery stranded retained work")
	}
	if err := b.Ack(ctx, "beacon", "shared", "workers", receipt(beacon)); !errors.Is(err, ErrStale) {
		t.Fatal("foreign expiration was mistaken for successful ACK", err)
	}
	if err := b.Ack(ctx, "atlas", "shared", "workers", receipt(resumed)); err != nil {
		t.Fatal("owner recovery changed another live lease", err)
	}
}

func TestRecoveryPreservesStillValidLeasesAndAcknowledgementHistory(t *testing.T) {
	// The live lease must outlast the old message's expiry by a margin that survives CI scheduling stalls.
	b := testBroker(t, func(c *Config) {
		shortRetention(c)
		c.Retention, c.Lease, c.AckRetention = 4*time.Second, 4*time.Second, 4*time.Second
	})
	ctx := context.Background()
	oldPub := publish(t, b, "old", api.LaneWork)
	old := claim(t, b, "atlas", "workers")
	msg, err := b.streams["shared"].GetMsg(ctx, oldPub.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(time.Until(msg.Time.Add(b.cfg.Retention * 3 / 4)))
	<-timer.C
	publish(t, b, "still-live", api.LaneWork)
	live := claim(t, b, "beacon", "workers")
	publish(t, b, "ack-history", api.LaneWork)
	acked := claim(t, b, "atlas", "workers")
	if err := b.Ack(ctx, "atlas", "shared", "workers", receipt(acked)); err != nil {
		t.Fatal(err)
	}
	waitExpiredPrefix(t, b, old.Sequence)
	awaitCondition(t, func() (bool, error) { return !old.Expires.After(time.Now()), nil })
	before, _, err := b.readGroup(ctx, groupKey("shared", "workers"))
	if err != nil {
		t.Fatal(err)
	}
	in := recoveryRequest(t, b, "atlas", "retire-only-expired")
	if len(in.Expired) != 1 || in.Expired[0].Sequence != old.Sequence {
		t.Fatalf("recoverable scope: %+v", in)
	}
	if _, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", in); err != nil {
		t.Fatal(err)
	}
	after, _, err := b.readGroup(ctx, groupKey("shared", "workers"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Leases[live.Sequence], after.Leases[live.Sequence]) || !reflect.DeepEqual(before.Leases[acked.Sequence], after.Leases[acked.Sequence]) {
		t.Fatal("recovery changed live/ACK state")
	}
	if err := b.Ack(ctx, "beacon", "shared", "workers", receipt(live)); err != nil {
		t.Fatal("still-valid lease fenced away", err)
	}
	if err := b.Ack(ctx, "atlas", "shared", "workers", receipt(acked)); err != nil {
		t.Fatal("ACK history lost", err)
	}
}

type heldGroupUpdate struct {
	jetstream.KeyValue
	reached chan struct{}
	release chan struct{}
}

func (h *heldGroupUpdate) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	close(h.reached)
	select {
	case <-h.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return h.KeyValue.Update(ctx, key, value, revision)
}

func TestRecoveryCASFencesConcurrentClaimAndAckAcrossServices(t *testing.T) {
	b := testBroker(t, shortRetention)
	ctx := context.Background()
	publish(t, b, "old", api.LaneWork)
	old := claim(t, b, "atlas", "workers")
	waitExpiredPrefix(t, b, old.Sequence)
	awaitCondition(t, func() (bool, error) { return !old.Expires.After(time.Now()), nil })
	publish(t, b, "fresh", api.LaneWork)
	state, revision, err := b.readGroup(ctx, groupKey("shared", "workers"))
	if err != nil {
		t.Fatal(err)
	}
	state.RetryTurn = false
	if err := b.saveGroup(ctx, groupKey("shared", "workers"), state, revision); err != nil {
		t.Fatal(err)
	}
	peer, err := Open(ctx, b.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	original := b.state
	var fresh api.LaneClaim
	for _, operation := range []string{"claim", "ack"} {
		in := recoveryRequest(t, b, "atlas", "fenced-"+operation)
		h := &heldGroupUpdate{KeyValue: original, reached: make(chan struct{}), release: make(chan struct{})}
		b.state = h
		result := make(chan error, 1)
		go func() { _, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", in); result <- err }()
		select {
		case <-h.reached:
		case <-time.After(testWait):
			t.Fatal("recovery did not reach CAS")
		}
		if operation == "claim" {
			fresh = claim(t, peer, "beacon", "workers")
		} else if err := peer.Ack(ctx, "beacon", "shared", "workers", receipt(fresh)); err != nil {
			t.Fatal(err)
		}
		close(h.release)
		if err := <-result; !errors.Is(err, ErrConflict) {
			t.Fatal("concurrent state update not fenced", err)
		}
		b.state = original
		s, _, err := b.readGroup(ctx, groupKey("shared", "workers"))
		if _, exists := s.Leases[old.Sequence]; err != nil || !exists {
			t.Fatal("failed recovery retired a lease", err)
		}
	}
	in := recoveryRequest(t, b, "atlas", "retry-after-fence")
	if _, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", in); err != nil {
		t.Fatal(err)
	}
	if err := peer.Ack(ctx, "beacon", "shared", "workers", receipt(fresh)); err != nil {
		t.Fatal("ACK history not preserved", err)
	}
}

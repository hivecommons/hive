package lanes

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/nats-io/nats-server/v2/server"
)

const (
	reusableGroupCount  = 5
	reusableBucketBytes = 1 << 20
)

func reusableSlotConfig(c *Config) {
	c.MaxKVBytes = reusableBucketBytes
	c.MaxLedger, c.MaxPending = 2, 2
	for n := 0; n < reusableGroupCount; n++ {
		group := reusableGroup(n)
		c.Lanes[0].Groups[group] = []string{"atlas", "beacon"}
		c.Lanes[0].Recoverers[group] = []string{"atlas"}
	}
}

func reusableGroup(n int) string { return fmt.Sprintf("reusable-%d", n) }

func restartSlotFixture(t *testing.T, b *Broker, s *server.Server, opts *server.Options) (*Broker, *server.Server) {
	t.Helper()
	cfg, clock := b.Config(), b.now
	b.Close()
	s.Shutdown()
	s.WaitForShutdown()
	s = startServer(t, opts)
	t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
	b, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	b.now = clock
	t.Cleanup(b.Close)
	return b, s
}

func emptySlotGroups(t *testing.T, b *Broker) {
	t.Helper()
	for n := 0; n < reusableGroupCount; n++ {
		group := reusableGroup(n)
		if _, err := b.Claim(context.Background(), "atlas", "shared", group); !errors.Is(err, ErrEmpty) {
			t.Fatal("empty polling could not reserve first reusable slot", group, err)
		}
		status, err := b.InspectGroup(context.Background(), "atlas", "shared", group)
		if err != nil || status.Slots != minGroupSlots || status.Cursor != 0 {
			t.Fatalf("empty poll reservation: %+v %v", status, err)
		}
	}
	assertSlotBucketPressure(t, b)
}

func assertSlotBucketPressure(t *testing.T, b *Broker) {
	t.Helper()
	stream, err := b.js.Stream(context.Background(), "KV_"+b.cfg.Namespace+"_state")
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstSlot, err := groupReservation(minGroupSlots)
	if err != nil || info.State.Bytes > reusableBucketBytes ||
		info.State.Bytes+uint64(firstSlot) <= reusableBucketBytes {
		t.Fatal("regression must reach the pinned broker's replacement-growth boundary", err)
	}
}

func TestFiveEmptyGroupsRetainFirstClaimSlotsAcrossBrokerRestart(t *testing.T) {
	ctx := context.Background()
	s, opts := testServer(t)
	cfg := testConfig(s.ClientURL())
	reusableSlotConfig(&cfg)
	b, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	emptySlotGroups(t, b)
	// Ignore a repeated empty-poll response, then restart both components.
	if _, err := b.Claim(ctx, "atlas", "shared", reusableGroup(0)); !errors.Is(err, ErrEmpty) {
		t.Fatal(err)
	}
	b, _ = restartSlotFixture(t, b, s, opts)
	work := publish(t, b, "after-empty-polls", api.LaneWork)
	for n := 0; n < reusableGroupCount; n++ {
		group := reusableGroup(n)
		got := claim(t, b, "atlas", group)
		if got.Sequence != work.Sequence {
			t.Fatal("empty group permanently pinned its first claim", group)
		}
		if err := b.Ack(ctx, "atlas", "shared", group, receipt(got)); err != nil {
			t.Fatal(err)
		}
		status, err := b.InspectGroup(ctx, "atlas", "shared", group)
		if err != nil || status.Slots != minGroupSlots {
			t.Fatal("initial slot changed size at first claim", err)
		}
	}
}

func TestForeignRetirementReusesSlotAtFullBucketAfterLostResponse(t *testing.T) {
	ctx := context.Background()
	s, opts := testServer(t)
	cfg := testConfig(s.ClientURL())
	reusableSlotConfig(&cfg)
	shortRetention(&cfg)
	cfg.Retention, cfg.AckRetention = expirationTestRetention, expirationTestRetention
	b, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	emptySlotGroups(t, b)
	oldWork := publish(t, b, "absent-owner", api.LaneWork)
	group := reusableGroup(0)
	old := claim(t, b, "beacon", group)
	waitExpiredPrefix(t, b, oldWork.Sequence)
	awaitCondition(t, func() (bool, error) { return !old.Expires.After(time.Now()), nil })
	status, err := b.InspectGroup(ctx, "atlas", "shared", group)
	if err != nil || len(status.Recoverable) != 0 || len(status.Missing) != 1 {
		t.Fatal("foreign recovery must need no own receipt", err)
	}
	in := api.LaneRecover{ID: "reuse-after-expiration", Revision: status.Revision,
		CursorThrough: status.Cursor, Missing: []uint64{old.Sequence}}
	out, err := b.RecoverGroup(ctx, "atlas", "shared", group, in)
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := b.readGroup(ctx, groupKey("shared", group))
	if err != nil || len(state.Leases) != 0 || state.Slots != minGroupSlots {
		t.Fatal("retirement discarded reusable first-claim capacity", err)
	}
	b, _ = restartSlotFixture(t, b, s, opts)
	replayed, err := b.RecoverGroup(ctx, "atlas", "shared", group, in)
	if err != nil || !reflect.DeepEqual(out, replayed) {
		t.Fatal("lost recovery response or durable expiration audit", err)
	}
	fresh := publish(t, b, "after-retirement", api.LaneWork)
	got := claim(t, b, "atlas", group)
	if got.Sequence != fresh.Sequence {
		t.Fatal("retired slot cannot be reused at full bucket")
	}
	if err := b.Ack(ctx, "atlas", "shared", group, receipt(got)); err != nil {
		t.Fatal(err)
	}
	if err := b.Ack(ctx, "beacon", "shared", group, receipt(old)); !errors.Is(err, ErrStale) {
		t.Fatal("foreign retirement forged an ACK", err)
	}
}

func TestPrunedHighWaterSlotsRemainReusableAcrossLostClaimAndRestart(t *testing.T) {
	ctx := context.Background()
	s, opts := testServer(t)
	cfg := testConfig(s.ClientURL())
	reusableSlotConfig(&cfg)
	b, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	group := reusableGroup(0)
	publish(t, b, "first", api.LaneWork)
	publish(t, b, "second", api.LaneWork)
	first := claim(t, b, "atlas", group)
	second := claim(t, b, "atlas", group)
	for _, c := range []api.LaneClaim{first, second} {
		if err := b.Ack(ctx, "atlas", "shared", group, receipt(c)); err != nil {
			t.Fatal(err)
		}
	}
	for n := 1; n < reusableGroupCount; n++ {
		c := claim(t, b, "atlas", reusableGroup(n))
		if err := b.Ack(ctx, "atlas", "shared", reusableGroup(n), receipt(c)); err != nil {
			t.Fatal(err)
		}
	}
	assertSlotBucketPressure(t, b)
	clock := time.Now().UTC().Add(cfg.AckRetention + time.Nanosecond)
	b.now = func() time.Time { return clock }
	// Persist pruning through an empty poll before publishing anything new.
	if _, err := b.Claim(ctx, "atlas", "shared", group); !errors.Is(err, ErrEmpty) {
		t.Fatal("pruning failed", err)
	}
	pruned, _, err := b.readGroup(ctx, groupKey("shared", group))
	if err != nil || len(pruned.Leases) != 0 || pruned.Slots != cfg.MaxLedger {
		t.Fatal("pruning shrank the two-slot high-water reservation", err)
	}
	third := publish(t, b, "third", api.LaneWork)
	fourth := publish(t, b, "fourth", api.LaneWork)
	lost := claim(t, b, "atlas", group)
	if lost.Sequence != third.Sequence {
		t.Fatal("first pruned slot was not reusable")
	}
	b, _ = restartSlotFixture(t, b, s, opts)
	clock = lost.Expires.Add(time.Nanosecond)
	reassigned := claim(t, b, "beacon", group)
	if reassigned.Sequence != lost.Sequence || reassigned.Attempt != lost.Attempt+1 {
		t.Fatal("lost-response lease was not durable and fenced")
	}
	if err := b.Ack(ctx, "atlas", "shared", group, receipt(lost)); !errors.Is(err, ErrStale) {
		t.Fatal("lost old receipt bypassed reassignment", err)
	}
	if err := b.Ack(ctx, "beacon", "shared", group, receipt(reassigned)); err != nil {
		t.Fatal(err)
	}
	next := claim(t, b, "atlas", group)
	if next.Sequence != fourth.Sequence {
		t.Fatal("second pruned high-water slot was not reusable")
	}
	if err := b.Ack(ctx, "atlas", "shared", group, receipt(next)); err != nil {
		t.Fatal(err)
	}
	status, err := b.InspectGroup(ctx, "atlas", "shared", group)
	if err != nil || status.Slots != cfg.MaxLedger {
		t.Fatal("restart lost high-water slot count", err)
	}
}

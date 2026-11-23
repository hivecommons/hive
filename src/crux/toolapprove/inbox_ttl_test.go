package toolapprove

// Expiry of pending operator approvals (#19446): an approval granted long after
// the request was made must not release it.

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// ttlInbox returns an inbox with a controllable clock and the given TTL.
func ttlInbox(t *testing.T, ttl time.Duration) (*Inbox, *time.Time) {
	t.Helper()
	clock := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	in := NewInbox(filepath.Join(t.TempDir(), "approvals", "inbox.json"))
	in.now = func() time.Time { return clock }
	in.SetPendingTTL(ttl)
	return in, &clock
}

func TestNewInboxDefaultsPendingTTL(t *testing.T) {
	if got := NewInbox(filepath.Join(t.TempDir(), "inbox.json")).PendingTTL(); got != DefaultPendingTTL {
		t.Errorf("PendingTTL = %v, want %v", got, DefaultPendingTTL)
	}
}

func TestExpiredPendingItemCannotBeApproved(t *testing.T) {
	in, clock := ttlInbox(t, 15*time.Minute)
	id, err := in.Enqueue(pendingRequest(1), operatorVerdict())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	*clock = clock.Add(15 * time.Minute)

	if _, ok := in.Get(id); ok {
		t.Error("Get returned an expired item")
	}
	if n := in.Count(); n != 0 {
		t.Errorf("Count = %d, want 0 after expiry", n)
	}
	if l := in.List(); len(l) != 0 {
		t.Errorf("List = %d items, want 0 after expiry", len(l))
	}

	_, err = in.Resolve(id, true, "operator", "late grant")
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("Resolve of an expired item: err = %v, want ErrExpired", err)
	}
	if _, ok := in.ResolvedRecordFor(id); ok {
		t.Error("an expired item was journaled; it must be dropped, not resolved")
	}

	// The expired row is gone from disk too: a restart does not revive it.
	again := NewInbox(in.path)
	again.SetPendingTTL(0)
	if n := again.Count(); n != 0 {
		t.Errorf("after restart with no TTL, Count = %d, want 0 — the expired row was not swept from disk", n)
	}

	// A second resolve sees nothing to resolve.
	if _, err := in.Resolve(id, true, "operator", "late grant"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Resolve: err = %v, want ErrNotFound", err)
	}
}

func TestUnexpiredPendingItemResolves(t *testing.T) {
	in, clock := ttlInbox(t, 15*time.Minute)
	id, err := in.Enqueue(pendingRequest(1), operatorVerdict())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	*clock = clock.Add(14 * time.Minute)
	if _, err := in.Resolve(id, true, "operator", "ok"); err != nil {
		t.Fatalf("Resolve inside the TTL: %v", err)
	}
}

func TestReDeliveryAfterExpiryQueuesAfresh(t *testing.T) {
	in, clock := ttlInbox(t, time.Hour)
	id, err := in.Enqueue(pendingRequest(1), operatorVerdict())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	*clock = clock.Add(2 * time.Hour)

	id2, err := in.Enqueue(pendingRequest(1), operatorVerdict())
	if err != nil {
		t.Fatalf("re-Enqueue after expiry: %v", err)
	}
	if id2 != id {
		t.Fatalf("re-delivery got a different ID %s, want %s", id2, id)
	}
	p, ok := in.Get(id)
	if !ok {
		t.Fatal("re-delivered request is not pending")
	}
	if !p.QueuedAt.Equal(*clock) {
		t.Errorf("QueuedAt = %v, want the re-delivery time %v (the old row must not be revived)", p.QueuedAt, *clock)
	}
	if _, err := in.Resolve(id, true, "operator", "fresh"); err != nil {
		t.Errorf("Resolve of the fresh row: %v", err)
	}
}

func TestSweepDropsOnlyExpired(t *testing.T) {
	in, clock := ttlInbox(t, time.Hour)
	old, _ := in.Enqueue(pendingRequest(1), operatorVerdict())
	*clock = clock.Add(30 * time.Minute)
	fresh, _ := in.Enqueue(pendingRequest(2), operatorVerdict())
	*clock = clock.Add(45 * time.Minute)

	n, err := in.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("Sweep dropped %d, want 1", n)
	}
	if _, ok := in.Get(old); ok {
		t.Error("expired item survived the sweep")
	}
	if _, ok := in.Get(fresh); !ok {
		t.Error("unexpired item was swept")
	}
}

func TestResolveManyReportsExpired(t *testing.T) {
	in, clock := ttlInbox(t, time.Hour)
	old, _ := in.Enqueue(pendingRequest(1), operatorVerdict())
	*clock = clock.Add(90 * time.Minute)
	fresh, _ := in.Enqueue(pendingRequest(2), operatorVerdict())

	res := in.ResolveMany([]string{old, fresh}, true, "operator", "bulk")
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	if res[0].Ok {
		t.Error("bulk approve released an expired item")
	}
	if !res[1].Ok {
		t.Errorf("bulk approve of the fresh item failed: %s", res[1].Error)
	}
}

func TestNoTTLNeverExpires(t *testing.T) {
	in, clock := ttlInbox(t, 0)
	id, _ := in.Enqueue(pendingRequest(1), operatorVerdict())
	*clock = clock.Add(365 * 24 * time.Hour)
	if _, ok := in.ExpiresAt(PendingItem{}); ok {
		t.Error("ExpiresAt reported a deadline with expiry disabled")
	}
	if _, err := in.Resolve(id, true, "operator", "ok"); err != nil {
		t.Errorf("Resolve with expiry disabled: %v", err)
	}
}

func TestDeskFromConfigPendingTTL(t *testing.T) {
	for name, tc := range map[string]struct {
		seconds int
		want    time.Duration
	}{
		"unset uses default": {0, DefaultPendingTTL},
		"positive seconds":   {900, 15 * time.Minute},
		"negative disables":  {-1, 0},
	} {
		_, inbox, err := DeskFromConfig(config.ToolApprovalConfig{
			Enabled:           true,
			InboxPath:         filepath.Join(t.TempDir(), "inbox.json"),
			PendingTTLSeconds: tc.seconds,
		}, nil, nil)
		if err != nil {
			t.Fatalf("%s: DeskFromConfig: %v", name, err)
		}
		if got := inbox.PendingTTL(); got != tc.want {
			t.Errorf("%s: PendingTTL = %v, want %v", name, got, tc.want)
		}
	}
}

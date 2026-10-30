package automerge

import (
	"testing"
	"time"
)

func TestBasePreparationSnapshotCopiesTicketsWithoutMutatingLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 30, 20, 40, 0, 0, time.UTC)
	store := NewBasePreparationStore("")
	key := preparationKey("acme", "widget", "main")
	if err := store.change(key, func(st *basePreparation) error {
		st.Owner = preparationCandidate{Number: 7, Head: "admitted", Since: now.Add(-24 * time.Hour)}
		st.Waiting = []preparationCandidate{{Number: 8, Head: "waiting", Since: now}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c := New(nil, Options{BasePreparation: store, Now: func() time.Time { return now },
		RequireCurrentBase: func(string) bool { return true }})
	got, err := c.BasePreparationSnapshot("acme/widget", "main")
	if err != nil || !got.Enabled || got.Owner == nil || got.Owner.Number != 7 || len(got.Waiting) != 1 {
		t.Fatalf("snapshot=%+v error=%v", got, err)
	}
	if !got.Owner.ExpiresAt.Before(now) {
		t.Fatal("snapshot must expose expired ticket without silently expiring it")
	}
	got.Owner.HeadSHA, got.Waiting[0].HeadSHA = "changed", "changed"
	again, err := c.BasePreparationSnapshot("acme/widget", "main")
	if err != nil || again.Owner.HeadSHA != "admitted" || again.Waiting[0].HeadSHA != "waiting" {
		t.Fatalf("snapshot aliases stored tickets: %+v error=%v", again, err)
	}
	c.requireCurrentBase = nil
	off, err := c.BasePreparationSnapshot("acme/widget", "main")
	if err != nil || off.Enabled || off.Owner != nil || len(off.Waiting) != 0 {
		t.Fatalf("disabled snapshot=%+v error=%v", off, err)
	}
}

package serve

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/lanes"
)

const foreignRecoveryWait = 10 * time.Second

func TestAuthorizedAPIForeignExpirationWithoutReceiptOrGap(t *testing.T) {
	s, clients, _ := laneAPIFixtureConfig(t, func(c *lanes.Config) {
		c.Retention, c.AckRetention = 3*time.Second, 3*time.Second
		c.Lease, c.Dedup = lanes.MinLease, lanes.MinDedup
		c.MaxLedger, c.MaxPending = 1, 1
	})
	ctx := context.Background()
	if _, err := clients["atlas"].PublishLane(ctx, "exchange",
		api.LanePublish{ID: "foreign", Kind: api.LaneWork, Schema: "v1", Payload: []byte("data")}); err != nil {
		t.Fatal(err)
	}
	old, err := clients["beacon"].ClaimLane(ctx, "exchange", "workers")
	if err != nil || old == nil {
		t.Fatal(err)
	}
	status, err := clients["atlas"].InspectLaneGroup(ctx, "exchange", "workers")
	if err != nil {
		t.Fatal(err)
	}
	in := api.LaneRecover{ID: "foreign-expiry", Revision: status.Revision,
		CursorThrough: status.Cursor, Missing: []uint64{old.Sequence}}
	_, err = clients["atlas"].RecoverLaneGroup(ctx, "exchange", "workers", in)
	requireLaneStatus(t, err, http.StatusConflict)
	_, err = clients["beacon"].RecoverLaneGroup(ctx, "exchange", "workers", in)
	requireLaneStatus(t, err, http.StatusForbidden)
	oversized := in
	oversized.Missing = make([]uint64, lanes.MaxRecoveryBatch+1)
	_, err = clients["atlas"].RecoverLaneGroup(ctx, "exchange", "workers", oversized)
	requireLaneStatus(t, err, http.StatusBadRequest)
	ticker := time.NewTicker(lanes.MinRetryDelay)
	defer ticker.Stop()
	deadline := time.NewTimer(foreignRecoveryWait)
	defer deadline.Stop()
	for {
		status, err = clients["atlas"].InspectLaneGroup(ctx, "exchange", "workers")
		if err != nil {
			t.Fatal(err)
		}
		if status.FirstRetained > old.Sequence && len(status.Missing) == 1 {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("missing foreign delivery never became observable")
		}
	}
	if len(status.Recoverable) != 0 || status.Cursor != status.FirstRetained-1 {
		t.Fatal("regression requires no own receipt or unread gap")
	}
	in.Revision = status.Revision
	out, err := clients["atlas"].RecoverLaneGroup(ctx, "exchange", "workers", in)
	if err != nil || len(out.Expirations) != 1 || out.Expirations[0].Project != "beacon" {
		t.Fatalf("foreign expiration: %+v %v", out, err)
	}
	cfg := s.Lanes.Config()
	s.Lanes.Close()
	s.Lanes, err = lanes.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Lanes.Close)
	replayed, err := clients["atlas"].RecoverLaneGroup(ctx, "exchange", "workers", in)
	if err != nil || !reflect.DeepEqual(out, replayed) {
		t.Fatal("foreign recovery lost-response retry failed", err)
	}
	fresh, err := clients["atlas"].PublishLane(ctx, "exchange",
		api.LanePublish{ID: "fresh", Kind: api.LaneWork, Schema: "v1", Payload: []byte("fresh")})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := clients["atlas"].ClaimLane(ctx, "exchange", "workers")
	if err != nil || resumed == nil || resumed.Sequence != fresh.Sequence {
		t.Fatal("foreign expired lease still occupies sole capacity", err)
	}
	requireLaneStatus(t, clients["beacon"].AckLane(ctx, "exchange", "workers", api.LaneReceipt{
		Sequence: old.Sequence, Receipt: old.Receipt}), http.StatusConflict)
	if err := clients["atlas"].AckLane(ctx, "exchange", "workers", api.LaneReceipt{
		Sequence: resumed.Sequence, Receipt: resumed.Receipt}); err != nil {
		t.Fatal(err)
	}
}

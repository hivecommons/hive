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

func TestLaneRecoveryAPIAuthFencingAndSameNamespaceRestart(t *testing.T) {
	s, clients, _ := laneAPIFixtureConfig(t, func(c *lanes.Config) {
		c.Retention, c.Lease, c.AckRetention = 2*time.Second, 2*time.Second, 2*time.Second
		c.Dedup = lanes.MinDedup
	})
	ctx := context.Background()
	if _, err := clients["atlas"].PublishLane(ctx, "exchange", api.LanePublish{ID: "old", Kind: api.LaneWork, Schema: "v1", Payload: []byte("old")}); err != nil {
		t.Fatal(err)
	}
	old, err := clients["atlas"].ClaimLane(ctx, "exchange", "workers")
	if err != nil || old == nil {
		t.Fatal("old claim", err)
	}
	gap, err := clients["atlas"].PublishLane(ctx, "exchange", api.LanePublish{ID: "gap", Kind: api.LaneEvent, Schema: "v1", Payload: []byte("gap")})
	if err != nil {
		t.Fatal(err)
	}
	var status api.LaneGroupState
	ticker := time.NewTicker(lanes.MinRetryDelay)
	defer ticker.Stop()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		status, err = clients["atlas"].InspectLaneGroup(ctx, "exchange", "workers")
		if err != nil {
			t.Fatal(err)
		}
		if status.FirstRetained > gap.Sequence && !old.Expires.After(time.Now()) {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("expired group prefix never became observable")
		}
	}
	in := api.LaneRecover{ID: "recover-gap", Revision: status.Revision,
		CursorThrough: status.FirstRetained - 1, Expired: status.Recoverable}
	if len(in.Expired) != 1 {
		t.Fatalf("own recoverable receipts not reported: %+v", status)
	}
	foreign, err := clients["beacon"].InspectLaneGroup(ctx, "exchange", "workers")
	if err != nil || len(foreign.Recoverable) != 0 {
		t.Fatal("other project's recovery receipt exposed", err)
	}
	_, err = clients["beacon"].RecoverLaneGroup(ctx, "exchange", "workers", in)
	requireLaneStatus(t, err, http.StatusForbidden)
	_, err = clients["atlas"].RecoverLaneGroup(ctx, "exchange", "beacon-only", in)
	requireLaneStatus(t, err, http.StatusForbidden)
	_, err = clients["admin"].RecoverLaneGroup(ctx, "exchange", "workers", in)
	requireLaneStatus(t, err, http.StatusUnauthorized)
	stale := in
	stale.Revision++
	_, err = clients["atlas"].RecoverLaneGroup(ctx, "exchange", "workers", stale)
	requireLaneStatus(t, err, http.StatusConflict)
	fresh, err := clients["atlas"].PublishLane(ctx, "exchange", api.LanePublish{ID: "fresh", Kind: api.LaneWork, Schema: "v1", Payload: []byte("fresh")})
	if err != nil {
		t.Fatal(err)
	}
	skip := in
	skip.CursorThrough = fresh.Sequence
	_, err = clients["atlas"].RecoverLaneGroup(ctx, "exchange", "workers", skip)
	requireLaneStatus(t, err, http.StatusConflict)
	requireLaneStatus(t, clients["atlas"].Do(ctx, http.MethodPost, api.PathLanes+"/exchange/groups/workers/recover", map[string]any{"id": in.ID, "revision": in.Revision, "project": "beacon"}, nil), http.StatusBadRequest)
	recovered, err := clients["atlas"].RecoverLaneGroup(ctx, "exchange", "workers", in)
	if err != nil || recovered.GapThrough != gap.Sequence {
		t.Fatalf("group recovery: %+v %v", recovered, err)
	}
	cfg := s.Lanes.Config()
	s.Lanes.Close()
	s.Lanes, err = lanes.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Lanes.Close)
	replayed, err := clients["atlas"].RecoverLaneGroup(ctx, "exchange", "workers", in)
	if err != nil || !reflect.DeepEqual(recovered, replayed) {
		t.Fatal("response loss/restart recovery was not idempotent", err)
	}
	resumed, err := clients["beacon"].ClaimLane(ctx, "exchange", "workers")
	if err != nil || resumed == nil || resumed.Sequence != fresh.Sequence {
		t.Fatal("same-namespace work stranded after recovery", err)
	}
	requireLaneStatus(t, clients["atlas"].AckLane(ctx, "exchange", "workers", api.LaneReceipt{Sequence: old.Sequence, Receipt: old.Receipt}), http.StatusConflict)
}

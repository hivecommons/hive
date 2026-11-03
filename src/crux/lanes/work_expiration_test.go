package lanes

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

const expirationTestRetention = 3 * time.Second

func TestAbsentForeignOwnerRecoveryReleasesCapacityWithAndWithoutUnreadGap(t *testing.T) {
	for _, full := range []bool{true, false} {
		name := "no-unread-gap"
		if full {
			name = "sole-ledger-slot"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, opts := testServer(t)
			cfg := testConfig(s.ClientURL())
			shortRetention(&cfg)
			cfg.Retention, cfg.AckRetention = expirationTestRetention, expirationTestRetention
			if full {
				cfg.MaxLedger, cfg.MaxPending = 1, 1
			}
			atlas, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer atlas.Close()
			beacon, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			publish(t, atlas, "foreign", api.LaneWork)
			old := claim(t, beacon, "beacon", "workers")
			beacon.Close()
			through := old.Sequence
			if full {
				through = publish(t, atlas, "unread", api.LaneEvent).Sequence
			}
			waitExpiredPrefix(t, atlas, through)
			awaitCondition(t, func() (bool, error) { return !old.Expires.After(time.Now()), nil })
			fresh := publish(t, atlas, "fresh", api.LaneWork)
			status, err := atlas.InspectGroup(ctx, "atlas", "shared", "workers")
			if err != nil || len(status.Recoverable) != 0 || len(status.Missing) != 1 {
				t.Fatalf("receipt-free inspection: %+v %v", status, err)
			}
			if !full && status.Cursor != status.FirstRetained-1 {
				t.Fatal("regression requires no unread cursor gap")
			}
			in := recoveryRequest(t, atlas, "atlas", "absent-owner")
			if len(in.Expired) != 0 || len(in.Missing) != 1 {
				t.Fatalf("foreign recovery requires an unrelated own receipt: %+v", in)
			}
			out, err := atlas.RecoverGroup(ctx, "atlas", "shared", "workers", in)
			if err != nil || len(out.Expirations) != 1 || out.Expirations[0].Project != "beacon" {
				t.Fatalf("foreign expiration audit: %+v %v", out, err)
			}
			atlas.Close()
			s.Shutdown()
			s.WaitForShutdown()
			s = startServer(t, opts)
			defer func() { s.Shutdown(); s.WaitForShutdown() }()
			atlas, err = Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer atlas.Close()
			replayed, err := atlas.RecoverGroup(ctx, "atlas", "shared", "workers", in)
			if err != nil || !reflect.DeepEqual(out, replayed) {
				t.Fatalf("lost response after service/broker restart: %+v %v", replayed, err)
			}
			resumed := claim(t, atlas, "atlas", "workers")
			if resumed.Sequence != fresh.Sequence {
				t.Fatal("absent Beacon permanently stranded shared work")
			}
			if err := atlas.Ack(ctx, "atlas", "shared", "workers", receipt(resumed)); err != nil {
				t.Fatal(err)
			}
			for _, project := range []string{"atlas", "beacon"} {
				if err := atlas.Ack(ctx, project, "shared", "workers", receipt(old)); !errors.Is(err, ErrStale) {
					t.Fatal("expiration accepted a foreign or owner ACK", project, err)
				}
				if _, err := atlas.Renew(ctx, project, "shared", "workers", receipt(old)); !errors.Is(err, ErrStale) {
					t.Fatal("expired foreign lease renewed", project, err)
				}
			}
		})
	}
}

func TestMissingAcceptanceCannotRetireLiveOrRetainedForeignWork(t *testing.T) {
	b := testBroker(t, nil)
	ctx := context.Background()
	publish(t, b, "foreign", api.LaneWork)
	foreign := claim(t, b, "beacon", "workers")
	status, err := b.InspectGroup(ctx, "atlas", "shared", "workers")
	if err != nil {
		t.Fatal(err)
	}
	in := api.LaneRecover{ID: "foreign-expiration", Revision: status.Revision,
		CursorThrough: status.Cursor, Missing: []uint64{foreign.Sequence}}
	if _, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", in); !errors.Is(err, ErrStale) {
		t.Fatal("live foreign lease retired", err)
	}
	b.now = func() time.Time { return foreign.Expires.Add(time.Nanosecond) }
	if _, err := b.RecoverGroup(ctx, "atlas", "shared", "workers", in); !errors.Is(err, ErrCursor) {
		t.Fatal("retained foreign work retired", err)
	}
	if _, err := b.RecoverGroup(ctx, "atlas", "shared", "other", in); !errors.Is(err, ErrForbidden) {
		t.Fatal("foreign group accepted", err)
	}
	if _, err := b.RecoverGroup(ctx, "outsider", "shared", "workers", in); !errors.Is(err, ErrForbidden) {
		t.Fatal("unauthorized expiration accepted", err)
	}
}

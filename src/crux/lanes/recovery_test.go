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
)

func TestExpiredRetentionFailsExplicitly(t *testing.T) {
	b := testBroker(t, func(c *Config) {
		c.Retention, c.Lease, c.AckRetention = time.Second, time.Second, time.Second
		c.Dedup = MinDedup
	})
	first := publish(t, b, "first", api.LaneEvent)
	publish(t, b, "second", api.LaneEvent)
	publish(t, b, "work", api.LaneWork)
	claim(t, b, "atlas", "workers")
	awaitCondition(t, func() (bool, error) {
		info, err := b.streams["shared"].Info(context.Background())
		if err != nil {
			return false, err
		}
		return info.State.Msgs == 0, nil
	})
	if _, err := b.Replay(context.Background(), "beacon", "shared", first.Sequence, 1); !errors.Is(err, ErrCursor) {
		t.Fatalf("expired cursor: %v", err)
	}
	if _, err := b.Claim(context.Background(), "beacon", "shared", "workers"); !errors.Is(err, ErrCursor) {
		t.Fatalf("expired pending work: %v", err)
	}
	page, err := b.Replay(context.Background(), "beacon", "shared", 0, 1)
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("explicit earliest-retained replay: %+v %v", page, err)
	}
}

func TestPublishReservationCrashRecoveryAfterDedupWindow(t *testing.T) {
	b := testBroker(t, func(c *Config) { c.Dedup = MinDedup })
	in := api.LanePublish{ID: "reserved", Kind: api.LaneWork, Schema: "v1", Payload: []byte("work")}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	key := "shared.atlas.reserved"
	data, err := json.Marshal(publishReservation{Hash: digest(body), Created: time.Now().Add(-time.Second), Generation: strings.Repeat("a", receiptBytes*2)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.dedup.Create(context.Background(), key, data); err != nil {
		t.Fatal(err)
	}
	cfg := b.cfg
	b.Close()
	reopened, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	out, err := reopened.Publish(context.Background(), "atlas", "shared", in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reopened.Publish(context.Background(), "atlas", "shared", in)
	if err != nil || !again.Duplicate || again.Sequence != out.Sequence {
		t.Fatalf("pending publish recovery: %+v %v", again, err)
	}
}

func TestDisconnectFailsAndReconnectRestoresDurability(t *testing.T) {
	s, opts := testServer(t)
	reconnected := make(chan struct{}, 1)
	b, err := Open(context.Background(), testConfig(s.ClientURL()), nats.ReconnectHandler(func(*nats.Conn) {
		select {
		case reconnected <- struct{}{}:
		default:
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	publish(t, b, "before", api.LaneEvent)
	s.Shutdown()
	s.WaitForShutdown()
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*100)
	_, err = b.Replay(ctx, "atlas", "shared", 0, 1)
	cancel()
	if err == nil {
		t.Fatal("disconnected broker returned success")
	}
	s = startServer(t, opts)
	defer func() { s.Shutdown(); s.WaitForShutdown() }()
	select {
	case <-reconnected:
	case <-time.After(testWait):
		t.Fatal("broker did not reconnect")
	}
	out := publish(t, b, "after", api.LaneEvent)
	page, err := b.Replay(context.Background(), "beacon", "shared", 0, MaxReplayBatch)
	if err != nil || len(page.Events) != 2 || page.Cursor != out.Sequence {
		t.Fatalf("reconnect: %+v %v", page, err)
	}
}

func TestRegistryChangeAndCapacityDoNotOverwriteData(t *testing.T) {
	b := testBroker(t, func(c *Config) { c.MaxMessages = 1 })
	first := publish(t, b, "first", api.LaneEvent)
	_, err := b.Publish(context.Background(), "atlas", "shared", api.LanePublish{ID: "second", Kind: api.LaneEvent, Schema: "v1", Payload: []byte("value")})
	if err == nil {
		t.Fatal("full stream accepted and discarded an event")
	}
	page, err := b.Replay(context.Background(), "beacon", "shared", 0, 1)
	if err != nil || page.Events[0].Sequence != first.Sequence {
		t.Fatalf("full stream lost prior message: %+v %v", page, err)
	}
	cfg := b.cfg
	cfg.Lanes[0].Publishers = []string{"atlas"}
	if changed, err := Open(context.Background(), cfg); !errors.Is(err, ErrConflict) {
		if changed != nil {
			changed.Close()
		}
		t.Fatalf("registry overwritten: %v", err)
	}
}

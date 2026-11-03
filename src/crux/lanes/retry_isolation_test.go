package lanes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

func TestRetryScheduleAndFairTurnSurviveServiceRestart(t *testing.T) {
	b := testBroker(t, func(c *Config) { c.RetryDelay, c.MaxRetryDelay = MinRetryDelay, MinRetryDelay })
	ctx := context.Background()
	poison := publish(t, b, "poison", api.LaneWork)
	first := claim(t, b, "atlas", "workers")
	if err := b.Nack(ctx, "atlas", "shared", "workers", receipt(first)); err != nil {
		t.Fatal(err)
	}
	status, err := b.InspectGroup(ctx, "atlas", "shared", "workers")
	if err != nil || len(status.Retries) != 1 {
		t.Fatalf("scheduled retry: %+v %v", status, err)
	}
	cfg := b.Config()
	b.Close()
	peer, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	restored, err := peer.InspectGroup(ctx, "atlas", "shared", "workers")
	if err != nil || len(restored.Retries) != 1 || !restored.Retries[0].AvailableAt.Equal(status.Retries[0].AvailableAt) {
		t.Fatal("retry schedule lost on restart", err)
	}
	due := restored.Retries[0].AvailableAt.Add(DefaultPollForRetryTest)
	peer.now = func() time.Time { return due }
	fresh := publish(t, peer, "valid", api.LaneWork)
	retry := claim(t, peer, "atlas", "workers")
	if retry.Sequence != poison.Sequence {
		t.Fatal("retry turn lost on restart")
	}
	if err := peer.Nack(ctx, "atlas", "shared", "workers", receipt(retry)); err != nil {
		t.Fatal(err)
	}
	next, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	due = due.Add(peer.cfg.MaxRetryDelay + DefaultPollForRetryTest)
	next.now = func() time.Time { return due }
	valid := claim(t, next, "beacon", "workers")
	if valid.Sequence != fresh.Sequence {
		t.Fatal("poison retry starved fresh work after delay elapsed")
	}
	if err := next.Ack(ctx, "beacon", "shared", "workers", receipt(valid)); err != nil {
		t.Fatal(err)
	}
	remaining := claim(t, next, "atlas", "workers")
	if remaining.Sequence != poison.Sequence || remaining.Attempt != 3 {
		t.Fatal("poison work silently dropped or acknowledged")
	}
}

const DefaultPollForRetryTest = time.Second

func TestRetryBackoffIsBoundedAndNackIsIdempotent(t *testing.T) {
	b := testBroker(t, nil)
	if b.retryDelay(1) != b.cfg.RetryDelay || b.retryDelay(^uint64(0)) != b.cfg.MaxRetryDelay {
		t.Fatal("retry bounds")
	}
	publish(t, b, "job", api.LaneWork)
	c := claim(t, b, "atlas", "workers")
	ctx := context.Background()
	if err := b.Nack(ctx, "atlas", "shared", "workers", receipt(c)); err != nil {
		t.Fatal(err)
	}
	before, err := b.InspectGroup(ctx, "atlas", "shared", "workers")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Claim(ctx, "atlas", "shared", "workers"); !errors.Is(err, ErrEmpty) {
		t.Fatal("NACK was immediately retried", err)
	}
	if err := b.Nack(ctx, "atlas", "shared", "workers", receipt(c)); err != nil {
		t.Fatal(err)
	}
	after, err := b.InspectGroup(ctx, "atlas", "shared", "workers")
	if err != nil || !after.Retries[0].AvailableAt.Equal(before.Retries[0].AvailableAt) {
		t.Fatal("duplicate NACK moved retry time", err)
	}
}

func TestAmbiguousNamespaceLaneTuplesRemainIsolated(t *testing.T) {
	s, _ := testServer(t)
	a := testConfig(s.ClientURL())
	a.Namespace, a.Lanes[0].ID = "a", "b_lane_c"
	b := testConfig(s.ClientURL())
	b.Namespace, b.Lanes[0].ID = "a_lane_b", "c"
	first, err := Open(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.streamName(a.Lanes[0].ID) == second.streamName(b.Lanes[0].ID) {
		t.Fatal("ambiguous tuple stream name")
	}
	for _, service := range []*Broker{first, second} {
		lane := service.cfg.Lanes[0].ID
		if _, err := service.Publish(context.Background(), "atlas", lane, api.LanePublish{ID: "same", Kind: api.LaneEvent, Schema: "v1", Payload: []byte(service.cfg.Namespace)}); err != nil {
			t.Fatal(err)
		}
		page, err := service.Replay(context.Background(), "beacon", lane, 0, 1)
		if err != nil || len(page.Events) != 1 || string(page.Events[0].Envelope.Payload) != service.cfg.Namespace {
			t.Fatal("namespace leakage", err)
		}
	}
}

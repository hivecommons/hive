package lanes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

func TestRetryRotationCompletesPreviouslyClaimedWorkAcrossRestart(t *testing.T) {
	b := testBroker(t, func(c *Config) { c.RetryDelay, c.MaxRetryDelay = MinRetryDelay, MinRetryDelay })
	ctx := context.Background()
	poison := publish(t, b, "poison", api.LaneWork)
	valid := publish(t, b, "previously-claimed", api.LaneWork)
	low := claim(t, b, "atlas", "workers")
	lost := claim(t, b, "beacon", "workers")
	if low.Sequence != poison.Sequence || lost.Sequence != valid.Sequence {
		t.Fatal("regression requires two previously claimed deliveries")
	}
	if err := b.Nack(ctx, "atlas", "shared", "workers", receipt(low)); err != nil {
		t.Fatal(err)
	}
	// The high sequence's claim response was lost. Both deliveries are ready
	// behind the durable cursor; neither can be selected as fresh work.
	clock := lost.Expires.Add(DefaultPollForRetryTest)
	cfg := b.Config()
	b.Close()
	restarted, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.now = func() time.Time { return clock }
	retry := claim(t, restarted, "atlas", "workers")
	if retry.Sequence != poison.Sequence {
		t.Fatal("first retry should select low sequence")
	}
	if err := restarted.Nack(ctx, "atlas", "shared", "workers", receipt(retry)); err != nil {
		t.Fatal(err)
	}
	restarted.Close()
	clock = clock.Add(cfg.MaxRetryDelay + DefaultPollForRetryTest)
	next, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	next.now = func() time.Time { return clock }
	completed := claim(t, next, "beacon", "workers")
	if completed.Sequence != valid.Sequence || completed.Attempt != 2 {
		t.Fatal("always-ready poison starved previously claimed valid work")
	}
	if err := next.Ack(ctx, "beacon", "shared", "workers", receipt(completed)); err != nil {
		t.Fatal(err)
	}
	if err := next.Ack(ctx, "beacon", "shared", "workers", receipt(lost)); !errors.Is(err, ErrStale) {
		t.Fatal("lost original claim bypassed reassignment fencing", err)
	}
	clock = clock.Add(cfg.MaxRetryDelay + DefaultPollForRetryTest)
	remaining := claim(t, next, "atlas", "workers")
	if remaining.Sequence != poison.Sequence || remaining.Attempt != 3 {
		t.Fatal("poison was silently acknowledged or discarded")
	}
	if err := next.Nack(ctx, "atlas", "shared", "workers", receipt(remaining)); err != nil {
		t.Fatal(err)
	}
	status, err := next.InspectGroup(ctx, "atlas", "shared", "workers")
	if err != nil || len(status.Retries) != 1 || status.Retries[0].Sequence != poison.Sequence {
		t.Fatal("poison retry is not explicit", err)
	}
}

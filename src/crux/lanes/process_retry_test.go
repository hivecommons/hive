package lanes

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

func TestRetryFairnessAcrossIndependentCruxWorkerProcesses(t *testing.T) {
	b := testBroker(t, func(c *Config) { c.RetryDelay, c.MaxRetryDelay = MinRetryDelay, MinRetryDelay })
	poison := publish(t, b, "poison", api.LaneWork)
	first := startPublisherProcess(t, b.Config(), "", false, processWorkActionEnv+"=nack", processProjectEnv+"=atlas")
	rejected := processOutcome(t, first)
	if rejected.Error != "" || rejected.ClaimSequence != poison.Sequence || rejected.Attempt != 1 || rejected.AvailableAt.IsZero() {
		t.Fatalf("first worker: %+v", rejected)
	}
	fresh := publish(t, b, "valid", api.LaneWork)
	timer := time.NewTimer(time.Until(rejected.AvailableAt.Add(DefaultPollForRetryTest)))
	<-timer.C
	second := startPublisherProcess(t, b.Config(), "", false, processWorkActionEnv+"=nack", processProjectEnv+"=atlas")
	retried := processOutcome(t, second)
	if first.pid == second.pid || retried.Error != "" || retried.ClaimSequence != poison.Sequence || retried.Attempt != 2 {
		t.Fatalf("second worker: %+v", retried)
	}
	timer = time.NewTimer(time.Until(retried.AvailableAt.Add(DefaultPollForRetryTest)))
	<-timer.C
	third := startPublisherProcess(t, b.Config(), "", false, processWorkActionEnv+"=ack", processProjectEnv+"=beacon")
	completed := processOutcome(t, third)
	if third.pid == second.pid || completed.Error != "" || completed.ClaimSequence != fresh.Sequence || completed.Attempt != 1 {
		t.Fatalf("fresh work starved across worker restarts: %+v", completed)
	}
	status, err := b.InspectGroup(context.Background(), "atlas", "shared", "workers")
	if err != nil || len(status.Retries) != 1 || status.Retries[0].Sequence != poison.Sequence {
		t.Fatal("rejected work disappeared", err)
	}
	fourth := startPublisherProcess(t, b.Config(), "", false, processWorkActionEnv+"=nack", processProjectEnv+"=atlas")
	remaining := processOutcome(t, fourth)
	if remaining.Error != "" || remaining.ClaimSequence != poison.Sequence || remaining.Attempt != 3 {
		t.Fatalf("rejected work was silently ACKed/dropped: %+v", remaining)
	}
}

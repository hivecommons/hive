package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestRequestRelaySupervisorRestartChainsSameClient(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	client := github.NewClientForTest("http://127.0.0.1:1", "acme", []string{"widgets"}, quietLogger())
	started := make(chan int, 4)
	cancelled := make(chan int, 4)
	releaseFirst := make(chan struct{})

	var mu sync.Mutex
	starts := 0
	s := newRequestRelaySupervisor(parent, func(ctx context.Context, c *github.Client) <-chan struct{} {
		if c != client {
			t.Errorf("relay generation started on %p, want %p", c, client)
		}
		mu.Lock()
		starts++
		gen := starts
		mu.Unlock()
		started <- gen

		done := make(chan struct{})
		go func() {
			defer close(done)
			<-ctx.Done()
			cancelled <- gen
			if gen == 1 {
				<-releaseFirst
			}
		}()
		return done
	}, quietLogger())

	if !s.switchTo(client) {
		t.Fatal("switchTo did not start initial generation")
	}
	if got := waitRelayGeneration(t, started); got != 1 {
		t.Fatalf("initial generation = %d, want 1", got)
	}
	if !s.restart() {
		t.Fatal("restart returned false with a running client")
	}
	if got := waitRelayGeneration(t, cancelled); got != 1 {
		t.Fatalf("cancelled generation = %d, want 1", got)
	}
	select {
	case gen := <-started:
		t.Fatalf("generation %d started before the previous generation stopped", gen)
	default:
	}
	close(releaseFirst)
	if got := waitRelayGeneration(t, started); got != 2 {
		t.Fatalf("restarted generation = %d, want 2", got)
	}
	if c, gens := s.current(); c != client || gens != 2 {
		t.Fatalf("current = (%p, %d), want (%p, 2)", c, gens, client)
	}
}

func TestRequestRelaySupervisorRestartFalseWithoutClientOrParent(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	s := newRequestRelaySupervisor(parent, func(ctx context.Context, _ *github.Client) <-chan struct{} {
		return ctx.Done()
	}, quietLogger())
	if s.restart() {
		t.Fatal("restart returned true before a client was running")
	}

	client := github.NewClientForTest("http://127.0.0.1:1", "acme", []string{"widgets"}, quietLogger())
	cancel()
	if s.switchTo(client) {
		t.Fatal("switchTo started after parent cancellation")
	}
	if s.restart() {
		t.Fatal("restart returned true after parent cancellation")
	}
}

func TestACMMLevelChangedRestartsRelaysOnlyWhenSelfMergeVerdictFlips(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	client := github.NewClientForTest("http://127.0.0.1:1", "acme", []string{"widgets"}, quietLogger())
	started := make(chan struct{}, 4)
	b := &boot{
		cfg:    &config.Config{},
		logger: quietLogger(),
	}
	b.requestRelays = newRequestRelaySupervisor(parent, func(ctx context.Context, _ *github.Client) <-chan struct{} {
		started <- struct{}{}
		return ctx.Done()
	}, b.logger)
	if !b.requestRelays.switchTo(client) {
		t.Fatal("initial switchTo failed")
	}
	waitRelayStart(t, started)

	tests := []struct {
		name        string
		prev, next  int
		wantRestart bool
	}{
		{name: "below threshold remains disabled", prev: 3, next: 4},
		{name: "raise to self merge level starts sweep", prev: 5, next: 6, wantRestart: true},
		{name: "same self merge level unchanged", prev: 6, next: 6},
		{name: "drop below self merge level stops sweep", prev: 6, next: 5, wantRestart: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, before := b.requestRelays.current()
			b.onACMMLevelChanged(tc.prev, tc.next)
			_, after := b.requestRelays.current()
			if tc.wantRestart {
				if after != before+1 {
					t.Fatalf("generations = %d, want %d", after, before+1)
				}
				waitRelayStart(t, started)
				return
			}
			if after != before {
				t.Fatalf("generations = %d, want unchanged %d", after, before)
			}
		})
	}
}

func waitRelayGeneration(t *testing.T, ch <-chan int) int {
	t.Helper()
	select {
	case gen := <-ch:
		return gen
	case <-time.After(fakeRelayStartTimeout):
		t.Fatal("timed out waiting for relay generation")
		return 0
	}
}

func waitRelayStart(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(fakeRelayStartTimeout):
		t.Fatal("timed out waiting for relay start")
	}
}

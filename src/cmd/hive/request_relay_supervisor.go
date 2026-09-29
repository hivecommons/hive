package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/hivecommons/hive/pkg/github"
)

// relayStartFunc starts the request relays (PR/issue/review/merge request
// watchers and the self-authored merge sweep) on one client. It returns a
// channel that closes once every loop it started has exited after ctx is
// cancelled, including any request that was in flight.
type relayStartFunc func(ctx context.Context, client *github.Client) <-chan struct{}

// requestRelaySupervisor keeps the request relays running on the hive's
// CURRENT GitHub client (#9621).
//
// The relays used to be started once in bootAgents on the boot client. After
// an App credential rebuild they kept minting with the old AppAuth (and kept
// the old client's policy gates), and on a hosted spoke that booted without a
// usable App they never started at all: agent PR/issue/review/merge requests
// sat in their queues until the pod restarted.
//
// switchTo hands the relays over to a new client. Each hand-over is a
// generation: the new generation starts only after the previous generation's
// loops have fully exited, so two watchers never consume the same request
// directory at once and a request file is never processed twice across a
// switch. Files are the durable queue: a request the old generation had not
// reached stays on disk and the new generation picks it up on its first tick.
// The hand-over waits in its own goroutine, so a rebuild path (heartbeat
// callback, config watcher, dashboard save) never blocks on a slow in-flight
// request.
type requestRelaySupervisor struct {
	parent context.Context
	start  relayStartFunc
	logger *slog.Logger

	mu     sync.Mutex
	client *github.Client
	cancel context.CancelFunc
	// done closes when the latest generation has fully stopped (or was
	// superseded before it started). The next generation waits on it.
	done <-chan struct{}
	// generations counts hand-overs, for logs and tests.
	generations int
}

func newRequestRelaySupervisor(parent context.Context, start relayStartFunc, logger *slog.Logger) *requestRelaySupervisor {
	if logger == nil {
		logger = slog.Default()
	}
	return &requestRelaySupervisor{parent: parent, start: start, logger: logger}
}

// switchTo runs the relays on client from now on. It returns true when it
// scheduled a hand-over and false when there was nothing to do: a nil
// supervisor, client or start func, the client the relays already run on, or
// a parent context that is already done (shutdown).
func (s *requestRelaySupervisor) switchTo(client *github.Client) bool {
	if s == nil || client == nil || s.start == nil || s.parent == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if client == s.client || s.parent.Err() != nil {
		return false
	}
	prevDone := s.done
	if s.cancel != nil {
		s.cancel()
	}
	ctx, cancel := context.WithCancel(s.parent)
	done := make(chan struct{})
	s.client, s.cancel, s.done = client, cancel, done
	s.generations++
	generation := s.generations
	go func() {
		defer close(done)
		if prevDone != nil {
			// Unconditional: even when this generation is itself superseded
			// while waiting, its done must not close before the previous
			// generation has stopped, or the chain would let two overlap.
			<-prevDone
		}
		if ctx.Err() != nil {
			return
		}
		s.logger.Info("request relays running on GitHub client", "generation", generation, "restart", generation > 1)
		if running := s.start(ctx, client); running != nil {
			<-running
		}
	}()
	return true
}

// current returns the client the relays were last handed and the number of
// hand-overs so far.
func (s *requestRelaySupervisor) current() (*github.Client, int) {
	if s == nil {
		return nil, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client, s.generations
}

// stopped returns a channel that closes once the latest generation has fully
// stopped. Only meaningful after the parent context is cancelled; nil when
// the relays never started.
func (s *requestRelaySupervisor) stopped() <-chan struct{} {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// joinDone returns a channel that closes once every non-nil channel in chans
// has closed.
func joinDone(chans ...<-chan struct{}) <-chan struct{} {
	out := make(chan struct{})
	go func() {
		defer close(out)
		for _, c := range chans {
			if c != nil {
				<-c
			}
		}
	}()
	return out
}

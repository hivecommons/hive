package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

// normalVisualRuntimeStopper ends only the additive Visual Hive service inside
// ordinary Hive. It preserves the single-writer lease until the service loop
// and every ephemeral specialist child are proven stopped, then releases the
// exact ownership fence so supported uninstall finalization can validate and
// delete its state. Ordinary Hive agents and the dashboard remain running.
type normalVisualRuntimeStopper struct {
	mu               sync.Mutex
	cancel           context.CancelFunc
	done             <-chan struct{}
	manager          specialistChildShutdowner
	resetSpecialists func() error
	releaseOwnership func()
	logger           *slog.Logger
	cancelRequested  bool
	stopped          bool
}

func (stopper *normalVisualRuntimeStopper) Stop(ctx context.Context) error {
	if stopper == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stopper.mu.Lock()
	if stopper.stopped {
		stopper.mu.Unlock()
		return nil
	}
	if stopper.cancel == nil || stopper.done == nil || stopper.releaseOwnership == nil {
		stopper.mu.Unlock()
		return errors.New("normal Visual Hive runtime stop bindings are incomplete")
	}
	if !stopper.cancelRequested {
		stopper.cancel()
		stopper.cancelRequested = true
	}
	done := stopper.done
	stopper.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := shutdownOrdinarySpecialistChildren(stopper.manager, stopper.logger); err != nil {
		return err
	}
	if stopper.resetSpecialists != nil {
		if err := stopper.resetSpecialists(); err != nil {
			return err
		}
	}

	stopper.mu.Lock()
	defer stopper.mu.Unlock()
	if stopper.stopped {
		return nil
	}
	stopper.releaseOwnership()
	stopper.stopped = true
	return nil
}

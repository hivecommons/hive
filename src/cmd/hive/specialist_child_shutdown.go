package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

var ordinarySpecialistChildShutdownTimeout = 30 * time.Second

type specialistChildShutdowner interface {
	ShutdownSpecialistChildren(context.Context) error
}

// shutdownOrdinaryVisualRuntime preserves the single-writer fence until every
// ephemeral child is reaped. The application context is canceled first so no
// new work can begin; ownership is released last so another process cannot
// acquire the runtime while a child from this owner remains live.
func shutdownOrdinaryVisualRuntime(cancel context.CancelFunc, manager specialistChildShutdowner, releaseOwnership func(), logger *slog.Logger) error {
	if cancel != nil {
		cancel()
	}
	if err := shutdownOrdinarySpecialistChildren(manager, logger); err != nil {
		failure := fmt.Errorf("retain ordinary Visual Hive runtime ownership because specialist children are not proven dead: %w", err)
		if logger != nil {
			logger.Error("ordinary Visual Hive shutdown halted with ownership fence held", "error", failure)
		}
		// Returning would let main exit and the OS release the lease descriptor.
		// Halt the owner process instead; an operator may kill it, at which point
		// the OS process boundary is the final descendant/lease containment fence.
		for {
			time.Sleep(24 * time.Hour)
		}
	}
	if releaseOwnership != nil {
		releaseOwnership()
	}
	return nil
}

// shutdownOrdinarySpecialistChildren is deliberately typed to the ephemeral
// child-only shutdown surface. It cannot stop or restart persistent agents.
func shutdownOrdinarySpecialistChildren(manager specialistChildShutdowner, logger *slog.Logger) error {
	if manager == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), ordinarySpecialistChildShutdownTimeout)
	defer cancel()
	if err := manager.ShutdownSpecialistChildren(ctx); err != nil {
		if logger != nil {
			logger.Warn("shutdown ordinary Hive specialist children; runtime ownership retained", "error", err)
		}
		return err
	}
	return nil
}

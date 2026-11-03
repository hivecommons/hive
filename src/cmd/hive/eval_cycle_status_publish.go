package main

import (
	"log/slog"

	"github.com/hivecommons/hive/pkg/dashboard"
)

// statusPublisher is the slice of *dashboard.Server the eval cycle's status
// publishing touches, so the pre-kick publish can be exercised without a
// live server.
type statusPublisher interface {
	BeginStatusSnapshot() uint64
	UpdateStatusIfFresh(status *dashboard.StatusPayload, buildEpoch uint64) bool
}

// publishStatusBeforeKicks pushes this cycle's evaluation to the dashboard
// BEFORE the serial kick dispatch starts. Kick delivery blocks the cycle for
// up to the input-prompt timeout per agent, so a cycle with a dozen due
// agents left /api/status on the previous cycle's snapshot for 10-15 minutes
// — and a pod rolled mid-dispatch never published the evaluation at all.
//
// With no kicks due the end-of-cycle publish follows immediately, so the
// interim snapshot is skipped rather than built twice. The epoch guard still
// applies: a mutation racing the build drops this snapshot, and the
// end-of-cycle publish (or the mutation's own refresh) repaints. Returns
// whether an interim snapshot was published.
func publishStatusBeforeKicks(srv statusPublisher, pendingKicks int, build func() *dashboard.StatusPayload, logger *slog.Logger) bool {
	if srv == nil || pendingKicks == 0 || build == nil {
		return false
	}
	buildEpoch := srv.BeginStatusSnapshot()
	payload := build()
	if payload == nil {
		return false
	}
	published := srv.UpdateStatusIfFresh(payload, buildEpoch)
	if !published && logger != nil {
		logger.Info("pre-kick status snapshot dropped by a racing mutation; end-of-cycle publish follows",
			"pending_kicks", pendingKicks)
	}
	return published
}

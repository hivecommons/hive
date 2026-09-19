package main

import (
	"context"
	"os"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/tokens"
)

// Collector caches persisted on the /data PVC so a pod roll resumes from
// the last-known counts instead of nil (#2329).
const (
	fleetStatsPersistPath = "/data/fleet-stats.json"
	activityPersistPath   = "/data/activity.json"
	repoCostPersistPath   = "/data/repo-cost.json"
)

// ctxCollector is the shape shared by every ctx-bound dashboard collector
// bootCollectors starts (metrics, fleet stats, activity, repo cost).
type ctxCollector interface{ Start(ctx context.Context) }

// pvcPersister is the shape shared by the collectors that cache on the PVC.
type pvcPersister interface{ EnablePersistence(path string) }

// bootCollectorsDeps are bootCollectors' long-lived effects (#7571, step 2):
// the collector goroutines, the PVC persistence enables (each reads its
// cache from disk), the bot-login lookup that reaches GitHub when ai_author
// is unset, and the cached-actionable read. Collector construction and the
// refreshDashboard closure run for real.
type bootCollectorsDeps struct {
	startTokenCollector    func(c *tokens.Collector, stop <-chan struct{})
	startCollector         func(ctx context.Context, name string, c ctxCollector)
	enablePersistence      func(name string, p pvcPersister, path string)
	lookupTokenLogin       func(token, apiURL string) (string, error)
	startContributeMetrics func(ctx context.Context, srv *dashboard.Server)
	readLastActionable     func() ([]byte, error)
}

func defaultBootCollectorsDeps() bootCollectorsDeps {
	return bootCollectorsDeps{
		startTokenCollector: func(c *tokens.Collector, stop <-chan struct{}) { go c.Start(stop) },
		startCollector:      func(ctx context.Context, _ string, c ctxCollector) { go c.Start(ctx) },
		enablePersistence:   func(_ string, p pvcPersister, path string) { p.EnablePersistence(path) },
		lookupTokenLogin: func(token, apiURL string) (string, error) {
			botUser, err := github.ValidateToken(token, apiURL)
			if err != nil {
				return "", err
			}
			return botUser.Login, nil
		},
		startContributeMetrics: func(ctx context.Context, srv *dashboard.Server) { srv.StartContributeMetrics(ctx) },
		readLastActionable:     func() ([]byte, error) { return os.ReadFile(lastActionablePath) },
	}
}

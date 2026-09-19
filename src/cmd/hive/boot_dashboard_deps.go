package main

import (
	"log/slog"

	"github.com/hivecommons/hive/pkg/dashboard"
)

// Dashboard state persisted on the /data PVC (never the ephemeral /etc/hive
// ConfigMap mount — see the comments in bootDashboardWith).
const (
	dashboardSessionsPath = "/data/dashboard-sessions.json"
	lifecycleTimelinePath = "/data/lifecycle-timeline.json"
)

// bootDashboardDeps are bootDashboard's effects (#7571, step 2): the server
// constructor, and the two PVC-backed persistence enables that read state
// from disk. The scheduler/agent-manager hook wiring and the history seeds
// run for real against the constructed server.
type bootDashboardDeps struct {
	newServer                  func(port int, authToken string, logger *slog.Logger) *dashboard.Server
	enableSessionPersistence   func(srv *dashboard.Server, path string)
	enableLifecyclePersistence func(srv *dashboard.Server, path string)
}

func defaultBootDashboardDeps() bootDashboardDeps {
	return bootDashboardDeps{
		newServer:                  dashboard.NewServerWithAuth,
		enableSessionPersistence:   (*dashboard.Server).EnableSessionPersistence,
		enableLifecyclePersistence: (*dashboard.Server).EnableLifecyclePersistence,
	}
}

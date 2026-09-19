package main

import (
	"context"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
)

// bootDashboardAPIDeps are the long-lived effects bootDashboardAPI performs
// (#7571, step 2): registering the API surface on the dashboard server,
// installing the Forge App inventory provider, and starting the three
// background loops (installation-ID discovery, App-banner self-heal, the
// brainstorm inception watcher). Everything else the phase does — wiring the
// dashboard.Dependencies closures, applying the classified App state,
// installing the Re-check callback — is plain assignment against the real
// *dashboard.Server, which a test constructs cheaply with dashboard.NewServer.
//
// The loop constructors receive the *body* of each tick so a test can call
// the captured func directly and assert what one tick does, without running
// a ticker. defaultBootDashboardAPIDeps binds them to runTickLoop.
type bootDashboardAPIDeps struct {
	registerAPI          func(srv *dashboard.Server, d *dashboard.Dependencies)
	setForgeAppInventory func(srv *dashboard.Server, fn func() dashboard.ForgeAppInventory)
	// startInstallDiscovery runs tryDiscover once immediately and then every
	// githubAppDiscoveryInterval until ctx is done.
	startInstallDiscovery func(ctx context.Context, tryDiscover func())
	// startSelfHeal runs heal every githubAppSelfHealInterval until ctx is
	// done. It does NOT run immediately: boot has just applied the classified
	// App state and the first tick is cheap because heal no-ops while the
	// banner is not showing.
	startSelfHeal         func(ctx context.Context, heal func())
	startInceptionWatcher func(ctx context.Context, w *dashboard.InceptionWatcher)
}

// githubAppDiscoveryInterval paces the installation-ID discovery loop. It
// covers the delayed approval path: a non-admin requests installation, an
// org admin approves later, and the spoke adopts the installation ID without
// requiring anyone to paste it.
const githubAppDiscoveryInterval = 5 * time.Minute

// githubAppSelfHealInterval mirrors the heartbeat cadence so a stale banner
// clears within one heartbeat window of the app becoming healthy, without
// adding meaningful GitHub API load (the check only runs while the banner is
// actually showing).
const githubAppSelfHealInterval = 2 * time.Minute

func defaultBootDashboardAPIDeps() bootDashboardAPIDeps {
	return bootDashboardAPIDeps{
		registerAPI: func(srv *dashboard.Server, d *dashboard.Dependencies) { srv.RegisterAPI(d) },
		setForgeAppInventory: func(srv *dashboard.Server, fn func() dashboard.ForgeAppInventory) {
			srv.SetForgeAppInventoryFn(fn)
		},
		startInstallDiscovery: func(ctx context.Context, tryDiscover func()) {
			go runTickLoop(ctx, githubAppDiscoveryInterval, true, tryDiscover)
		},
		startSelfHeal: func(ctx context.Context, heal func()) {
			go runTickLoop(ctx, githubAppSelfHealInterval, false, heal)
		},
		startInceptionWatcher: func(ctx context.Context, w *dashboard.InceptionWatcher) { go w.Run(ctx) },
	}
}

// runTickLoop calls fn every interval until ctx is done, and first
// immediately when runFirst is set. It is the shape shared by the discovery
// and self-heal loops, split out so the ticker mechanics are tested once and
// the per-tick bodies are tested as plain funcs.
func runTickLoop(ctx context.Context, interval time.Duration, runFirst bool, fn func()) {
	if runFirst {
		fn()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn()
		}
	}
}

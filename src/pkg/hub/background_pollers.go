package hub

import (
	"context"
	"sync"
)

// StartBackgroundPollers launches the hub's long-lived SaaS pollers. It is the
// explicit lifecycle entrypoint the composition root (cmd/hive runHub) calls
// once, after constructing the server and before serving traffic.
//
// This used to happen implicitly inside registerSaaSRoutes behind a
// !testing.Testing() guard, which coupled production code to the testing
// package and hid a lifecycle decision inside route registration. Route
// registration is now side-effect free; anything that constructs a HubServer
// without calling this method (every test, and any embedder that only wants
// the handlers) gets no background goroutines. Pollers immediately hit the
// GitHub API and read the package-level saas path variables, so starting them
// must remain an explicit, caller-owned choice.
//
// The provided ctx bounds every poller; cancelling it stops them all. The
// returned channel closes once every poller goroutine has exited, giving
// callers (the composition root ignores it; tests rely on it) a deterministic
// join for that contract.
func (s *HubServer) StartBackgroundPollers(ctx context.Context) <-chan struct{} {
	var wg sync.WaitGroup
	start := func(poller func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			poller(ctx)
		}()
	}
	start(s.startProvisionWatcher)
	start(s.StartLatestSHAPoller)
	// Periodically probe every spoke's unauthenticated /api/status and alert
	// on any that answer 200 (wide open) — catches auth drift automatically.
	start(s.StartAuthAudit)
	// Advisory-suppression profile (#4167): one structured log line every
	// cycle saying how many hives are stale and how many are stale but
	// UNREPORTED. Read-only measurement — no alert, no registry write.
	start(s.StartAdvisoryDiagnostics)
	if s.githubActivityFeed != nil {
		start(s.githubActivityFeed.Run)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

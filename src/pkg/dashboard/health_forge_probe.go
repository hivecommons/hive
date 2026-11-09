package dashboard

import (
	"context"
	"errors"
	"net"
	"net/url"
	"sync"
	"time"
)

// forgeProbeBudget is how long the github_auth check in healthSummaryFor waits
// for the App token probe. The probe can mint a token, which is a live call to
// the forge; when the forge host is unreachable from the pod that call blocks
// for the full mint timeout. HealthSummary runs inside the hub heartbeat
// collect, whose whole budget is 10s, so an unbounded probe made every beat on
// a hive bound to a dead GHE host ship stale stats (#17427). Past the budget
// the check reports the last cached probe result and the probe keeps running
// off the critical path. A var so tests can shrink it.
var forgeProbeBudget = 3 * time.Second

// healthTokenProbeFn overrides the App token probe. A package-level seam
// (mirroring healthAgentStatuses) so tests can stage a slow or unreachable
// forge without a real App key. nil ⇒ s.deps.GHAppAuth.Token.
var healthTokenProbeFn func(ctx context.Context) error

// forgeProbeState caches the last forge (App token) probe result. inFlight is
// non-nil while a probe goroutine runs and is closed when it finishes, so at
// most one probe is outstanding however many beats time out behind it.
type forgeProbeState struct {
	mu          sync.Mutex
	inFlight    chan struct{}
	hasResult   bool
	lastErr     error
	unreachable bool
}

// forgeProbeResult is what one github_auth check sees: the last completed
// probe's error (Known=false when no probe has completed yet) and whether this
// call gave up waiting after forgeProbeBudget.
type forgeProbeResult struct {
	Err      error
	Known    bool
	TimedOut bool
}

// probeGitHubToken runs the App token probe under forgeProbeBudget. When the
// probe does not finish in time it returns the cached result of the last
// completed probe and marks the forge unreachable; the probe goroutine is not
// cancelled, so a slow-but-alive forge still refreshes the cache for the next
// beat.
func (s *Server) probeGitHubToken() forgeProbeResult {
	p := &s.forgeProbe
	p.mu.Lock()
	done := p.inFlight
	if done == nil {
		done = make(chan struct{})
		p.inFlight = done
		go s.runForgeProbe(healthTokenProbeFn, done)
	}
	p.mu.Unlock()

	timer := time.NewTimer(forgeProbeBudget)
	defer timer.Stop()
	timedOut := false
	select {
	case <-done:
	case <-timer.C:
		timedOut = true
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if timedOut {
		p.unreachable = true
	}
	return forgeProbeResult{Err: p.lastErr, Known: p.hasResult, TimedOut: timedOut}
}

func (s *Server) runForgeProbe(probeFn func(ctx context.Context) error, done chan struct{}) {
	ctx := context.Background()
	if s.deps != nil && s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	var err error
	if probeFn != nil {
		err = probeFn(ctx)
	} else {
		_, err = s.deps.GHAppAuth.Token(ctx)
	}

	p := &s.forgeProbe
	p.mu.Lock()
	p.lastErr = err
	p.hasResult = true
	p.unreachable = isForgeUnreachableErr(err)
	p.inFlight = nil
	close(done)
	p.mu.Unlock()
}

// isForgeUnreachableErr reports whether err means the forge could not be
// reached at all (dial/TLS/timeout), as opposed to the forge answering with an
// auth or permission error.
func isForgeUnreachableErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// ForgeUnreachable reports whether the last forge probe found the GitHub host
// unreachable from this hive (it timed out or failed at the transport layer).
// The hub heartbeat carries it as forge_unreachable so the hub can tell a dead
// forge route from a slow hive.
func (s *Server) ForgeUnreachable() bool {
	if s == nil {
		return false
	}
	s.forgeProbe.mu.Lock()
	defer s.forgeProbe.mu.Unlock()
	return s.forgeProbe.unreachable
}

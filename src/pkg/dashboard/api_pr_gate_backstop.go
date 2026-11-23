package dashboard

import (
	"context"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	// prGateBackstopInterval is how often the PR gate's backstop sweep looks
	// for open PR heads the webhook path missed.
	prGateBackstopInterval = 10 * time.Minute
	// prGateBackstopTimeout bounds one sweep (a page of open PRs per scoped
	// repository plus a bounded number of status reads and posts).
	prGateBackstopTimeout = 5 * time.Minute
)

// StartPRGateBackstop runs the PR gate's backstop sweep until ctx ends. With
// auto_merge.pr_gate off, or no repository in scope, a tick makes no request.
func (s *Server) StartPRGateBackstop(ctx context.Context) {
	go func() {
		t := time.NewTicker(prGateBackstopInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweepPRGate(ctx)
			}
		}
	}()
}

// prGateActiveScope returns the scoped repositories whose mode is not off.
func (s *Server) prGateActiveScope() []string {
	var active []string
	for _, repo := range s.prGateScope() {
		if s.prGateModeOf(repo) != config.PRGateModeOff {
			active = append(active, repo)
		}
	}
	return active
}

// sweepPRGate runs one backstop sweep over the active scope.
func (s *Server) sweepPRGate(ctx context.Context) {
	if s == nil || s.deps == nil || s.deps.Config == nil || s.deps.GHClient == nil {
		return
	}
	repos := s.prGateActiveScope()
	if len(repos) == 0 {
		return
	}
	g := s.prGateInstance()
	if g == nil {
		return
	}
	sweepCtx, cancel := context.WithTimeout(ctx, prGateBackstopTimeout)
	defer cancel()
	g.Sweep(sweepCtx, s.deps.GHClient, repos)
}

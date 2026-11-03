package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	cruxconfig "github.com/hivecommons/hive/crux/config"
	cruxruntime "github.com/hivecommons/hive/crux/runtime"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/fleetreport"
)

// Failure semantics for a sidecar runtime (hivecommons/hive#14138 stage 4,
// documented in docs/crux-serve.md):
//
//   - The governor keeps its loop; nothing in hive blocks on the socket.
//   - Every agent-dependent call fails with crux.ErrRuntimeUnavailable and
//     observer reads answer zero (crux/api/client). Kicks are REFUSED, not
//     queued: the governor re-derives what is due on its next tick, so a
//     replay buffer would only duplicate work after a long outage.
//   - The hive raises the crux_runtime_unavailable fleet-report condition on
//     the first failed probe and clears it on the first success; the
//     dashboard renders it like every other runtime condition and
//     /api/health carries crux_runtime.
//   - Under CRUX_SUPERVISE=child (the default) hive spawns `crux serve`
//     itself and restarts it with backoff; under external it only
//     reconnects.

// cruxRuntimeProbeInterval is how often the watcher checks the socket when
// no other call has done so.
const cruxRuntimeProbeInterval = 15 * time.Second

// cruxRuntimeConditionKey scopes the fleet-report condition; one runtime
// per hive.
const cruxRuntimeConditionKey = "sidecar"

// runtimeProber is the reachability surface of crux/api/client.Runtime,
// promoted through sidecarHost's embedded crux.Runtime.
type runtimeProber interface {
	Status() api.Status
	Client() *api.Client
}

// cruxRuntimeSupervision is the stage-4 wiring bootAgentsWith installs for
// a sidecar runtime: the optional child supervisor and the probe loop.
type cruxRuntimeSupervision struct {
	cfg        cruxconfig.RuntimeConfig
	prober     runtimeProber
	supervisor *cruxruntime.Supervisor
	logger     *slog.Logger
	interval   time.Duration
	observe    func(class, key, message, diagnostics string)
	clear      func(class, key string)
}

// newCruxRuntimeSupervision prepares supervision for rt under cfg. It
// returns nil for a runtime that is not a sidecar client (in-process has
// nothing to supervise).
func newCruxRuntimeSupervision(cfg cruxconfig.RuntimeConfig, rt crux.Runtime, logger *slog.Logger) *cruxRuntimeSupervision {
	if cfg.Mode != cruxconfig.ModeSidecar {
		return nil
	}
	p, ok := rt.(runtimeProber)
	if !ok {
		// sidecarHost embeds the client as an interface, so the probe
		// surface is not promoted; look through it.
		sh, isHost := rt.(*sidecarHost)
		if !isHost {
			return nil
		}
		if p, ok = sh.Runtime.(runtimeProber); !ok {
			return nil
		}
	}
	s := &cruxRuntimeSupervision{cfg: cfg, prober: p, logger: logger, interval: cruxRuntimeProbeInterval, observe: fleetreport.Observe, clear: fleetreport.Clear}
	if cfg.Supervise == cruxconfig.SuperviseChild {
		s.supervisor = &cruxruntime.Supervisor{Config: cfg, Logger: logger}
	}
	return s
}

// start runs the supervisor (when configured) and the probe loop until ctx
// ends. A missing binary under child supervision is loud but not fatal: the
// hive falls back to reconnect-only, exactly as external supervision.
func (s *cruxRuntimeSupervision) start(ctx context.Context) {
	if s.supervisor != nil {
		go func() {
			err := s.supervisor.Run(ctx)
			if errors.Is(err, cruxruntime.ErrBinaryNotFound) {
				s.logger.Error("crux serve child supervision disabled: binary not found; set "+cruxconfig.BinaryEnvVar+" or "+cruxconfig.SuperviseEnvVar+"=external",
					"binary", s.cfg.Binary, "error", err)
			} else if err != nil {
				s.logger.Error("crux serve supervisor stopped", "error", err)
			}
		}()
	}
	go s.watch(ctx)
}

// watch probes the runtime and keeps the fleet-report condition in step
// with the client's reachability.
func (s *cruxRuntimeSupervision) watch(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	down := false
	for {
		s.probe(ctx)
		down = s.reconcile(down)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// probe makes one health call so the client's Status reflects the socket
// even when no agent traffic has exercised it.
func (s *cruxRuntimeSupervision) probe(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, s.interval)
	defer cancel()
	_, _ = s.prober.Client().Health(pctx)
}

// reconcile raises or clears the condition on a transition and returns the
// new down state.
func (s *cruxRuntimeSupervision) reconcile(wasDown bool) bool {
	st := s.prober.Status()
	switch {
	case !st.Reachable && !wasDown:
		s.logger.Error("crux runtime unavailable: crux server unreachable; kicks refused until it returns",
			"server", s.cfg.Server, "supervise", s.cfg.Supervise, "error", st.LastError)
		s.observe(fleetreport.ClassCruxRuntimeUnavailable, cruxRuntimeConditionKey,
			"crux runtime unavailable: crux server unreachable at "+s.cfg.Server, "supervise="+string(s.cfg.Supervise)+" error="+st.LastError)
	case st.Reachable && wasDown:
		s.logger.Info("crux runtime reachable again", "server", s.cfg.Server, "outage", time.Since(st.Since).Round(time.Second))
		s.clear(fleetreport.ClassCruxRuntimeUnavailable, cruxRuntimeConditionKey)
	}
	return !st.Reachable
}

// health is the /api/health crux_runtime body.
func (s *cruxRuntimeSupervision) health() dashboard.CruxRuntimeHealth {
	st := s.prober.Status()
	h := dashboard.CruxRuntimeHealth{
		Mode:      string(s.cfg.Mode),
		Reachable: st.Reachable,
		LastError: st.LastError,
		Since:     st.Since,
		Supervise: string(s.cfg.Supervise),
	}
	if s.supervisor != nil {
		c := s.supervisor.State()
		h.Child = &dashboard.CruxRuntimeChild{Running: c.Running, Ready: c.Ready, PID: c.PID, Starts: c.Starts, LastExit: c.LastExit, Binary: c.Binary}
	}
	return h
}

// cruxRuntimeHealthFunc is the Dependencies.CruxRuntimeHealth hook: nil for
// in-process (always reachable, nothing to report), else the supervision's
// view.
func cruxRuntimeHealthFunc(s *cruxRuntimeSupervision) func() dashboard.CruxRuntimeHealth {
	if s == nil {
		return nil
	}
	return s.health
}

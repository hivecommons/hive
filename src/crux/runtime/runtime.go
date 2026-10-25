// Package runtime constructs a crux.Runtime for a host (hivecommons/hive#14138,
// stage 1 of the sidecar plan). It is the one place outside crux/agent that
// names *agent.Manager on the construction path: New picks the implementation
// from config.RuntimeConfig.Mode and returns it as the interface, so cmd/hive
// and the dashboard program against crux.Runtime whichever mode is selected.
//
// Two modes exist. ModeInProcess (the default) links crux/agent and returns
// the manager, exactly as cmd/hive constructed it before the seam existed; no
// behaviour changes. ModeSidecar (stage 3) returns a crux/api/client.Runtime
// that drives a `crux serve` process at RuntimeConfig.Server over the Crux
// API; the host does not link crux/agent's machinery for it.
//
// The package also re-exports the two process-level entry points cmd/hive
// uses that are not methods of a runtime instance: RunAgyTurn (the
// `hive agy-turn` subprocess) and StartPermissionsWatcher. Hive-provided
// capabilities (crux.AppTokenMinter, crux.AgentMintIssuer,
// crux.LinearCredential, crux.PRCreator and the rest of crux.Host) are
// installed after construction through HostOf, which only the in-process
// implementation satisfies; a sidecar host serves them back over the
// callback socket (crux/api/callback, stage 2).
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/api/client"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/watchdog"
)

// ErrModeUnavailable is returned by New for a mode that is recognised but
// has no implementation in this build. Every mode config.Mode accepts has
// one today; the sentinel stays so a host can keep distinguishing "unknown
// mode" from "not built here".
var ErrModeUnavailable = errors.New("crux runtime mode not yet available")

// ErrNotInProcess is returned by helpers that need the in-process manager
// (HostOf) or a runtime with a watchdog seam (NewWatchdogFleet) when given
// another implementation.
var ErrNotInProcess = errors.New("crux runtime is not the in-process implementation")

// Option configures New.
type Option func(*options)

type options struct {
	defs       map[string]config.AgentConfig
	logger     *slog.Logger
	project    crux.ProjectContext
	uidMapPath *string
}

// WithDefinitions supplies the agent definitions the runtime manages. The
// default is none; definitions can also be added later through
// crux.Definitions.
func WithDefinitions(defs map[string]config.AgentConfig) Option {
	return func(o *options) { o.defs = defs }
}

// WithLogger sets the runtime's logger. The default is slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// WithProject supplies the project facts (ACMM level, org, repos) the runtime
// gates launches on.
func WithProject(project crux.ProjectContext) Option {
	return func(o *options) { o.project = project }
}

// WithUIDMapPath points the in-process runtime at a specific persisted UID
// map. Production keeps the default; tests and the crux CLI inject a path.
func WithUIDMapPath(path string) Option {
	return func(o *options) { o.uidMapPath = &path }
}

// New validates cfg and returns the runtime its Mode selects. Options are
// applied in order; a nil option is ignored.
func New(cfg config.RuntimeConfig, opts ...Option) (crux.Runtime, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	o := options{logger: slog.Default()}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	switch cfg.Mode {
	case config.ModeInProcess:
		return newInProcess(o), nil
	case config.ModeSidecar:
		return newSidecar(cfg, o)
	}
	return nil, fmt.Errorf("%w: %q", ErrModeUnavailable, cfg.Mode)
}

// newSidecar is the sidecar implementation: a crux/api/client.Runtime over
// the `crux serve` process at cfg.Server. Nothing is dialed here; the first
// call does, so a host can construct the runtime before the sidecar is up
// and wait on it with client.Runtime.Ready.
func newSidecar(cfg config.RuntimeConfig, o options) (crux.Runtime, error) {
	rt, err := client.New(cfg.Server, cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("%s=%s: %w", config.ModeEnvVar, cfg.Mode, err)
	}
	rt.Logger = o.logger
	return rt, nil
}

// newInProcess is the in-process implementation: *agent.Manager itself,
// which satisfies crux.Runtime and crux.Host (asserted in crux/agent).
func newInProcess(o options) crux.Runtime {
	var mopts []agent.ManagerOption
	if o.uidMapPath != nil {
		mopts = append(mopts, agent.WithUIDMapPath(*o.uidMapPath))
	}
	return agent.AsRuntime(agent.NewManagerWithOptions(o.defs, o.logger, o.project, mopts...))
}

// HostOf returns rt's crux.Host when rt is the in-process implementation.
// A sidecar runtime has no Host: its capabilities arrive over the API
// (stage 2 and 3), so callers treat ok == false as "nothing to wire".
func HostOf(rt crux.Runtime) (crux.Host, bool) {
	h, ok := rt.(crux.Host)
	return h, ok && h != nil
}

// WatchdogHooks are the governor-side facts the fleet watchdog needs and the
// runtime cannot know: queue depth, deliberate kick suppression and the
// agent's cadence period. A nil hook means "unknown" to the reconciler.
type WatchdogHooks struct {
	Queued         func() (int, bool)
	KickSuppressed func(name string) string
	CadencePeriod  func(name string) (time.Duration, bool)
}

// WatchdogSourcer is the seam a runtime exposes for the fleet watchdog:
// the in-process manager answers from its agent table, the sidecar client
// from the server's watchdog routes.
type WatchdogSourcer interface {
	WatchdogSource() watchdog.Source
}

// NewWatchdogFleet adapts rt to watchdog.Fleet for the self-healing
// reconciler (RFC #4665). The in-process manager gets agent.WatchdogFleet,
// which reads the agent table under the manager's lock; any other runtime
// with a WatchdogSource (the sidecar client) gets a fleet composed of that
// source and the runtime's Lifecycle. A runtime with neither returns
// ErrNotInProcess.
func NewWatchdogFleet(rt crux.Runtime, hooks WatchdogHooks) (watchdog.Fleet, error) {
	if m, ok := rt.(*agent.Manager); ok && m != nil {
		return agent.WatchdogFleet{
			M:              m,
			Queued:         hooks.Queued,
			KickSuppressed: hooks.KickSuppressed,
			CadencePeriod:  hooks.CadencePeriod,
		}, nil
	}
	ws, ok := rt.(WatchdogSourcer)
	if !ok || rt == nil {
		return nil, ErrNotInProcess
	}
	src := ws.WatchdogSource()
	if src == nil {
		return nil, ErrNotInProcess
	}
	return sourceFleet{Source: src, rt: rt, hooks: hooks}, nil
}

// sourceFleet is watchdog.Fleet over a watchdog.Source plus the runtime's
// Lifecycle, for runtimes that are not the in-process manager.
type sourceFleet struct {
	watchdog.Source
	rt    crux.Runtime
	hooks WatchdogHooks
}

func (f sourceFleet) IsPaused(name string) bool { return f.rt.IsPaused(name) }

func (f sourceFleet) Restart(ctx context.Context, name string) error {
	return f.rt.Restart(ctx, name)
}

func (f sourceFleet) Pause(name, trigger, reason string) error {
	return f.rt.Pause(name, trigger, reason)
}

// QueuedWork mirrors agent.WatchdogFleet: advisory-tier agents report an
// empty queue because they cannot drain it, read off the agent's config.
func (f sourceFleet) QueuedWork(name string) (int, bool) {
	if f.hooks.Queued == nil {
		return 0, false
	}
	p, err := f.rt.GetStatusFast(name)
	if err != nil || p == nil {
		return 0, false
	}
	if advisoryOnly(p.Config) {
		return 0, true
	}
	return f.hooks.Queued()
}

func (f sourceFleet) KickSuppressedReason(name string) string {
	if f.hooks.KickSuppressed == nil {
		return ""
	}
	return f.hooks.KickSuppressed(name)
}

func (f sourceFleet) KickPeriod(name string) (time.Duration, bool) {
	if f.hooks.CadencePeriod == nil {
		return 0, false
	}
	return f.hooks.CadencePeriod(name)
}

func advisoryOnly(cfg config.AgentConfig) bool {
	const advisoryMode = "ADVISORY"
	if strings.EqualFold(strings.TrimSpace(cfg.Mode), advisoryMode) {
		return true
	}
	return cfg.Tools.EffectiveMode() == advisoryMode
}

// RunAgyTurn implements the `hive agy-turn` subprocess: one headless agy
// turn, exit code returned. It is agent.RunAgyTurn.
func RunAgyTurn(args []string, stdout, stderr io.Writer) int {
	return agent.RunAgyTurn(args, stdout, stderr)
}

// StartPermissionsWatcher runs the agent home-directory permissions watcher
// for the life of the process. It is agent.StartPermissionsWatcher; the
// host starts it as a goroutine.
func StartPermissionsWatcher(logger *slog.Logger) {
	agent.StartPermissionsWatcher(logger)
}

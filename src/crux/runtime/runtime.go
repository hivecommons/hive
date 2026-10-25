// Package runtime constructs a crux.Runtime for a host (hivecommons/hive#14138,
// stage 1 of the sidecar plan). It is the one place outside crux/agent that
// names *agent.Manager on the construction path: New picks the implementation
// from config.RuntimeConfig.Mode and returns it as the interface, so cmd/hive
// and the dashboard program against crux.Runtime whichever mode is selected.
//
// Two modes exist. ModeInProcess (the default) links crux/agent and returns
// the manager, exactly as cmd/hive constructed it before the seam existed; no
// behaviour changes. ModeSidecar will drive a `crux serve` process over the
// Crux API; until stage 3 ships that client, New returns ErrModeUnavailable
// for it so a misconfigured host fails at boot rather than silently running
// in-process.
//
// The package also re-exports the two process-level entry points cmd/hive
// uses that are not methods of a runtime instance: RunAgyTurn (the
// `hive agy-turn` subprocess) and StartPermissionsWatcher. Hive-provided
// capabilities (crux.AppTokenMinter, crux.AgentMintIssuer,
// crux.LinearCredential, crux.PRCreator and the rest of crux.Host) are
// installed after construction through HostOf, which only the in-process
// implementation satisfies.
package runtime

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/watchdog"
)

// ErrModeUnavailable is returned by New for a mode that is recognised but
// has no implementation in this build. Today that is config.ModeSidecar.
var ErrModeUnavailable = errors.New("crux runtime mode not yet available")

// ErrNotInProcess is returned by helpers that need the in-process manager
// (HostOf, NewWatchdogFleet) when given another implementation.
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
		return nil, fmt.Errorf("%w: %s (%s) is stage 3 of hivecommons/hive#14138; run with %s=%s or unset", ErrModeUnavailable, cfg.Mode, cfg.Server, config.ModeEnvVar, config.ModeInProcess)
	}
	return nil, fmt.Errorf("%w: %q", ErrModeUnavailable, cfg.Mode)
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

// NewWatchdogFleet adapts rt to watchdog.Fleet for the self-healing
// reconciler (RFC #4665). It needs the in-process manager because the fleet
// reads the manager's agent table under its lock; a sidecar fleet is a stage
// 3 endpoint family and returns ErrNotInProcess until then.
func NewWatchdogFleet(rt crux.Runtime, hooks WatchdogHooks) (watchdog.Fleet, error) {
	m, ok := rt.(*agent.Manager)
	if !ok || m == nil {
		return nil, ErrNotInProcess
	}
	return agent.WatchdogFleet{
		M:              m,
		Queued:         hooks.Queued,
		KickSuppressed: hooks.KickSuppressed,
		CadencePeriod:  hooks.CadencePeriod,
	}, nil
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

// Package cruxtest builds real Crux runtimes and process output buffers for
// consumer tests without coupling those tests to crux/agent. Constructors do
// not start agents; calling lifecycle methods still has real runtime effects.
package cruxtest

import (
	"context"
	"log/slog"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/config"
)

// Runtime adds fixture controls to the production runtime surface. Consumer
// tests can configure kick logs, inject dispatch results and observe routing
// callbacks without obtaining the concrete manager. Keep these controls here,
// rather than widening crux.Runtime with test-only setup methods.
type Runtime interface {
	crux.Runtime
	// Host is the in-process wiring surface cmd/hive installs after
	// construction; consumer tests that exercise a boot path need it too.
	crux.Host
	crux.ActivityObserver
	RecordKickDispatchForTest(crux.KickDispatch)
	SetKickLogDir(dir string)
	SetKickLogSourceResolver(func() string)
	KickLogSource() string
	SetInferenceCallbacks(setRoute func(name, backend, model string), clearRoute func(name string))
	SetSandboxConfig(config.AgentSandboxConfig)

	// Login-scanner tests pass the runtime through their credential interface.
	AgentHasValidCredential(name string) bool
	RefreshAgentTokenFor(ctx context.Context, name string) error
}

// Option configures a test runtime's construction.
type Option func(*options)

type options struct {
	uidMapPath *string
}

// WithUIDMapPath selects the UID map used by the fixture. A path under the
// test's temporary directory keeps host UID assignments out of the fixture.
func WithUIDMapPath(path string) Option {
	return func(opts *options) { opts.uidMapPath = &path }
}

// NewRuntime constructs the same stopped agents as the production manager.
// Project facts and logger are supplied by the caller, preserving the test's
// ACMM gates and logging. With no options, production constructor defaults apply.
func NewRuntime(defs map[string]config.AgentConfig, logger *slog.Logger, project crux.ProjectContext, opts ...Option) Runtime {
	var cfg options
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	var managerOpts []agent.ManagerOption
	if cfg.uidMapPath != nil {
		managerOpts = append(managerOpts, agent.WithUIDMapPath(*cfg.uidMapPath))
	}
	return agent.NewManagerWithOptions(defs, logger, project, managerOpts...)
}

// NewRingBuffer constructs output for a crux.Process fixture. It is
// crux.NewRingBuffer; it stays here so existing fixtures keep one import.
func NewRingBuffer(capacity int) *crux.RingBuffer {
	return crux.NewRingBuffer(capacity)
}

// NewUIDMap constructs a persisted UID-map fixture, including the runtime's
// default base and proxy UIDs. Tests can save it under a temporary directory
// and pass that path to WithUIDMapPath.
func NewUIDMap() *agent.UIDMap {
	return agent.NewUIDMap()
}

package main

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/outputschema"
)

// External-execution wiring (#8361). pkg/dashboard owns the extwork-free
// seams (lease authority, hub accessor, ExternalExecution status); this file
// binds the concrete pkg/extwork pieces to them. Each adapter is registered
// only under its build tag (extwork_flue.go, extwork_omp.go), so without the
// tag the registry lacks that engine and its constructor here fails closed
// with extwork.ErrEngineNotLinked.

// extExecEngineFlue and extExecEngineOMP mirror dashboard's private engine
// names; the adapter packages cannot be imported here without linking them
// into every build.
const (
	extExecEngineFlue = "flue"
	extExecEngineOMP  = "omp"
)

var (
	errExternalBindingOff    = errors.New("external execution binding is off")
	errExternalHubNotRunning = errors.New("external execution binding needs the contributor hub")
)

// extworkStatus implements dashboard.ExternalExecution over the registry.
type extworkStatus struct{ registry *extwork.Registry }

// Linked reports whether the engine is compiled into this binary.
func (s extworkStatus) Linked(engine string) bool {
	if s.registry == nil {
		return false
	}
	return s.registry.Linked(engine)
}

// hubLeaseAuthority adapts the contributor hub to extwork.LeaseAuthority.
type hubLeaseAuthority struct {
	hub *dashboard.ContributeWSHub
	now func() time.Time
}

func (a hubLeaseAuthority) Verify(identity, taskID, workKey, tier, stage string, gen uint64) error {
	return a.hub.VerifyLeaseAuthority(identity, taskID, workKey, tier, stage, gen, a.now())
}

func (a hubLeaseAuthority) Flush(identity, taskID, workKey, tier, stage string, gen uint64) error {
	return a.hub.FlushLeaseAuthority(identity, taskID, workKey, tier, stage, gen, a.now())
}

// extworkRecordDir is where admission records and verified receipts live.
func extworkRecordDir() string {
	return filepath.Join(outputschema.AgentReportDir, dashboard.ExtworkRecordDirName)
}

// newExternalFlueBinding builds the Flue binding for this hub. It fails
// closed: off by configuration, an engine that is not linked into this build,
// or no contributor hub each return a typed error and construct nothing.
func newExternalFlueBinding(srv *dashboard.Server, cfg *config.Config, registry *extwork.Registry, now func() time.Time) (*extwork.Binding, error) {
	mode := cfg.FlueBindingMode()
	if mode == config.FlueBindingModeOff {
		return nil, errExternalBindingOff
	}
	if now == nil {
		now = time.Now
	}
	flueCfg := cfg.Runs.External.Flue
	adapter, err := registry.Open(extExecEngineFlue, map[string]string{
		extwork.SettingEndpoint:        flueCfg.Endpoint,
		extwork.SettingWorkflowVersion: flueCfg.WorkflowVersion,
	})
	if err != nil {
		return nil, err
	}
	hub := srv.ContributeHub()
	if hub == nil {
		return nil, errExternalHubNotRunning
	}
	store := extwork.NewLeaseStore(hubLeaseAuthority{hub: hub, now: now}, extworkRecordDir())
	return extwork.New(adapter, store, store, extwork.NewAuditProgressSink(srv.AgentAuditSink()), mode), nil
}

// newExternalOMPBinding builds the OMP workbench host binding for this hub
// (#8361 step 9). It fails closed exactly like the Flue one: off by
// configuration, an engine that is not linked into this build, or no
// contributor hub each return a typed error and construct nothing. The
// adapter resolves workbench peers from omp.DefaultBroker, which the hub
// populates from contributor connections that declared ext-exec/omp.
func newExternalOMPBinding(srv *dashboard.Server, cfg *config.Config, registry *extwork.Registry, now func() time.Time) (*extwork.Binding, error) {
	mode := cfg.OMPBindingMode()
	if mode == config.FlueBindingModeOff {
		return nil, errExternalBindingOff
	}
	if now == nil {
		now = time.Now
	}
	adapter, err := registry.Open(extExecEngineOMP, map[string]string{
		extwork.SettingWorkflowVersion: cfg.Runs.External.OMP.WorkflowVersion,
	})
	if err != nil {
		return nil, err
	}
	hub := srv.ContributeHub()
	if hub == nil {
		return nil, errExternalHubNotRunning
	}
	store := extwork.NewLeaseStore(hubLeaseAuthority{hub: hub, now: now}, extworkRecordDir())
	return extwork.New(adapter, store, store, extwork.NewAuditProgressSink(srv.AgentAuditSink()), mode), nil
}

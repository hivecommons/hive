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
// binds the concrete pkg/extwork pieces to them. The Flue adapter itself is
// registered only under the extwork_flue build tag (extwork_flue.go), so
// without the tag the registry is empty and every constructor here fails
// closed with extwork.ErrEngineNotLinked.

// extExecEngineFlue mirrors dashboard's private engine name; pkg/extwork/flue
// cannot be imported here without linking the adapter into every build.
const extExecEngineFlue = "flue"

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

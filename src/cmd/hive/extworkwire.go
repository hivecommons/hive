package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
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
type extworkStatus struct {
	registry *extwork.Registry
	srv      *dashboard.Server
	cfg      *config.Config
	now      func() time.Time
	mu       sync.Mutex
	bindings map[string]*externalExecutionBinding
}

type externalExecutionBinding struct {
	engine  string
	config  string
	binding *extwork.Binding
	adapter extwork.Adapter
}

// Linked reports whether the engine is compiled into this binary.
func (s *extworkStatus) Linked(engine string) bool {
	if s == nil || s.registry == nil {
		return false
	}
	return s.registry.Linked(engine)
}

// Dispatch builds the durable admission and bounded payload for an assigned
// external task, then invokes the engine binding. Peer-backed adapters that
// expose a Host step (OMP) receive the offer before context is delivered.
func (s *extworkStatus) Dispatch(ctx context.Context, task dashboard.ExternalExecutionTask) error {
	if s == nil {
		return errExternalBindingOff
	}
	b, err := s.bindingFor(task.Engine)
	if err != nil {
		return err
	}
	adm := extwork.Admission{
		WorkKey:          task.WorkKey,
		AssignmentID:     task.TaskID,
		Generation:       task.TaskGen,
		Stage:            task.Stage,
		ContractRevision: task.ContractRevision,
		Engine:           task.Engine,
		WorkflowVersion:  task.WorkflowVersion,
		InputRevision:    task.InputRevision,
		Authority: extwork.AuthorityBinding{
			Identity:   task.Identity,
			Tier:       task.Tier,
			Capability: task.Capability,
			Mode:       task.Mode,
		},
	}
	payload, err := buildExternalPayload(adm, task)
	if err != nil {
		return err
	}
	adm.RequestDigest = extwork.RequestDigest(payload)
	if hoster, ok := b.adapter.(interface {
		Host(string, extwork.Admission) extwork.Host
	}); ok {
		if _, err := b.binding.Offer(ctx, hoster.Host(task.Identity, adm), adm, task.Summary); err != nil {
			return err
		}
	}
	_, err = b.binding.Dispatch(ctx, adm, payload)
	return err
}

func (s *extworkStatus) bindingFor(engine string) (*externalExecutionBinding, error) {
	if s.registry == nil || s.cfg == nil || s.srv == nil {
		return nil, errExternalBindingOff
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindings == nil {
		s.bindings = map[string]*externalExecutionBinding{}
	}
	configKey := configForEngine(s.cfg, engine)
	if b := s.bindings[engine]; b != nil && b.config == configKey {
		return b, nil
	}
	var (
		b   *externalExecutionBinding
		err error
	)
	switch engine {
	case extExecEngineFlue:
		b, err = newExternalFlueExecutionBinding(s.srv, s.cfg, s.registry, s.now)
	case extExecEngineOMP:
		b, err = newExternalOMPExecutionBinding(s.srv, s.cfg, s.registry, s.now)
	default:
		err = fmt.Errorf("%w: %s", extwork.ErrEngineNotLinked, engine)
	}
	if err != nil {
		return nil, err
	}
	b.config = configKey
	s.bindings[engine] = b
	return b, nil
}

func (s *extworkStatus) AttachPeer(ctx context.Context, peer dashboard.ExternalExecutionPeer) error {
	return attachExternalOMPPeer(ctx, peer)
}

func configForEngine(cfg *config.Config, engine string) string {
	if cfg == nil {
		return ""
	}
	switch engine {
	case extExecEngineFlue:
		return strings.Join([]string{cfg.FlueBindingMode(), cfg.Runs.External.Flue.Endpoint, cfg.Runs.External.Flue.WorkflowVersion}, "\x00")
	case extExecEngineOMP:
		return strings.Join([]string{cfg.OMPBindingMode(), cfg.Runs.External.OMP.WorkflowVersion}, "\x00")
	default:
		return ""
	}
}

func buildExternalPayload(adm extwork.Admission, task dashboard.ExternalExecutionTask) ([]byte, error) {
	type bundleAdmission struct {
		WorkKey          string `json:"work_key"`
		AssignmentID     string `json:"assignment_id"`
		Generation       uint64 `json:"generation"`
		Stage            string `json:"stage"`
		ContractRevision string `json:"contract_revision"`
		InputRevision    string `json:"input_revision"`
	}
	type bundle struct {
		Admission bundleAdmission   `json:"admission"`
		Summary   string            `json:"summary"`
		Repo      string            `json:"repo"`
		Hints     map[string]string `json:"hints,omitempty"`
	}
	hints := map[string]string{}
	if task.SourceType != "" {
		hints["source_type"] = task.SourceType
	}
	if task.ExternalID != "" {
		hints["external_id"] = task.ExternalID
	}
	if task.URL != "" {
		hints["url"] = task.URL
	}
	if task.Number > 0 {
		hints["number"] = fmt.Sprintf("%d", task.Number)
	}
	if len(hints) == 0 {
		hints = nil
	}
	return json.Marshal(bundle{
		Admission: bundleAdmission{
			WorkKey:          adm.WorkKey,
			AssignmentID:     adm.AssignmentID,
			Generation:       adm.Generation,
			Stage:            adm.Stage,
			ContractRevision: adm.ContractRevision,
			InputRevision:    adm.InputRevision,
		},
		Summary: task.Summary,
		Repo:    task.Repo,
		Hints:   hints,
	})
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
	b, err := newExternalFlueExecutionBinding(srv, cfg, registry, now)
	if err != nil {
		return nil, err
	}
	return b.binding, nil
}

func newExternalFlueExecutionBinding(srv *dashboard.Server, cfg *config.Config, registry *extwork.Registry, now func() time.Time) (*externalExecutionBinding, error) {
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
	return &externalExecutionBinding{engine: extExecEngineFlue, adapter: adapter, binding: extwork.New(adapter, store, store, extwork.NewAuditProgressSink(srv.AgentAuditSink()), mode)}, nil
}

// newExternalOMPBinding builds the OMP workbench host binding for this hub
// (#8361 step 9). It fails closed exactly like the Flue one: off by
// configuration, an engine that is not linked into this build, or no
// contributor hub each return a typed error and construct nothing. The
// adapter resolves workbench peers from omp.DefaultBroker, which the hub
// populates from contributor connections that declared ext-exec/omp.
func newExternalOMPBinding(srv *dashboard.Server, cfg *config.Config, registry *extwork.Registry, now func() time.Time) (*extwork.Binding, error) {
	b, err := newExternalOMPExecutionBinding(srv, cfg, registry, now)
	if err != nil {
		return nil, err
	}
	return b.binding, nil
}

func newExternalOMPExecutionBinding(srv *dashboard.Server, cfg *config.Config, registry *extwork.Registry, now func() time.Time) (*externalExecutionBinding, error) {
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
	return &externalExecutionBinding{engine: extExecEngineOMP, adapter: adapter, binding: extwork.New(adapter, store, store, extwork.NewAuditProgressSink(srv.AgentAuditSink()), mode)}, nil
}

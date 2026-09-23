package dashboard

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/outputschema"
)

// External-execution binding seams (#8361, #8201 Gate 1).
//
// The dashboard owns three things here and nothing more: the durable
// admission record is the task lease (plus the fields the lease cannot hold,
// kept beside the receipt under the agent report directory); progress events
// land on the agent audit trail; and the binding is constructed only when the
// operator enabled it AND the engine was compiled in. No route here starts an
// external run; the assignment path decides that separately.

const (
	// extExecEngineFlue is the only engine the pilot admits. The dashboard
	// spells it as a string on purpose: importing pkg/extwork/flue here would
	// link the adapter into every build, which the extwork guard forbids.
	extExecEngineFlue = "flue"
	// capExtExecFlue is the contributor capability a relay must declare to be
	// offered Flue-bound work. It must equal flue.Capability; a dashboard test
	// pins the two together.
	capExtExecFlue = "ext-exec/flue"
	// extworkRecordDirName is the subdirectory of the agent report directory
	// that holds admission records and verified receipts.
	extworkRecordDirName = "extwork"
)

// extworkRegistry is the engine registry the dashboard consults. Tests swap it
// for a registry holding a fake engine.
var extworkRegistry = extwork.DefaultRegistry

var (
	errExternalBindingOff    = errors.New("external execution binding is off")
	errExternalHubNotRunning = errors.New("external execution binding needs the contributor hub")
	errLeaseAdmission        = errors.New("extwork: admission does not match a live lease")
)

// leaseAdmissionStore backs extwork.AdmissionStore and extwork.ReceiptStore.
// The task lease is the authority record: Persist refuses an admission that
// does not name a live lease with the same work key, generation, stage, and
// tier held by the same identity, and forces that lease to disk before the
// dispatch may proceed (#8287/#8322 make that write fail loudly). The record
// file beside the receipt carries only what the lease cannot: contract and
// input revisions, request digest, engine version, pinned incarnation. Load
// re-checks the lease, so a record whose lease expired or was revoked grants
// nothing on recovery.
type leaseAdmissionStore struct {
	hub   *ContributeWSHub
	files *extwork.FileStore
	now   func() time.Time
}

func newLeaseAdmissionStore(hub *ContributeWSHub, dir string, now func() time.Time) *leaseAdmissionStore {
	if now == nil {
		now = time.Now
	}
	return &leaseAdmissionStore{hub: hub, files: extwork.NewFileStore(dir), now: now}
}

func (s *leaseAdmissionStore) leaseMatchesLocked(adm extwork.Admission) error {
	l := s.hub.leaseForLocked(adm.Authority.Identity, adm.AssignmentID)
	if l == nil {
		return fmt.Errorf("%w: %s holds no lease for %s", errLeaseAdmission, adm.Authority.Identity, adm.AssignmentID)
	}
	if l.expiresAt.IsZero() || s.now().After(l.expiresAt) {
		return fmt.Errorf("%w: lease for %s expired", errLeaseAdmission, adm.AssignmentID)
	}
	if l.key != adm.WorkKey || l.gen != adm.Generation || l.stage != adm.Stage || l.tier != adm.Authority.Tier {
		return fmt.Errorf("%w: lease is %s gen %d stage %q tier %q, admission is %s gen %d stage %q tier %q",
			errLeaseAdmission, l.key, l.gen, l.stage, l.tier, adm.WorkKey, adm.Generation, adm.Stage, adm.Authority.Tier)
	}
	return nil
}

// Persist implements extwork.AdmissionStore.
func (s *leaseAdmissionStore) Persist(adm extwork.Admission) error {
	if err := adm.Validate(); err != nil {
		return err
	}
	s.hub.leaseMu.Lock()
	if err := s.leaseMatchesLocked(adm); err != nil {
		s.hub.leaseMu.Unlock()
		return err
	}
	if err := s.hub.saveLeasesLocked(); err != nil {
		s.hub.leaseMu.Unlock()
		return fmt.Errorf("lease not durable: %w", err)
	}
	s.hub.leaseMu.Unlock()
	return s.files.Persist(adm)
}

// Load implements extwork.AdmissionStore. A record without a matching live
// lease is reported as absent: no lease, no authority.
func (s *leaseAdmissionStore) Load(assignmentID string) (extwork.Admission, bool, error) {
	adm, ok, err := s.files.Load(assignmentID)
	if err != nil || !ok {
		return extwork.Admission{}, false, err
	}
	s.hub.leaseMu.Lock()
	err = s.leaseMatchesLocked(adm)
	s.hub.leaseMu.Unlock()
	if err != nil {
		return extwork.Admission{}, false, nil
	}
	return adm, true, nil
}

// SaveReceipt implements extwork.ReceiptStore.
func (s *leaseAdmissionStore) SaveReceipt(assignmentID string, raw []byte) error {
	return s.files.SaveReceipt(assignmentID, raw)
}

// LoadReceipt implements extwork.ReceiptStore.
func (s *leaseAdmissionStore) LoadReceipt(assignmentID string) ([]byte, bool, error) {
	return s.files.LoadReceipt(assignmentID)
}

// auditProgressSink writes extwork progress events to the agent audit trail
// as system actions keyed by assignment id, which is the lease's task id.
type auditProgressSink struct {
	sink agent.AuditSink
}

// Record implements extwork.ProgressSink.
func (a auditProgressSink) Record(ev extwork.ProgressEvent) {
	if a.sink == nil {
		return
	}
	fields := agent.Fields("execution_key", string(ev.ExecutionKey))
	if ev.State != "" {
		fields["state"] = string(ev.State)
	}
	for k, v := range ev.Fields {
		fields[k] = v
	}
	a.sink.Record("system", ev.Action, ev.AssignmentID, fields)
}

// extworkRecordDir is where admission records and verified receipts live.
func extworkRecordDir() string {
	return filepath.Join(outputschema.AgentReportDir, extworkRecordDirName)
}

// externalFlueBinding builds the Flue binding for this hub. It fails closed:
// off by configuration, no contributor hub, or an engine that is not linked
// into this build each return an error and construct nothing.
func (s *Server) externalFlueBinding(now func() time.Time) (*extwork.Binding, error) {
	var cfg *config.Config
	if s != nil && s.deps != nil {
		cfg = s.deps.Config
	}
	mode := cfg.FlueBindingMode()
	if mode == config.FlueBindingModeOff {
		return nil, errExternalBindingOff
	}
	if s.contributeHub == nil {
		return nil, errExternalHubNotRunning
	}
	flueCfg := cfg.Runs.External.Flue
	adapter, err := extworkRegistry.Open(extExecEngineFlue, map[string]string{
		extwork.SettingEndpoint:        flueCfg.Endpoint,
		extwork.SettingWorkflowVersion: flueCfg.WorkflowVersion,
	})
	if err != nil {
		return nil, err
	}
	store := newLeaseAdmissionStore(s.contributeHub, extworkRecordDir(), now)
	return extwork.New(adapter, store, store, auditProgressSink{sink: s.AgentAuditSink()}, mode), nil
}

// extExecEngineFromIssueMap reads the external engine an item asks for. Only
// run-stage items carry it; a plain issue never does.
func extExecEngineFromIssueMap(issue map[string]any) string {
	for _, key := range []string{"ext_exec", "external_engine"} {
		if raw, ok := issue[key].(string); ok {
			if engine := strings.TrimSpace(raw); engine != "" {
				return engine
			}
		}
	}
	return ""
}

// extExecAdmissible decides whether an item bound to an external engine may be
// offered to this relay. It is refuse-only, never downgrade: an item that asks
// for an engine is skipped unless the engine is the piloted one, the operator
// enabled the binding, and the relay declared the capability. The item is
// never handed out as ordinary local work.
func (h *ContributeWSHub) extExecAdmissible(engine string, c *ContributorConnection) (bool, string) {
	if engine != extExecEngineFlue {
		return false, "unsupported external engine"
	}
	if h == nil || h.server == nil || h.server.deps == nil || !h.server.deps.Config.FlueBindingEnabled() {
		return false, "external binding disabled"
	}
	if c == nil || c.capabilities == nil || !c.capabilities.DeclaresCapability(capExtExecFlue) {
		return false, "relay lacks capability " + capExtExecFlue
	}
	return true, ""
}

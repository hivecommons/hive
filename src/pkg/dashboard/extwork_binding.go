package dashboard

import (
	"fmt"
	"strings"
	"time"
)

// External-execution seams (#8361, #8201 Gate 1).
//
// pkg/dashboard deliberately imports neither pkg/extwork nor pkg/extwork/flue:
// the dashboard is at its internal-import ceiling and the adapter must stay
// out of every build that lacks the extwork_flue tag. What lives here is the
// extwork-free half of the contract: the relay capability token, the
// refuse-never-downgrade rule for engine-bound items, the lease as the
// admission and authority record (LeaseAuthority), the hub accessor, and the
// ExternalExecution status seam the Features panel reads. cmd/hive wires the
// concrete extwork pieces to these (extworkwire.go).

const (
	// extExecEngineFlue is the only engine the pilot admits. It must equal
	// flue.Engine; a test pins the two together.
	extExecEngineFlue = "flue"
	// capExtExecFlue is the contributor capability a relay must declare to be
	// offered Flue-bound work. It must equal flue.Capability; a test pins the
	// two together.
	capExtExecFlue = "ext-exec/flue"
	// ExtworkRecordDirName is the subdirectory of the agent report directory
	// that holds admission records and verified receipts.
	ExtworkRecordDirName = "extwork"
)

// ExternalExecution is the status seam the dashboard reads about external
// engines. cmd/hive backs it with the extwork registry; nil means no engine is
// compiled into this build.
type ExternalExecution interface {
	// Linked reports whether the named engine is compiled into this binary.
	Linked(engine string) bool
}

// externalExecLinked is nil-safe: no seam, nothing linked.
func (s *Server) externalExecLinked(engine string) bool {
	if s == nil || s.deps == nil || s.deps.ExternalExec == nil {
		return false
	}
	return s.deps.ExternalExec.Linked(engine)
}

// ContributeHub returns the contributor WebSocket hub, or nil before the
// contribute routes are registered.
func (s *Server) ContributeHub() *ContributeWSHub {
	if s == nil {
		return nil
	}
	return s.contributeHub
}

// errLeaseAuthority is the refusal a lease-authority check returns.
var errLeaseAuthority = fmt.Errorf("no live lease matches the admission")

func (h *ContributeWSHub) leaseAuthorityLocked(identity, taskID, workKey, tier, stage string, gen uint64, now time.Time) error {
	l := h.leaseForLocked(identity, taskID)
	if l == nil {
		return fmt.Errorf("%w: %s holds no lease for %s", errLeaseAuthority, identity, taskID)
	}
	if l.expiresAt.IsZero() || now.After(l.expiresAt) {
		return fmt.Errorf("%w: lease for %s expired", errLeaseAuthority, taskID)
	}
	if l.key != workKey || l.gen != gen || l.stage != stage || l.tier != tier {
		return fmt.Errorf("%w: lease is %s gen %d stage %q tier %q, admission is %s gen %d stage %q tier %q",
			errLeaseAuthority, l.key, l.gen, l.stage, l.tier, workKey, gen, stage, tier)
	}
	return nil
}

// VerifyLeaseAuthority reports nil only when a live lease held by identity for
// taskID carries exactly this work key, tier, stage, and generation. It is the
// extwork.LeaseAuthority Verify half.
func (h *ContributeWSHub) VerifyLeaseAuthority(identity, taskID, workKey, tier, stage string, gen uint64, now time.Time) error {
	if h == nil {
		return fmt.Errorf("%w: no contributor hub", errLeaseAuthority)
	}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	return h.leaseAuthorityLocked(identity, taskID, workKey, tier, stage, gen, now)
}

// FlushLeaseAuthority verifies the lease and forces the registry to disk, so a
// caller may treat the lease as durable-before-dispatch (#8287/#8322). It is
// the extwork.LeaseAuthority Flush half.
func (h *ContributeWSHub) FlushLeaseAuthority(identity, taskID, workKey, tier, stage string, gen uint64, now time.Time) error {
	if h == nil {
		return fmt.Errorf("%w: no contributor hub", errLeaseAuthority)
	}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	if err := h.leaseAuthorityLocked(identity, taskID, workKey, tier, stage, gen, now); err != nil {
		return err
	}
	if err := h.saveLeasesLocked(); err != nil {
		return fmt.Errorf("lease not durable: %w", err)
	}
	return nil
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

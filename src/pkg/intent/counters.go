package intent

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Machine-readable would-block reason codes keyed in the intent counters
// (#12362). They mirror the Reason* prose so a counter line never carries the
// free text (or an alignment rationale).
const (
	ReasonCodeTier1NeedsIssue       = "tier1_needs_issue"
	ReasonCodeTier2NeedsPlan        = "tier2_needs_plan"
	ReasonCodeTier3NeedsApproval    = "tier3_needs_approval"
	ReasonCodeAlignmentMisaligned   = "alignment_misaligned"
	ReasonCodeEvidenceUnavailable   = "evidence_unavailable"
	ReasonCodeUnknown               = "unknown"
	reasonEvidenceUnavailablePrefix = "intent evidence unavailable"
)

// EvalCounts is a snapshot of intent evaluations seen since the process
// started. Attempted is every evaluation; Blocked is the subset the gate
// would have withheld with intent.enforce on (Verdict.BlocksMerge(true)),
// whether or not enforcement was actually on. The field names match the
// proxy's WriteCounts so the dashboard reads both files with one shape.
// Observation only: nothing here reads into or alters a merge decision.
type EvalCounts struct {
	Attempted int64 `json:"attempted"`
	Blocked   int64 `json:"blocked"`
	// BlockedByReason is keyed by the ReasonCode* value.
	BlockedByReason map[string]int64 `json:"blocked_by_reason"`
}

// Observation is one counted intent evaluation, as handed to the audit sink.
type Observation struct {
	Lane       string
	Repo       string
	Number     int
	Tier       string
	WouldBlock bool
	ReasonCode string
	Enforced   bool
}

// Counters is zero-value usable; recording never affects gating.
type Counters struct {
	mu    sync.Mutex
	tiers map[string]*EvalCounts
	// pending holds counts not yet merged into the durable file.
	pending DurableCounts
	now     func() time.Time
	audit   func(Observation)
}

// TierKey is the counter key for t ("tier0".."tier3"), matching the
// evidence bundle's shadow_intent.tier.
func TierKey(t Tier) string { return fmt.Sprintf("tier%d", t) }

// WouldBlockReasonCode returns the ReasonCode* the counters key v under when
// it would block, or "" when it would not.
func WouldBlockReasonCode(v Verdict) string {
	if !v.BlocksMerge(true) {
		return ""
	}
	if v.Authorized && v.Alignment != nil && v.Alignment.Misaligned() {
		return ReasonCodeAlignmentMisaligned
	}
	switch {
	case v.Reason == ReasonTier1NeedsIssue:
		return ReasonCodeTier1NeedsIssue
	case v.Reason == ReasonTier2NeedsPlan:
		return ReasonCodeTier2NeedsPlan
	case v.Reason == ReasonTier3NeedsApproval:
		return ReasonCodeTier3NeedsApproval
	case strings.HasPrefix(v.Reason, reasonEvidenceUnavailablePrefix):
		return ReasonCodeEvidenceUnavailable
	}
	return ReasonCodeUnknown
}

// SetAuditSink installs the sink that receives one Observation per recorded
// evaluation (the audit-log line). nil disables it.
func (c *Counters) SetAuditSink(fn func(Observation)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.audit = fn
}

// Record counts one evaluation of v made by lane for repo#number and emits
// the audit observation. enforced is recorded on the observation only; the
// counter counts would-block regardless of enforcement.
func (c *Counters) Record(lane, repo string, number int, v Verdict, enforced bool) Observation {
	obs := Observation{
		Lane: lane, Repo: repo, Number: number,
		Tier:       TierKey(v.Tier),
		ReasonCode: WouldBlockReasonCode(v),
		Enforced:   enforced,
	}
	obs.WouldBlock = obs.ReasonCode != ""
	c.mu.Lock()
	if c.tiers == nil {
		c.tiers = make(map[string]*EvalCounts)
	}
	ec := c.tiers[obs.Tier]
	if ec == nil {
		ec = &EvalCounts{BlockedByReason: make(map[string]int64)}
		c.tiers[obs.Tier] = ec
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	c.pending.add(obs.Tier, obs.ReasonCode, now())
	ec.Attempted++
	if obs.WouldBlock {
		ec.Blocked++
		ec.BlockedByReason[obs.ReasonCode]++
	}
	audit := c.audit
	c.mu.Unlock()
	if audit != nil {
		audit(obs)
	}
	return obs
}

// Snapshot returns a copy of the in-memory counters keyed by tier.
func (c *Counters) Snapshot() map[string]EvalCounts {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]EvalCounts, len(c.tiers))
	for tier, ec := range c.tiers {
		cp := *ec
		cp.BlockedByReason = make(map[string]int64, len(ec.BlockedByReason))
		for k, v := range ec.BlockedByReason {
			cp.BlockedByReason[k] = v
		}
		out[tier] = cp
	}
	return out
}

// defaultCounters is the process-wide instance every evaluation lane records
// into; tests construct their own Counters.
var defaultCounters Counters

// RecordEvaluation records v into the process-wide counters.
func RecordEvaluation(lane, repo string, number int, v Verdict, enforced bool) Observation {
	return defaultCounters.Record(lane, repo, number, v, enforced)
}

// SetEvaluationAuditSink installs the audit sink on the process-wide counters.
func SetEvaluationAuditSink(fn func(Observation)) { defaultCounters.SetAuditSink(fn) }

// EvaluationCounts snapshots the process-wide counters.
func EvaluationCounts() map[string]EvalCounts { return defaultCounters.Snapshot() }

package scheduler

import (
	"fmt"

	"github.com/kubestellar/hive/v2/pkg/ioscan"
)

// AuditFunc records a security-relevant enforcement event. It is deliberately a
// plain function (action, detail, agent) so pkg/scheduler and pkg/ioscan never
// import pkg/dashboard: the dashboard/server passes its own
// (*Server).AuditLog-backed closure in via SetAuditFunc. A nil AuditFunc is a
// safe no-op — enforcement records nothing but never panics.
type AuditFunc func(action, detail, agent string)

const (
	// auditActionIoscanBlock is the audit-log action recorded when ioscan blocks
	// untrusted input (the raw text is withheld from the kick).
	auditActionIoscanBlock = "ioscan_block"
	// auditActionIoscanFailClosed is recorded when ioscan.fail_mode=closed
	// suppresses a whole kick after a Critical injection finding.
	auditActionIoscanFailClosed = "ioscan_fail_closed"
	// ioscanAuditUser is the audit-log user/agent attribution for kick-path
	// enforcement events. Untrusted issue text has no single owning agent at
	// list-assembly time, so events are attributed to the scheduler itself.
	ioscanAuditUser = "scheduler"
)

// SetAuditFunc attaches an audit-logging callback used by ioscan input
// enforcement. When nil (the default) enforcement still redacts blocked text
// but records no audit entry. The dashboard server wires this to its AuditLog so
// blocks surface in the existing audit-trail UI with no new sink.
func (s *Scheduler) SetAuditFunc(f AuditFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditFunc = f
}

// auditLogger returns the attached audit callback, or nil if none is set.
func (s *Scheduler) auditLogger() AuditFunc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.auditFunc
}

// ioscanEnabled reports whether input/output scanning is turned on for this
// hive. Per F11 (audit rec #7) scanning now defaults ON (fail-safe): an absent
// `ioscan:` block, or an omitted `enabled:` key, scans by default. Only text
// the input block policy trips (Critical / High-severity injection) is
// redacted, so ordinary titles pass through byte-identically. An operator can
// still opt out with an explicit `ioscan.enabled: false`.
func (s *Scheduler) ioscanEnabled() bool {
	return s.cfg != nil && s.cfg.Ioscan.IsEnabled()
}

func (s *Scheduler) ioscanFailClosed() bool {
	return s.cfg != nil && s.cfg.Ioscan.FailClosed()
}

// enforceIssueText runs ioscan over one piece of untrusted external text (an
// issue title about to be injected into a kick) and returns the text that is
// safe to inject. When ioscan is disabled it is a strict no-op — the input is
// returned unchanged with no scan and no allocation. When enabled and the input
// is blocked, the raw text is replaced with a secret-safe annotation and the
// event is written to the existing dashboard audit log (when an AuditFunc is
// attached). Enforcement is fail-safe: it never errors and never drops the item,
// only the untrusted text is withheld.
func (s *Scheduler) enforceIssueText(text string) string {
	sanitized, _ := s.enforceIssueTextVerdict(text)
	return sanitized
}

func (s *Scheduler) enforceIssueTextVerdict(text string) (string, ioscan.Verdict) {
	if !s.ioscanEnabled() {
		return text, ioscan.Verdict{}
	}
	sanitized, v := ioscan.EnforceInput(text)
	if v.Blocked {
		s.recordIoscanBlock(v)
	}
	if s.ioscanFailClosed() && v.HasCriticalInjection() {
		s.recordIoscanFailClosed(v)
	}
	return sanitized, v
}

// enforceLabels runs ioscan over each untrusted label before it is joined into
// a kick line. Labels are attacker-controllable on public issues/PRs and drive
// classification routing (pkg/classify), so a crafted label must not reach an
// agent prompt raw. Like enforceIssueText it is a strict no-op when ioscan is
// disabled (returns the input slice unchanged, no allocation). When enabled,
// each label is scanned independently and a blocked label is annotated rather
// than emitted raw. Fail-safe: never errors, never drops a label.
func (s *Scheduler) enforceLabels(labels []string) []string {
	out, _ := s.enforceLabelsWithPolicy(labels)
	return out
}

func (s *Scheduler) enforceLabelsWithPolicy(labels []string) ([]string, bool) {
	if !s.ioscanEnabled() || len(labels) == 0 {
		return labels, false
	}
	out := make([]string, len(labels))
	failClosed := false
	for i, l := range labels {
		var v ioscan.Verdict
		out[i], v = s.enforceIssueTextVerdict(l)
		failClosed = failClosed || (s.ioscanFailClosed() && v.HasCriticalInjection())
	}
	return out, failClosed
}

// recordIoscanBlock writes one audit entry per blocked verdict, one line per
// finding, in the same `key=value, ...` detail shape the dashboard's own
// auditDetail helper produces. A nil audit callback is a no-op.
func (s *Scheduler) recordIoscanBlock(v ioscan.Verdict) {
	log := s.auditLogger()
	if log == nil {
		return
	}
	for _, f := range v.Findings {
		detail := fmt.Sprintf("rule=%s, severity=%s, kind=%s",
			f.Rule, f.Severity.String(), string(f.Kind))
		log(auditActionIoscanBlock, detail, ioscanAuditUser)
	}
}

func (s *Scheduler) recordIoscanFailClosed(v ioscan.Verdict) {
	log := s.auditLogger()
	if log == nil {
		return
	}
	for _, f := range v.Findings {
		if f.Kind != ioscan.KindInjection || f.Severity < ioscan.SeverityCritical {
			continue
		}
		detail := fmt.Sprintf("rule=%s, severity=%s, kind=%s, fail_mode=closed",
			f.Rule, f.Severity.String(), string(f.Kind))
		log(auditActionIoscanFailClosed, detail, ioscanAuditUser)
	}
}

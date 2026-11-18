package main

import (
	"github.com/hivecommons/hive/crux/sessiontaint"
	"github.com/hivecommons/hive/pkg/ioscan"
)

// canaryTaintOrigin is the origin recorded on the `secret` label a canary hit
// sets. It is the logscrub category name of the planted marker, never the
// token itself.
const canaryTaintOrigin = "hive-canary"

// sessionTaintObserver returns the process-wide observe-only session taint
// observer (#18987, phase 0), building it on first use. It is nil unless
// `session_taint.enabled` is true, and every sessiontaint method is a no-op on
// nil, so a hive that sets nothing records nothing. The observer is shared by
// every GitHub client rebuild, so labels survive a credential re-delivery; a
// config change takes effect at the next restart.
func (b *boot) sessionTaintObserver() *sessiontaint.Observer {
	b.sessionTaintOnce.Do(func() {
		if b.cfg == nil {
			return
		}
		cfg := b.cfg.SessionTaint
		b.sessionTaint = sessiontaint.New(sessiontaint.Options{
			Enabled:             cfg.Enabled,
			MaxSessions:         cfg.MaxSessionsOrDefault(),
			MaxLabelsPerSession: cfg.MaxLabelsPerSessionOrDefault(),
			MaxDecisions:        cfg.MaxDecisionsOrDefault(),
			Logger:              b.logger,
			Audit:               b.sessionTaintAudit,
		})
	})
	return b.sessionTaint
}

// sessionTaintAudit writes one observer entry to the audit log. The detail
// carries labels and ids only.
func (b *boot) sessionTaintAudit(agent, action, detail string) {
	if b.dashSrv == nil {
		return
	}
	b.dashSrv.AuditLog(agent, action, detail, agent)
}

// recordCanaryTaint is source S3: a canary hit means the agent whose token it
// is has been exposed to a secret-shaped marker. Observe only.
func (b *boot) recordCanaryTaint(leak ioscan.CanaryLeak) {
	b.sessionTaintObserver().RecordLabel(leak.Agent, sessiontaint.LabelSecret, canaryTaintOrigin, sessiontaint.SourceCanaryHit)
}

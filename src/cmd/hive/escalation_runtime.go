package main

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalate"
	"github.com/hivecommons/hive/pkg/logscrub"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/review"
)

var escalationRuntime = struct {
	sync.RWMutex
	d     *escalate.Dispatcher
	email *escalate.EmailSink
	key   *escalationRuntimeKey
}{}

// escalationRuntimeKey is every input that shapes the dispatcher and its
// sinks. A hive.yaml reload whose key is unchanged keeps the running
// dispatcher, so queued escalations and the pending email digest survive it.
type escalationRuntimeKey struct {
	email    config.EscalationEmailConfig
	push     config.EscalationPushConfig
	hiveID   string
	spoke    string
	notifier *notify.Notifier
}

func newEscalationRuntimeKey(cfg *config.Config, notifier *notify.Notifier) *escalationRuntimeKey {
	return &escalationRuntimeKey{
		email:    cfg.Escalation.Email,
		push:     cfg.Escalation.Push,
		hiveID:   cfg.HiveID,
		spoke:    cfg.Project.Org,
		notifier: notifier,
	}
}

func configureEscalationDispatcher(ctx context.Context, cfg *config.Config, notifier *notify.Notifier, audit agent.AuditSink, logger *slog.Logger) {
	escalationRuntime.Lock()
	defer escalationRuntime.Unlock()
	var key *escalationRuntimeKey
	if cfg != nil {
		key = newEscalationRuntimeKey(cfg, notifier)
		if escalationRuntime.key != nil && reflect.DeepEqual(*escalationRuntime.key, *key) {
			return
		}
	}
	// Build the replacement before stopping the old dispatcher: Stop drains
	// what is already queued, and producers never see a nil dispatcher in
	// between.
	var d *escalate.Dispatcher
	var email *escalate.EmailSink
	if cfg != nil {
		d, email = buildEscalationDispatcher(ctx, cfg, notifier, audit, logger, escalationRuntime.email)
	}
	if escalationRuntime.d != nil {
		escalationRuntime.d.Stop()
	}
	escalationRuntime.d, escalationRuntime.email, escalationRuntime.key = d, email, key
}

// buildEscalationDispatcher returns nil when no sink is configured. A new
// email sink inherits prev's pending digest.
func buildEscalationDispatcher(ctx context.Context, cfg *config.Config, notifier *notify.Notifier, audit agent.AuditSink, logger *slog.Logger, prev *escalate.EmailSink) (*escalate.Dispatcher, *escalate.EmailSink) {
	auditFn := func(action, detail, sink string) {
		if audit != nil {
			audit.Record("system", action, sink, map[string]any{"detail": detail})
		}
	}
	d := escalate.NewDispatcher(ctx, logger, auditFn)
	registered := false
	var email *escalate.EmailSink
	if cfg.Escalation.Email.Enabled {
		email = escalate.NewEmailSink(escalate.EmailConfig{
			Host:     cfg.Escalation.Email.SMTP.Host,
			Port:     cfg.Escalation.Email.SMTP.Port,
			Username: cfg.Escalation.Email.SMTP.Username,
			Password: cfg.Escalation.Email.SMTP.Password,
			From:     cfg.Escalation.Email.From,
			To:       cfg.Escalation.Email.To,
			DigestTo: cfg.Escalation.Email.Digest.To,
			DigestAt: cfg.Escalation.Email.Digest.At,
			HiveName: cfg.HiveID,
			Spoke:    cfg.Project.Org,
			Version:  reportedVersion(),
			Logger:   logger,
			Audit:    auditFn,
		})
		email.InheritDigest(prev)
		d.Register(email, escalate.SeverityInfo, 64)
		email.StartDigest(d.Context())
		registered = true
	}
	if cfg.Escalation.Push.Enabled {
		min := escalate.SeverityPage
		if parsed, ok := escalate.ParseSeverity(cfg.Escalation.Push.MinSeverity); ok && parsed == escalate.SeverityDecision {
			min = parsed
		}
		if cfg.Escalation.Push.Ntfy.URL != "" {
			d.Register(&escalate.NtfySink{URL: cfg.Escalation.Push.Ntfy.URL, Token: cfg.Escalation.Push.Ntfy.Token}, min, 32)
			registered = true
		}
		if cfg.Escalation.Push.Pushover.AppToken != "" && cfg.Escalation.Push.Pushover.UserKey != "" {
			d.Register(&escalate.PushoverSink{AppToken: cfg.Escalation.Push.Pushover.AppToken, UserKey: cfg.Escalation.Push.Pushover.UserKey}, min, 32)
			registered = true
		}
		if cfg.Escalation.Push.PagerDuty.RoutingKey != "" {
			d.Register(&escalate.PagerDutySink{RoutingKey: cfg.Escalation.Push.PagerDuty.RoutingKey}, min, 32)
			registered = true
		}
	}
	if notifier != nil && registered {
		d.Register(notifyEscalationSink{n: notifier}, escalate.SeverityPage, 16)
	}
	if !registered {
		d.Stop()
		return nil, nil
	}
	return d, email
}

func currentEscalationDispatcher() *escalate.Dispatcher {
	escalationRuntime.RLock()
	defer escalationRuntime.RUnlock()
	return escalationRuntime.d
}

type notifyEscalationSink struct{ n *notify.Notifier }

func (s notifyEscalationSink) Name() string { return "chat" }
func (s notifyEscalationSink) Deliver(ctx context.Context, ev escalate.Event) error {
	_ = ctx
	if s.n != nil {
		s.n.Send(ev.Title, strings.TrimSpace(ev.Body+"\n"+ev.Link), notify.PriorityHigh)
	}
	return nil
}

func emitReviewHumanEscalations(plan review.DispatchPlan) {
	d := currentEscalationDispatcher()
	if d == nil {
		return
	}
	for _, h := range plan.NewHuman {
		repo := logscrub.ScrubString(h.Repo)
		ref := fmt.Sprintf("%s#%d", repo, h.Number)
		body := logscrub.ScrubString(fmt.Sprintf("%s\nHead SHA: %s", h.Reason, h.HeadSHA))
		d.Dispatch(escalate.Event{Severity: escalate.SeverityDecision, Title: logscrub.ScrubString("HUMAN DECISION NEEDED: " + ref), Body: body, Link: "https://github.com/" + repo + "/pull/" + fmt.Sprint(h.Number)})
	}
}

func emitAgentPauseEscalation(event agent.PauseTransitionEvent) {
	if !event.Paused {
		return
	}
	d := currentEscalationDispatcher()
	if d == nil {
		return
	}
	title := logscrub.ScrubString(fmt.Sprintf("Agent paused: %s", event.Agent))
	body := logscrub.ScrubString(fmt.Sprintf("trigger=%s\nreason=%s\nby=%s", event.Trigger, event.Reason, event.By))
	d.Dispatch(escalate.Event{Severity: escalate.SeverityPage, Title: title, Body: body})
}

func emitBudgetExhaustedEscalation(detail string) {
	d := currentEscalationDispatcher()
	if d == nil {
		return
	}
	title := logscrub.ScrubString("Governor budget exhausted: kicks stopped")
	body := logscrub.ScrubString(detail)
	d.Dispatch(escalate.Event{Severity: escalate.SeverityPage, Title: title, Body: body})
}

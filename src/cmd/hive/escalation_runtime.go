package main

import (
	"context"
	"fmt"
	"log/slog"
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
	d *escalate.Dispatcher
}{}

func configureEscalationDispatcher(ctx context.Context, cfg *config.Config, notifier *notify.Notifier, audit agent.AuditSink, logger *slog.Logger) {
	escalationRuntime.Lock()
	if escalationRuntime.d != nil {
		escalationRuntime.d.Stop()
		escalationRuntime.d = nil
	}
	escalationRuntime.Unlock()
	if cfg == nil {
		return
	}
	auditFn := func(action, detail, sink string) {
		if audit != nil {
			audit.Record("system", action, sink, map[string]any{"detail": detail})
		}
	}
	d := escalate.NewDispatcher(ctx, logger, auditFn)
	registered := false
	if cfg.Escalation.Email.Enabled {
		s := escalate.NewEmailSink(escalate.EmailConfig{
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
		})
		d.Register(s, escalate.SeverityInfo, 64)
		s.StartDigest(d.Context())
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
		return
	}
	escalationRuntime.Lock()
	escalationRuntime.d = d
	escalationRuntime.Unlock()
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

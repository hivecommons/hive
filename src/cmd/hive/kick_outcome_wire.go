package main

import (
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/kickoutcome"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

// Per-kick outcome record, host wiring (hivecommons/hive#18916).
//
// pkg/kickoutcome keeps the record; this file feeds it the signals the hive
// already has: the fan-out kick observer (a kick was delivered), the runtime's
// turn-end classification, the relay's pull-request, push and comment events,
// and the callers that know who or what is kicking (the governor here; the
// dashboard in pkg/dashboard). Nothing here reads agent text into a prompt.

// kickOutcomesFile is the durable record under the hive's data root.
const kickOutcomesFile = "kick-outcomes.json"

var (
	kickOutcomesOnce  sync.Once
	kickOutcomesStore *kickoutcome.Store
)

// kickOutcomesFor is the process's kick outcome store, opened on first use. A
// corrupt or unreadable file starts the store empty with a warning: a lost
// record must not stop kicks.
func kickOutcomesFor(cfg *config.Config, logger *slog.Logger) *kickoutcome.Store {
	kickOutcomesOnce.Do(func() {
		path := ""
		if cfg != nil {
			path = filepath.Join(wfstore.Root(cfg.Data.WorkflowsDir), kickOutcomesFile)
		}
		s, err := kickoutcome.Open(path, logger)
		if err != nil && logger != nil {
			logger.Warn("kick outcomes unreadable; starting empty", "path", path, "error", err)
		}
		kickOutcomesStore = s
	})
	return kickOutcomesStore
}

// kickOutcomeTurnEnded files how an agent's kicked turn ended. The agent
// manager's refusal flag, when set and not a startup deferral, also marks the
// kick as declined by the agent's own model.
func kickOutcomeTurnEnded(s *kickoutcome.Store, status func(agent string) (*crux.Process, error), agent string, o crux.KickOutcome) {
	if s == nil {
		return
	}
	s.TurnEnded(agent, o.Kind, o.Reason, o.KickAt, o.At)
	if status == nil {
		return
	}
	if p, err := status(agent); err == nil && p != nil && p.KickRefused &&
		!strings.Contains(p.KickRefusalReason, workflowStartupDeferral) && p.LastKick != nil && p.LastKick.Equal(o.KickAt) {
		s.Declined(agent, p.KickRefusalReason, o.At)
	}
}

// kickOutcomeDelivered files a delivered kick from a fan-out kick event.
func kickOutcomeDelivered(s *kickoutcome.Store, ev crux.KickEvent) {
	if ev.Event == crux.KickEventDelivered {
		s.Delivered(ev.Agent, ev.At)
	}
}

// kickOutcomeAgentAction files a relay push or comment.
func kickOutcomeAgentAction(s *kickoutcome.Store, a github.AgentAction) {
	switch a.Kind {
	case github.AgentActionPush:
		s.Pushed(a.Agent, a.Branch, a.SHA, a.At)
	case github.AgentActionComment:
		s.Commented(a.Agent, a.At)
	}
}

// kickOutcomeSender wraps the governor's kick delivery: it says the kick is
// the governor's (or a workflow hand-off's, when workflowKick says so) and
// files a refusal the hive's delivery checks raise. A busy bounce is a
// deferral, not a refusal, and is left to the next pass.
func kickOutcomeSender(s *kickoutcome.Store, send func(agent, message string) error, workflowKick func(agent, message string) bool) func(agent, message string) error {
	return func(agent, message string) error {
		kind := kickoutcome.TriggerGovernor
		if workflowKick != nil && workflowKick(agent, message) {
			kind = kickoutcome.TriggerWorkflowDispatch
		}
		s.Intend(agent, kickoutcome.Trigger{Kind: kind, Actor: "governor"})
		err := send(agent, message)
		if err != nil && !isKickBusy(err) {
			if code := dashboard.ClassifyKickRefusal(err.Error()); code != "" {
				s.Refused(agent, kickoutcome.Trigger{Kind: kind, Actor: "governor"}, code, err.Error(), time.Now())
			}
		}
		return err
	}
}

// kickOutcomeBackoffWithheld files a governor kick withheld during provider
// backoff as a refusal (provider_backoff, kick-refusals.md). The reason is
// built from the hive's own error class and retry time only: the provider
// error line is agent output, so it is never quoted here.
func kickOutcomeBackoffWithheld(s *kickoutcome.Store, agent, class string, retryIn time.Duration, at time.Time) {
	if s == nil {
		return
	}
	reason := "agent " + agent + " blocked: inference (" + class + "); next provider probe in " + retryIn.Round(time.Second).String()
	s.Refused(agent, kickoutcome.Trigger{Kind: kickoutcome.TriggerGovernor, Actor: "governor"}, "provider_backoff", reason, at)
}

// isKickBusy reports a busy bounce (agent mid-turn).
func isKickBusy(err error) bool { return errors.Is(err, crux.ErrAgentBusy) }

// kickOutcomeSource adapts the store to the dashboard's KickOutcomeSource, so
// the dashboard takes no import of pkg/kickoutcome.
type kickOutcomeSource struct{ s *kickoutcome.Store }

func (a kickOutcomeSource) Intend(agent, kind, actor string) {
	a.s.Intend(agent, kickoutcome.Trigger{Kind: kind, Actor: actor})
}

func (a kickOutcomeSource) Refused(agent, kind, actor, code, reason string, at time.Time) {
	a.s.Refused(agent, kickoutcome.Trigger{Kind: kind, Actor: actor}, code, reason, at)
}

func (a kickOutcomeSource) Latest() any { return a.s.Latest() }

func (a kickOutcomeSource) Recent(agent string, n int) (any, any) {
	kicks := a.s.Recent(agent, n)
	if kicks == nil {
		kicks = []kickoutcome.Detail{}
	}
	if len(kicks) == 0 {
		return nil, kicks
	}
	return kicks[0].View, kicks
}

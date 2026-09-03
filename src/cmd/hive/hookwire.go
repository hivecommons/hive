package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/hub"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/timeline"
)

// Sentinel errors for an adapter whose backing object was never installed.
// They surface in the audit log as a hook failure rather than a silent no-op:
// an action that quietly does nothing is the failure mode this whole feature
// is meant to avoid.
var (
	errNoAgentManager = errors.New("hooks: agent manager not wired; pause unavailable")
	errNoTimeline     = errors.New("hooks: timeline store not wired; annotate unavailable")
)

// This file wires pkg/hooks (RFC #4001) into the running process. It owns the
// adapters from hive's concrete objects onto the narrow sink interfaces the
// hooks package declares, plus the recompile-on-config-change cache — the same
// structure celwire.go uses for CEL triggers.
//
// Keeping the adapters HERE rather than in pkg/hooks is what lets that package
// declare its mutation surface as four interfaces without importing the
// dashboard, agent manager, or notifier (which would cycle).

// hookDispatcherCache memoizes the compiled hook registry so it is rebuilt only
// when the operator's `hooks:` list actually changes, not on every config
// reload tick. Compilation is fail-closed: a malformed hook list leaves the
// PREVIOUS registry in place, so a bad edit never disarms working hooks and
// never crashes the process.
type hookDispatcherCache struct {
	mu         sync.Mutex
	sig        string
	dispatcher *hooks.Dispatcher
	built      bool
}

// globalHookDispatcher is the process-wide hook dispatcher. Emitting sites
// reach it through hookDispatcher(); a nil return means no hooks are
// configured and firing is a no-op.
var globalHookDispatcher hookDispatcherCache

// hookDispatcher returns the process-wide dispatcher, or nil when no hooks are
// configured. Nil-safe at the call site: hooks.Dispatcher.Fire tolerates a nil
// receiver, so emitters need no guard.
func hookDispatcher() *hooks.Dispatcher {
	globalHookDispatcher.mu.Lock()
	defer globalHookDispatcher.mu.Unlock()
	return globalHookDispatcher.dispatcher
}

// hookSinks bundles the concrete objects the hook actions act through. Every
// one is optional; a missing sink makes its action a reported failure rather
// than a silent no-op.
type hookSinks struct {
	Notifier  *notify.Notifier
	AgentMgr  *agent.Manager
	Timeline  *timeline.Store
	Audit     hooks.AuditSink
	Approvals hooks.ApprovalQueue // #4000; nil until that RFC lands
}

// buildHookDispatcher compiles the operator's `hooks:` list and installs the
// dispatcher, rebuilding only when the list changed.
//
// Call it at startup and after each config reload. On a compile error it logs
// and KEEPS the previous dispatcher, matching celEngineFor's fail-closed
// posture: an operator's typo must not silently disarm hooks that were working.
func buildHookDispatcher(cfg *config.Config, sinks hookSinks, logger *slog.Logger) {
	if cfg == nil {
		return
	}
	sig := hooks.SignatureFromConfig(cfg)

	globalHookDispatcher.mu.Lock()
	defer globalHookDispatcher.mu.Unlock()

	if globalHookDispatcher.built && globalHookDispatcher.sig == sig {
		return
	}
	globalHookDispatcher.sig = sig
	globalHookDispatcher.built = true

	if len(cfg.Hooks) == 0 {
		// Disarm by swapping in an EMPTY registry rather than dropping the
		// dispatcher. Nil'ing it here would discard the rate-limit windows
		// with it, so "remove all hooks, re-add them" — two ordinary config
		// edits — would rebuild a fresh limiter and clear the storm ceiling,
		// defeating the very bypass the in-place swap below exists to prevent.
		// An empty registry is already a total no-op (one map lookup per
		// transition), so keeping the dispatcher costs nothing.
		if globalHookDispatcher.dispatcher != nil {
			globalHookDispatcher.dispatcher.SetRegistry(nil)
		}
		return
	}

	reg, err := hooks.CompileFromConfig(cfg)
	if err != nil {
		// Fail closed, and loudly: the operator asked for behavior that hive
		// is refusing to arm, which they must be able to see.
		logger.Error("hooks: rejecting invalid hook config; keeping the previous hook set",
			"error", err)
		return
	}

	// Hot reload of an existing dispatcher swaps the registry in place, which
	// PRESERVES the rate-limit windows. Rebuilding the dispatcher instead
	// would reset them, letting a reload loop bypass the storm ceiling.
	if globalHookDispatcher.dispatcher != nil {
		globalHookDispatcher.dispatcher.SetRegistry(reg)
		logger.Info("hooks: reloaded hook set", "hooks", reg.Len())
		return
	}

	opts := []hooks.Option{}
	if sinks.Notifier != nil {
		opts = append(opts, hooks.WithNotifier(&notifierAdapter{n: sinks.Notifier}))
	}
	if sinks.AgentMgr != nil {
		opts = append(opts, hooks.WithPauser(&pauserAdapter{mgr: sinks.AgentMgr}))
	}
	if sinks.Timeline != nil {
		opts = append(opts, hooks.WithAnnotator(&annotatorAdapter{store: sinks.Timeline}))
	}
	if sinks.Audit != nil {
		opts = append(opts, hooks.WithAuditSink(sinks.Audit))
	}
	if sinks.Approvals != nil {
		opts = append(opts, hooks.WithApprovalQueue(sinks.Approvals))
	}

	globalHookDispatcher.dispatcher = hooks.NewDispatcher(reg, logger, opts...)
	logger.Info("hooks: compiled hook set", "hooks", reg.Len())
}

// ---------------------------------------------------------------------------
// Emitters
// ---------------------------------------------------------------------------

// installGovernorModeChangeEmitter wires the governor_mode_change transition to
// its hooks. The governor invokes the observer AFTER committing the change and
// AFTER releasing its mutex, which is what makes this a post-commit emission
// rather than an inline one.
//
// This is the template for cataloging the remaining implicit transitions: find
// the durable commit, emit after it, and let the dispatcher do the rest.
func installGovernorModeChangeEmitter(gov *governor.Governor) {
	if gov == nil {
		return
	}
	gov.SetModeChangeObserver(func(change governor.ModeChange) {
		// hookDispatcher() may be nil (no hooks configured); Fire is nil-safe.
		hookDispatcher().Fire(context.Background(), hooks.Payload{
			Transition: hooks.TransitionGovernorModeChange,
			From:       string(change.From),
			To:         string(change.To),
			Reason:     change.Reason,
			Actor:      "system",
			At:         change.Timestamp.UnixMilli(),
		})
	})
}

func installAgentPauseEmitter(mgr *agent.Manager) {
	if mgr == nil {
		return
	}
	mgr.SetPauseTransitionObserver(func(event agent.PauseTransitionEvent) {
		transition := hooks.TransitionAgentResumed
		if event.Paused {
			transition = hooks.TransitionAgentPaused
		}
		hookDispatcher().Fire(context.Background(), hooks.Payload{
			Transition: transition,
			Agent:      event.Agent,
			Actor:      event.By,
			Trigger:    event.Trigger,
			Reason:     event.Reason,
			At:         event.At.UnixMilli(),
			Causation: hooks.Causation{
				Depth:            event.Causation.Depth,
				HookName:         event.Causation.HookName,
				OriginTransition: hooks.Transition(event.Causation.OriginTransition),
			},
		})
	})
}

func installUpgradePauseEmitter(hubSrv *hub.HubServer) {
	if hubSrv == nil {
		return
	}
	hubSrv.SetUpgradePauseObserver(func(event hub.UpgradePauseEvent) {
		to := "off"
		if event.Paused {
			to = "on"
		}
		hookDispatcher().Fire(context.Background(), hooks.Payload{
			Transition: hooks.TransitionUpgradePause,
			To:         to,
			Actor:      event.By,
			Reason:     event.Target,
			Attrs:      map[string]string{"target": event.Target},
		})
	})
}

// ---------------------------------------------------------------------------
// Adapters
// ---------------------------------------------------------------------------

// notifierAdapter bridges hooks.Notifier (which speaks plain strings, so
// pkg/hooks need not import pkg/notify) onto the real fanout.
type notifierAdapter struct{ n *notify.Notifier }

func (a *notifierAdapter) Send(title, message, priority string) {
	if a == nil || a.n == nil {
		return
	}
	a.n.Send(title, message, notify.Priority(priority))
}

// hookPauseTrigger is the PausedTrigger stamped when a hook pauses an agent.
// It makes "why is this agent paused?" answerable from the pause record alone,
// distinguishing a hook-driven pause from an operator's, the login-detector's,
// or the fleet-breaker's — the provenance discipline #4041/#4055 established.
const hookPauseTrigger = "hook"

// hookPauseTriggerFor derives the paused_trigger provenance value for a
// hook-driven pause, naming the responsible hook when one is known so the
// durable pause record answers "why is this agent paused?" on its own.
func hookPauseTriggerFor(cause hooks.Causation) string {
	if cause.HookName == "" {
		return hookPauseTrigger
	}
	return hookPauseTrigger + ":" + cause.HookName
}

// pauserAdapter routes the pause action through the AUDITED agent-manager
// pause — the same entry point the dashboard's pause button uses. It never
// writes the paused flag directly.
type pauserAdapter struct{ mgr *agent.Manager }

func (a *pauserAdapter) PauseAgent(ctx context.Context, agentName, reason string, cause hooks.Causation) error {
	if a == nil || a.mgr == nil {
		return errNoAgentManager
	}
	// The causation is folded into the trigger provenance so the durable pause
	// record names the responsible hook ("hook:<name>").
	//
	// LOOP-SAFETY REQUIREMENT: installAgentPauseEmitter must carry this
	// structured cause through on the payload it fires. With agent_paused wired,
	// the depth-1 guard is the ONLY thing stopping a pause→agent_paused→pause
	// cycle, and it works solely off Payload.Causation.
	//
	// Do NOT try to recover the depth by parsing the trigger string below.
	// It is human-readable provenance, not a machine-readable causation
	// chain; a pause routed through the manager loses the structured cause
	// unless the emitter is given it directly.
	//
	// PauseBy rather than Pause (#4055): a hook-driven pause must not appear
	// anonymous, because "paused, actor unknown" is exactly the state #4041
	// found indistinguishable from a malfunction days later. The acting
	// identity is the HOOK, not a person — PauseBy's contract is "never
	// fabricate" a human actor, so hookPauseActor is an explicitly
	// non-human identity that cannot be mistaken for a dashboard user.
	return a.mgr.PauseByCause(agentName, hookPauseTriggerFor(cause), reason, hookPauseActor(cause), agent.PauseCausation{
		Depth:            cause.Depth,
		HookName:         cause.HookName,
		OriginTransition: string(cause.OriginTransition),
	})
}

// hookPauseActor is the PausedBy identity recorded for a hook-driven pause.
//
// It is deliberately NOT a username. PauseBy documents `by` as the acting user
// and warns against fabricating one, so this returns a clearly-machine
// identity ("hook:<name>") that a reader — or the fleet view — cannot confuse
// with a person, while still answering "what paused this agent?" without a
// join against the audit log.
func hookPauseActor(cause hooks.Causation) string {
	if cause.HookName == "" {
		return hookPauseTrigger
	}
	return hookPauseTrigger + ":" + cause.HookName
}

// annotatorAdapter records hook annotations on the EXISTING lifecycle timeline
// rather than a parallel event bus, per the RFC's "build on timeline.Store".
type annotatorAdapter struct{ store *timeline.Store }

func (a *annotatorAdapter) Annotate(ctx context.Context, agentName, issueRef, note string, attrs map[string]string) error {
	if a == nil || a.store == nil {
		return errNoTimeline
	}
	// Copy the attrs and carry the note as one, so the timeline entry is
	// self-describing without needing a new Event field.
	merged := make(map[string]string, len(attrs)+1)
	for k, v := range attrs {
		merged[k] = v
	}
	merged["note"] = note

	a.store.Record(timeline.Event{
		IssueRef: issueRef,
		// A hook annotation is a blocked/attention-worthy marker rather than a
		// lifecycle stage of its own; KindBlocked is the closest existing kind
		// and keeps the timeline's closed Kind set closed.
		Kind:  timeline.KindBlocked,
		Agent: agentName,
		Attrs: merged,
	})
	return nil
}

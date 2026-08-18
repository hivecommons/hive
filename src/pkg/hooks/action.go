package hooks

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Action names the vetted operation a hook performs. The set is CLOSED and
// small by design (RFC #4001 security position): an operator can observe and
// notify freely, but every mutating action goes through an existing audited
// API, and there is deliberately no exec/script action in this slice.
//
// Adding an action is a security review, not a config change: it means adding
// a constant here, a vetted implementation, and a validation rule. An unknown
// action in config is rejected at load time.
type Action string

const (
	// ActionNotify sends a notification through the existing ntfy/Slack/Discord
	// fanout (pkg/notify). Pure observation — zero mutation risk. This is the
	// action the first shipped hook uses.
	ActionNotify Action = "notify"

	// ActionPause pauses an agent THROUGH THE AUDITED PAUSE API — the same
	// entry point the dashboard's pause button calls. It never writes the
	// paused flag directly.
	ActionPause Action = "pause"

	// ActionAnnotate records a timeline entry (pkg/timeline), building on the
	// existing event spine rather than a parallel bus.
	ActionAnnotate Action = "annotate"

	// ActionEnqueueApproval places an approval request into the #4000
	// tool-approval queue. It CONSUMES that queue's API (see ApprovalQueue);
	// this package does not implement an approvals store of its own.
	ActionEnqueueApproval Action = "enqueue-approval"
)

// vettedActions is the closed action set, used for validation and error text.
var vettedActions = map[Action]string{
	ActionNotify:          "Send a notification via the configured ntfy/Slack/Discord fanout.",
	ActionPause:           "Pause an agent through the audited pause API.",
	ActionAnnotate:        "Record an entry on the lifecycle timeline.",
	ActionEnqueueApproval: "Enqueue an approval request into the tool-approval queue.",
}

// KnownActions returns the vetted action names in sorted order.
func KnownActions() []Action {
	out := make([]Action, 0, len(vettedActions))
	for a := range vettedActions {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// IsVettedAction reports whether a is in the closed action set.
func IsVettedAction(a Action) bool {
	_, ok := vettedActions[a]
	return ok
}

// actionList renders the vetted set for an operator-facing error message.
func actionList() string {
	acts := KnownActions()
	parts := make([]string, 0, len(acts))
	for _, a := range acts {
		parts = append(parts, string(a))
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// Sinks: the narrow interfaces through which actions reach the rest of hive.
//
// Every one of these is an INTERFACE rather than a concrete dependency for two
// reasons. First, import hygiene: pkg/dashboard already imports the agent and
// config packages, so a direct import here would cycle — the same reason
// agent.AuditSink exists, and this package follows that established pattern.
// Second, and more importantly, it makes the mutation surface of the whole
// hooks feature auditable at a glance: these four interfaces are the COMPLETE
// list of things a hook can do to the system.
//
// The wiring layer (cmd/hive) owns the concrete objects and injects them.
// Every sink is optional: a nil sink makes its action a no-op that reports a
// clear error, never a panic and never a silent success.
// ---------------------------------------------------------------------------

// Notifier is the notification fanout a notify action uses. Satisfied by
// *notify.Notifier.
type Notifier interface {
	Send(title, message string, priority string)
}

// Pauser is the AUDITED pause API — the same one the dashboard's pause button
// calls. It takes a Causation so the agent_paused transition the pause itself
// emits is correctly marked hook-caused and cannot cascade (depth-1 guard).
//
// It deliberately does NOT expose an "unpause" or a raw state write: a hook may
// stop work, but resuming it is a human decision.
type Pauser interface {
	PauseAgent(ctx context.Context, agent, reason string, cause Causation) error
}

// Annotator is the lifecycle timeline's write side. Satisfied by an adapter
// over *timeline.Store (see the wiring layer) — hooks build on the existing
// event spine rather than introducing a second one.
type Annotator interface {
	Annotate(ctx context.Context, agent, issueRef, note string, attrs map[string]string) error
}

// ApprovalQueue is the #4000 tool-approval queue's enqueue side.
//
// WIRING POINT (#4000): this interface is deliberately narrow so it can be
// satisfied by that RFC's queue without this package depending on its internal
// shape. When #4000's queue lands, implement this over its enqueue API in
// cmd/hive and pass it to Dispatcher via WithApprovalQueue — no change is
// needed in this package. Until then the sink is nil and an enqueue-approval
// hook reports an unwired-sink error per firing (visible in the audit log)
// rather than silently doing nothing.
type ApprovalQueue interface {
	EnqueueApproval(ctx context.Context, req ApprovalRequest) error
}

// ApprovalRequest is what an enqueue-approval hook places on the queue. Fields
// are the intersection of what a hook can know from a transition payload and
// what an approval decision needs; #4000's queue is free to carry more.
type ApprovalRequest struct {
	// Kind categorizes the approval, taken from the hook's `params.kind` or
	// defaulted to the transition name.
	Kind string `json:"kind"`
	// Agent and Repo scope the request.
	Agent string `json:"agent,omitempty"`
	Repo  string `json:"repo,omitempty"`
	// Summary is the human-readable ask shown in the approvals UI.
	Summary string `json:"summary"`
	// Transition and HookName record what produced the request, so an approver
	// can see which config rule asked and why.
	Transition Transition `json:"transition"`
	HookName   string     `json:"hookName"`
	// Cause carries the depth-1 causation so any transition the approval's
	// resolution produces stays correctly marked.
	Cause Causation `json:"cause"`
}

// ---------------------------------------------------------------------------
// Action execution
// ---------------------------------------------------------------------------

// notifyPriority maps a hook's optional params.priority onto the notifier's
// vocabulary, defaulting to "default" for anything unrecognized. Validation
// (see registry.go) rejects an unknown priority at load time, so this is a
// second, defensive layer rather than the primary check.
func notifyPriority(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "high":
		return "high"
	case "low":
		return "low"
	default:
		return "default"
	}
}

// execute runs one hook's action against a payload. It returns an error rather
// than logging-and-swallowing so the dispatcher can record the outcome in the
// audit trail; the dispatcher is what isolates a failure so it cannot affect
// the transition or another hook.
func (d *Dispatcher) execute(ctx context.Context, h Hook, p Payload) error {
	switch h.Action {
	case ActionNotify:
		if d.notifier == nil {
			return fmt.Errorf("notify: no notifier wired")
		}
		title, message := renderNotification(h, p)
		d.notifier.Send(title, message, notifyPriority(h.Params["priority"]))
		return nil

	case ActionPause:
		if d.pauser == nil {
			return fmt.Errorf("pause: no pause API wired")
		}
		agent := firstNonEmpty(h.Params["agent"], p.Agent)
		if agent == "" {
			return fmt.Errorf("pause: no agent in params or transition payload")
		}
		reason := firstNonEmpty(h.Params["reason"],
			fmt.Sprintf("hook %q on %s", h.Name, p.Transition))
		// Child() marks the agent_paused transition this pause emits as
		// hook-caused at depth 1, so it cannot fire further hooks.
		return d.pauser.PauseAgent(ctx, agent, reason, p.Causation.Child(h.Name, p.Transition))

	case ActionAnnotate:
		if d.annotator == nil {
			return fmt.Errorf("annotate: no timeline wired")
		}
		note := firstNonEmpty(h.Params["note"],
			fmt.Sprintf("%s (hook %s)", p.Transition, h.Name))
		return d.annotator.Annotate(ctx, p.Agent, firstNonEmpty(h.Params["issue_ref"], p.attr("issue_ref")),
			note, annotationAttrs(h, p))

	case ActionEnqueueApproval:
		if d.approvals == nil {
			// #4000 has not landed / is not wired. Report it: an approval that
			// silently never queues is worse than a loud failure.
			return fmt.Errorf("enqueue-approval: no approval queue wired (#4000)")
		}
		return d.approvals.EnqueueApproval(ctx, ApprovalRequest{
			Kind:  firstNonEmpty(h.Params["kind"], string(p.Transition)),
			Agent: firstNonEmpty(h.Params["agent"], p.Agent),
			Repo:  firstNonEmpty(h.Params["repo"], p.Repo),
			Summary: firstNonEmpty(h.Params["summary"],
				fmt.Sprintf("%s requires approval (hook %s)", p.Transition, h.Name)),
			Transition: p.Transition,
			HookName:   h.Name,
			Cause:      p.Causation.Child(h.Name, p.Transition),
		})
	}

	// Unreachable for a registry-validated hook: an unknown action is rejected
	// at load time. Kept as a fail-closed backstop for a Hook constructed in
	// code that bypassed validation.
	return fmt.Errorf("unknown action %q", h.Action)
}

// annotationAttrs builds the timeline attrs for an annotate action: the hook's
// provenance plus the model metadata when the transition carries it, so a
// timeline reader sees the same context the notification would have shown.
func annotationAttrs(h Hook, p Payload) map[string]string {
	attrs := map[string]string{
		"hook":       h.Name,
		"transition": string(p.Transition),
	}
	if p.Model != "" {
		attrs["model"] = p.Model
	}
	if p.Backend != "" {
		attrs["backend"] = p.Backend
	}
	if p.Pin != "" {
		attrs["pin"] = p.Pin
	}
	return attrs
}

// firstNonEmpty returns the first argument that is not empty after trimming.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

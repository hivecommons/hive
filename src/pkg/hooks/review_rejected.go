package hooks

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// This file implements the FIRST SHIPPED HOOK end-to-end: review_rejected.
//
// # The ask (RFC #4001, bluefin fleet-owner feedback)
//
// When a review's output is judged low quality, the owner wants to send the
// work back AND see the relevant knob in that moment. The observed failure was
// a review produced by an outdated pinned model, where finding the model
// setting meant hunting through the admin UI. The owners were explicit that
// the knob belongs in the review loop, not on a separate admin surface.
//
// So the transition carries the PRODUCING agent's backend/model/pin metadata
// (the vocabulary standardized in pkg/tracing/semconv.go), and the notification
// this hook sends deep-links straight to that agent's model-pin control.
//
// It exercises the entire pipeline — transition emission with typed payload,
// declarative registration, CEL predicate, action dispatch, audit trail — with
// a notify-only action, i.e. zero mutation risk.

// ReviewRejection is the input an emitting site supplies when a human rejects
// a review's output. It is a purpose-built struct rather than a raw Payload so
// the emitter cannot forget the model metadata that is the entire point of the
// transition: the fields are named and the constructor fills the rest.
type ReviewRejection struct {
	// Agent is the agent whose output was rejected. Required — without it the
	// notification cannot name a model knob to link.
	Agent string
	// Repo is the repository, e.g. "org/name".
	Repo string
	// PRNumber is the pull request whose review was rejected, 0 when unknown.
	PRNumber int
	// Actor is the human who rejected it.
	Actor string
	// Reason is the operator's stated reason ("hallucinated the API", …).
	Reason string

	// Backend, Model, and Pin identify what PRODUCED the rejected output.
	// These are the fields that make the notification actionable.
	Backend string
	Model   string
	Pin     string
	// ACMMLevel is the autonomy level in effect, 0 when unknown.
	ACMMLevel int

	// DashboardBaseURL is the hive dashboard's external base URL, used to
	// build the model-knob deep link. When empty the notification still sends,
	// naming the model but without a link — degraded, not broken.
	DashboardBaseURL string
}

// modelKnobPath is the dashboard route that focuses an agent's model/pin
// control. Kept as a named constant next to the link builder so the coupling
// to the dashboard's routing is in exactly one place and shows up in a grep
// for the route.
const modelKnobPath = "/agents"

// ModelKnobURL builds the deep link to the producing agent's model-pin knob —
// the "surface the tuning knob in-context" half of the fleet-owner ask.
//
// Returns "" when there is no base URL or no agent, in which case the
// notification degrades to naming the model without linking it.
func ModelKnobURL(baseURL, agent string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" || strings.TrimSpace(agent) == "" {
		return ""
	}
	// The agent name lands in a query parameter, so it must be escaped: an
	// agent named with a `&` or a space would otherwise truncate or corrupt
	// the link.
	return fmt.Sprintf("%s%s?agent=%s&focus=model", base, modelKnobPath, url.QueryEscape(agent))
}

// Attribute keys carried in a review_rejected payload's Attrs.
const (
	// AttrPR is the pull request number, as a string.
	AttrPR = "pr"
	// AttrModelKnobURL is the deep link to the producing agent's model knob.
	AttrModelKnobURL = "model_knob_url"
)

// NewReviewRejectedPayload builds the review_rejected transition payload.
//
// Causation is left at depth 0: a human rejecting a review is a
// world-originated transition, so its hooks are allowed to fire.
func NewReviewRejectedPayload(r ReviewRejection) Payload {
	attrs := map[string]string{}
	if r.PRNumber > 0 {
		attrs[AttrPR] = strconv.Itoa(r.PRNumber)
	}
	if knob := ModelKnobURL(r.DashboardBaseURL, r.Agent); knob != "" {
		attrs[AttrModelKnobURL] = knob
	}
	return Payload{
		Transition: TransitionReviewRejected,
		Agent:      r.Agent,
		Repo:       r.Repo,
		Actor:      r.Actor,
		Reason:     r.Reason,
		Backend:    r.Backend,
		Model:      r.Model,
		Pin:        r.Pin,
		ACMMLevel:  r.ACMMLevel,
		Attrs:      attrs,
	}
}

// EmitReviewRejected is the emitter a review surface calls when a human rejects
// output. It builds the payload and fires the transition.
//
// Call it AFTER the rejection is durably recorded, consistent with the
// post-commit rule that applies to every transition.
func EmitReviewRejected(ctx context.Context, d *Dispatcher, r ReviewRejection) {
	if d == nil {
		return
	}
	d.Fire(ctx, NewReviewRejectedPayload(r))
}

// ---------------------------------------------------------------------------
// Notification rendering
// ---------------------------------------------------------------------------

// renderNotification builds the title and body for a notify action.
//
// An operator may override either via params.title / params.message. When they
// do not, the default rendering is transition-aware, and for review_rejected it
// produces the in-context model affordance the RFC asks for: the body names the
// model and pin that produced the rejected output and links its knob directly.
func renderNotification(h Hook, p Payload) (title, message string) {
	title = firstNonEmpty(h.Params["title"], defaultNotificationTitle(p))
	if custom := strings.TrimSpace(h.Params["message"]); custom != "" {
		return title, custom
	}
	return title, defaultNotificationBody(p)
}

// defaultNotificationTitle names the transition in operator-facing terms.
func defaultNotificationTitle(p Payload) string {
	if p.Transition == TransitionReviewRejected {
		if p.Agent != "" {
			return fmt.Sprintf("Review rejected: %s", p.Agent)
		}
		return "Review rejected"
	}
	if p.Agent != "" {
		return fmt.Sprintf("%s: %s", p.Transition, p.Agent)
	}
	return string(p.Transition)
}

// defaultNotificationBody renders the transition's context. For
// review_rejected it leads with the model metadata and the knob link, because
// that is the information the fleet owner needed and could not find.
func defaultNotificationBody(p Payload) string {
	var b strings.Builder

	if p.Transition == TransitionReviewRejected {
		if pr := p.attr(AttrPR); pr != "" && p.Repo != "" {
			fmt.Fprintf(&b, "%s#%s review rejected", p.Repo, pr)
		} else if p.Repo != "" {
			fmt.Fprintf(&b, "%s review rejected", p.Repo)
		} else {
			b.WriteString("Review rejected")
		}
		if p.Actor != "" {
			fmt.Fprintf(&b, " by %s", p.Actor)
		}
		b.WriteString(".\n")
		if p.Reason != "" {
			fmt.Fprintf(&b, "Reason: %s\n", p.Reason)
		}

		// The producing model — the part that makes this actionable.
		if desc := describeModel(p); desc != "" {
			fmt.Fprintf(&b, "Produced by %s\n", desc)
		}
		if knob := p.attr(AttrModelKnobURL); knob != "" {
			fmt.Fprintf(&b, "Adjust the model pin: %s", knob)
		}
		return strings.TrimRight(b.String(), "\n")
	}

	// Generic rendering for every other transition.
	fmt.Fprintf(&b, "Transition: %s", p.Transition)
	if p.From != "" || p.To != "" {
		fmt.Fprintf(&b, "\n%s → %s", orDash(p.From), orDash(p.To))
	}
	if p.Agent != "" {
		fmt.Fprintf(&b, "\nAgent: %s", p.Agent)
	}
	if p.Repo != "" {
		fmt.Fprintf(&b, "\nRepo: %s", p.Repo)
	}
	if p.Trigger != "" {
		fmt.Fprintf(&b, "\nTrigger: %s", p.Trigger)
	}
	if p.Reason != "" {
		fmt.Fprintf(&b, "\nReason: %s", p.Reason)
	}
	return b.String()
}

// describeModel renders the producing model in the form an operator recognizes
// from the admin UI: "backend/model (pin: X)", omitting whatever is unknown.
func describeModel(p Payload) string {
	var parts []string
	switch {
	case p.Backend != "" && p.Model != "":
		parts = append(parts, p.Backend+"/"+p.Model)
	case p.Model != "":
		parts = append(parts, p.Model)
	case p.Backend != "":
		parts = append(parts, p.Backend)
	}
	if len(parts) == 0 {
		return ""
	}
	if p.Pin != "" {
		parts = append(parts, fmt.Sprintf("(pin: %s)", p.Pin))
	}
	return strings.Join(parts, " ")
}

// orDash renders an empty state as "—" so a one-sided transition still reads.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

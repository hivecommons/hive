package extwork

import (
	"context"
	"errors"
	"sync"
)

// Offer is everything a host learns about an assignment BEFORE it accepts. It
// is a summary only: no payload, no credential, no context bundle. That is
// the WebSocket acceptance step of the contributor protocol expressed at the
// transport level so an interactive host (the OMP workbench of #6899) can
// decline without ever having been handed the work.
type Offer struct {
	ExecutionKey ExecutionKey `json:"execution_key"`
	Engine       string       `json:"engine"`
	WorkKey      string       `json:"work_key"`
	AssignmentID string       `json:"assignment_id"`
	Stage        string       `json:"stage"`
	Summary      string       `json:"summary"`
}

// OfferDecision is the host's answer to an Offer.
type OfferDecision string

const (
	OfferAccepted OfferDecision = "accepted"
	OfferDeclined OfferDecision = "declined"
)

// Host decides whether to take an offered assignment. The decision happens
// before Dispatch delivers anything beyond the Offer.
type Host interface {
	Decide(ctx context.Context, offer Offer) (OfferDecision, string, error)
}

// HostFunc adapts a function to Host.
type HostFunc func(ctx context.Context, offer Offer) (OfferDecision, string, error)

// Decide calls f.
func (f HostFunc) Decide(ctx context.Context, offer Offer) (OfferDecision, string, error) {
	return f(ctx, offer)
}

// AutoAcceptHost accepts every offer. It is the non-interactive default that
// matches the relay's auto-accept posture.
var AutoAcceptHost Host = HostFunc(func(context.Context, Offer) (OfferDecision, string, error) {
	return OfferAccepted, "auto-accept", nil
})

// ErrOfferPending is returned by InteractiveHost.Decide when no answer has been
// recorded for the offer and the context ends first.
var ErrOfferPending = errors.New("extwork: offer still pending")

// InteractiveHost is the second-host seam: offers are parked until something
// outside the binding (a workbench, a test) answers them by execution key. It
// holds no work and no credential; a declined or unanswered offer never sees
// the payload.
type InteractiveHost struct {
	mu      sync.Mutex
	pending map[ExecutionKey]chan answer
}

type answer struct {
	decision OfferDecision
	reason   string
}

// NewInteractiveHost returns an empty host.
func NewInteractiveHost() *InteractiveHost {
	return &InteractiveHost{pending: map[ExecutionKey]chan answer{}}
}

// Decide parks the offer until Answer is called for its key or ctx ends.
func (h *InteractiveHost) Decide(ctx context.Context, offer Offer) (OfferDecision, string, error) {
	h.mu.Lock()
	ch, ok := h.pending[offer.ExecutionKey]
	if !ok {
		ch = make(chan answer, 1)
		h.pending[offer.ExecutionKey] = ch
	}
	h.mu.Unlock()
	select {
	case a := <-ch:
		h.mu.Lock()
		delete(h.pending, offer.ExecutionKey)
		h.mu.Unlock()
		return a.decision, a.reason, nil
	case <-ctx.Done():
		return OfferDeclined, ErrOfferPending.Error(), ErrOfferPending
	}
}

// Answer records the decision for an execution key. It may be called before
// or after Decide; a second call for the same key while the first answer is
// unread is dropped so a workbench cannot flip a decision already delivered.
func (h *InteractiveHost) Answer(key ExecutionKey, decision OfferDecision, reason string) {
	h.mu.Lock()
	ch, ok := h.pending[key]
	if !ok {
		ch = make(chan answer, 1)
		h.pending[key] = ch
	}
	h.mu.Unlock()
	select {
	case ch <- answer{decision: decision, reason: reason}:
	default:
	}
}

// Pending lists the execution keys whose offers await an answer.
func (h *InteractiveHost) Pending() []ExecutionKey {
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]ExecutionKey, 0, len(h.pending))
	for k, ch := range h.pending {
		if len(ch) == 0 {
			keys = append(keys, k)
		}
	}
	return keys
}

// ProgressEvent is one transport-level fact recorded to the lease audit: an
// offer decision, a persisted admission, a start, a state change, a cancel
// fact, a receipt verdict. Events are how a workbench renders state without
// polling the receipt (#8361 step 9b). They are not store fields.
type ProgressEvent struct {
	Action       string         `json:"action"`
	ExecutionKey ExecutionKey   `json:"execution_key"`
	AssignmentID string         `json:"assignment_id"`
	State        State          `json:"state,omitempty"`
	Fields       map[string]any `json:"fields,omitempty"`
}

// Audit action names for progress events. They are recorded verbatim as the
// audit action so the audit log can be filtered on the ext_work_ prefix.
const (
	EventOfferAccepted      = "ext_work_offer_accepted"
	EventOfferDeclined      = "ext_work_offer_declined"
	EventAdmissionPersisted = "ext_work_admission_persisted"
	EventStarted            = "ext_work_started"
	EventShadowObserved     = "ext_work_shadow_observed"
	EventProgress           = "ext_work_progress"
	EventRecovered          = "ext_work_recovered"
	EventCancelRequested    = "ext_work_cancel_requested"
	EventReceiptVerified    = "ext_work_receipt_verified"
	EventReceiptRefused     = "ext_work_receipt_refused"
	EventDecision           = "ext_work_decision"
)

// ProgressSink receives progress events. The dashboard backs it with the agent
// audit sink so every event lands on the lease's audit trail.
type ProgressSink interface {
	Record(ev ProgressEvent)
}

// ProgressSinkFunc adapts a function to ProgressSink.
type ProgressSinkFunc func(ev ProgressEvent)

// Record calls f.
func (f ProgressSinkFunc) Record(ev ProgressEvent) { f(ev) }

// MemorySink collects events for tests and for a workbench that renders them.
type MemorySink struct {
	mu     sync.Mutex
	events []ProgressEvent
}

// Record appends the event.
func (m *MemorySink) Record(ev ProgressEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
}

// Events returns a copy of everything recorded so far.
func (m *MemorySink) Events() []ProgressEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ProgressEvent(nil), m.events...)
}

// Actions returns the recorded action names in order.
func (m *MemorySink) Actions() []string {
	events := m.Events()
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Action)
	}
	return out
}

package extwork

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// Binding drives one Adapter under Hive's rules: accept or decline first,
// persist before dispatch, shadow never starts, every state change is a
// progress event, cancellation facts stay separate, receipts are verified
// before they are parsed, and a settled result replays without a second run.
type Binding struct {
	adapter  Adapter
	store    AdmissionStore
	receipts ReceiptStore
	sink     ProgressSink
	mode     string
	limits   FetchLimits
	hooks    Hooks
	now      func() time.Time

	mu   sync.Mutex
	last map[ExecutionKey]State
}

// Hooks are crash-window injection points for tests: each is called at the
// named point and, when it returns an error, the operation stops there exactly
// as a process death would, leaving whatever was already durable in place.
type Hooks struct {
	AfterPersist func() error
	AfterStart   func() error
}

// Option configures a Binding.
type Option func(*Binding)

// WithLimits sets receipt fetch limits.
func WithLimits(l FetchLimits) Option { return func(b *Binding) { b.limits = l } }

// WithHooks installs crash-window hooks.
func WithHooks(h Hooks) Option { return func(b *Binding) { b.hooks = h } }

// WithClock sets the clock used to stamp observations.
func WithClock(now func() time.Time) Option { return func(b *Binding) { b.now = now } }

// Binding-level errors.
var (
	ErrDisabled           = errors.New("extwork: binding is off")
	ErrDeclined           = errors.New("extwork: host declined the offer")
	ErrNoDurableAdmission = errors.New("extwork: no durable admission; refusing to issue new authority")
	ErrPayloadDigest      = errors.New("extwork: payload does not match the admission request digest")
	ErrUncertain          = errors.New("extwork: native state uncertain; not starting")
)

// New builds a Binding. mode must be one of ModeOff, ModeShadow, ModeReportOnly;
// any other value resolves to off, so a setting this build cannot honour never
// selects a posture the operator did not ask for.
func New(adapter Adapter, store AdmissionStore, receipts ReceiptStore, sink ProgressSink, mode string, opts ...Option) *Binding {
	if !ValidMode(mode) {
		mode = ModeOff
	}
	if sink == nil {
		sink = ProgressSinkFunc(func(ProgressEvent) {})
	}
	if receipts == nil {
		receipts = NewMemoryStore()
	}
	b := &Binding{adapter: adapter, store: store, receipts: receipts, sink: sink, mode: mode, now: time.Now, last: map[ExecutionKey]State{}}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Mode returns the operating mode.
func (b *Binding) Mode() string { return b.mode }

func (b *Binding) record(action string, adm Admission, state State, fields map[string]any) {
	b.sink.Record(ProgressEvent{Action: action, ExecutionKey: adm.ExecutionKey(), AssignmentID: adm.AssignmentID, State: state, Fields: fields})
}

// Gate is implemented by adapters that can refuse an admission on Hive's
// side before anything is offered or persisted: a confinement rule such as
// "an unconfined host takes report-only stages only". The binding consults
// it first in Offer and Dispatch, so a refused admission never produces a
// frame to the host; the error is returned as-is, not as a host decline.
type Gate interface {
	Admit(adm Admission) error
}

// gate applies the adapter's Gate, if any, and records a refusal as an
// offer decline so the audit shows why nothing was offered.
func (b *Binding) gate(adm Admission) error {
	g, ok := b.adapter.(Gate)
	if !ok {
		return nil
	}
	if err := g.Admit(adm); err != nil {
		b.record(EventOfferDeclined, adm, "", map[string]any{"reason": err.Error(), "gate": true})
		return err
	}
	return nil
}

// Offer presents the assignment summary to the host and records the decision.
// Nothing beyond the Offer is delivered before the host accepts, and nothing
// at all is sent when the adapter's Gate refuses the admission.
func (b *Binding) Offer(ctx context.Context, host Host, adm Admission, summary string) (OfferDecision, error) {
	if b.mode == ModeOff {
		return OfferDeclined, ErrDisabled
	}
	if err := adm.Validate(); err != nil {
		return OfferDeclined, err
	}
	if err := b.gate(adm); err != nil {
		return OfferDeclined, err
	}
	if host == nil {
		host = AutoAcceptHost
	}
	offer := Offer{ExecutionKey: adm.ExecutionKey(), Engine: adm.Engine, WorkKey: adm.WorkKey, AssignmentID: adm.AssignmentID, Stage: adm.Stage, Summary: summary}
	decision, reason, err := host.Decide(ctx, offer)
	if err != nil {
		b.record(EventOfferDeclined, adm, "", map[string]any{"reason": err.Error()})
		return OfferDeclined, fmt.Errorf("%w: %v", ErrDeclined, err)
	}
	if decision != OfferAccepted {
		b.record(EventOfferDeclined, adm, "", map[string]any{"reason": reason})
		return OfferDeclined, fmt.Errorf("%w: %s", ErrDeclined, reason)
	}
	b.record(EventOfferAccepted, adm, "", map[string]any{"reason": reason})
	return OfferAccepted, nil
}

// DispatchResult says what Dispatch did. Started is false in shadow mode.
type DispatchResult struct {
	Started bool
	Shadow  bool
	Run     StartResult
}

// Dispatch persists the admission, then starts the keyed run. In shadow mode
// it persists and records an observation-only event but performs no external
// start. Persist failure stops everything before any dispatch.
func (b *Binding) Dispatch(ctx context.Context, adm Admission, payload []byte) (DispatchResult, error) {
	if b.mode == ModeOff {
		return DispatchResult{}, ErrDisabled
	}
	if err := adm.Validate(); err != nil {
		return DispatchResult{}, err
	}
	if adm.RequestDigest != RequestDigest(payload) {
		return DispatchResult{}, ErrPayloadDigest
	}
	if err := b.gate(adm); err != nil {
		return DispatchResult{}, err
	}
	// A durable admission already on record for this assignment is the
	// authority; a second dispatch may repeat it (same key, same payload) or
	// replace it with a NEW execution identity (a new generation moves the
	// key), but the same key with a different payload is a conflict here,
	// before the engine is asked and before the record is touched.
	existing, ok, err := b.store.Load(adm.AssignmentID)
	if err != nil {
		return DispatchResult{}, fmt.Errorf("load admission: %w", err)
	}
	if ok && existing.ExecutionKey() == adm.ExecutionKey() {
		if existing.RequestDigest != adm.RequestDigest {
			return DispatchResult{}, fmt.Errorf("%w: assignment %s is already admitted under this key with a different payload", ErrConflict, adm.AssignmentID)
		}
		if adm.EngineIncarnation == "" {
			adm.EngineIncarnation = existing.EngineIncarnation
		}
		adm.RemoteRunID = existing.RemoteRunID
	}
	if pinner, ok := b.adapter.(Pinner); ok && adm.EngineIncarnation == "" {
		incarnation, err := pinner.Incarnation(ctx)
		if err != nil {
			return DispatchResult{}, err
		}
		adm.EngineIncarnation = incarnation
	}
	if err := b.store.Persist(adm); err != nil {
		return DispatchResult{}, fmt.Errorf("persist admission before dispatch: %w", err)
	}
	b.record(EventAdmissionPersisted, adm, "", map[string]any{"request_digest": adm.RequestDigest, "mode": b.mode, "engine_incarnation": adm.EngineIncarnation})
	if b.hooks.AfterPersist != nil {
		if err := b.hooks.AfterPersist(); err != nil {
			return DispatchResult{}, err
		}
	}
	if b.mode == ModeShadow {
		b.record(EventShadowObserved, adm, "", map[string]any{"external_start": false})
		return DispatchResult{Shadow: true}, nil
	}
	return b.start(ctx, adm, payload)
}

// Admission returns the admission as persisted for the assignment, which is
// the caller's admission plus any engine incarnation pinned at dispatch.
func (b *Binding) Admission(assignmentID string) (Admission, bool, error) {
	return b.store.Load(assignmentID)
}

// incarnationFor resolves the incarnation to observe under: an explicit value
// wins, otherwise the one pinned in the admission.
func incarnationFor(adm Admission, explicit string) string {
	if explicit != "" {
		return explicit
	}
	return adm.EngineIncarnation
}

func (b *Binding) start(ctx context.Context, adm Admission, payload []byte) (DispatchResult, error) {
	run, err := b.adapter.Start(ctx, StartRequest{Admission: adm, Payload: payload})
	if err != nil {
		return DispatchResult{}, err
	}
	b.record(EventStarted, adm, StateAccepted, map[string]any{"remote_run_id": run.RemoteRunID, "remote_incarnation": run.RemoteIncarnation, "deduplicated": run.Deduplicated})
	if b.hooks.AfterStart != nil {
		if err := b.hooks.AfterStart(); err != nil {
			return DispatchResult{Started: true, Run: run}, err
		}
	}
	if err := b.bindRun(adm, run); err != nil {
		return DispatchResult{Started: true, Run: run}, err
	}
	return DispatchResult{Started: true, Run: run}, nil
}

// bindRun records the native run identity on the durable admission so a later
// recovery can tell a dispatched run from one that never left. A failure here
// is the "after remote accept, before record" window: the run exists, the
// record does not say so, and Recover resolves it by asking the engine.
func (b *Binding) bindRun(adm Admission, run StartResult) error {
	adm.RemoteRunID = run.RemoteRunID
	if run.RemoteIncarnation != "" {
		adm.EngineIncarnation = run.RemoteIncarnation
	}
	if err := b.store.Persist(adm); err != nil {
		return fmt.Errorf("record native run after start: %w", err)
	}
	return nil
}

// Observe reads the native state and records a progress event whenever the
// state differs from the last one recorded for the key. A transport failure
// yields StateUnknown and ErrTransport; it is never turned into a failure.
func (b *Binding) Observe(ctx context.Context, adm Admission, incarnation string) (Observation, error) {
	if b.mode == ModeOff {
		return Observation{State: StateUnknown}, ErrDisabled
	}
	obs, err := b.adapter.Observe(ctx, adm.ExecutionKey(), incarnationFor(adm, incarnation))
	if err != nil {
		obs = Observation{State: StateUnknown, Detail: err.Error()}
	}
	if obs.ObservedAt.IsZero() {
		obs.ObservedAt = b.now()
	}
	if !ValidState(obs.State) {
		obs.State = StateUnknown
	}
	b.progress(adm, obs)
	return obs, err
}

func (b *Binding) progress(adm Admission, obs Observation) {
	key := adm.ExecutionKey()
	b.mu.Lock()
	changed := b.last[key] != obs.State
	b.last[key] = obs.State
	b.mu.Unlock()
	if !changed {
		return
	}
	fields := map[string]any{"stage": obs.Stage, "remote_run_id": obs.RemoteRunID, "detail": obs.Detail}
	if obs.ResultClass != "" {
		fields["result_class"] = obs.ResultClass
	}
	b.record(EventProgress, adm, obs.State, fields)
}

// RecoverResult reports how a restart resolved the start-ack window.
type RecoverResult struct {
	// Adopted is true when a native run already existed for the key.
	Adopted bool
	// Started is true when no native run existed and a keyed start was issued.
	Started bool
	// Uncertain is true when the engine could not be reached; nothing was
	// started and the run stays visibly unknown.
	Uncertain   bool
	Observation Observation
	Run         StartResult
}

// Recover resolves the ambiguous windows after a process death: before start,
// after remote acceptance, or after receipt persistence. It refuses to act
// without a durable admission, adopts an existing native run by execution
// key and pinned incarnation, and only issues a keyed (idempotent) start when
// the engine positively reports no run. A transport failure starts nothing.
// A recreated engine is never adopted: with a recorded native run the
// operation is uncertain; without one the admission is re-pinned and started.
func (b *Binding) Recover(ctx context.Context, adm Admission, payload []byte) (RecoverResult, error) {
	if b.mode == ModeOff {
		return RecoverResult{}, ErrDisabled
	}
	stored, ok, err := b.store.Load(adm.AssignmentID)
	if err != nil {
		return RecoverResult{}, fmt.Errorf("load admission: %w", err)
	}
	if !ok || stored.ExecutionKey() != adm.ExecutionKey() || stored.RequestDigest != adm.RequestDigest {
		return RecoverResult{}, ErrNoDurableAdmission
	}
	// The durable record, not the caller, says which engine incarnation was
	// pinned and whether a native run was ever recorded; a caller cannot
	// widen recovery by omitting either.
	adm.EngineIncarnation = stored.EngineIncarnation
	adm.RemoteRunID = stored.RemoteRunID
	if raw, ok, _ := b.receipts.LoadReceipt(adm.AssignmentID); ok && len(raw) > 0 {
		b.record(EventRecovered, adm, StateTerminal, map[string]any{"window": "after_receipt_persistence"})
		return RecoverResult{Adopted: true, Observation: Observation{State: StateTerminal, ObservedAt: b.now(), Detail: "settled receipt on record"}}, nil
	}
	obs, err := b.adapter.Observe(ctx, adm.ExecutionKey(), adm.EngineIncarnation)
	switch {
	case err == nil:
		run := StartResult{RemoteRunID: obs.RemoteRunID, RemoteIncarnation: obs.RemoteIncarnation, Deduplicated: true}
		b.record(EventRecovered, adm, obs.State, map[string]any{"window": "after_remote_accept", "remote_run_id": obs.RemoteRunID})
		b.progress(adm, obs)
		if adm.RemoteRunID == "" {
			if err := b.bindRun(adm, run); err != nil {
				return RecoverResult{Adopted: true, Observation: obs, Run: run}, err
			}
		}
		return RecoverResult{Adopted: true, Observation: obs, Run: run}, nil
	case errors.Is(err, ErrNotFound):
		if b.mode == ModeShadow {
			b.record(EventShadowObserved, adm, StateUnknown, map[string]any{"external_start": false, "window": "before_start"})
			return RecoverResult{Observation: Observation{State: StateUnknown, ObservedAt: b.now()}}, nil
		}
		if adm.RequestDigest != RequestDigest(payload) {
			return RecoverResult{}, ErrPayloadDigest
		}
		res, err := b.start(ctx, adm, payload)
		if err != nil {
			return RecoverResult{}, err
		}
		b.record(EventRecovered, adm, StateAccepted, map[string]any{"window": "before_start", "remote_run_id": res.Run.RemoteRunID})
		return RecoverResult{Started: true, Run: res.Run, Observation: Observation{State: StateAccepted, RemoteRunID: res.Run.RemoteRunID, RemoteIncarnation: res.Run.RemoteIncarnation, ObservedAt: b.now()}}, nil
	case errors.Is(err, ErrIncarnationMismatch):
		// The engine was deleted and recreated (or is a different instance)
		// since admission: whatever it holds under this key is unrelated
		// work and is never adopted. If the durable record says a native run
		// was accepted, that run is lost with the old instance and the
		// operation stays uncertain. If it says no run was ever recorded,
		// nothing is outstanding: re-pin to the live instance and start.
		if adm.RemoteRunID != "" {
			b.record(EventRecovered, adm, StateUnknown, map[string]any{"window": "incarnation_mismatch", "recorded_run": adm.RemoteRunID, "error": err.Error()})
			return RecoverResult{Uncertain: true, Observation: Observation{State: StateUnknown, Detail: err.Error(), ObservedAt: b.now()}}, fmt.Errorf("%w: %w", ErrUncertain, err)
		}
		if b.mode == ModeShadow {
			b.record(EventShadowObserved, adm, StateUnknown, map[string]any{"external_start": false, "window": "incarnation_mismatch"})
			return RecoverResult{Observation: Observation{State: StateUnknown, ObservedAt: b.now()}}, nil
		}
		if adm.RequestDigest != RequestDigest(payload) {
			return RecoverResult{}, ErrPayloadDigest
		}
		adm.EngineIncarnation = ""
		if pinner, ok := b.adapter.(Pinner); ok {
			incarnation, err := pinner.Incarnation(ctx)
			if err != nil {
				return RecoverResult{}, err
			}
			adm.EngineIncarnation = incarnation
		}
		if err := b.store.Persist(adm); err != nil {
			return RecoverResult{}, fmt.Errorf("re-pin admission to the live engine: %w", err)
		}
		res, err := b.start(ctx, adm, payload)
		if err != nil {
			return RecoverResult{}, err
		}
		b.record(EventRecovered, adm, StateAccepted, map[string]any{"window": "incarnation_mismatch_not_started", "remote_run_id": res.Run.RemoteRunID})
		return RecoverResult{Started: true, Run: res.Run, Observation: Observation{State: StateAccepted, RemoteRunID: res.Run.RemoteRunID, RemoteIncarnation: res.Run.RemoteIncarnation, ObservedAt: b.now()}}, nil
	default:
		b.record(EventRecovered, adm, StateUnknown, map[string]any{"window": "uncertain", "error": err.Error()})
		return RecoverResult{Uncertain: true, Observation: Observation{State: StateUnknown, Detail: err.Error(), ObservedAt: b.now()}}, fmt.Errorf("%w: %w", ErrUncertain, err)
	}
}

// Cancel requests cancellation and records exactly what the engine confirmed.
// Stopped is only ever what the adapter reported; the binding never infers it.
func (b *Binding) Cancel(ctx context.Context, adm Admission, incarnation string) (CancelFacts, error) {
	if b.mode == ModeOff {
		return CancelFacts{}, ErrDisabled
	}
	facts, err := b.adapter.Cancel(ctx, adm.ExecutionKey(), incarnationFor(adm, incarnation))
	fields := map[string]any{"requested": facts.Requested, "acknowledged": facts.Acknowledged, "stopped": facts.Stopped, "detail": facts.Detail}
	if err != nil {
		fields["error"] = err.Error()
	}
	b.record(EventCancelRequested, adm, "", fields)
	return facts, err
}

// FetchReceipt verifies and binds the referenced receipt, records the verdict
// of that verification, and stores the raw bytes for replay on success.
func (b *Binding) FetchReceipt(ctx context.Context, adm Admission, incarnation string, ref ReceiptRef) (*outputschema.StageReceipt, error) {
	if b.mode == ModeOff {
		return nil, ErrDisabled
	}
	receipt, raw, err := FetchReceipt(ctx, b.adapter, adm, incarnationFor(adm, incarnation), ref, b.limits)
	if err != nil {
		b.record(EventReceiptRefused, adm, "", map[string]any{"path": ref.Path, "error": err.Error()})
		return nil, err
	}
	if err := b.receipts.SaveReceipt(adm.AssignmentID, raw); err != nil {
		b.record(EventReceiptRefused, adm, "", map[string]any{"path": ref.Path, "error": err.Error()})
		return nil, fmt.Errorf("persist receipt: %w", err)
	}
	b.record(EventReceiptVerified, adm, StateTerminal, map[string]any{"path": ref.Path, "digest": ref.Digest, "result_class": string(receipt.ResultClass), "remote_run_id": receipt.RemoteRunID})
	return receipt, nil
}

// Replay rehydrates the settled receipt from the receipt store. It performs no
// external call; ok is false when nothing is on record.
func (b *Binding) Replay(adm Admission) (*outputschema.StageReceipt, bool, error) {
	raw, ok, err := b.receipts.LoadReceipt(adm.AssignmentID)
	if err != nil || !ok {
		return nil, false, err
	}
	receipt, err := ParseReceipt(raw)
	if err != nil {
		return nil, true, err
	}
	if err := BindReceipt(adm, receipt); err != nil {
		return nil, true, err
	}
	return receipt, true, nil
}

// Decide applies Hive's acceptance step to a verified receipt and records the
// verdict as a progress event.
func (b *Binding) Decide(adm Admission, receipt *outputschema.StageReceipt, predicate Predicate, authorityCurrent bool) Decision {
	d := Decide(adm, receipt, predicate, authorityCurrent)
	b.record(EventDecision, adm, "", map[string]any{"verdict": string(d.Verdict), "reason": d.Reason, "execution_fact": d.ExecutionFact})
	return d
}

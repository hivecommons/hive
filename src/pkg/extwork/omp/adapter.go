package omp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/extwork"
)

const (
	// SettingWorkflowVersion is the registry setting naming the workbench
	// workflow version the adapter pins.
	SettingWorkflowVersion = extwork.SettingWorkflowVersion

	// DefaultOfferTimeout bounds how long an offer waits for the workbench to
	// answer before it counts as declined.
	DefaultOfferTimeout = 10 * time.Minute
	// DefaultAckTimeout bounds how long a start or cancel waits for its
	// acknowledgement.
	DefaultAckTimeout = 30 * time.Second
)

// WriteCapableStages names lease stages that imply a repository or host
// mutation. OMP is tier T3 (unconfined), so a stage in this set is refused
// for an ext-exec/omp peer regardless of mode; only report-only stages may be
// bound to it. The three Hive-owned run stages (spec, plan, implement) are
// report-only under this binding because the receipt carries artifacts and
// publication stays Hive-side.
var WriteCapableStages = map[string]bool{
	"publish": true,
	"merge":   true,
	"release": true,
	"deploy":  true,
	"push":    true,
}

// ErrWriteCapableStage is the refusal for a stage or mode that could write.
var ErrWriteCapableStage = fmt.Errorf("%w: OMP is unconfined (T3); only report-only stages may be bound to it", extwork.ErrRefused)

// CheckReportOnly is the confinement assertion: an admission may be bound to
// an OMP host only when its mode is report-only or shadow and its stage is
// not write-capable. It is applied before an offer is made and again at
// Start, so a caller cannot reach the workbench by skipping the offer.
func CheckReportOnly(adm extwork.Admission) error {
	switch adm.Authority.Mode {
	case extwork.ModeReportOnly, extwork.ModeShadow:
	default:
		return fmt.Errorf("%w: mode %q", ErrWriteCapableStage, adm.Authority.Mode)
	}
	if WriteCapableStages[strings.ToLower(strings.TrimSpace(adm.Stage))] {
		return fmt.Errorf("%w: stage %q", ErrWriteCapableStage, adm.Stage)
	}
	return nil
}

// Config configures an Adapter.
type Config struct {
	// Peers resolves the attached workbench for an admission's identity.
	// Nil means DefaultBroker.
	Peers PeerSource
	// WorkflowVersion is the workbench workflow version the peer must
	// declare; a different version is refused, never downgraded.
	WorkflowVersion string
	// OfferTimeout and AckTimeout override the defaults (tests).
	OfferTimeout time.Duration
	AckTimeout   time.Duration
}

// Adapter implements extwork.Adapter and, per identity, extwork.Host over
// attached OMP workbench peers.
type Adapter struct {
	peers        PeerSource
	version      string
	offerTimeout time.Duration
	ackTimeout   time.Duration

	mu sync.Mutex
	// bound maps every execution key this adapter started to the identity
	// and incarnation it was started on, so Observe and Cancel never reach a
	// different workbench than the one that holds the run.
	bound map[extwork.ExecutionKey]binding
}

type binding struct {
	identity    string
	incarnation string
}

// New validates the configuration and builds an adapter.
func New(cfg Config) (*Adapter, error) {
	if strings.TrimSpace(cfg.WorkflowVersion) == "" {
		return nil, errors.New("omp workflow version is required")
	}
	if cfg.Peers == nil {
		cfg.Peers = DefaultBroker
	}
	if cfg.OfferTimeout <= 0 {
		cfg.OfferTimeout = DefaultOfferTimeout
	}
	if cfg.AckTimeout <= 0 {
		cfg.AckTimeout = DefaultAckTimeout
	}
	return &Adapter{peers: cfg.Peers, version: strings.TrimSpace(cfg.WorkflowVersion), offerTimeout: cfg.OfferTimeout, ackTimeout: cfg.AckTimeout, bound: map[extwork.ExecutionKey]binding{}}, nil
}

// Factory is the extwork.Registry constructor. Settings carry the workflow
// version only; peers come from DefaultBroker, which the hub populates.
func Factory(settings map[string]string) (extwork.Adapter, error) {
	return New(Config{WorkflowVersion: settings[SettingWorkflowVersion]})
}

// Engine returns "omp".
func (a *Adapter) Engine() string { return Engine }

// WorkflowVersion returns the pinned version.
func (a *Adapter) WorkflowVersion() string { return a.version }

// peer resolves and gates the workbench for identity: it must be attached,
// still declare Capability, and run the pinned workflow version.
func (a *Adapter) peer(identity string) (*Peer, error) {
	p, ok := a.peers.Peer(identity)
	if !ok {
		return nil, fmt.Errorf("%w: no OMP workbench attached for %q", extwork.ErrTransport, identity)
	}
	if !p.caps.Declares(Capability) {
		return nil, ErrCapabilityMissing
	}
	if p.version != a.version {
		return nil, fmt.Errorf("%w: workbench runs %q, pinned %q", extwork.ErrRefused, p.version, a.version)
	}
	return p, nil
}

// Admit implements extwork.Gate: the binding calls it before any offer is
// made or any admission persisted, so a write-capable stage, a non
// report-only mode, or an authority without the capability is refused on
// Hive's side with no frame reaching the workbench. The same checks are
// repeated defensively in Host and Start.
func (a *Adapter) Admit(adm extwork.Admission) error {
	if err := CheckReportOnly(adm); err != nil {
		return err
	}
	if adm.Authority.Capability != Capability {
		return ErrCapabilityMissing
	}
	return nil
}

// Host returns the accept-or-decline step for the workbench held by
// identity. Binding.Offer calls it with the summary; the workbench sees the
// Offer fields and nothing else until it accepts. The Admit checks are
// repeated here so a caller that bypasses the binding is still refused
// before any frame is sent.
func (a *Adapter) Host(identity string, adm extwork.Admission) extwork.Host {
	return extwork.HostFunc(func(ctx context.Context, offer extwork.Offer) (extwork.OfferDecision, string, error) {
		if err := a.Admit(adm); err != nil {
			return extwork.OfferDeclined, err.Error(), err
		}
		if adm.Authority.Mode == extwork.ModeShadow {
			// Shadow observes configured state only: no frame reaches the
			// workbench, so the admission can be persisted and audited
			// without the host ever seeing an offer.
			return extwork.OfferAccepted, "shadow: offer not delivered to the workbench", nil
		}
		p, err := a.peer(identity)
		if err != nil {
			return extwork.OfferDeclined, err.Error(), err
		}
		ctx, cancel := context.WithTimeout(ctx, a.offerTimeout)
		defer cancel()
		return p.Offer(ctx, offer, adm.Generation)
	})
}

// Start delivers the bundle to the workbench that accepted the offer. It
// refuses an admission whose authority lacks Capability, whose engine is not
// omp, whose version is not the pinned one, or whose stage could write.
func (a *Adapter) Start(ctx context.Context, req extwork.StartRequest) (extwork.StartResult, error) {
	adm := req.Admission
	if err := adm.Validate(); err != nil {
		return extwork.StartResult{}, err
	}
	if adm.Authority.Capability != Capability {
		return extwork.StartResult{}, fmt.Errorf("%w: authority capability %q is not %s", extwork.ErrRefused, adm.Authority.Capability, Capability)
	}
	if adm.Engine != Engine {
		return extwork.StartResult{}, fmt.Errorf("%w: admission engine %q is not %s", extwork.ErrRefused, adm.Engine, Engine)
	}
	if adm.WorkflowVersion != a.version {
		return extwork.StartResult{}, fmt.Errorf("%w: admission workflow version %q is not the pinned %q", extwork.ErrRefused, adm.WorkflowVersion, a.version)
	}
	if err := CheckReportOnly(adm); err != nil {
		return extwork.StartResult{}, err
	}
	if adm.RequestDigest != extwork.RequestDigest(req.Payload) {
		return extwork.StartResult{}, extwork.ErrPayloadDigest
	}
	p, err := a.peer(adm.Authority.Identity)
	if err != nil {
		return extwork.StartResult{}, err
	}
	if adm.EngineIncarnation != "" && adm.EngineIncarnation != p.incarnation {
		return extwork.StartResult{}, fmt.Errorf("%w: workbench incarnation %q, pinned %q", extwork.ErrIncarnationMismatch, p.incarnation, adm.EngineIncarnation)
	}
	key := adm.ExecutionKey()
	ctx, cancel := context.WithTimeout(ctx, a.ackTimeout)
	defer cancel()
	res, err := p.Start(ctx, key, adm.Generation, adm.Stage, req.Payload)
	if err != nil {
		return extwork.StartResult{}, err
	}
	a.mu.Lock()
	a.bound[key] = binding{identity: adm.Authority.Identity, incarnation: p.incarnation}
	a.mu.Unlock()
	return res, nil
}

// resolve finds the workbench holding key. A key this adapter never started
// is ErrNotFound (a restart clears the map, and the keyed Start is idempotent
// at the workbench). A pinned incarnation that no longer matches the live
// peer is ErrIncarnationMismatch; a peer that is gone is ErrPeerGone.
func (a *Adapter) resolve(key extwork.ExecutionKey, incarnation string) (*Peer, error) {
	a.mu.Lock()
	b, ok := a.bound[key]
	a.mu.Unlock()
	if !ok {
		return nil, extwork.ErrNotFound
	}
	p, ok := a.peers.Peer(b.identity)
	if !ok {
		return nil, ErrPeerGone
	}
	want := incarnation
	if want == "" {
		want = b.incarnation
	}
	if want != "" && p.incarnation != want {
		return nil, fmt.Errorf("%w: workbench incarnation %q, pinned %q", extwork.ErrIncarnationMismatch, p.incarnation, want)
	}
	return p, nil
}

// Observe reads the last reported state for key.
func (a *Adapter) Observe(_ context.Context, key extwork.ExecutionKey, incarnation string) (extwork.Observation, error) {
	p, err := a.resolve(key, incarnation)
	if err != nil {
		return extwork.Observation{State: extwork.StateUnknown, Detail: err.Error()}, err
	}
	return p.Observe(key)
}

// Cancel requests cancellation and reports exactly what the workbench
// acknowledged. Stopped is never inferred.
func (a *Adapter) Cancel(ctx context.Context, key extwork.ExecutionKey, incarnation string) (extwork.CancelFacts, error) {
	p, err := a.resolve(key, incarnation)
	if err != nil {
		return extwork.CancelFacts{Detail: err.Error()}, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.ackTimeout)
	defer cancel()
	return p.Cancel(ctx, key)
}

// OpenArtifact streams an artifact the workbench published for key.
func (a *Adapter) OpenArtifact(_ context.Context, key extwork.ExecutionKey, incarnation, path string) (io.ReadCloser, error) {
	clean, err := extwork.CleanArtifactPath(path)
	if err != nil {
		return nil, err
	}
	p, err := a.resolve(key, incarnation)
	if err != nil {
		return nil, err
	}
	return p.OpenArtifact(key, clean)
}
